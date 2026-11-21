package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGuardEmptyTextPartsArray(t *testing.T) {
	in := `{
		"model":"kimi-k3.1-pro",
		"messages":[
			{"role":"user","content":[{"type":"text","text":""},{"type":"text","text":null},{"type":"text"},{"type":"text","text":"   "},{"type":"textarea","text":""},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}
		]
	}`
	out := PrepareBodyOptWithEffortsAndDefault([]byte(in), false, nil, nil)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	parts := obj["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if parts[0].(map[string]any)["text"] != " " {
		t.Errorf("empty string text not filled: %v", parts[0])
	}
	if parts[1].(map[string]any)["text"] != " " {
		t.Errorf("null text not filled: %v", parts[1])
	}
	if parts[2].(map[string]any)["text"] != " " {
		t.Errorf("missing text not filled: %v", parts[2])
	}
	if parts[3].(map[string]any)["text"] != "   " {
		t.Errorf("whitespace-only text altered: %v", parts[3])
	}
	if parts[4].(map[string]any)["text"] != "" {
		t.Errorf("non-text part (textarea) must stay untouched: %v", parts[4])
	}
}

func TestGuardEmptyTextPartsSniffedJSONStringLossless(t *testing.T) {
	rawToolStr := "  \n[{\"big\":9007199254740993,\"html\":\"<a>&</a>\",\"type\":\"text\",\"text\":\"\"},{\"type\":\"text\",\"text\":null},{\"type\":\"text\",\"z\":1}]"
	payload := map[string]any{
		"model": "kimi-k3.1-pro",
		"messages": []any{
			map[string]any{"role": "tool", "content": rawToolStr},
		},
	}
	raw, _ := json.Marshal(payload)
	out := PrepareBodyOptWithEffortsAndDefault(raw, true, nil, nil)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	gotStr := obj["messages"].([]any)[0].(map[string]any)["content"].(string)
	// 验证字节级保留大整数、HTML 字符与键序，只把空/null/缺 text 补成空格
	if !strings.Contains(gotStr, "9007199254740993") {
		t.Errorf("large integer lost precision: %s", gotStr)
	}
	if !strings.Contains(gotStr, `"html":"<a>&</a>"`) {
		t.Errorf("HTML chars escaped or altered: %s", gotStr)
	}
	if strings.Contains(gotStr, `"text":""`) || strings.Contains(gotStr, `"text":null`) {
		t.Errorf("empty text inside sniffed JSON string not filled: %s", gotStr)
	}
	if !strings.Contains(gotStr, `"z":1,"text":" "`) {
		t.Errorf("missing text key not inserted cleanly: %s", gotStr)
	}
}

func TestGuardEmptyTextPartsAfterSanitizeHeaderOnly(t *testing.T) {
	in := `{"model":"glm-5.2","messages":[{"role":"system","content":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1; cc_entrypoint=cli;"}]}]}`
	out := PrepareBodyOptWithEffortsAndDefault([]byte(in), true, nil, nil)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	p0 := obj["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if p0["text"] != " " {
		t.Errorf("text cleared by sanitize should be refilled with single space to prevent 11151, got %q", p0["text"])
	}
}
