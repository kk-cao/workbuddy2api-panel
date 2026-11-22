package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestEgressBackoffLadder(t *testing.T) {
	base := time.Second
	cases := []struct {
		streak int
		want   time.Duration
	}{
		{0, 0},
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 15 * time.Second},
		{9, 15 * time.Second},
	}
	for _, c := range cases {
		got := egressBackoff(c.streak, base)
		if c.want == 0 {
			if got != 0 {
				t.Errorf("streak=%d got=%v want 0", c.streak, got)
			}
			continue
		}
		lo := time.Duration(float64(c.want) * 0.75)
		hi := time.Duration(float64(c.want) * 1.25)
		if got < lo || got > hi {
			t.Errorf("streak=%d got=%v want in [%v,%v]", c.streak, got, lo, hi)
		}
	}
	if got := egressBackoff(3, 0); got != 0 {
		t.Errorf("base=0 must return 0, got=%v", got)
	}
}

func egressTransportErrUpstream(calls *int) *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			*calls++
			return nil, errors.New("dial tcp: connection refused")
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

func TestTransportFailureChargedToEgressNotAccount(t *testing.T) {
	var calls int
	p := testPoolWith(
		&auth.Auth{UID: "e1", AccessToken: "at-e1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "e2", AccessToken: "at-e2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "e3", AccessToken: "at-e3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: egressTransportErrUpstream(&calls)})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s want 503", rec.Code, rec.Body)
	}
	if calls == 0 {
		t.Fatal("expected upstream calls > 0")
	}
	for _, uid := range []string{"e1", "e2", "e3"} {
		st, _ := p.Status(uid)
		if st.ConsecutiveFails != 0 || !st.DegradeUntil.IsZero() {
			t.Errorf("account %s penalized for egress jitter: consecutiveFails=%d degradeUntil=%v",
				uid, st.ConsecutiveFails, st.DegradeUntil)
		}
	}
	if got := p.EgressStreak(egressKey(&auth.Auth{UID: "e1"}), time.Now()); got != calls {
		t.Errorf("egress streak=%d want %d", got, calls)
	}
}

func TestEgressGiveUpStopsRotating(t *testing.T) {
	var calls int
	p := testPoolWith(
		&auth.Auth{UID: "g1", AccessToken: "at-g1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g2", AccessToken: "at-g2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g3", AccessToken: "at-g3", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g4", AccessToken: "at-g4", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g5", AccessToken: "at-g5", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: egressTransportErrUpstream(&calls), MaxRotate: egressGiveUpStreak})
	send := func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("code=%d body=%s want 503", rec.Code, rec.Body)
		}
	}
	send()
	first := calls
	if first < egressGiveUpStreak {
		t.Fatalf("first request calls=%d < egressGiveUpStreak=%d", first, egressGiveUpStreak)
	}
	send()
	if second := calls - first; second != 1 {
		t.Fatalf("second request calls=%d want 1 (should stop rotating)", second)
	}
}

func TestEgressStreakSeparatedByRealm(t *testing.T) {
	if got := egressKey(&auth.Auth{UID: "x", AccessToken: "a"}); got != "direct:cn" {
		t.Fatalf("cn default got=%q", got)
	}
	auth.SetGlobalEnabled(true)
	ga := &auth.Auth{UID: "x", AccessToken: "a", Domain: "www.workbuddy.ai"}
	if got := egressKey(ga); got != "direct:global" {
		t.Fatalf("global got=%q", got)
	}
	if got := egressKey(nil); got != "direct:cn" {
		t.Fatalf("nil got=%q", got)
	}
}

func TestChatQueueWaiting6020WaitsOnceThenRescuesAndReleasesSlot(t *testing.T) {
	const body6020 = `{"code":6020,"msg":"queue.waiting.title","requestId":"rid-6020","data":{"queue_position":2502,"queue_size":"999+","retry_after":40,"estimated_wait":12510}}`

	origWait := busyWaitMax
	busyWaitMax = 120 * time.Millisecond
	defer func() { busyWaitMax = origWait }()

	var mu sync.Mutex
	var calls int
	firstDone := make(chan struct{}, 1)
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			select {
			case firstDone <- struct{}{}:
			default:
			}
			return 429, body6020, false
		}
		return 200, sseOK, true
	})

	p := testPoolWith(&auth.Auth{UID: "q1", AccessToken: "at-q1", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Available: func() []string { return []string{"q1"} },
	})
	sess.Bind("conv-q1", "q1")
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, MaxRotate: 1})

	// 并发检查等待窗口期间并发槽位已释放且粘性仍在。
	slotReleased := make(chan bool, 1)
	go func() {
		<-firstDone
		deadline := time.Now().Add(90 * time.Millisecond)
		for time.Now().Before(deadline) {
			if st, ok := p.Status("q1"); ok && st.InFlight == 0 {
				if p.Acquire("q1") {
					p.Release("q1")
					slotReleased <- true
					return
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
		slotReleased <- false
	}()

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[],"metadata":{"conversation_id":"conv-q1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !<-slotReleased {
		t.Fatal("6020 等待期间必须先调用 releaseHeld 释放并发槽位")
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("code=%d body=%s want 200 SSE", rec.Code, rec.Body)
	}
	if calls != 2 {
		t.Fatalf("calls=%d want 2 (1次6020等待 + 1次同号重试)", calls)
	}
	if uid, ok := sess.Resolve("conv-q1"); !ok || uid != "q1" {
		t.Fatalf("6020 不应解绑粘性会话: (%q, %v)", uid, ok)
	}
}

func TestChatQueueWaiting6020FallsThroughTo503AfterSecondBusy(t *testing.T) {
	const body6020 = `{"code":6020,"msg":"queue.waiting.title","requestId":"rid-6020-2","data":{"queue_position":3000,"queue_size":"999+","retry_after":40}}`

	origWait := busyWaitMax
	busyWaitMax = 10 * time.Millisecond
	defer func() { busyWaitMax = origWait }()

	var calls int
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 429, body6020, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "q1", AccessToken: "at-q1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "q2", AccessToken: "at-q2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s want 503", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "upstream_busy") {
		t.Fatalf("body=%s want upstream_busy", rec.Body)
	}
	if calls != 2 {
		t.Fatalf("calls=%d want 2 (最多只等一轮，二次仍 6020 直接 503)", calls)
	}
	for _, uid := range []string{"q1", "q2"} {
		st, _ := p.Status(uid)
		if st.Cooling || st.Disabled || st.ErrTotal != 0 {
			t.Errorf("account %s must not be penalized for 6020: %+v", uid, st)
		}
	}
}
