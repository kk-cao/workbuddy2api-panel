// login.go — WorkBuddy OAuth 登录（设备授权流程，CN realm；--realm=global 供国际版）。
//
// 两个子命令，由 login.sh 顺序驱动：
//
//	login [--site=cn|intl|workbuddy] [--realm=cn|global] url   → POST /v2/plugin/auth/state?platform=<站点 Platform>
//	login [--site=cn|intl|workbuddy] [--realm=cn|global] poll  → 读 state，GET /v2/plugin/auth/token 与 /v2/plugin/login/account
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"time"

	auth2 "github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/loginsite"
)

var stateFile = filepath.Join(os.TempDir(), "wb2api-login-state.json")

var exitFunc = os.Exit

func realmConfig(realm string) (base, origin string) {
	s := loginsite.ForRealm(realm)
	return s.API, s.Origin
}

func commonHeaders(site loginsite.Site) func(*http.Request) {
	hdrs := site.EndpointHeaders()
	return func(req *http.Request) {
		for k, v := range hdrs {
			if v != "" {
				req.Header.Set(k, v)
			}
		}
	}
}

func withExtra(base func(*http.Request), extra map[string]string) func(*http.Request) {
	return func(req *http.Request) {
		base(req)
		for k, v := range extra {
			if v != "" {
				req.Header.Set(k, v)
			}
		}
	}
}

type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

type envelopeError struct {
	Code   int
	Msg    string
	Status int
}

func (e *envelopeError) Error() string {
	return fmt.Sprintf("code=%d msg=%s", e.Code, e.Msg)
}

func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		commonHeaders(loginsite.For(""))(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, &envelopeError{Code: env.Code, Msg: env.Msg, Status: resp.StatusCode}
	}
	return env.Data, resp.StatusCode, nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "login: "+format+"\n", args...)
	exitFunc(1)
}

type loginState struct {
	State string `json:"state"`
	Realm string `json:"realm,omitempty"`
	Site  string `json:"site,omitempty"`
}

const (
	realmCN     = "cn"
	realmGlobal = "global"
)

func parseRealmArgs(args []string) (realm string, rest []string, err error) {
	realm = realmCN
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--realm":
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("--realm requires a value")
			}
			v := strings.ToLower(strings.TrimSpace(args[i+1]))
			if v != realmCN && v != realmGlobal {
				return "", nil, fmt.Errorf("invalid --realm %q (want cn|global)", args[i+1])
			}
			realm = v
			i++
		case strings.HasPrefix(a, "--realm="):
			v := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(a, "--realm=")))
			if v != realmCN && v != realmGlobal {
				return "", nil, fmt.Errorf("invalid --realm %q (want cn|global)", v)
			}
			realm = v
		default:
			rest = append(rest, a)
		}
	}
	return realm, rest, nil
}

func resolveRealmInput(input string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "", "1", "cn":
		return realmCN, true
	case "2", "global":
		return realmGlobal, true
	}
	return "", false
}

func parseSiteArgs(args []string) (site loginsite.Site, rest []string, err error) {
	var siteID string
	var kept []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--site":
			if i+1 >= len(args) {
				return loginsite.Site{}, nil, fmt.Errorf("--site requires a value")
			}
			siteID = args[i+1]
			i++
		case strings.HasPrefix(a, "--site="):
			siteID = strings.TrimPrefix(a, "--site=")
		default:
			kept = append(kept, a)
		}
	}
	if siteID != "" {
		id := strings.ToLower(strings.TrimSpace(siteID))
		if !loginsite.ValidID(id) {
			return loginsite.Site{}, nil, fmt.Errorf("invalid --site %q (want cn|intl|workbuddy)", siteID)
		}
		_, kept, err = parseRealmArgs(kept)
		if err != nil {
			return loginsite.Site{}, nil, err
		}
		return loginsite.For(id), kept, nil
	}
	realm, kept, err := parseRealmArgs(kept)
	if err != nil {
		return loginsite.Site{}, nil, err
	}
	return loginsite.ForRealm(realm), kept, nil
}

func promptRealm(in io.Reader, out io.Writer) string {
	fmt.Fprintln(out, "选择登录版本: 1) 国内版(cn) 2) 国际版(global) [默认 1/cn]: ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return realmCN
	}
	if realm, ok := resolveRealmInput(line); ok {
		return realm
	}
	fmt.Fprintln(out, "无效选择，默认国内版 cn")
	return realmCN
}

func validateRealmMatch(stateRealm, cliRealm string) error {
	if stateRealm != "" && stateRealm != cliRealm {
		return fmt.Errorf("realm mismatch: state file realm=%q, command --realm=%q（url 与 poll 需同一 realm）", stateRealm, cliRealm)
	}
	return nil
}

