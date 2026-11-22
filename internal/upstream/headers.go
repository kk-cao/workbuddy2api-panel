// Package headers 构造三类上游请求头（common / chat / billing / refresh）。
// 规则来自 docs/api-reference.md §0/§4/§6。
package upstream

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

const (
	// defaultClientVersion 出站 WorkBuddy 客户端版本段（UA 的 `<brand>/<ver>` 与
	// 白名单头组的 X-IDE-Version、X-Client-Version）。对齐官方 CN WorkBuddy 5.11.3 安装包。
	// config upstream.client_version 可覆盖（空 = 内置默认）。
	defaultClientVersion = "5.11.3"
	// defaultCliVersion 出站 UA 中 `CLI/<ver>` 段版本。对齐 5.11.3 桌面端内置的
	// `@genie/agent-cli`（2.176.0，见 resources/app.asar.unpacked/cli/package.json）。
	defaultCliVersion = "2.176.0"
	// defaultClientVersionGlobal / defaultCliVersionGlobal 国际版（global realm）桌面分发包
	// 版本段。2026-11-16 国际版 WorkBuddy AI 安装包实测为 5.11.4（内置 CLI 2.176.0）。
	defaultClientVersionGlobal = "5.11.4"
	defaultCliVersionGlobal    = "2.176.0"

	// defaultClientIdentityPlatform 5.11.x 桌面端 X-Client-Platform 固定值（client-identity.js）。
	defaultClientIdentityPlatform = "windows"

	originRefererCN     = "https://www.workbuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
)

// originRefererFor 按账号 realm 返回 Web 层 Origin/Referer 基础域：
// global → https://www.workbuddy.ai；cn → https://www.workbuddy.cn。
func originRefererFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return originRefererGlobal
	}
	return originRefererCN
}

// clientVersion 生效的 CN 默认客户端版本（无参兼容入口）。
func (c *Client) clientVersion() string {
	return c.clientVersionFor(nil)
}

// cliVersion 生效的 CN 默认 CLI 版本（无参兼容入口）。
func (c *Client) cliVersion() string {
	return c.cliVersionFor(nil)
}

// clientVersionFor 按账号 realm 取生效的 WorkBuddy 客户端版本段。
// 优先级：realm 专用显式配置（仅 global）> 通用显式配置 > realm 内置默认（CN 5.11.3 / global 5.11.4）。
func (c *Client) clientVersionFor(a *auth.Auth) string {
	global := a != nil && a.IsGlobal()
	if global && c != nil && c.ClientVersionGlobal != "" {
		return c.ClientVersionGlobal
	}
	if c != nil && c.ClientVersion != "" {
		return c.ClientVersion
	}
	if global {
		return defaultClientVersionGlobal
	}
	return defaultClientVersion
}

// cliVersionFor 按账号 realm 取生效的 `CLI/<ver>` 段（与 clientVersionFor 同优先级）。
func (c *Client) cliVersionFor(a *auth.Auth) string {
	global := a != nil && a.IsGlobal()
	if global && c != nil && c.CliVersionGlobal != "" {
		return c.CliVersionGlobal
	}
	if c != nil && c.CliVersion != "" {
		return c.CliVersion
	}
	if global {
		return defaultCliVersionGlobal
	}
	return defaultCliVersion
}

// uaBrandFor 按 realm 返回 applicationName / 品牌段（CN `"WorkBuddy"` / global `"workbuddy-ai"`）。
func uaBrandFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return "workbuddy-ai"
	}
	return "WorkBuddy"
}

// modelsProbeUA 返回 /v3/config 目录探测使用的 UA。
// 优先级：config upstream.models_probe_ua_cn / _global（按 realm）> 内置默认。
func (c *Client) modelsProbeUA(a *auth.Auth) string {
	global := a != nil && a.IsGlobal()
	if c != nil {
		if global && c.ModelsProbeUAGlobal != "" {
			return c.ModelsProbeUAGlobal
		}
		if !global && c.ModelsProbeUACN != "" {
			return c.ModelsProbeUACN
		}
	}
	if global {
		return c.defaultWorkBuddyUAFor(a)
	}
	return codeBuddyIDEUA
}

// defaultWorkBuddyUAFor 组装 chat SDK 链出站 UA（`WorkBuddy/<ver> <brand>/<ver> CLI/<cli>`）：
// 首段恒为驼峰 `WorkBuddy`，第二段随 realm 取 `WorkBuddy`（CN）或 `workbuddy-ai`（global）。
func (c *Client) defaultWorkBuddyUAFor(a *auth.Auth) string {
	ver := c.clientVersionFor(a)
	return "WorkBuddy/" + ver + " " + uaBrandFor(a) + "/" + ver + " CLI/" + c.cliVersionFor(a)
}

