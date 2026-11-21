// restrict.go 上游 11140「账号被风控限制」的判据（连坐暂停与停手防线，见 server/restrict.go）。
package upstream

import "strings"

// restrictionMarkers 账号风控限制文书专属措辞（避免把模型限流或泛化文案误判为设备级扫荡）。
var restrictionMarkers = []string{
	"账号存在异常行为",
	"unusual activity",
}

// IsRestricted 报告响应是否为 code 11140 + 账号风控限制文书。
func IsRestricted(body string) bool {
	if !has11140(body) {
		return false
	}
	lower := strings.ToLower(body)
	for _, m := range restrictionMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// IsAccountRestriction 报告 body 是否属于 11140 账号级限制的任一形态（限制文书或 request illegal）。
// 两种形态均要求结构化 code 11140，且显式排除模型级限流文案，避免误命中 11-128 或模型限流。
func IsAccountRestriction(body string) bool {
	if IsRestricted(body) {
		return true
	}
	if !has11140(body) {
		return false
	}
	lower := strings.ToLower(body)
	if !strings.Contains(lower, "request illegal") {
		return false
	}
	for _, m := range softRateMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return false
		}
	}
	return true
}

// IsAccountRestricted 保留既有导出的兼容名称（等价 IsAccountRestriction）。
func IsAccountRestricted(body string) bool {
	return IsAccountRestriction(body)
}

func has11140(body string) bool {
	return hasBusinessCode(body, "11140")
}
