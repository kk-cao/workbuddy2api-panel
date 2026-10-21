package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// issue231ChannelBody 是服务器 journald 里抓到的真实 11-128 报文（JSON 数字
// 形式的业务码 + 与 11140 完全相同的 "request illegal" 文案）。
const issue231ChannelBody = `{"code":11-128,"msg":"request illegal","requestId":"921f6d034b3bce901fa2f823500f1487","displayMsg":{"en":"Request blocked. Please send it again","zh":"请求被拦截，请重新发送"},"displayTips":{"en":"This call failed channel verification, it may not be sent from an official client","zh":"这次调用没通过渠道校验，可能不是官方客户端发出的"}}`

// TestIssue231ApplyPolicyChannelRefusedNeverBans 通道校验失败：不冷却、不禁用、
// 不计失败。旧实现在此处调用 Disable，把健康账号永久踢出池子。
func TestIssue231ApplyPolicyChannelRefusedNeverBans(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p})
	h.applyErrorPolicy("u1", upstream.ErrChannelRefused, issue231ChannelBody, "glm-5.2", nil)
	st, _ := p.Status("u1")
	if st.Disabled {
		t.Fatalf("channel refusal must not disable the account: %+v", st)
	}
	if st.Cooling {
		t.Fatalf("channel refusal must not cool the account: reason=%q", st.Reason)
	}
	if st.ErrTotal != 0 || st.ConsecutiveFails != 0 || st.BreakerFails != 0 {
		t.Fatalf("channel refusal must not count as account failure: %+v", st)
	}
}

// TestIssue231AccountFaultWithChannelCodeStillNeverBans 防御路径：即使调用方把
// 11-128 报文归到 ErrAccountFault，applyErrorPolicy 也不得禁用账号。
func TestIssue231AccountFaultWithChannelCodeStillNeverBans(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p})
	h.applyErrorPolicy("u1", upstream.ErrAccountFault, issue231ChannelBody, "glm-5.2", nil)
	st, _ := p.Status("u1")
	if st.Disabled {
		t.Fatalf("11-128 body must never be treated as a ban: %+v", st)
	}
}

// TestIssue231TrueRestrictionStillDisabled 真实账号限制（11140）保留禁用，
// 并把业务码与 requestId 写进 reason 作为原始错误依据。
func TestIssue231TrueRestrictionStillDisabled(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p})
	body := `{"code":11140,"msg":"request illegal","requestId":"deadbeefcafe"}`
	h.applyErrorPolicy("u1", upstream.ErrAccountFault, body, "glm-5.2", nil)
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatal("real 11140 restriction must still disable the account")
	}
	if !strings.Contains(st.Reason, "11140") || !strings.Contains(st.Reason, "deadbeefcafe") {
		t.Fatalf("disable reason lost evidence: %q", st.Reason)
	}
}

// TestIssue231AccountFaultWithRateLimitTextIsSoft 11140 信封里明确写着模型限流
// 时按软限流处理：不禁用，只做有界冷却。
func TestIssue231AccountFaultWithRateLimitTextIsSoft(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p})
	body := `{"code":11140,"msg":"request illegal","displayMsg":{"en":"The model provider is rate-limiting requests."}}`
	h.applyErrorPolicy("u1", upstream.ErrAccountFault, body, "glm-5.2", nil)
	st, _ := p.Status("u1")
	if st.Disabled {
		t.Fatalf("rate-limit variant must not be banned: reason=%q", st.Reason)
	}
	if !st.Cooling {
		t.Fatalf("rate-limit variant should get a bounded cooldown: %+v", st)
	}
}

// TestIssue231ChannelRefusedFailsFastWithoutRotation 端到端：同一份请求换号也
// 撞同一道通道校验，轮转只会放大无效请求。必须 1 次上游调用、400 透传原文、
// 账号无惩罚、响应里不出现封禁语义。
func TestIssue231ChannelRefusedFailsFastWithoutRotation(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(_ string) (int, string, bool) {
		calls++
		return http.StatusBadRequest, issue231ChannelBody, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "a", AccessToken: "at-a", ExpiresAt: 9999999999},
		&auth.Auth{UID: "b", AccessToken: "at-b", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if calls != 1 {
		t.Fatalf("channel refusal must not rotate to another account, upstream calls=%d", calls)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s (want 400 passthrough)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "channel_refused") {
		t.Fatalf("response should carry code=channel_refused: %s", body)
	}
	if !strings.Contains(body, "11-128") {
		t.Fatalf("response should carry the upstream body verbatim: %s", body)
	}
	if strings.Contains(body, "11140") || strings.Contains(body, "banned") {
		t.Fatalf("channel refusal must not be reported as a ban: %s", body)
	}
	for _, uid := range []string{"a", "b"} {
		st, _ := p.Status(uid)
		if st.Disabled || st.Cooling || st.ErrTotal != 0 {
			t.Fatalf("account %s must stay healthy: %+v", uid, st)
		}
	}
}

// TestIssue231TrueRestrictionEndToEndDisables 端到端：11140 真限制仍然禁用账号。
func TestIssue231TrueRestrictionEndToEndDisables(t *testing.T) {
	up := newFakeUpstream(t, func(_ string) (int, string, bool) {
		return http.StatusForbidden, `{"code":11140,"msg":"request illegal","requestId":"cafebabe"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "a", AccessToken: "at-a", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	st, _ := p.Status("a")
	if !st.Disabled {
		t.Fatalf("real restriction must disable the account (status=%d body=%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(st.Reason, "11140") {
		t.Fatalf("disable reason must keep the business code: %q", st.Reason)
	}
}
