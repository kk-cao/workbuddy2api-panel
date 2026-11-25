// Package loginsite 官方登录站点表与登录协议常量（对齐 CodeBuddy CLI 的
// product.json 域表与 auth 模块码表）。
//
// 为什么单独一个包：设备授权链路有两个调用方——面板内嵌流程
// （internal/panel/login.go）与命令行工具（cmd/login）。域表、UA 版本、登录码
// 语义全部收敛在这里，避免两边分叉。
package loginsite

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ClientVersion 官方 CLI 版本。UA 与授权链接的 version 参数同源。
const ClientVersion = "2.176.0"

// ClientUA 未复核站点的兜底 User-Agent（CodeBuddy CLI 家族形态）。
// WorkBuddy 桌面端（CN 5.11.3 / Global 5.11.7）设备码三端点发桌面端 RPC 族 UA，见 Site.UserAgent。
const ClientUA = "CLI/" + ClientVersion + " CodeBuddy/" + ClientVersion

// WorkBuddy 桌面端版本与内置 CLI 版本（真机 UA 段，保留本仓已验证的 5.11.7 / 2.176.0 国际版口径）。
const (
	ClientVersionWorkBuddyCN  = "5.11.3"
	ClientVersionWorkBuddyGL  = "5.11.7"
	ClientVersionBuiltinCLICN = "2.176.0"
	ClientVersionBuiltinCLIGL = "2.176.0"
)

// 站点标识（对外 API 取值）。
const (
	SiteCN        = "cn"        // 国内站：API copilot.tencent.com，Origin www.codebuddy.cn
	SiteIntl      = "intl"      // 官方国际站：www.codebuddy.ai
	SiteWorkBuddy = "workbuddy" // 旧桌面端国际版：www.workbuddy.ai
)

// Realm 取值（池内分域标识，与 auth.Realm() 输出一致）。
const (
	RealmCN     = "cn"
	RealmGlobal = "global"
)

// Site 一个登录站点的全部端点要素。
type Site struct {
	ID        string
	Realm     string // 落盘 realm：cn / global
	API       string // 上游 API base（三端点同域）
	Origin    string // Origin/Referer（仅授权页跳转用；设备码三端点不发）
	Platform  string // 授权页 platform 口径：workbuddy / workbuddy-ai
	UserAgent string // 设备码三端点（auth/state、auth/token、login/account）的 UA
	Domain    string // 设备码端点的 X-Domain 取值（前端主机名，非 API base）
	Version   string // 授权链接 version 参数（与包版本同源）
}

var (
	siteCN = Site{
		ID: SiteCN, Realm: RealmCN,
		API: "https://copilot.tencent.com", Origin: "https://www.codebuddy.cn",
		Platform:  "workbuddy",
		UserAgent: "WorkBuddy/" + ClientVersionWorkBuddyCN + " WorkBuddy/" + ClientVersionWorkBuddyCN + " CLI/" + ClientVersionBuiltinCLICN,
		Domain:    "www.workbuddy.cn",
		Version:   ClientVersionWorkBuddyCN,
	}
	siteIntl = Site{
		ID: SiteIntl, Realm: RealmGlobal,
		API: "https://www.codebuddy.ai", Origin: "https://www.codebuddy.ai",
		Platform:  "workbuddy-ai",
		UserAgent: "workbuddy-ai/" + ClientVersionWorkBuddyGL + " workbuddy-ai/" + ClientVersionWorkBuddyGL + " CLI/" + ClientVersionBuiltinCLIGL,
		Domain:    "www.codebuddy.ai",
		Version:   ClientVersionWorkBuddyGL,
	}
	siteWorkBuddy = Site{
		ID: SiteWorkBuddy, Realm: RealmGlobal,
		API: "https://www.workbuddy.ai", Origin: "https://www.workbuddy.ai",
		Platform:  "workbuddy-ai",
		UserAgent: "workbuddy-ai/" + ClientVersionWorkBuddyGL + " workbuddy-ai/" + ClientVersionWorkBuddyGL + " CLI/" + ClientVersionBuiltinCLIGL,
		Domain:    "www.workbuddy.ai",
		Version:   ClientVersionWorkBuddyGL,
	}
)

// For 按站点标识返回站点定义；空/未知 → 国内站（零回归）。
func For(id string) Site {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case SiteIntl:
		return siteIntl
	case SiteWorkBuddy:
		return siteWorkBuddy
	}
	return siteCN
}

// Default 缺省站点（国内站）。
func Default() Site { return siteCN }

// ForRealm 按 realm 映射站点：global → workbuddy.ai（保持老前端/脚本 realm=global 语义），cn → 国内站。
func ForRealm(realm string) Site {
	if strings.TrimSpace(realm) == RealmGlobal {
		return siteWorkBuddy
	}
	return siteCN
}

// ValidID 报告 id 是否是已知站点标识。
func ValidID(id string) bool {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case SiteCN, SiteIntl, SiteWorkBuddy:
		return true
	}
	return false
}