// defaultWorkBuddyUA 返回 CN 形态的默认 UA（兼容既有调用/测试）。
func (c *Client) defaultWorkBuddyUA() string {
	return c.defaultWorkBuddyUAFor(nil)
}

// rpcUAFor 组装桌面端 RPC/REST 层出站 UA（`<brand>/<ver> <brand>/<ver> CLI/<cli>`）。
// 显式配置 Client.UserAgent 时优先覆盖。
func (c *Client) rpcUAFor(a *auth.Auth) string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	ver := c.clientVersionFor(a)
	brand := uaBrandFor(a)
	return brand + "/" + ver + " " + brand + "/" + ver + " CLI/" + c.cliVersionFor(a)
}

// userAgent 返回 chat/refresh/FetchModels 等 SDK/客户端路径的生效 UA。
func (c *Client) userAgent(a *auth.Auth) string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	return c.defaultWorkBuddyUAFor(a)
}

// billingUA 返回 billing/checkin/banner 层出站 UA：对齐官方 RestOperations（axios/1.16.1）；
// 显式 client_name="SaaS" 保留不设 UA 的逃生行为。
func (c *Client) billingUA(a *auth.Auth) string {
	if c == nil || c.attributionClientName() == "SaaS" {
		return ""
	}
	return "axios/1.16.1"
}

func clientIdentityDisabled() bool {
	return os.Getenv("WB2A_NO_CLIENT_IDENTITY") == "1" || os.Getenv("WB2_NO_CLIENT_IDENTITY") == "1"
}

// injectClientIdentityHeaders 注入 5.11.3（CN）/ 5.11.4（global）app.asar client-identity.js
// 对齐的 X-Client-* 三件套；设环境变量 WB2A_NO_CLIENT_IDENTITY=1（或 WB2_NO_CLIENT_IDENTITY=1）可关闭。
func (c *Client) injectClientIdentityHeaders(req *http.Request, a *auth.Auth) {
	if clientIdentityDisabled() {
		return
	}
	req.Header.Set("X-Client-Version", c.clientVersionFor(a))
	req.Header.Set("X-Client-Platform", defaultClientIdentityPlatform)
	req.Header.Set("X-Client-Product", uaBrandFor(a))
}

// dropClientIdentityHeaders 清除由 CommonHeaders/BillingHeaders 注入的桌面端 X-Client-* 三头，
// 供微信小程序链路（tasks.go MP 端点）在覆写为 miniprogram 前调用。
func (c *Client) dropClientIdentityHeaders(req *http.Request) {
	req.Header.Del("X-Client-Version")
	req.Header.Del("X-Client-Platform")
	req.Header.Del("X-Client-Product")
}

// resolveDeviceToken 解析本次请求的 X-Device-Token 取值。
// 优先级：auth.Auth.DeviceToken（每号）> Client.DeviceToken（config 全局）> 文件兜底。
func (c *Client) resolveDeviceToken(a *auth.Auth) string {
	if a != nil && a.DeviceToken != "" {
		return a.DeviceToken
	}
	if c != nil && c.DeviceToken != "" {
		return c.DeviceToken
	}
	if c != nil && c.DeviceTokenFile != "" {
		return readDeviceTokenFile(c.DeviceTokenFile)
	}
	return ""
}

// ResolveGlobalDeviceToken 返回全局配置或文件解析出的设备风控 token（不依赖单账号）。
func (c *Client) ResolveGlobalDeviceToken() string { return c.resolveDeviceToken(nil) }

// injectDeviceToken 在 req 注入 X-Device-Token 头（仅当取到非空 token）。
func (c *Client) injectDeviceToken(req *http.Request, a *auth.Auth) {
	if tok := c.resolveDeviceToken(a); tok != "" {
		req.Header.Set("X-Device-Token", tok)
	}
}

// deriveAccountStableID 按 uid + 用途盐稳定派生 36 hex 设备/会话标识（仅供 /v2/report JSON body 使用；
// 不再作为 X-Machine-ID / X-Session-ID 注入 HTTP 头）。
func deriveAccountStableID(uid, purpose string) string {
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	return hex.EncodeToString(sum[:18]) // 36 hex chars
}

// CommonHeaders 设置桌面端 RPC/REST 共享请求头。
func (c *Client) CommonHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("User-Agent", c.rpcUAFor(a))
	c.injectClientIdentityHeaders(req, a)
}

