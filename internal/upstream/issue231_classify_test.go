package upstream

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

// realChannelRefusedBody 是 Oracle 服务器 journald 里抓到的真实 11-128 报文
// （2026-10-20 10:30:24，acct acct-redacted）。"code" 是 JSON 数字 11-128，msg 与
// 11140 封禁完全相同 —— 这正是 issue #231 误判的根因。
const realChannelRefusedBody = `{"code":11-128,"msg":"request illegal","requestId":"921f6d034b3bce901fa2f823500f1487","displayMsg":{"en":"Request blocked. Please send it again","zh":"请求被拦截，请重新发送"},"displayTips":{"en":"This call failed channel verification, it may not be sent from an official client","zh":"这次调用没通过渠道校验，可能不是官方客户端发出的"}}`

// TestClassifyIssue231BusinessCodeSplit 锁定分类层的业务码分流：11-128 通道校验
// 失败绝不落 account_fault（否则 handler 会 Disable 健康账号）。
func TestClassifyIssue231BusinessCodeSplit(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		{
			name:   "real 11-128 numeric code from server log",
			status: http.StatusBadRequest,
			body:   realChannelRefusedBody,
			want:   ErrChannelRefused,
		},
		{
			name:   "11-128 as quoted string",
			status: http.StatusBadRequest,
			body:   `{"code":"11-128","msg":"request illegal"}`,
			want:   ErrChannelRefused,
		},
		{
			name:   "11-128 with en dash typography",
			status: http.StatusBadRequest,
			body:   `{"code":"11–128","msg":"request illegal"}`,
			want:   ErrChannelRefused,
		},
		{
			name:   "11-128 nested envelope, conversation id missing (issue 188)",
			status: http.StatusBadRequest,
			body:   `{"error":{"data":{"code":11-128,"msg":"conversation id missing"}}}`,
			want:   ErrChannelRefused,
		},
		{
			name:   "11-128 first message not system (issue 231 variant)",
			status: http.StatusBadRequest,
			body:   `{"code":11-128,"msg":"first message is not system prompt"}`,
			want:   ErrChannelRefused,
		},
		{
			name:   "11-128 content-review variant stays content blocked",
			status: http.StatusBadRequest,
			body:   `{"code":11-128,"msg":"Illegal API invocation from an unapproved channel"}`,
			want:   ErrContentBlocked,
		},
		{
			name:   "11140 real restriction stays account fault",
			status: http.StatusForbidden,
			body:   `{"code":11140,"msg":"request illegal"}`,
			want:   ErrAccountFault,
		},
		{
			name:   "11140 with model rate-limit text degrades to soft rate",
			status: http.StatusBadRequest,
			body:   `{"code":11140,"msg":"request illegal","displayMsg":{"en":"The model provider is rate-limiting requests."}}`,
			want:   ErrSoftRate,
		},
		{
			name:   "bare request illegal without code is not a ban",
			status: http.StatusBadRequest,
			body:   `{"msg":"request illegal"}`,
			want:   ErrClient,
		},
		{
			name:   "14017 trial not activated stays account fault",
			status: http.StatusTooManyRequests,
			body:   `{"code":14017,"msg":"trial not activated"}`,
			want:   ErrAccountFault,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.status, c.body); got != c.want {
				t.Fatalf("Classify(%d) = %s, want %s", c.status, got, c.want)
			}
		})
	}
}

// TestIsAccountRestrictedIssue231 锁定封禁判据的「三条件」：业务码 + 文案 +
// 无限流语义。任何单条件命中都不足以禁用账号。
func TestIsAccountRestrictedIssue231(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"11140 + request illegal", `{"code":11140,"msg":"request illegal"}`, true},
		{"11140 nested + request illegal", `{"error":{"data":{"code":11140,"msg":"request illegal"}}}`, true},
		{"11140 + request illegal + rate limit", `{"code":11140,"msg":"request illegal","tips":"The model provider is rate-limiting requests."}`, false},
		{"11-128 + request illegal (channel, not ban)", realChannelRefusedBody, false},
		{"request illegal without code", `{"msg":"request illegal"}`, false},
		{"11140 without request illegal text", `{"code":11140,"msg":"account suspended"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsAccountRestricted(c.body); got != c.want {
				t.Fatalf("IsAccountRestricted = %v, want %v (body=%s)", got, c.want, c.body)
			}
		})
	}
}

// TestIsChannelRefusedIssue231 锁定通道校验判据：业务码为准，11140 优先，
// 内容审核变体让路。
func TestIsChannelRefusedIssue231(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"real 11-128 body", realChannelRefusedBody, true},
		{"11-128 string form", `{"code":"11-128","msg":"request illegal"}`, true},
		{"11-128 content review variant", `{"code":11-128,"msg":"blocked by security policy"}`, false},
		{"11140 wins over channel wording", `{"code":11140,"msg":"request illegal","displayTips":{"en":"channel verification failed"}}`, false},
		{"no business code", `{"msg":"request illegal"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsChannelRefused(c.body); got != c.want {
				t.Fatalf("IsChannelRefused = %v, want %v (body=%s)", got, c.want, c.body)
			}
		})
	}
}

// TestAccountRestrictionReasonKeepsEvidence 禁用原因必须带结构化业务码；
// 有 requestId 时一并保留（原始错误依据可追溯）。
func TestAccountRestrictionReasonKeepsEvidence(t *testing.T) {
	reason := AccountRestrictionReason(`{"code":11140,"msg":"request illegal","requestId":"abc123"}`)
	if !strings.Contains(reason, "11140") || !strings.Contains(reason, "abc123") {
		t.Fatalf("reason lost evidence: %q", reason)
	}
	if got := AccountRestrictionReason(`{"code":11140,"msg":"request illegal"}`); strings.Contains(got, "requestId=") {
		t.Fatalf("reason must not invent requestId: %q", got)
	}
}

// TestClassifyRealChannelBodyFromJournal 用服务器 journald 抓到的**原始字节**做回归：
// 上游下发的 code 是 5 位数字 11-128（无连字符），日志/issue 里的 "11-128" 是终端
// 渲染插入的连字符。参考实现按带连字符的字符串比较，在这个报文上永远不命中。
func TestClassifyRealChannelBodyFromJournal(t *testing.T) {
	raw, err := os.ReadFile("testdata/issue231_channel_body.txt")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	body := strings.TrimSpace(string(raw))
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden} {
		if got := Classify(status, body); got != ErrChannelRefused {
			t.Fatalf("status=%d Classify=%s want channel_refused (body=%q)", status, got, body)
		}
	}
	if !IsChannelRefused(body) {
		t.Fatal("IsChannelRefused must accept the real 11-128 body")
	}
	if IsAccountRestricted(body) {
		t.Fatal("real 11-128 body must never be an account restriction")
	}
}
