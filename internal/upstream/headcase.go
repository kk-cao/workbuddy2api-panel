// headcase.go 提供 wire 层请求头大小写精确拼写表（供 headorder.go 在明文写出前还原，
// 不修改内存 req.Header 的 Go Canonical 键）。
package upstream

import "strings"

// officialHeaderSpelling 官方出站头的精确大小写拼写（key 一律小写，value 为 wire 字面量）。
var officialHeaderSpelling = map[string]string{
	"accept":            "Accept",
	"accept-encoding":   "Accept-Encoding",
	"accept-language":   "Accept-Language",
	"authorization":     "Authorization",
	"content-encoding":  "Content-Encoding",
	"content-length":    "Content-Length",
	"content-type":      "Content-Type",
	"cookie":            "Cookie",
	"origin":            "Origin",
	"referer":           "Referer",
	"user-agent":        "User-Agent",
	"x-agent-intent":    "X-Agent-Intent",
	"x-agent-purpose":   "X-Agent-Purpose",
	"x-agent-runtime":   "X-Agent-Runtime",
	"x-agent-type":      "X-Agent-Type",
	"x-b3-sampled":      "X-B3-Sampled",
	"x-domain":          "X-Domain",
	"x-private-data":    "X-Private-Data",
	"x-product":         "X-Product",
	"x-requested-with":  "X-Requested-With",
	"x-user-id":         "X-User-Id",
	"x-client-version":  "X-Client-Version",
	"x-client-platform": "X-Client-Platform",
	"x-client-product":  "X-Client-Product",

	// 官方全小写
	"x-stainless-arch":            "x-stainless-arch",
	"x-stainless-lang":            "x-stainless-lang",
	"x-stainless-os":              "x-stainless-os",
	"x-stainless-package-version": "x-stainless-package-version",
	"x-stainless-retry-count":     "x-stainless-retry-count",
	"x-stainless-runtime":         "x-stainless-runtime",
	"x-stainless-runtime-version": "x-stainless-runtime-version",
	"acp-connection-id":           "acp-connection-id",
	"x-codebuddy-request":         "x-codebuddy-request",
	"baggage":                     "baggage",
	"traceparent":                 "traceparent",
	"b3":                          "b3",

	// 官方保留大写缩写（Go Canonical 会破坏它们）
	"x-conversation-id":         "X-Conversation-ID",
	"x-conversation-message-id": "X-Conversation-Message-ID",
	"x-conversation-request-id": "X-Conversation-Request-ID",
	"x-request-id":              "X-Request-ID",
	"x-root-request-id":         "X-Root-Request-ID",
	"x-trace-id":                "X-Trace-ID",
	"x-ide-type":                "X-IDE-Type",
	"x-ide-name":                "X-IDE-Name",
	"x-ide-version":             "X-IDE-Version",
	"x-b3-traceid":              "X-B3-TraceId",
	"x-b3-parentspanid":         "X-B3-ParentSpanId",
	"x-b3-spanid":               "X-B3-SpanId",
	"x-auth-refresh-source":     "X-Auth-Refresh-Source",
	"x-device-token":            "X-Device-Token",
	"x-enterprise-id":           "X-Enterprise-Id",
	"x-tenant-id":               "X-Tenant-Id",
	"x-userinfo":                "X-Userinfo",
	"x-auth-method":             "X-Auth-Method",
	"x-id-source":               "X-Id-Source",
}

// officialHeaderName 返回 headerName 对应的官方 wire 拼写；未命中则返回原名。
func officialHeaderName(headerName string) string {
	if want, ok := officialHeaderSpelling[strings.ToLower(headerName)]; ok {
		return want
	}
	return headerName
}
