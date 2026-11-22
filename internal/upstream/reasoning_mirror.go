// reasoning_mirror.go 给「上游按 reasoning 字段读历史」的模型补一份非空镜像（kimi/glm/minimax/mimo/step/space-bunny）。
package upstream

import "strings"

var upstreamReasoningModelPrefixes = []string{
	"kimi", "glm", "minimax", "mimo", "step", "space-bunny",
}

// readsUpstreamReasoning 该模型是否按 `reasoning` 字段读历史（入参可带 realm 前缀）。
func readsUpstreamReasoning(model string) bool {
	_, bare := SplitRealmPrefix(model)
	lower := strings.ToLower(strings.TrimSpace(bare))
	if lower == "" {
		return false
	}
	for _, p := range upstreamReasoningModelPrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// mirrorUpstreamReasoning 把这些模型的 assistant 历史里非空 reasoning_content 镜像到空的 reasoning 字段；
// 绝不新建占位空串，已有非空 reasoning 时也不覆盖。
func mirrorUpstreamReasoning(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !readsUpstreamReasoning(model) {
		return
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}
	for _, rawMsg := range msgs {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "assistant" {
			continue
		}
		rc, _ := msg["reasoning_content"].(string)
		if strings.TrimSpace(rc) == "" {
			continue
		}
		if r, ok := msg["reasoning"].(string); ok && strings.TrimSpace(r) != "" {
			continue
		}
		msg["reasoning"] = rc
	}
}
