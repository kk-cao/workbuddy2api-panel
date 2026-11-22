package upstream

import (
	"encoding/json"
	"testing"
)

func TestMirrorUpstreamReasoning(t *testing.T) {
	cases := []struct {
		name  string
		model string
		want  bool
	}{
		{"glm", "glm-5.3-flash", true},
		{"prefixed global glm", "global:glm-5.3-flash", true},
		{"kimi", "kimi-k2.5", true},
		{"minimax", "minimax-m3.1-flash", true},
		{"mimo", "mimo-v2-pro", true},
		{"step", "step-3.5-flash", true},
		{"space-bunny", "space-bunny-pro", true},
		{"deepseek untouched", "deepseek-v4.1-flash", false},
		{"gpt untouched", "gpt-5.6-sol", false},
	}
	for _, c := range cases {
		in := map[string]any{
			"model": c.model,
			"messages": []any{
				map[string]any{"role": "user", "content": "hi", "reasoning_content": "should-not-mirror"},
				map[string]any{"role": "assistant", "content": "a", "reasoning_content": "step-by-step"},
				map[string]any{"role": "assistant", "content": "b", "reasoning_content": "new-rc", "reasoning": "existing"},
				map[string]any{"role": "assistant", "content": "c", "reasoning_content": "   "},
			},
		}
		mirrorUpstreamReasoning(in)
		msgs := in["messages"].([]any)
		if _, ok := msgs[0].(map[string]any)["reasoning"]; ok {
			t.Errorf("%s: user message must not receive reasoning", c.name)
		}
		got, has := msgs[1].(map[string]any)["reasoning"].(string)
		if c.want {
			if !has || got != "step-by-step" {
				t.Errorf("%s: assistant reasoning=%q want step-by-step", c.name, got)
			}
		} else if has {
			t.Errorf("%s: non-target model must not mirror reasoning, got %q", c.name, got)
		}
		if msgs[2].(map[string]any)["reasoning"] != "existing" {
			t.Errorf("%s: existing reasoning overwritten: %v", c.name, msgs[2])
		}
		if _, blankMirrored := msgs[3].(map[string]any)["reasoning"]; blankMirrored {
			t.Errorf("%s: blank reasoning_content must not mirror placeholder", c.name)
		}
	}

	// Pipeline check via PrepareBodyOpt: glm mirrors non-empty rc
	body := `{"model":"cn:glm-5.3-flash","messages":[{"role":"assistant","content":"ok","reasoning_content":"trace-1"}]}`
	out := PrepareBodyOpt([]byte(body), false)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msg := obj["messages"].([]any)[0].(map[string]any)
	if msg["reasoning"] != "trace-1" {
		t.Errorf("PrepareBodyOpt did not mirror reasoning: %v", msg)
	}
}