func runURL(site loginsite.Site, statePath string, client *http.Client, out io.Writer) {
	stateURL, _, _, _ := site.Endpoints()
	headers := withExtra(commonHeaders(site), site.HeadersForState(loginDeviceToken()))
	data, _, err := doJSON(client, http.MethodPost, stateURL, headers, bytes.NewReader([]byte("{}")))
	if err != nil {
		fatal("auth state failed: %v", err)
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		fatal("auth state: missing state or authUrl")
	}
	raw, _ := json.Marshal(loginState{State: st.State, Realm: site.Realm, Site: site.ID})
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		fatal("write state: %v", err)
	}
	fmt.Fprintln(out, loginsite.DecorateAuthURL(st.AuthURL, site, loginDeviceNonce(), loginsite.NewLoginSessionID()))
}

func loginDeviceNonce() string {
	return strings.TrimSpace(os.Getenv("CODEBUDDY_LOGIN_DEVICE_NONCE"))
}

func loginDeviceToken() string {
	if v := strings.TrimSpace(os.Getenv("CODEBUDDY_LOGIN_DEVICE_TOKEN")); v != "" {
		return v
	}
	if p := strings.TrimSpace(os.Getenv("CODEBUDDY_LOGIN_DEVICE_TOKEN_FILE")); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

func runPoll(site loginsite.Site, statePath string, client *http.Client, out io.Writer) {
	raw, err := os.ReadFile(statePath)
	if err != nil {
		fatal("read state: %v (先跑 login url)", err)
	}
	var ls loginState
	if err := json.Unmarshal(raw, &ls); err != nil {
		fatal("parse state: %v", err)
	}
	if err := validateRealmMatch(ls.Realm, site.Realm); err != nil {
		fatal("%v", err)
	}
	if ls.Site != "" && ls.Site != site.ID {
		fatal("site mismatch: state file site=%q, command site=%q（url 与 poll 需同一站点）", ls.Site, site.ID)
	}
	tokenH := withExtra(commonHeaders(site), site.HeadersForToken())
	tokRaw, status, errTok := doJSON(client, http.MethodGet, site.API+"/v2/plugin/auth/token?state="+ls.State, tokenH, nil)
	if errTok != nil {
		var envErr *envelopeError
		if errors.As(errTok, &envErr) {
			if loginsite.CodeFatal(envErr.Code) {
				fatal("登录被上游拒绝（code=%d）：%s", envErr.Code, envErr.Msg)
			}
			if loginsite.CodePending(envErr.Code) {
				fatal("登录未完成（waiting for login）。请确认已在浏览器完成登录再按 y")
			}
		}
		if status == 0 || status >= 500 {
			fatal("token endpoint error: %v", errTok)
		}
		fatal("登录状态查询被拒（HTTP %d）：%v\nstate 可能已过期，请重新运行 login url", status, errTok)
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		fatal("登录未完成（waiting for login）。请确认已在浏览器完成登录再按 y")
	}
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	acctHeaders := withExtra(commonHeaders(site), site.HeadersForAccount(tok.AccessToken))
	acctRaw, _, errAcct := doJSON(client, http.MethodGet, site.API+"/v2/plugin/login/account?state="+ls.State, acctHeaders, nil)
	if errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	} else {
		var envErr *envelopeError
		if errors.As(errAcct, &envErr) {
			if loginsite.CodeFatal(envErr.Code) {
				fatal("登录被上游拒绝（code=%d）：%s", envErr.Code, envErr.Msg)
			}
			if loginsite.CodePending(envErr.Code) {
				fatal("账号信息尚未就绪（code=%d）。请稍等几秒再按 y 重试", envErr.Code)
			}
		}
	}
	oraw, _ := json.Marshal(buildLoginOutput(tok, site.Realm, acct))
	fmt.Fprintln(out, string(oraw))
	os.Remove(statePath)
}

func buildLoginOutput(tok struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	Domain       string `json:"domain"`
}, realm string, acct struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}) map[string]any {
	return map[string]any{
		"access_token":  tok.AccessToken,
		"refresh_token": tok.RefreshToken,
		"expires_in":    tok.ExpiresIn,
		"domain":        tok.Domain,
		"realm":         auth2.ResolveRealm(realm, tok.Domain),
		"uid":           acct.UID,
		"enterprise_id": acct.EnterpriseID,
		"nickname":      acct.Nickname,
	}
}

func main() {
	site, rest, err := parseSiteArgs(os.Args[1:])
	if err != nil {
		fatal("%v (usage: login [--site=cn|intl|workbuddy] [--realm=cn|global] <url|poll|realm|site>)", err)
	}
	if len(rest) < 1 {
		fatal("usage: login [--site=cn|intl|workbuddy] [--realm=cn|global] <url|poll|realm|site>")
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar}

	switch rest[0] {
	case "url":
		runURL(site, stateFile, client, os.Stdout)
	case "poll":
		runPoll(site, stateFile, client, os.Stdout)
	case "realm":
		fmt.Println(promptRealm(os.Stdin, os.Stderr))
	case "site":
		fmt.Println(site.ID)
	default:
		fatal("unknown subcommand %q (want url|poll|realm|site)", rest[0])
	}
}
