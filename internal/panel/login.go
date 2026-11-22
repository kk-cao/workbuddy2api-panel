// login.go 面板内嵌的 WorkBuddy OAuth 设备授权流程（基于 internal/loginsite 桌面端真机协议）。
package panel

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/loginsite"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const clientUA = loginsite.ClientUA

type loginSite = loginsite.Site

const (
	siteCN        = loginsite.SiteCN
	siteIntl      = loginsite.SiteIntl
	siteWorkBuddy = loginsite.SiteWorkBuddy
)

var siteResolver = loginsite.For

func siteOrDefault(id string) loginSite { return siteResolver(id) }

func siteForRealm(realm string) loginSite { return siteResolver(loginsite.ForRealm(realm).ID) }

func loginEndpointsFor(s loginSite) (state, token, account, origin string) { return s.Endpoints() }

func loginEndpoints(realm string) (state, token, account, origin string) {
	return loginEndpointsFor(siteForRealm(realm))
}

const (
	loginCodeRetryFetchToken   = loginsite.CodeRetryFetchToken
	loginCodeRetryFetchAccount = loginsite.CodeRetryFetchAccount
	loginCodeLicenseSeatLimit  = loginsite.CodeLicenseSeatLimit
	loginCodeLicenseExpired    = loginsite.CodeLicenseExpired
	loginCodeTrialExpired      = loginsite.CodeTrialExpired
)

func loginCodePending(code int) bool { return loginsite.CodePending(code) }

func loginCodeFatal(code int) bool { return loginsite.CodeFatal(code) }

type loginEnvelopeError struct {
	Code   int
	Msg    string
	Status int
}

func (e *loginEnvelopeError) Error() string {
	return fmt.Sprintf("code=%d msg=%s", e.Code, e.Msg)
}

// loginHTTP 设备授权专用 client：禁 HTTP/2（避免半死 h2 流卡死到总超时）+ 分档连接/握手/响应头超时。
var loginHTTP = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   8 * time.Second,
			KeepAlive: 15 * time.Second,
		}).DialContext,
		TLSNextProto:          make(map[string]func(string, *tls.Conn) http.RoundTripper),
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       60 * time.Second,
	},
}

func commonHeaders(req *http.Request, site loginSite) {
	applyHeaders(req, site.EndpointHeaders())
}

func applyHeaders(req *http.Request, h map[string]string) {
	for k, v := range h {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
}

func stateHeaders(site loginSite, deviceToken string) map[string]string {
	return site.HeadersForState(deviceToken)
}

func tokenHeaders(site loginSite) map[string]string {
	return site.HeadersForToken()
}

func accountHeaders(site loginSite, accessToken string) map[string]string {
	return site.HeadersForAccount(accessToken)
}

func validUID(uid string) bool {
	if uid == "" || len(uid) > 64 {
		return false
	}
	for _, c := range uid {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

const loginAttempts = 3

func doJSON(method, fullURL string, extra map[string]string, body []byte, site loginSite) (json.RawMessage, int, error) {
	var lastErr error
	for attempt := 0; attempt < loginAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 400 * time.Millisecond)
		}
		data, status, err := doJSONOnce(method, fullURL, extra, body, site)
		if err == nil {
			return data, status, nil
		}
		if status != 0 {
			return nil, status, err
		}
		lastErr = err
	}
	return nil, 0, lastErr
}

func doJSONOnce(method, fullURL string, extra map[string]string, body []byte, site loginSite) (json.RawMessage, int, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, fullURL, rdr)
	if err != nil {
		return nil, 0, err
	}
	commonHeaders(req, site)
	applyHeaders(req, extra)
	resp, err := loginHTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, &loginEnvelopeError{Code: env.Code, Msg: env.Msg, Status: resp.StatusCode}
	}
	return env.Data, resp.StatusCode, nil
}

func loginDeviceNonce() string {
	return strings.TrimSpace(os.Getenv("CODEBUDDY_LOGIN_DEVICE_NONCE"))
}

func loginDeviceToken(up *upstream.Client) string {
	if v := strings.TrimSpace(os.Getenv("CODEBUDDY_LOGIN_DEVICE_TOKEN")); v != "" {
		return v
	}
	if p := strings.TrimSpace(os.Getenv("CODEBUDDY_LOGIN_DEVICE_TOKEN_FILE")); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			if tok := strings.TrimSpace(string(b)); tok != "" {
				return tok
			}
		}
	}
	if up != nil {
		return up.ResolveGlobalDeviceToken()
	}
	return ""
}