// injectCodeBuddyRequest 注入 chat 链路 X-CodeBuddy-Request: 1。
func (c *Client) injectCodeBuddyRequest(req *http.Request) {
	req.Header.Set("X-CodeBuddy-Request", "1")
}

// AgentType* X-Agent-Type 枚举值（main = 用户主对话，auxiliary = 后台辅助/任务链路）。
const (
	AgentTypeMain      = "main"
	AgentTypeAuxiliary = "auxiliary"

	sdkClientArch    = "x64"
	sdkClientOS      = "Windows"
	sdkClientLang    = "js"
	sdkClientRuntime = "node"
	sdkClientNodeVer = "v22.21.1"
	sdkClientVer     = "6.25.0"
	agentIntentCraft = "craft"
	agentRuntimeLoc  = "local-agent"
)

// ChatMeta 一次 chat 出站的会话头族元数据（后台按 X-Conversation-Request-ID 聚合请求）。
type ChatMeta struct {
	ConversationID        string // X-Conversation-ID：入站 conversationId；空则回落粘性 UUIDv7 / 新建 UUIDv7
	CacheKeyID            string // prompt_cache_key 与 X-Conversation-ID / acp-connection-id 兜底推导用的稳定粘性会话键
	ConversationRequestID string // X-Conversation-Request-ID：轮级聚合主键
	RootRequestID         string // X-Root-Request-ID：会话级根请求 ID；空回落 ConversationRequestID
	TraceID               string // X-Trace-ID：入站透传值；空则独立生成 NewTraceID()（不等于 convReqID）
	AgentType             string // X-Agent-Type：空默认 "main"，后台任务填 AgentTypeAuxiliary ("auxiliary")
}

// ChatHeaders 在 common 之上加 chat 专属头族（对齐 5.11.3 CN / 5.11.4 Global 桌面端完整 46 头形态）。
func (c *Client) ChatHeaders(req *http.Request, a *auth.Auth, clientIP string, meta ChatMeta) {
	c.CommonHeaders(req, a)
	req.Header.Del("Origin")
	req.Header.Del("Referer")
	req.Header.Del("Accept-Language")
	req.Header.Del("X-Machine-ID")
	req.Header.Del("X-Session-ID")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("User-Agent", c.userAgent(a))

	uid := ""
	at := ""
	if a != nil {
		uid = a.UID
		at = a.AccessTokenValue()
	}
	if at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	} else {
		req.Header.Set("X-No-Authorization", "true")
	}
	if uid != "" {
		req.Header.Set("X-User-Id", uid)
	} else {
		req.Header.Set("X-No-User-Id", "true")
	}
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。
	// 不再发送 X-No-Enterprise-Id / X-No-Department-Info 哨兵头（桌面端仅在有值时发对应头）。
	if a == nil || !a.IsGlobal() {
		if a != nil && a.EnterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		}
		if d := bareDomain(domainOf(a)); d != "" {
			req.Header.Set("X-Domain", d)
		} else {
			req.Header.Set("X-Domain", c.chatHost(a))
		}
	} else {
		c.injectGlobalChatHeaders(req, a)
	}
	c.injectAttribution(req, a)
	req.Header.Set("X-Private-Data", privateDataHeaderValue(a))
	c.injectIdentityHeaders(req, a)
	c.injectClientIP(req, clientIP)
	c.injectDeviceToken(req, a)
	c.injectConversationHeaders(req, uid, meta)
	c.injectClientSDKHeaders(req, meta.AgentType)
	c.injectCodeBuddyRequest(req)
	req.Header.Set("X-Agent-Runtime", agentRuntimeLoc)
	c.injectBaggage(req, meta)
}