// Endpoints 返回设备授权三端点（auth/state、token、account）与 Origin。
func (s Site) Endpoints() (state, token, account, origin string) {
	platform := s.Platform
	if platform == "" {
		platform = "workbuddy"
	}
	return s.API + "/v2/plugin/auth/state?platform=" + platform,
		s.API + "/v2/plugin/auth/token?state=",
		s.API + "/v2/plugin/login/account?state=",
		s.Origin
}

// EndpointHeaders 设备码三端点的公共出站头族（真机 node fetch 形态，不带 Origin / Referer / X-Requested-With / Content-Type）。
func (s Site) EndpointHeaders() map[string]string {
	return map[string]string{
		"Accept":       "application/json, text/plain, */*",
		"User-Agent":   s.userAgent(),
		"X-Product":    "SaaS",
		"X-Request-ID": newTraceID(),
	}
}

func newTraceID() string {
	var b [16]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		ms := time.Now().UnixMilli()
		return fmt.Sprintf("%012x%020x", ms, uint64(ms))
	}
	return hex.EncodeToString(b[:])
}

func (s Site) userAgent() string {
	if s.UserAgent != "" {
		return s.UserAgent
	}
	return ClientUA
}

func (s Site) host() string {
	if s.Domain != "" {
		return s.Domain
	}
	h := strings.TrimPrefix(strings.TrimPrefix(s.Origin, "https://"), "http://")
	return strings.TrimSuffix(h, "/")
}

func sentinelNo(noAuth bool) map[string]string {
	out := map[string]string{
		"X-No-User-Id":         "true",
		"X-No-Enterprise-Id":   "true",
		"X-No-Department-Info": "true",
	}
	if noAuth {
		out["X-No-Authorization"] = "true"
	}
	return out
}

// HeadersForState POST /v2/plugin/auth/state 的出站头（Content-Type + X-Domain + X-Custom-Channel-Id + 4 个 X-No-* + 可选 X-Device-Token）。
func (s Site) HeadersForState(deviceToken string) map[string]string {
	h := s.EndpointHeaders()
	h["Content-Type"] = "application/json"
	h["X-Domain"] = s.host()
	h["X-Custom-Channel-Id"] = "unknown"
	for k, v := range sentinelNo(true) {
		h[k] = v
	}
	if deviceToken != "" {
		h["X-Device-Token"] = deviceToken
	}
	return h
}

// HeadersForToken GET /v2/plugin/auth/token 的出站头（真机唯一不带 X-Domain 的端点）。
func (s Site) HeadersForToken() map[string]string {
	h := s.EndpointHeaders()
	for k, v := range sentinelNo(true) {
		h[k] = v
	}
	return h
}

// HeadersForAccount GET /v2/plugin/login/account 的出站头（X-Domain + Bearer + 3 个 X-No-*）。
func (s Site) HeadersForAccount(accessToken string) map[string]string {
	h := s.EndpointHeaders()
	h["X-Domain"] = s.host()
	for k, v := range sentinelNo(false) {
		h[k] = v
	}
	if accessToken != "" {
		h["Authorization"] = "Bearer " + accessToken
	}
	return h
}

// 官方 global 域主机（按 auth 文件 domain 选用）。
const (
	BaseOfficial  = "https://www.codebuddy.ai"
	BaseWorkBuddy = "https://www.workbuddy.ai"
)

// BaseForDomain 按 auth 文件的 domain 返回 global 域上游 base。
func BaseForDomain(domain string) string {
	d := strings.ToLower(strings.TrimSpace(domain))
	switch {
	case d == "codebuddy.ai" || strings.HasSuffix(d, ".codebuddy.ai"):
		return BaseOfficial
	case d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai"):
		return BaseWorkBuddy
	}
	return ""
}

// 官方登录业务码（CLI auth 模块枚举）。
const (
	CodeRetryFetchToken   = 11217
	CodeRetryFetchAccount = 12151
	CodeLicenseSeatLimit  = 12005
	CodeLicenseExpired    = 11212
	CodeTrialExpired      = 11216
)

// CodePending 报告该业务码是否表示「登录进行中，应继续轮询」。
func CodePending(code int) bool {
	return code == CodeRetryFetchToken || code == CodeRetryFetchAccount
}

// CodeFatal 报告该业务码是否属于终态许可错误。
func CodeFatal(code int) bool {
	switch code {
	case CodeLicenseSeatLimit, CodeLicenseExpired, CodeTrialExpired:
		return true
	}
	return false
}

// DecorateAuthURL 按真机实测补齐授权链接参数：version +（仅 platform=workbuddy）loginSessionId / ww_device_nonce。
func DecorateAuthURL(raw string, s Site, nonce, sessionID string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	if s.Version != "" && q.Get("version") == "" {
		q.Set("version", s.Version)
	}
	if s.Platform == "workbuddy" && sessionID != "" && q.Get("loginSessionId") == "" {
		q.Set("loginSessionId", sessionID)
	}
	if s.Platform == "workbuddy" && nonce != "" && q.Get("ww_device_nonce") == "" {
		q.Set("ww_device_nonce", nonce)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// NewLoginSessionID 生成 uuid v4 形态的登录会话标识。
func NewLoginSessionID() string {
	var b [16]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