// loginStart 发起设备授权：POST auth/state 拿授权 URL。
// body 支持 {"site":"cn"|"intl"|"workbuddy"}（优先）或 {"realm":"cn"|"global"}（兼容老前端）。
func (p *Panel) loginStart(w http.ResponseWriter, r *http.Request) {
	site := siteOrDefault("")
	if r.Body != nil {
		var reqBody struct {
			Realm string `json:"realm"`
			Site  string `json:"site"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&reqBody); err == nil {
			if reqBody.Site != "" {
				site = siteOrDefault(strings.ToLower(strings.TrimSpace(reqBody.Site)))
			} else if reqBody.Realm != "" {
				site = siteForRealm(reqBody.Realm)
			}
		}
	}
	epState, _, _, _ := loginEndpointsFor(site)
	data, status, err := doJSON(http.MethodPost, epState, stateHeaders(site, loginDeviceToken(p.cfg.Upstream)), []byte("{}"), site)
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("auth state (upstream %d): %v", status, err))
		return
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		writeErr(w, http.StatusBadGateway, "auth state: missing state or authUrl")
		return
	}
	st.AuthURL = loginsite.DecorateAuthURL(st.AuthURL, site, loginDeviceNonce(), loginsite.NewLoginSessionID())
	p.loginMu.Lock()
	for s, sess := range p.logins {
		if time.Since(sess.created) > loginTTL {
			delete(p.logins, s)
		}
	}
	p.logins[st.State] = loginSession{created: time.Now(), realm: site.Realm, site: site.ID}
	p.loginMu.Unlock()
	log.Printf("panel: 发起 OAuth 添加账号 realm=%s site=%s（state=%s...）", site.Realm, site.ID, st.State[:min(8, len(st.State))])
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": st.AuthURL, "state": st.State, "realm": site.Realm, "site": site.ID})
}

func (p *Panel) logPollOnce(state, msg string) {
	p.loginMu.Lock()
	s, ok := p.logins[state]
	if !ok || s.lastMsg == msg {
		p.loginMu.Unlock()
		return
	}
	s.lastMsg = msg
	p.logins[state] = s
	p.loginMu.Unlock()
	log.Printf("panel: 登录轮询 state=%s... 上游=%s", state[:min(8, len(state))], msg)
}

// loginPoll 轮询登录态。未完成 → {done:false}；终态拒登 → {done:false,fatal:true}；完成 → 建凭证、落盘、热加载、签到。
func (p *Panel) loginPoll(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		writeErr(w, http.StatusBadRequest, "missing state")
		return
	}
	p.loginMu.Lock()
	sess, known := p.logins[state]
	if known && sess.processing {
		p.loginMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "正在写入凭据…"})
		return
	}
	if known {
		sess.processing = true
		p.logins[state] = sess
	}
	p.loginMu.Unlock()
	if !known {
		writeErr(w, http.StatusGone, "login session 已过期，请关闭后重新添加账号")
		return
	}
	defer func() {
		p.loginMu.Lock()
		if s, ok := p.logins[state]; ok {
			s.processing = false
			p.logins[state] = s
		}
		p.loginMu.Unlock()
	}()

	pollSite := siteOrDefault(sess.site)
	if sess.site == "" && sess.realm == "global" {
		pollSite = siteForRealm("global")
	}
	_, epToken, epAcct, _ := loginEndpointsFor(pollSite)

	tokRaw, _, err := doJSON(http.MethodGet, epToken+state, tokenHeaders(pollSite), nil, pollSite)
	if err != nil {
		var envErr *loginEnvelopeError
		if errors.As(err, &envErr) && loginCodeFatal(envErr.Code) {
			p.loginMu.Lock()
			delete(p.logins, state)
			p.loginMu.Unlock()
			log.Printf("panel: 登录被上游拒绝 state=%s... code=%d msg=%s", state[:min(8, len(state))], envErr.Code, envErr.Msg)
			writeJSON(w, http.StatusOK, map[string]any{
				"done":    false,
				"fatal":   true,
				"code":    envErr.Code,
				"message": fmt.Sprintf("登录被上游拒绝（code=%d）：%s", envErr.Code, envErr.Msg),
			})
			return
		}
		p.logPollOnce(state, err.Error())
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": err.Error()})
		return
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "waiting for login"})
		return
	}

	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	acctRaw, _, errAcct := doJSON(http.MethodGet, epAcct+state, accountHeaders(pollSite, tok.AccessToken), nil, pollSite)
	if errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	} else {
		var envErr *loginEnvelopeError
		if errors.As(errAcct, &envErr) {
			if loginCodePending(envErr.Code) {
				p.logPollOnce(state, errAcct.Error())
				writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": errAcct.Error()})
				return
			}
			if loginCodeFatal(envErr.Code) {
				p.loginMu.Lock()
				delete(p.logins, state)
				p.loginMu.Unlock()
				log.Printf("panel: 登录账号信息被上游拒绝 state=%s... code=%d msg=%s", state[:min(8, len(state))], envErr.Code, envErr.Msg)
				writeJSON(w, http.StatusOK, map[string]any{
					"done":    false,
					"fatal":   true,
					"code":    envErr.Code,
					"message": fmt.Sprintf("登录被上游拒绝（code=%d）：%s", envErr.Code, envErr.Msg),
				})
				return
			}
		}
	}
	if acct.UID == "" {
		writeErr(w, http.StatusBadGateway, "login done but no uid（token 已发但账号信息获取失败，请重试）")
		return
	}
	if !validUID(acct.UID) {
		writeErr(w, http.StatusBadGateway, "上游返回的 uid 含非法字符，拒绝落盘（防路径穿越）")
		return
	}

	if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "mkdir auth dir: "+err.Error())
		return
	}
	a := &auth.Auth{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
		Domain:       tok.Domain,
		UID:          acct.UID,
		EnterpriseID: acct.EnterpriseID,
		Nickname:     acct.Nickname,
		FilePath:     filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", acct.UID)),
	}
	if sess.realm == "global" {
		if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
			writeErr(w, http.StatusInternalServerError, "set realm: "+err.Error())
			return
		}
	} else {
		_, _ = a.BackfillRealm()
	}
	if err := a.SaveAtomic(); err != nil {
		writeErr(w, http.StatusInternalServerError, "save auth: "+err.Error())
		return
	}
	p.cfg.Pool.Add(a)
	p.cfg.Pool.Revive(acct.UID)

	checkinMsg := ""
	remain := int64(-1)
	total := int64(0)
	if sess.realm == "global" {
		if activated, err := p.cfg.Upstream.GlobalCompleteRegistration(a); err != nil {
			checkinMsg = "注册激活失败: " + err.Error()
			log.Printf("panel: global 注册激活 uid=%s: %v", acct.UID, err)
		} else if activated {
			log.Printf("panel: global 注册激活 uid=%s 完成", acct.UID)
		}
		if claimed, err := p.cfg.Upstream.ClaimTrial(a); err != nil {
			checkinMsg = joinMsg(checkinMsg, "trial 领取失败: "+err.Error())
			log.Printf("panel: global trial uid=%s: %v", acct.UID, err)
		} else if claimed {
			log.Printf("panel: global trial uid=%s 已领", acct.UID)
		}
	} else if !a.IsEnterprise() {
		if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
			checkinMsg = err.Error()
		}
	}
	balanceErr := ""
	if rm, tt, err := p.cfg.Upstream.UserResource(a); err == nil {
		remain, total = rm, tt
		p.cfg.Pool.ReenableIfCredits(acct.UID, rm, tt)
	} else {
		balanceErr = err.Error()
		log.Printf("panel: 登录后余额校验失败 uid=%s: %v", acct.UID, err)
	}

	p.loginMu.Lock()
	delete(p.logins, state)
	p.loginMu.Unlock()
	log.Printf("panel: 新账号已热加载 uid=%s nickname=%q realm=%s（免重启生效）", acct.UID, acct.Nickname, sess.realm)
	if a.IsEnterpriseSession() && !a.IsEnterprise() {
		log.Printf("panel: WARN uid=%s 的凭证为企业切换会话（token_source=enterprise_switch）：成长任务行为事件会被上游静默丢弃；建议移除该账号，在浏览器退出企业切换（个人身份）后重新登录", acct.UID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"done":            true,
		"uid":             acct.UID,
		"nickname":        acct.Nickname,
		"realm":           sess.realm,
		"credits":         remain,
		"credits_total":   total,
		"checkin_message": checkinMsg,
		"balance_error":   balanceErr,
	})
}

func joinMsg(parts ...string) string {
	out := ""
	for _, s := range parts {
		if s == "" {
			continue
		}
		if out != "" {
			out += "；"
		}
		out += s
	}
	return out
}

func (p *Panel) loginRegions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"regions": []map[string]string{
			{"code": "HK", "name": "Hong Kong"},
			{"code": "MO", "name": "Macao"},
			{"code": "SG", "name": "Singapore"},
			{"code": "TH", "name": "Thailand"},
			{"code": "PH", "name": "Philippines"},
			{"code": "MY", "name": "Malaysia"},
			{"code": "ID", "name": "Indonesia"},
		},
	})
}