// injectIdentityHeaders 按账号企业/身份声明条件注入 X-Id-Source / X-Auth-Method / X-Userinfo；
// 纯个人号（即使 token 带 token_source=enterprise_switch）不发 X-Auth-Method / X-Userinfo。
func (c *Client) injectIdentityHeaders(req *http.Request, a *auth.Auth) {
	if a == nil || a.UID == "" {
		return
	}
	enterprise := strings.TrimSpace(a.EnterpriseID)
	idSource := a.IdSource()
	method := a.TokenSource()
	if idSource != "" {
		req.Header.Set("X-Id-Source", idSource)
	}
	if enterprise == "" && idSource == "" {
		return
	}
	if enterprise != "" && method != "" {
		req.Header.Set("X-Auth-Method", method)
	}
	payload := map[string]any{"uin": a.UID}
	if enterprise != "" {
		payload["owner_uin"] = enterprise
	}
	if idSource != "" {
		payload["id_source"] = idSource
	}
	if enterprise != "" && method != "" {
		payload["token_source"] = method
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	req.Header.Set("X-Userinfo", base64.StdEncoding.EncodeToString(raw))
}

func domainOf(a *auth.Auth) string {
	if a == nil {
		return ""
	}
	return a.DomainValue()
}

// privateDataHeaderValue 根据发行版（Global 5.11.4 DefaultModelOptimizationOff=true）与企业态返回 X-Private-Data。
func privateDataHeaderValue(a *auth.Auth) string {
	if a != nil && (a.IsEnterprise() || a.IsGlobal()) {
		return "true"
	}
	return "false"
}

// injectGlobalChatHeaders global 账号 chat 域声明头。
func (c *Client) injectGlobalChatHeaders(req *http.Request, a *auth.Auth) {
	if a == nil || !a.IsGlobal() {
		return
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	req.Header.Set("X-Domain", "www.workbuddy.ai")
}

// injectConversationHeaders 注入官方客户端会话、acp-connection-id 与 B3/W3C 链路追踪头族。
func (c *Client) injectConversationHeaders(req *http.Request, uid string, meta ChatMeta) {
	convReqID := meta.ConversationRequestID
	if convReqID == "" {
		convReqID = session.NewMessageID()
	}
	messageID := session.NewMessageID()
	conversationID := meta.ConversationID
	src := cacheKeySource(meta)
	if conversationID == "" {
		if src != "" {
			conversationID = buildTimeOrderedUUID(uid, src, "")
		} else {
			conversationID = session.NewTimeOrderedID()
		}
	}
	req.Header.Set("X-Conversation-ID", conversationID)
	req.Header.Set("X-Conversation-Request-ID", convReqID)
	req.Header.Set("X-Conversation-Message-ID", messageID)
	req.Header.Set("X-Request-ID", messageID)
	rootReqID := meta.RootRequestID
	if rootReqID == "" {
		rootReqID = convReqID
	}
	req.Header.Set("X-Root-Request-ID", rootReqID)

	traceID := meta.TraceID
	if traceID == "" {
		traceID = session.NewTraceID()
	}
	req.Header.Set("X-Trace-ID", traceID)
	b3Trace := traceID
	if !validTraceID(b3Trace) {
		b3Trace = messageID
	}
	spanID := session.NewSpanID()
	parentSpanID := session.NewSpanID()
	req.Header.Set("X-B3-TraceId", b3Trace)
	req.Header.Set("X-B3-SpanId", spanID)
	req.Header.Set("X-B3-ParentSpanId", parentSpanID)
	req.Header.Set("X-B3-Sampled", "1")
	req.Header.Set("b3", b3Trace+"-"+spanID+"-1-"+parentSpanID)
	req.Header.Set("traceparent", "00-"+b3Trace+"-"+spanID+"-01")
	if src != "" {
		req.Header.Set("acp-connection-id", buildTimeOrderedUUID(uid, src, "acp"))
	} else {
		req.Header.Set("acp-connection-id", session.NewTimeOrderedID())
	}
}

// injectClientSDKHeaders 注入官方 OpenAI-Node SDK 与 agent 语义指纹头。
func (c *Client) injectClientSDKHeaders(req *http.Request, agentType string) {
	if agentType == "" {
		agentType = AgentTypeMain
	}
	req.Header.Set("X-Agent-Type", agentType)
	req.Header.Set("X-Agent-Intent", agentIntentCraft)
	req.Header.Set("X-Stainless-Lang", sdkClientLang)
	req.Header.Set("X-Stainless-Package-Version", sdkClientVer)
	req.Header.Set("X-Stainless-OS", sdkClientOS)
	req.Header.Set("X-Stainless-Arch", sdkClientArch)
	req.Header.Set("X-Stainless-Runtime", sdkClientRuntime)
	req.Header.Set("X-Stainless-Runtime-Version", sdkClientNodeVer)
	req.Header.Set("X-Stainless-Retry-Count", "0")
}

// injectBaggage 注入 W3C baggage 链路传播头，与同请求会话与轮级 ID 对齐。
func (c *Client) injectBaggage(req *http.Request, meta ChatMeta) {
	conv := meta.ConversationID
	if conv == "" {
		conv = req.Header.Get("X-Conversation-ID")
	}
	reqID := meta.ConversationRequestID
	if reqID == "" {
		reqID = req.Header.Get("X-Conversation-Request-ID")
	}
	req.Header.Set("baggage", "buddyId=,buddyName=,codebuddy.session_id="+conv+
		",codebuddy.conversation_request_id="+reqID)
}

// validTraceID 判断 B3 TraceId 是否合法：16 或 32 位 hex。
func validTraceID(s string) bool {
	if len(s) != 16 && len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			return false
		}
	}
	return true
}

