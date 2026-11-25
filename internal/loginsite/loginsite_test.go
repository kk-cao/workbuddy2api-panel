package loginsite

import (
	"net/url"
	"regexp"
	"testing"
)

func TestFor(t *testing.T) {
	cases := []struct {
		id        string
		wantID    string
		wantRealm string
		wantAPI   string
	}{
		{id: "", wantID: SiteCN, wantRealm: RealmCN, wantAPI: "https://copilot.tencent.com"},
		{id: "cn", wantID: SiteCN, wantRealm: RealmCN, wantAPI: "https://copilot.tencent.com"},
		{id: "CN", wantID: SiteCN, wantRealm: RealmCN, wantAPI: "https://copilot.tencent.com"},
		{id: "intl", wantID: SiteIntl, wantRealm: RealmGlobal, wantAPI: "https://www.codebuddy.ai"},
		{id: "workbuddy", wantID: SiteWorkBuddy, wantRealm: RealmGlobal, wantAPI: "https://www.workbuddy.ai"},
		{id: "global", wantID: SiteCN, wantRealm: RealmCN, wantAPI: "https://copilot.tencent.com"},
		{id: "codebuddy.ai", wantID: SiteCN, wantRealm: RealmCN, wantAPI: "https://copilot.tencent.com"},
	}
	for _, c := range cases {
		s := For(c.id)
		if s.ID != c.wantID || s.Realm != c.wantRealm || s.API != c.wantAPI {
			t.Errorf("For(%q)={id:%s realm:%s api:%s} want {id:%s realm:%s api:%s}",
				c.id, s.ID, s.Realm, s.API, c.wantID, c.wantRealm, c.wantAPI)
		}
	}
}

func TestForRealm(t *testing.T) {
	cases := []struct {
		realm   string
		wantID  string
		wantAPI string
	}{
		{realm: "", wantID: SiteCN, wantAPI: "https://copilot.tencent.com"},
		{realm: "cn", wantID: SiteCN, wantAPI: "https://copilot.tencent.com"},
		{realm: "global", wantID: SiteWorkBuddy, wantAPI: "https://www.workbuddy.ai"},
		{realm: "GLOBAL", wantID: SiteCN, wantAPI: "https://copilot.tencent.com"},
	}
	for _, c := range cases {
		s := ForRealm(c.realm)
		if s.ID != c.wantID || s.API != c.wantAPI {
			t.Errorf("ForRealm(%q)={id:%s api:%s} want {id:%s api:%s}", c.realm, s.ID, s.API, c.wantID, c.wantAPI)
		}
	}
}

func TestEndpoints(t *testing.T) {
	for _, id := range []string{SiteCN, SiteIntl, SiteWorkBuddy} {
		s := For(id)
		state, token, account, origin := s.Endpoints()
		if want := s.API + "/v2/plugin/auth/state?platform=" + s.Platform; state != want {
			t.Errorf("%s state=%q want %q", id, state, want)
		}
		if want := s.API + "/v2/plugin/auth/token?state="; token != want {
			t.Errorf("%s token=%q want %q", id, token, want)
		}
		if want := s.API + "/v2/plugin/login/account?state="; account != want {
			t.Errorf("%s account=%q want %q", id, account, want)
		}
		apiHost := mustHost(t, s.API)
		originHost := mustHost(t, origin)
		if id == SiteCN {
			if apiHost != "copilot.tencent.com" || originHost != "www.codebuddy.cn" {
				t.Errorf("cn 站点域被改动：api=%s origin=%s", apiHost, originHost)
			}
			continue
		}
		if apiHost != originHost {
			t.Errorf("%s Origin 与 API 不同域：api=%s origin=%s", id, apiHost, originHost)
		}
	}
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Host
}

func TestCodeSemantics(t *testing.T) {
	for _, c := range []int{CodeRetryFetchToken, CodeRetryFetchAccount} {
		if !CodePending(c) || CodeFatal(c) {
			t.Errorf("code %d pending/fatal mismatch", c)
		}
	}
	for _, c := range []int{CodeLicenseSeatLimit, CodeLicenseExpired, CodeTrialExpired} {
		if !CodeFatal(c) || CodePending(c) {
			t.Errorf("code %d fatal/pending mismatch", c)
		}
	}
	for _, c := range []int{0, 10001, 11102, 14051, 99999} {
		if CodePending(c) || CodeFatal(c) {
			t.Errorf("code %d should not be pending or fatal", c)
		}
	}
}

