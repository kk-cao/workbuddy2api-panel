package upstream

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestChatHeadersAlwaysSendConversationTrio 验证零值 ChatMeta 下会话与 B3/W3C 头族形态齐全，
// 且 X-Conversation-ID 符合 UUIDv7 形态、X-Trace-ID 独立于 X-Conversation-Request-ID。
func TestChatHeadersAlwaysSendConversationTrio(t *testing.T) {
	c := &Client{}
	req, err := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.ChatHeaders(req, testAuth(), "", ChatMeta{})

	v := req.Header.Get("X-Conversation-ID")
	if !uuidV7Pattern.MatchString(v) {
		t.Errorf("X-Conversation-ID = %q, want UUIDv7", v)
	}
	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	for _, h := range []string{"X-Conversation-Request-ID", "X-Conversation-Message-ID", "X-Request-ID", "X-Trace-ID"} {
		hv := req.Header.Get(h)
		if !hex32.MatchString(hv) {
			t.Errorf("%s = %q, want 32 hex", h, hv)
		}
	}
	if req.Header.Get("X-Trace-ID") == req.Header.Get("X-Conversation-Request-ID") {
		t.Errorf("X-Trace-ID should be independent from X-Conversation-Request-ID")
	}
	if req.Header.Get("b3") == "" || req.Header.Get("traceparent") == "" {
		t.Errorf("missing b3=%q or traceparent=%q", req.Header.Get("b3"), req.Header.Get("traceparent"))
	}
	for _, h := range []string{"Origin", "Referer", "Accept-Language", "X-Machine-ID", "X-Session-ID", "X-No-Enterprise-Id", "X-No-Department-Info"} {
		if got := req.Header.Get(h); got != "" {
			t.Errorf("ChatHeaders must not send %s=%q", h, got)
		}
	}

	// CacheKeyID 稳定生成 UUIDv7，无 CacheKeyID 且无 ConversationID 时每次独立。
	req2, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	c.ChatHeaders(req2, testAuth(), "", ChatMeta{})
	if req.Header.Get("X-Conversation-ID") == req2.Header.Get("X-Conversation-ID") {
		t.Error("fallback X-Conversation-ID should be fresh per request")
	}
	reqSticky1, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	reqSticky2, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	c.ChatHeaders(reqSticky1, testAuth(), "", ChatMeta{CacheKeyID: "derived:sticky-1"})
	c.ChatHeaders(reqSticky2, testAuth(), "", ChatMeta{CacheKeyID: "derived:sticky-1"})
	if cid := reqSticky1.Header.Get("X-Conversation-ID"); !uuidV7Pattern.MatchString(cid) || cid != reqSticky2.Header.Get("X-Conversation-ID") {
		t.Errorf("CacheKeyID fallback must derive stable UUIDv7, got %q vs %q", cid, reqSticky2.Header.Get("X-Conversation-ID"))
	}
}

// TestChatHeadersPreservesConversationID 入站带来的会话 ID 优先透传，不被兜底覆盖。
func TestChatHeadersPreservesConversationID(t *testing.T) {
	c := &Client{}
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	const conv = "0123456789abcdef0123456789abcdef"
	c.ChatHeaders(req, testAuth(), "", ChatMeta{ConversationID: conv})
	if got := req.Header.Get("X-Conversation-ID"); got != conv {
		t.Errorf("X-Conversation-ID = %q, want supplied %q", got, conv)
	}
}

// testAuth 返回最小的 CN 账号（ChatHeaders 会读 UID/凭证，nil 会解引用）。
func testAuth() *auth.Auth {
	return &auth.Auth{UID: "u1", AccessToken: "at"}
}

