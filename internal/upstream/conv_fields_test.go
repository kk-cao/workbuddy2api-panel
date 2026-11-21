package upstream

import (
	"encoding/json"
	"testing"
)

func TestInjectConversationFields(t *testing.T) {
	in := []byte(`{"model":"glm-5.2","messages":[{"role":"system","content":"sys"},{"role":"user","content":"q1"},{"role":"assistant","content":"a1"},{"role":"user","content":"q2"}]}`)
	reqID := "1234567890abcdef1234567890abcdef"
	out := injectConversationFields(in, reqID)

	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := obj["messages"].([]any)

	// 历史消息不编造轮级字段
	firstUser := msgs[1].(map[string]any)
	if _, ok := firstUser["conversationRequestId"]; ok {
		t.Fatalf("historical user message should not have conversationRequestId, got %v", firstUser)
	}
	asst := msgs[2].(map[string]any)
	if _, ok := asst["messageId"]; ok {
		t.Fatalf("historical assistant message should not have messageId, got %v", asst)
	}

	// 当前轮最后一条 user 写入同源字段
	lastUser := msgs[3].(map[string]any)
	if lastUser["conversationRequestId"] != reqID {
		t.Errorf("conversationRequestId=%v want %q", lastUser["conversationRequestId"], reqID)
	}
	if lastUser["startsNewUserRequest"] != true {
		t.Errorf("startsNewUserRequest=%v want true", lastUser["startsNewUserRequest"])
	}
	if lastUser["agent"] != agentCLI {
		t.Errorf("agent=%v want %q", lastUser["agent"], agentCLI)
	}

	// 客户端已有 conversationRequestId 时保留原值
	withExisting := []byte(`{"messages":[{"role":"user","content":"hi","conversationRequestId":"client-req-1"}]}`)
	outExisting := injectConversationFields(withExisting, reqID)
	var objEx map[string]any
	_ = json.Unmarshal(outExisting, &objEx)
	m0 := objEx["messages"].([]any)[0].(map[string]any)
	if m0["conversationRequestId"] != "client-req-1" {
		t.Errorf("existing conversationRequestId overwritten: %v", m0["conversationRequestId"])
	}
}
