// conv_fields.go 把会话轮级字段写入出站请求体最后一条 user 消息，
// 与请求头 X-Conversation-Request-ID 保持头体一致（历史消息不编造）。
package upstream

import (
	"encoding/json"
	"strings"
)

const agentCLI = "cli"

// injectConversationFields 在已改写的出站 body 上补会话轮级字段（字节入口）。
func injectConversationFields(body []byte, convReqID string) []byte {
	if len(body) == 0 || convReqID == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if !injectConversationFieldsObj(obj, convReqID) {
		return body
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// injectConversationFieldsObj 在已解析的 obj 上为最后一条 user 消息补 conversationRequestId /
// startsNewUserRequest / agent，返回是否产生修改；客户端已带非空 conversationRequestId 时不覆盖。
func injectConversationFieldsObj(obj map[string]any, convReqID string) bool {
	if obj == nil || convReqID == "" {
		return false
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return false
	}
	idx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		m, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); strings.EqualFold(strings.TrimSpace(role), "user") {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}
	cur, ok := msgs[idx].(map[string]any)
	if !ok {
		return false
	}
	if v, _ := cur["conversationRequestId"].(string); v != "" {
		return false
	}
	cur["conversationRequestId"] = convReqID
	cur["startsNewUserRequest"] = true
	if v, _ := cur["agent"].(string); v == "" {
		cur["agent"] = agentCLI
	}
	msgs[idx] = cur
	obj["messages"] = msgs
	return true
}
