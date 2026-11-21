package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestRestriction11140StopsRotationAndIsolatesByRealm(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-gl" {
			return 200, sseOK, true
		}
		return 403, `{"code":11140,"msg":"账号存在异常行为，已被限制使用"}`, false
	})

	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at-cn1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "cn2", AccessToken: "at-cn2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "gl1", AccessToken: "at-gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	p.SetCredits("cn1", 3000, 0)
	p.SetCredits("cn2", 2000, 0)
	p.SetCredits("gl1", 1000, 0)

	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: true})

	// 第 1 次 CN 请求命中 cn1 11140：必须立即停手，不能顺带把 cn2 也撞封
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec1.Code != http.StatusForbidden {
		t.Fatalf("req1 code=%d body=%s want 403", rec1.Code, rec1.Body)
	}
	if calls["Bearer at-cn1"] != 1 || calls["Bearer at-cn2"] != 0 {
		t.Fatalf("req1 must stop rotation after 11140: calls=%v", calls)
	}
	if st1, _ := p.Status("cn1"); !st1.Disabled {
		t.Errorf("cn1 should be disabled after 11140")
	}

	// 第 2 次 CN 请求命中 cn2 11140：达阈值 2 → 触发 CN 域连坐暂停
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("req2 code=%d want 403", rec2.Code)
	}

	// 即使再加回一个健康 CN 号，CN 域在暂停窗口内仍必须快速 503 + Retry-After，0 上游调用
	p.Add(&auth.Auth{UID: "cn3", AccessToken: "at-cn3", ExpiresAt: 9999999999})
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec3.Code != http.StatusServiceUnavailable || !strings.Contains(rec3.Body.String(), "upstream_restriction_hold") {
		t.Fatalf("req3 code=%d body=%s want 503 upstream_restriction_hold", rec3.Code, rec3.Body)
	}
	if rec3.Header().Get("Retry-After") == "" {
		t.Errorf("req3 missing Retry-After header")
	}
	if calls["Bearer at-cn3"] != 0 {
		t.Errorf("cn3 must not be called during CN restriction hold: calls=%v", calls)
	}

	// global 域完全不受 CN 连坐暂停影响
	recGL := httptest.NewRecorder()
	h.ServeHTTP(recGL, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if recGL.Code != http.StatusOK {
		t.Fatalf("global request failed during CN hold: code=%d body=%s", recGL.Code, recGL.Body)
	}
}

func TestRestriction11140StreamErrorFrameDisablesAccountAndFeedsGate(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at-cn1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "cn2", AccessToken: "at-cn2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "cn3", AccessToken: "at-cn3", ExpiresAt: 9999999999},
	)
	p.SetCredits("cn1", 3000, 0)
	p.SetCredits("cn2", 2000, 0)
	p.SetCredits("cn3", 1000, 0)

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, "data: {\"error\":{\"code\":11140,\"message\":\"Account has unusual activity\"}}\n\ndata: [DONE]\n\n", false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})

	for _, uid := range []string{"cn1", "cn2"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
		if st, _ := p.Status(uid); !st.Disabled {
			t.Fatalf("%s must be disabled after SSE 11140 restriction frame: %+v", uid, st)
		}
	}

	// cn1 与 cn2 经 SSE 首帧命中 11140 后，第 3 次 CN 请求同样命中连坐暂停
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec3.Code != http.StatusServiceUnavailable || !strings.Contains(rec3.Body.String(), "upstream_restriction_hold") {
		t.Fatalf("req3 code=%d body=%s want 503 upstream_restriction_hold", rec3.Code, rec3.Body)
	}
}

