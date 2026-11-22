package panel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/loginsite"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

func TestLoginEndpoints(t *testing.T) {
	cases := []struct {
		realm      string
		wantState  string
		wantOrigin string
		wantBase   string
	}{
		{"global", "https://www.workbuddy.ai/v2/plugin/auth/state?platform=workbuddy-ai", "https://www.workbuddy.ai", "https://www.workbuddy.ai"},
		{"cn", "https://copilot.tencent.com/v2/plugin/auth/state?platform=workbuddy", "https://www.codebuddy.cn", "https://copilot.tencent.com"},
		{"", "https://copilot.tencent.com/v2/plugin/auth/state?platform=workbuddy", "https://www.codebuddy.cn", "https://copilot.tencent.com"},
		{"weird", "https://copilot.tencent.com/v2/plugin/auth/state?platform=workbuddy", "https://www.codebuddy.cn", "https://copilot.tencent.com"},
	}
	for _, c := range cases {
		t.Run(c.realm, func(t *testing.T) {
			st, tok, acct, origin := loginEndpoints(c.realm)
			if st != c.wantState {
				t.Errorf("realm=%q state=%q want %q", c.realm, st, c.wantState)
			}
			if origin != c.wantOrigin {
				t.Errorf("realm=%q origin=%q want %q", c.realm, origin, c.wantOrigin)
			}
			if !strings.HasPrefix(tok, c.wantBase) || !strings.HasPrefix(acct, c.wantBase) {
				t.Errorf("token/account endpoints %q / %q should share base %q", tok, acct, c.wantBase)
			}
		})
	}
}

func TestLoginStartAndPollFatalVsPending(t *testing.T) {
	var seenStateUA, seenTokenDomain string
	pollCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			seenStateUA = r.Header.Get("User-Agent")
			if r.Header.Get("Origin") != "" || r.Header.Get("Referer") != "" || r.Header.Get("X-Requested-With") != "" {
				t.Errorf("unexpected browser headers on desktop auth/state: %+v", r.Header)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]string{
					"state":   "st-test-1",
					"authUrl": "https://www.workbuddy.cn/login?platform=workbuddy&state=st-test-1",
				},
			})
		case "/v2/plugin/auth/token":
			seenTokenDomain = r.Header.Get("X-Domain")
			pollCount++
			if pollCount == 1 {
				_ = json.NewEncoder(w).Encode(map[string]any{"code": loginsite.CodeRetryFetchToken, "msg": "login ing"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": loginsite.CodeLicenseSeatLimit, "msg": "seat limit exceeded"})
		}
	}))
	defer srv.Close()

	origResolver := siteResolver
	siteResolver = func(id string) loginsite.Site {
		s := loginsite.For(id)
		s.API = srv.URL
		return s
	}
	defer func() { siteResolver = origResolver }()

	p := New(Config{Pool: pool.New(t.TempDir()), AuthDir: t.TempDir()})
	reqStart := httptest.NewRequest(http.MethodPost, "/panel/api/login/start", bytes.NewBufferString(`{"realm":"cn"}`))
	wStart := httptest.NewRecorder()
	p.loginStart(wStart, reqStart)
	if wStart.Code != http.StatusOK {
		t.Fatalf("loginStart status=%d body=%s", wStart.Code, wStart.Body.String())
	}
	var startResp struct {
		OK    bool   `json:"ok"`
		URL   string `json:"url"`
		State string `json:"state"`
	}
	_ = json.Unmarshal(wStart.Body.Bytes(), &startResp)
	if !startResp.OK || startResp.State != "st-test-1" {
		t.Fatalf("unexpected start response: %+v", startResp)
	}
	u, _ := url.Parse(startResp.URL)
	if u.Query().Get("version") != loginsite.ClientVersionWorkBuddyCN || u.Query().Get("loginSessionId") == "" {
		t.Errorf("decorated URL missing version/loginSessionId: %s", startResp.URL)
	}
	if !strings.HasPrefix(seenStateUA, "WorkBuddy/") {
		t.Errorf("auth/state UA=%q want WorkBuddy/...", seenStateUA)
	}

	// First poll: 11217 pending
	w1 := httptest.NewRecorder()
	p.loginPoll(w1, httptest.NewRequest(http.MethodGet, "/panel/api/login/poll?state=st-test-1", nil))
	if seenTokenDomain != "" {
		t.Errorf("auth/token should not send X-Domain, got %q", seenTokenDomain)
	}
	var r1 map[string]any
	_ = json.Unmarshal(w1.Body.Bytes(), &r1)
	if r1["done"] != false || r1["fatal"] == true {
		t.Errorf("pending response unexpected: %v", r1)
	}

	// Second poll: 12005 fatal
	w2 := httptest.NewRecorder()
	p.loginPoll(w2, httptest.NewRequest(http.MethodGet, "/panel/api/login/poll?state=st-test-1", nil))
	var r2 map[string]any
	_ = json.Unmarshal(w2.Body.Bytes(), &r2)
	if r2["fatal"] != true {
		t.Errorf("fatal response expected fatal=true, got %v", r2)
	}

	// Third poll: state removed after fatal -> 410 Gone
	w3 := httptest.NewRecorder()
	p.loginPoll(w3, httptest.NewRequest(http.MethodGet, "/panel/api/login/poll?state=st-test-1", nil))
	if w3.Code != http.StatusGone {
		t.Errorf("after fatal delete state should return 410, got %d", w3.Code)
	}
}