// TestBillingHeadersBareDomainAndStableIDs 验证 BillingHeaders 剥离 a.Domain 的协议头、不发 HTTP X-Machine-ID/X-Session-ID，并注入 X-Client-* 身份头与环境变量逃生开关。
func TestBillingHeadersBareDomainAndStableIDs(t *testing.T) {
	c := &Client{}
	req, err := http.NewRequest(http.MethodPost, "https://example.com/v2/report", nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &auth.Auth{UID: "u-billing", AccessToken: "at", Domain: "https://copilot.tencent.com/"}
	c.BillingHeaders(req, a)

	if got := req.Header.Get("X-Domain"); got != "copilot.tencent.com" {
		t.Errorf("X-Domain = %q, want copilot.tencent.com", got)
	}
	for _, h := range []string{"X-Machine-ID", "X-Session-ID", "X-CodeBuddy-Request", "Accept-Language"} {
		if got := req.Header.Get(h); got != "" {
			t.Errorf("BillingHeaders must not send %s=%q", h, got)
		}
	}
	if req.Header.Get("X-Client-Version") != defaultClientVersion ||
		req.Header.Get("X-Client-Platform") != "windows" ||
		req.Header.Get("X-Client-Product") != "WorkBuddy" {
		t.Errorf("CN X-Client-* mismatch: ver=%q plat=%q prod=%q",
			req.Header.Get("X-Client-Version"), req.Header.Get("X-Client-Platform"), req.Header.Get("X-Client-Product"))
	}

	// Global 账号带 Global 版本与 workbuddy-ai 产品段
	reqGL, _ := http.NewRequest(http.MethodPost, "https://www.workbuddy.ai/v2/report", nil)
	gl := &auth.Auth{UID: "u-gl", AccessToken: "at", Domain: "www.workbuddy.ai"}
	c.BillingHeaders(reqGL, gl)
	if reqGL.Header.Get("X-Client-Version") != defaultClientVersionGlobal ||
		reqGL.Header.Get("X-Client-Platform") != "windows" ||
		reqGL.Header.Get("X-Client-Product") != "workbuddy-ai" {
		t.Errorf("Global X-Client-* mismatch: ver=%q plat=%q prod=%q",
			reqGL.Header.Get("X-Client-Version"), reqGL.Header.Get("X-Client-Platform"), reqGL.Header.Get("X-Client-Product"))
	}

	// WB2A_NO_CLIENT_IDENTITY=1 关闭三头注入
	t.Setenv("WB2A_NO_CLIENT_IDENTITY", "1")
	reqOff, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/report", nil)
	c.BillingHeaders(reqOff, a)
	for _, h := range []string{"X-Client-Version", "X-Client-Platform", "X-Client-Product"} {
		if got := reqOff.Header.Get(h); got != "" {
			t.Errorf("%s=%q should be suppressed when WB2A_NO_CLIENT_IDENTITY=1", h, got)
		}
	}
}


func TestChatHeadersPhase2SDKAndIdentity(t *testing.T) {
	c := New()

	// 1. Personal account: 46+ headers present, X-Auth-Method / X-Userinfo omitted
	reqPers, _ := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	pers := &auth.Auth{UID: "u-pers", AccessToken: "at"}
	c.ChatHeaders(reqPers, pers, "", ChatMeta{RootRequestID: "root-1"})
	for _, k := range []string{
		"X-Stainless-Lang", "X-Stainless-Package-Version", "X-Stainless-OS",
		"X-Stainless-Arch", "X-Stainless-Runtime", "X-Stainless-Runtime-Version",
		"X-Stainless-Retry-Count", "X-Agent-Intent", "X-Agent-Type", "X-Agent-Runtime",
		"acp-connection-id", "baggage", "X-Root-Request-ID",
	} {
		if reqPers.Header.Get(k) == "" {
			t.Errorf("personal ChatHeaders missing %s", k)
		}
	}
	if reqPers.Header.Get("X-Agent-Type") != AgentTypeMain {
		t.Errorf("default X-Agent-Type=%q want %q", reqPers.Header.Get("X-Agent-Type"), AgentTypeMain)
	}
	if reqPers.Header.Get("X-Agent-Runtime") != "local-agent" {
		t.Errorf("X-Agent-Runtime=%q want local-agent", reqPers.Header.Get("X-Agent-Runtime"))
	}
	if reqPers.Header.Get("X-Root-Request-ID") != "root-1" {
		t.Errorf("X-Root-Request-ID=%q want root-1", reqPers.Header.Get("X-Root-Request-ID"))
	}
	if reqPers.Header.Get("X-Auth-Method") != "" || reqPers.Header.Get("X-Userinfo") != "" {
		t.Errorf("personal account should omit X-Auth-Method/X-Userinfo, got method=%q info=%q",
			reqPers.Header.Get("X-Auth-Method"), reqPers.Header.Get("X-Userinfo"))
	}

	// 2. Enterprise + auxiliary agentType
	reqEnt, _ := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	ent := &auth.Auth{UID: "u-ent", AccessToken: "at", EnterpriseID: "ent-1"}
	c.ChatHeaders(reqEnt, ent, "", ChatMeta{AgentType: AgentTypeAuxiliary})
	if reqEnt.Header.Get("X-Agent-Type") != AgentTypeAuxiliary {
		t.Errorf("auxiliary X-Agent-Type=%q want %q", reqEnt.Header.Get("X-Agent-Type"), AgentTypeAuxiliary)
	}
	if reqEnt.Header.Get("X-Userinfo") == "" {
		t.Errorf("enterprise account should inject X-Userinfo")
	}

	// 3. RefreshHeaders carries X-Requested-With: XMLHttpRequest
	reqRef, _ := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/plugin/auth/refresh", nil)
	c.RefreshHeaders(reqRef, pers)
	if got := reqRef.Header.Get("X-Requested-With"); got != "XMLHttpRequest" {
		t.Errorf("RefreshHeaders X-Requested-With=%q want XMLHttpRequest", got)
	}
}

func TestChatStreamGzipThresholdAndOptOut(t *testing.T) {
	var gotEnc string
	var gotPlain []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEnc = r.Header.Get("Content-Encoding")
		if gotEnc == "gzip" {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Errorf("gzip.NewReader: %v", err)
				return
			}
			defer gz.Close()
			gotPlain, _ = io.ReadAll(gz)
		} else {
			gotPlain, _ = io.ReadAll(r.Body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer ts.Close()

	c := New()
	c.ChatBaseCN = ts.URL
	acct := &auth.Auth{AccessToken: "at", Domain: "copilot.tencent.com", UID: "u1"}
	largeText := strings.Repeat("x", 1500)
	body := []byte(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"` + largeText + `"}]}`)

	rc, status, _, err := c.ChatStreamContext(t.Context(), acct, body, "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("large chat failed: status=%d err=%v", status, err)
	}
	rc.Close()
	if gotEnc != "gzip" {
		t.Errorf(">=1024B body Content-Encoding=%q want gzip", gotEnc)
	}
	var obj map[string]any
	if err := json.Unmarshal(gotPlain, &obj); err != nil {
		t.Fatalf("decompressed body not valid JSON: %v", err)
	}

	// Opt-out via WB2A_NO_CHAT_GZIP=1
	t.Setenv("WB2A_NO_CHAT_GZIP", "1")
	gotEnc = ""
	rc2, status2, _, err2 := c.ChatStreamContext(t.Context(), acct, body, "", ChatMeta{})
	if err2 != nil || status2 != 200 {
		t.Fatalf("opt-out chat failed: status=%d err=%v", status2, err2)
	}
	rc2.Close()
	if gotEnc != "" {
		t.Errorf("WB2A_NO_CHAT_GZIP=1 expected empty Content-Encoding, got %q", gotEnc)
	}
}
