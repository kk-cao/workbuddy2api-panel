package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSplitConcatenatedJSON(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		want  []string
		parts int
	}{
		{
			name:  "两个对象空格分隔",
			in:    `{"id":"a","choices":[]} {"id":"b","choices":[]}`,
			parts: 2,
			want:  []string{`{"id":"a","choices":[]}`, `{"id":"b","choices":[]}`},
		},
		{
			name:  "两个对象无分隔",
			in:    `{"id":"a"}{"id":"b"}`,
			parts: 2,
			want:  []string{`{"id":"a"}`, `{"id":"b"}`},
		},
		{
			name:  "三个对象含换行分隔",
			in:    "{\"i\":1}\n{\"i\":2}\n{\"i\":3}",
			parts: 3,
			want:  []string{`{"i":1}`, `{"i":2}`, `{"i":3}`},
		},
		{
			name:  "字符串里的花括号与转义引号不能算结构",
			in:    `{"t":"}{ \"x\" }"} {"t":"ok"}`,
			parts: 2,
			want:  []string{`{"t":"}{ \"x\" }"}`, `{"t":"ok"}`},
		},
		{
			name:  "数组与对象混排",
			in:    `[1,2][{"a":1}]`,
			parts: 2,
			want:  []string{`[1,2]`, `[{"a":1}]`},
		},
		{name: "单个对象不拆", in: `{"id":"a"}`, parts: 1},
		{name: "半截对象不拆", in: `{"id":"a"} {"id":`, parts: 1},
		{name: "括号不配平不拆", in: `{"id":"a"}}}}`, parts: 1},
		{name: "纯文本不拆", in: `hello world`, parts: 1},
		{name: "空串", in: ``, parts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SplitConcatenatedJSON(tc.in)
			if len(got) != tc.parts {
				t.Fatalf("拆出 %d 份 want %d：%q", len(got), tc.parts, got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("第 %d 份 = %q want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestStreamSplitsGluedFrames(t *testing.T) {
	glued := `data: {"id":"cmb-1","object":"chat.completion.chunk","model":"glm-5.3-flash","choices":[{"index":0,"delta":{"reasoning_content":"User"},"finish_reason":""}]}` +
		` {"id":"cmb-1","object":"chat.completion.chunk","model":"glm-5.3-flash","choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":"stop"}]}` + "\n\n"
	upstreamBody := glued + "data: [DONE]\n\n"

	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(upstreamBody), nil); err != nil {
		t.Fatalf("StreamHint: %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "\n\ndata: ") {
		t.Fatalf("帧之间没有空行分隔：%q", out)
	}
	s := strings.ReplaceAll(out, "\r\n", "\n")
	dataEvents := 0
	for _, ev := range strings.Split(s, "\n\n") {
		if strings.TrimSpace(ev) == "" {
			continue
		}
		for _, l := range strings.Split(ev, "\n") {
			if !strings.HasPrefix(l, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(l, "data:"))
			if payload == "[DONE]" {
				continue
			}
			dataEvents++
			if !json.Valid([]byte(payload)) || strings.Contains(payload, "}{") {
				t.Fatalf("帧不是单个合法 JSON：%q", payload)
			}
		}
	}
	if dataEvents < 2 {
		t.Fatalf("应至少拆出 2 个内容帧，实际 %d：%q", dataEvents, out)
	}
	if !strings.Contains(out, `"reasoning_content":"User"`) || !strings.Contains(out, `"content":"你好"`) {
		t.Fatalf("拆开后内容丢失：%q", out)
	}
	if c := strings.Count(out, "data: [DONE]"); c != 1 {
		t.Fatalf("[DONE] 应为 1 个，实际 %d：%q", c, out)
	}
}

func TestStreamKeepsUnparsablePayload(t *testing.T) {
	body := "data: {\"broken\":\n\n" + "data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	_ = StreamHint(rec, strings.NewReader(body), nil)
	if !strings.Contains(rec.Body.String(), `data: {"broken":`) {
		t.Fatalf("不可解析的帧必须原样透传：%q", rec.Body.String())
	}
}

func TestAggregateSplitsGluedFrames(t *testing.T) {
	a := `{"id":"a","choices":[{"index":0,"delta":{"content":"你"}}]}`
	b := `{"id":"a","choices":[{"index":0,"delta":{"content":"好"},"finish_reason":"stop"}]}`
	resp, err := Aggregate(strings.NewReader("data: " + a + " " + b + "\n\ndata: [DONE]\n\n"))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Fatalf("粘帧内容丢失，content=%q", msg["content"])
	}
}