func TestDecorateAuthURL(t *testing.T) {
	const base = "https://example.com/login?platform=CLI&state=abc"
	const sid = "caf6af35-8aa2-4037-89e8-c8a70ffbda5a"

	got := DecorateAuthURL(base, For(SiteCN), "", sid)
	if !containsParam(t, got, "version", ClientVersionWorkBuddyCN) {
		t.Errorf("cn 站未补 version: %s", got)
	}
	if !containsParam(t, got, "loginSessionId", sid) {
		t.Errorf("cn 站未补 loginSessionId: %s", got)
	}
	for _, id := range []string{SiteIntl, SiteWorkBuddy} {
		got := DecorateAuthURL(base, For(id), "", sid)
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("%s: parse %q: %v", id, got, err)
		}
		if v := u.Query().Get("version"); v != ClientVersionWorkBuddyGL {
			t.Errorf("%s version=%q want %q", id, v, ClientVersionWorkBuddyGL)
		}
		if containsParam(t, got, "loginSessionId", "") {
			t.Errorf("%s 不该补 loginSessionId: %s", id, got)
		}
		if s := u.Query().Get("state"); s != "abc" || u.Query().Get("platform") != "CLI" {
			t.Errorf("%s 原有参数被破坏: %s", id, got)
		}
	}
	if got := DecorateAuthURL(base, For(SiteCN), "nonce-1", ""); !containsParam(t, got, "ww_device_nonce", "nonce-1") {
		t.Errorf("cn 站未补 ww_device_nonce: %s", got)
	}
	for _, id := range []string{SiteIntl, SiteWorkBuddy} {
		if got := DecorateAuthURL(base, For(id), "nonce-1", ""); containsParam(t, got, "ww_device_nonce", "") {
			t.Errorf("%s 站不该补 ww_device_nonce: %s", id, got)
		}
	}
	if got := DecorateAuthURL(base, For(SiteCN), "", ""); containsParam(t, got, "ww_device_nonce", "") {
		t.Errorf("nonce 为空时不该补 ww_device_nonce: %s", got)
	}
	if got := DecorateAuthURL(base+"&version=9.9.9&loginSessionId=up", For(SiteCN), "", sid); !containsParam(t, got, "version", "9.9.9") || !containsParam(t, got, "loginSessionId", "up") {
		t.Errorf("已有参数被覆盖: %s", got)
	}
	if got := DecorateAuthURL("://bad", For(SiteCN), "", sid); got != "://bad" {
		t.Errorf("非法 URL 未原样返回: %q", got)
	}
}