// attributionClientName 生效的用量归属名：ClientName 非空取之；空默认 "WorkBuddy"。
func (c *Client) attributionClientName() string {
	if c != nil && c.ClientName != "" {
		return c.ClientName
	}
	return "WorkBuddy"
}

// injectAttribution 注入用量归属头（X-Product / X-Agent-Purpose / X-IDE-*）。
func (c *Client) injectAttribution(req *http.Request, a *auth.Auth) {
	name := c.attributionClientName()
	req.Header.Set("X-Product", "SaaS")
	if name == "SaaS" {
		return
	}
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", name)
	req.Header.Set("X-IDE-Type", name)
	req.Header.Set("X-IDE-Version", c.clientVersionFor(a))
}

// injectClientIP 在 PassthroughIP 开启时把 clientIP 参数透传给上游。
func (c *Client) injectClientIP(req *http.Request, clientIP string) {
	if c == nil || !c.PassthroughIP || clientIP == "" {
		return
	}
	req.Header.Set("X-Forwarded-For", clientIP)
	req.Header.Set("X-Real-IP", clientIP)
	req.Header.Set("X-Client-IP", clientIP)
}

// ExtractClientIP 从入站请求提取客户端 IP 首段。
func ExtractClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return strings.TrimSpace(xff[:i])
			}
		}
		return strings.TrimSpace(xff)
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	return ""
}

func (c *Client) chatHost(a *auth.Auth) string {
	return hostOf(c.chatBase(a), "www.workbuddy.cn")
}

func (c *Client) billingHost(a *auth.Auth) string {
	return hostOf(c.billingBase(a), "www.workbuddy.cn")
}

// hostOf 提取裸主机名；若指向本地测试回环地址则返回 fallback，避免污染 X-Domain。
func hostOf(base, fallback string) string {
	if i := strings.Index(base, "://"); i >= 0 {
		base = base[i+3:]
	}
	if i := strings.IndexByte(base, '/'); i >= 0 {
		base = base[:i]
	}
	if base == "" ||
		strings.HasPrefix(base, "127.0.0.1") ||
		strings.HasPrefix(base, "localhost") ||
		strings.HasPrefix(base, "[::1]") {
		return fallback
	}
	return base
}

// BillingHeaders billing 接口请求头（对齐官方 RestOperations axios 栈与 X-Client-* 注入）。
func (c *Client) BillingHeaders(req *http.Request, a *auth.Auth) {
	if a != nil {
		req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", session.NewTraceID())
	if c != nil && c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	} else if ua := c.billingUA(a); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if a != nil {
		if a.UID != "" {
			req.Header.Set("X-User-Id", a.UID)
		}
		if a.EnterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
			req.Header.Set("X-Tenant-Id", a.EnterpriseID)
		}
	}
	if d := bareDomain(domainOf(a)); d != "" {
		req.Header.Set("X-Domain", d)
	} else {
		req.Header.Set("X-Domain", c.billingHost(a))
	}
	c.injectDeviceToken(req, a)
	c.injectClientIdentityHeaders(req, a)
}

// bareDomain 剥离 domain 字符串可能携带的协议头与末尾斜杠，确保 X-Domain 恒为纯主机名。
func bareDomain(raw string) string {
	d := strings.TrimSpace(raw)
	d = strings.TrimPrefix(d, "https://")
	d = strings.TrimPrefix(d, "http://")
	return strings.TrimSuffix(d, "/")
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
func (c *Client) RefreshHeaders(req *http.Request, a *auth.Auth) {
	c.CommonHeaders(req, a)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	if a != nil {
		req.Header.Set("X-Refresh-Token", a.RefreshToken)
		if a.EnterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		}
		if at := a.AccessTokenValue(); at != "" {
			req.Header.Set("Authorization", "Bearer "+at)
		} else {
			req.Header.Set("X-No-Authorization", "true")
		}
		if a.UID != "" {
			req.Header.Set("X-User-Id", a.UID)
		}
	} else {
		req.Header.Set("X-No-Authorization", "true")
	}
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Request-ID", session.NewTraceID())
	if d := bareDomain(domainOf(a)); d != "" {
		req.Header.Set("X-Domain", d)
	} else {
		req.Header.Set("X-Domain", c.chatHost(a))
	}
}