func TestRequestLevelErrorsFailFastWithoutRotationOrPenalty(t *testing.T) {
	cases := []struct {
		name string
		body string
		code string
	}{
		{"11128 channel refused", `{"code":"11-128","msg":"request illegal"}`, "channel_refused"},
		{"11129 bad tool schema", `{"code":11129,"msg":"invalid_function_call_parameters"}`, "bad_params"},
		{"11152 bad params", `{"code":11152,"msg":"invalid request"}`, "bad_params"},
		{"11151 empty content", `{"code":11151,"msg":"a message has empty content"}`, "empty_content"},
		{"11148 tool mismatch", `{"code":11148,"msg":"tool calls and tool results do not match"}`, "tool_mismatch"},
		{"11133 model param invalid", `{"code":11133,"msg":"model_param_invalid"}`, "model_param_invalid"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			up := newFakeUpstream(t, func(authz string) (int, string, bool) {
				calls++
				return 400, tc.body, false
			})
			p := testPoolWith(
				&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
				&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
			)
			h := NewHandler(Config{Pool: p, Upstream: up})

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
				strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code=%d body=%s want 400", rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), tc.code) {
				t.Errorf("body=%s want code %q", rec.Body, tc.code)
			}
			if calls != 1 {
				t.Errorf("calls=%d want 1 (must fail-fast without rotating to u2)", calls)
			}
			for _, uid := range []string{"u1", "u2"} {
				st, _ := p.Status(uid)
				if st.Disabled || st.Cooling || st.ErrTotal != 0 {
					t.Errorf("%s should have zero penalty: %+v", uid, st)
				}
			}
		})
	}
}

func Test11102SameRealmFailsFastAndCrossDomainSkipsRemainingCN(t *testing.T) {
	t.Run("no cross fallback stops after single CN 11102", func(t *testing.T) {
		calls := map[string]int{}
		up := newFakeUpstream(t, func(authz string) (int, string, bool) {
			calls[authz]++
			return 400, `{"code":11102,"msg":"model [only-global] service info not found"}`, false
		})
		p := testPoolWith(
			&auth.Auth{UID: "cn1", AccessToken: "at-cn1", ExpiresAt: 9999999999},
			&auth.Auth{UID: "cn2", AccessToken: "at-cn2", ExpiresAt: 9999999999},
		)
		h := NewHandler(Config{Pool: p, Upstream: up})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"only-global","messages":[{"role":"user","content":"hi"}]}`)))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "model_unavailable") {
			t.Fatalf("code=%d body=%s want 400 model_unavailable", rec.Code, rec.Body)
		}
		if calls["Bearer at-cn1"]+calls["Bearer at-cn2"] != 1 {
			t.Errorf("must stop after single CN 11102, calls=%v", calls)
		}
	})

	t.Run("cross fallback jumps straight to global without hitting second CN", func(t *testing.T) {
		calls := map[string]int{}
		up := newFakeUpstream(t, func(authz string) (int, string, bool) {
			calls[authz]++
			if authz == "Bearer at-gl" {
				return 200, sseOK, true
			}
			return 400, `{"code":11102,"msg":"model [only-global] service info not found"}`, false
		})
		p := testPoolWith(
			&auth.Auth{UID: "cn1", AccessToken: "at-cn1", ExpiresAt: 9999999999},
			&auth.Auth{UID: "cn2", AccessToken: "at-cn2", ExpiresAt: 9999999999},
			&auth.Auth{UID: "gl1", AccessToken: "at-gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
		)
		p.SetCredits("cn1", 3000, 0)
		p.SetCredits("cn2", 2000, 0)
		p.SetCredits("gl1", 1000, 0)
		p.SetCrossDomainFallback(true)

		h := NewHandler(Config{Pool: p, Upstream: up})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"only-global","messages":[{"role":"user","content":"hi"}]}`)))
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s want 200", rec.Code, rec.Body)
		}
		if calls["Bearer at-cn1"] != 1 || calls["Bearer at-cn2"] != 0 || calls["Bearer at-gl"] != 1 {
			t.Errorf("expected cn1=1 cn2=0 gl=1, got calls=%v", calls)
		}
	})
}
