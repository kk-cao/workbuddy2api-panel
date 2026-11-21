package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestIsAccountRestrictionAndFrameKind(t *testing.T) {
	if !IsRestricted(`{"code":11140,"msg":"账号存在异常行为，已被限制使用"}`) {
		t.Errorf("expected IsRestricted true for CN 11140 unusual activity")
	}
	if !IsRestricted(`{"code":11140,"msg":"Account has Unusual Activity"}`) {
		t.Errorf("expected IsRestricted true for EN 11140 unusual activity")
	}
	// 无 code 11140 的泛化限制文案或 11-128 request illegal 不得判为账号级限制
	if IsAccountRestriction(`{"code":"11-128","msg":"request illegal"}`) {
		t.Errorf("11-128 must not match IsAccountRestriction")
	}
	if IsAccountRestriction(`{"code":11140,"msg":"The model provider is rate-limiting requests. request illegal"}`) {
		t.Errorf("soft-rate marker must stay excluded from IsAccountRestriction")
	}
	if FrameKind(`{"error":{"code":11140,"message":"账号存在异常行为，已被限制使用"}}`) != ErrAccountFault {
		t.Errorf("FrameKind did not return ErrAccountFault for SSE 11140 restriction")
	}
	if FrameKind(`{"error":{"code":"11-128","message":"request illegal"}}`) != ErrChannelRefused {
		t.Errorf("FrameKind did not preserve error.code 11-128 -> ErrChannelRefused")
	}
	if FrameKind(`{"error":{"code":11151,"message":"a message has empty content"}}`) != ErrEmptyContent {
		t.Errorf("FrameKind did not map 11151 -> ErrEmptyContent")
	}
	if hint := GatewayHint(ErrUpstreamBusy, `{"code":11134,"msg":"overloaded"}`, HintContext{}); !strings.Contains(hint, "11134") || strings.Contains(hint, "queue.full") {
		t.Errorf("11134 GatewayHint=%q should distinguish provider overload from queue.full", hint)
	}
}

func TestChatHeadersPrivateDataByEditionAndEnterprise(t *testing.T) {
	c := &Client{}
	cases := []struct {
		name string
		auth *auth.Auth
		want string
	}{
		{"CN personal", &auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.workbuddy.cn"}, "false"},
		{"CN enterprise", &auth.Auth{UID: "u2", AccessToken: "at", EnterpriseID: "ent1", Domain: "www.workbuddy.cn"}, "true"},
		{"Global personal", &auth.Auth{UID: "u3", AccessToken: "at", Domain: "www.workbuddy.ai"}, "true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
			c.ChatHeaders(req, tc.auth, "", ChatMeta{})
			if got := req.Header.Get("X-Private-Data"); got != tc.want {
				t.Errorf("X-Private-Data=%q want %q", got, tc.want)
			}
		})
	}
}

func TestDesktopFingerprintGlobalAndChatStreamConvFieldsWire(t *testing.T) {
	gl := &auth.Auth{UID: "gl1", Domain: "www.workbuddy.ai"}
	fp := desktopFingerprint(gl)
	if fp["commit"] != desktopCommitGlobal || fp["ideVersion"] != defaultClientVersionGlobal {
		t.Errorf("global desktopFingerprint mismatch: commit=%v ideVersion=%v", fp["commit"], fp["ideVersion"])
	}

	var gotBody string
	var gotReqIDHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReqIDHeader = r.Header.Get("X-Conversation-Request-ID")
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatHTTP: srv.Client(), ChatBaseCN: srv.URL}
	rc, status, _, err := c.ChatStream(
		&auth.Auth{UID: "cn1", AccessToken: "at"},
		[]byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`),
		"",
		ChatMeta{},
	)
	if err != nil || status != 200 {
		t.Fatalf("ChatStream status=%d err=%v", status, err)
	}
	if rc != nil {
		rc.Close()
	}
	if gotReqIDHeader == "" || !strings.Contains(gotBody, `"conversationRequestId":"`+gotReqIDHeader+`"`) {
		t.Errorf("wire body must contain header-matched conversationRequestId=%q, body=%s", gotReqIDHeader, gotBody)
	}
}