func TestNewLoginSessionID(t *testing.T) {
	a, b := NewLoginSessionID(), NewLoginSessionID()
	if a == "" || a == b {
		t.Fatalf("loginSessionId 应非空且每次不同: %q / %q", a, b)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(a) {
		t.Fatalf("不是 uuid v4 形态: %q", a)
	}
}

func containsParam(t *testing.T, raw, key, want string) bool {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	v, ok := u.Query()[key]
	if want == "" {
		return ok
	}
	return ok && len(v) == 1 && v[0] == want
}

func TestBaseForDomain(t *testing.T) {
	cases := []struct {
		domain string
		want   string
	}{
		{domain: "www.codebuddy.ai", want: BaseOfficial},
		{domain: "codebuddy.ai", want: BaseOfficial},
		{domain: "api.codebuddy.ai", want: BaseOfficial},
		{domain: "www.workbuddy.ai", want: BaseWorkBuddy},
		{domain: "workbuddy.ai", want: BaseWorkBuddy},
		{domain: "WWW.CODEBUDDY.AI", want: BaseOfficial},
		{domain: " www.codebuddy.ai ", want: BaseOfficial},
		{domain: "", want: ""},
		{domain: "www.codebuddy.cn", want: ""},
		{domain: "copilot.tencent.com", want: ""},
		{domain: "evil-codebuddy.ai", want: ""},
	}
	for _, c := range cases {
		if got := BaseForDomain(c.domain); got != c.want {
			t.Errorf("BaseForDomain(%q)=%q want %q", c.domain, got, c.want)
		}
	}
}

func TestValidID(t *testing.T) {
	for _, ok := range []string{"cn", "intl", "workbuddy", "CN", " Intl "} {
		if !ValidID(ok) {
			t.Errorf("ValidID(%q)=false want true", ok)
		}
	}
	for _, bad := range []string{"", "global", "codebuddy", "www.codebuddy.ai"} {
		if ValidID(bad) {
			t.Errorf("ValidID(%q)=true want false", bad)
		}
	}
}

func TestClientUA(t *testing.T) {
	if ClientUA != "CLI/"+ClientVersion+" CodeBuddy/"+ClientVersion {
		t.Errorf("ClientUA=%q mismatch", ClientUA)
	}
	if ClientVersionWorkBuddyGL != "5.11.7" || ClientVersionBuiltinCLIGL != "2.176.0" {
		t.Errorf("GL versions mismatch: %s / %s", ClientVersionWorkBuddyGL, ClientVersionBuiltinCLIGL)
	}
}

func TestEndpointHeaders(t *testing.T) {
	wantGLUA := "workbuddy-ai/" + ClientVersionWorkBuddyGL + " workbuddy-ai/" + ClientVersionWorkBuddyGL + " CLI/" + ClientVersionBuiltinCLIGL
	wantCNUA := "WorkBuddy/" + ClientVersionWorkBuddyCN + " WorkBuddy/" + ClientVersionWorkBuddyCN + " CLI/" + ClientVersionBuiltinCLICN

	cases := []struct {
		site   Site
		wantUA string
	}{
		{For(SiteCN), wantCNUA},
		{For(SiteIntl), wantGLUA},
		{For(SiteWorkBuddy), wantGLUA},
	}
	for _, c := range cases {
		h := c.site.EndpointHeaders()
		if got := h["User-Agent"]; got != c.wantUA {
			t.Errorf("%s User-Agent=%q want %q", c.site.ID, got, c.wantUA)
		}
		if got := h["X-Product"]; got != "SaaS" {
			t.Errorf("%s X-Product=%q want SaaS", c.site.ID, got)
		}
		if got := h["Accept"]; got != "application/json, text/plain, */*" {
			t.Errorf("%s Accept=%q mismatch", c.site.ID, got)
		}
		if got := h["X-Request-ID"]; len(got) != 32 {
			t.Errorf("%s X-Request-ID=%q want 32 hex", c.site.ID, got)
		}
		for _, k := range []string{"Origin", "Referer", "X-Requested-With"} {
			if v, ok := h[k]; ok {
				t.Errorf("%s 不应出现 %s=%q", c.site.ID, k, v)
			}
		}
	}
}

func TestLoginEndpointHeaderVariants(t *testing.T) {
	s := For(SiteCN)

	st := s.HeadersForState("v3:abc")
	for _, k := range []string{"X-Domain", "X-No-Authorization", "X-No-User-Id",
		"X-No-Enterprise-Id", "X-No-Department-Info", "X-Custom-Channel-Id", "X-Device-Token"} {
		if st[k] == "" {
			t.Errorf("auth/state 缺 %s", k)
		}
	}
	if got := st["X-Domain"]; got != "www.workbuddy.cn" {
		t.Errorf("auth/state X-Domain=%q want www.workbuddy.cn", got)
	}
	if got := s.HeadersForState("")["X-Device-Token"]; got != "" {
		t.Errorf("无设备 token 时不应发 X-Device-Token，实际 %q", got)
	}

	tk := s.HeadersForToken()
	if got := tk["X-Domain"]; got != "" {
		t.Errorf("auth/token 真机无 X-Domain，实际 %q", got)
	}
	for _, k := range []string{"X-No-Authorization", "X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
		if tk[k] != "true" {
			t.Errorf("auth/token %s=%q want true", k, tk[k])
		}
	}
	if tk["X-Device-Token"] != "" || tk["X-Custom-Channel-Id"] != "" {
		t.Errorf("auth/token 不应带设备头: %v", tk)
	}

	ac := s.HeadersForAccount("AT")
	if got := ac["Authorization"]; got != "Bearer AT" || ac["X-Domain"] != "www.workbuddy.cn" || ac["X-No-Authorization"] != "" {
		t.Errorf("login/account header mismatch: %v", ac)
	}
	for _, k := range []string{"X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
		if ac[k] != "true" {
			t.Errorf("login/account %s=%q want true", k, ac[k])
		}
	}
}
