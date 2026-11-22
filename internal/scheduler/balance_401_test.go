package scheduler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

type healFake struct {
	mu         sync.Mutex
	refresh    int
	probe      int
	sessionDie bool
	dieMsg     string
}

func (f *healFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token/refresh") {
			f.refresh++
			if f.sessionDie {
				msg := f.dieMsg
				if msg == "" {
					msg = "Offline user session not found"
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"code":12153,"msg":%q}`, msg)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"code":0,"data":{"accessToken":"at-new","refreshToken":"rt-new","expiresIn":5184000}}`)
			return
		}
		f.probe++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func healScheduler(t *testing.T, srvURL string, a *auth.Auth) *Scheduler {
	t.Helper()
	up := &upstream.Client{HTTP: &http.Client{}, ChatBaseCN: srvURL, BillingBaseCN: srvURL, WebBaseCN: srvURL}
	p := pool.New("")
	p.Add(a)
	return New(Config{Pool: p, Upstream: up})
}

func TestBalanceProbe401RefreshSaysSessionDead(t *testing.T) {
	f := &healFake{sessionDie: true}
	srv := f.server(t)
	a := &auth.Auth{UID: "u1", AccessToken: "at-old", RefreshToken: "rt-1", ExpiresAt: 1}
	s := healScheduler(t, srv.URL, a)

	thresh := pool.SessionDeadThreshold()
	for i := 1; i < thresh; i++ {
		s.RunBalanceRefreshNow()
		st, _ := s.cfg.Pool.Status("u1")
		if st.Disabled {
			t.Fatalf("第 %d/%d 次不应提前禁用", i, thresh)
		}
	}
	s.RunBalanceRefreshNow()
	st, _ := s.cfg.Pool.Status("u1")
	if !st.Disabled {
		t.Fatalf("第 %d 次 401+12153 应触发禁用: %+v", thresh, st)
	}
	if f.refresh != thresh {
		t.Fatalf("refresh 调用=%d want %d", f.refresh, thresh)
	}
}

func TestBalanceProbe401RefreshOKIsDegradedToken(t *testing.T) {
	f := &healFake{}
	srv := f.server(t)
	a := &auth.Auth{UID: "u1", AccessToken: "at-old", RefreshToken: "rt-1", ExpiresAt: 1}
	s := healScheduler(t, srv.URL, a)

	s.RunBalanceRefreshNow()
	st, _ := s.cfg.Pool.Status("u1")
	if st.Disabled {
		t.Fatalf("refresh 成功不该禁用: %+v", st)
	}
	if got := a.AccessTokenValue(); got != "at-new" {
		t.Fatalf("accessToken=%q want at-new", got)
	}
	if f.refresh != 1 {
		t.Fatalf("refresh 调用=%d want 1", f.refresh)
	}

	s.RunBalanceRefreshNow()
	if f.refresh != 1 {
		t.Fatalf("退避窗口内 refresh 调用=%d want 1", f.refresh)
	}
	if f.probe != 2 {
		t.Fatalf("余额探测=%d want 2", f.probe)
	}
}

func TestBalanceProbeNonAuthDoesNotRefresh(t *testing.T) {
	f := &healFake{}
	srv := f.server(t)
	a := &auth.Auth{UID: "u1", AccessToken: "at-old", RefreshToken: "rt-1", ExpiresAt: 1}
	s := healScheduler(t, srv.URL, a)

	for _, err := range []error{
		&upstream.Error{Kind: upstream.ErrUpstreamBusy, Status: http.StatusInternalServerError, Msg: "boom"},
		&upstream.Error{Kind: upstream.ErrSoftRate, Status: http.StatusTooManyRequests, Msg: "429 rate limit"},
		&upstream.Error{Kind: upstream.ErrClient, Status: http.StatusForbidden, Msg: "forbidden"},
	} {
		s.noteBalanceAuthFailure(a, "u1", err)
	}
	if f.refresh != 0 {
		t.Fatalf("非 401 不该触发 refresh 求证，实际 %d 次", f.refresh)
	}
	// 确认此前未累积任何 session dead：需要完整走满 thresh 轮 401+12153 才会禁用。
	f.mu.Lock()
	f.sessionDie = true
	f.mu.Unlock()
	for i := 1; i < pool.SessionDeadThreshold(); i++ {
		s.RunBalanceRefreshNow()
		if st, _ := s.cfg.Pool.Status("u1"); st.Disabled {
			t.Fatalf("非 401 不应提前消耗 session dead 计数（第 %d 轮禁用）", i)
		}
	}
}

func TestAuthHealBackoffOnlyOnSuccess(t *testing.T) {
	s := New(Config{})
	if !s.allowAuthHeal("u1") {
		t.Fatal("首次应允许求证")
	}
	s.noteAuthHeal("u1")
	if s.allowAuthHeal("u1") {
		t.Fatal("成功后应进入退避窗口")
	}
	s.authHealMu.Lock()
	s.authHealAt["u1"] = time.Now().Add(-authHealBackoff - time.Minute)
	s.authHealMu.Unlock()
	if !s.allowAuthHeal("u1") {
		t.Fatal("窗口过期后应恢复求证")
	}
	if !s.allowAuthHeal("u2") {
		t.Fatal("不同账号互不影响")
	}
}

func TestBalanceProbe401SessionDeadLocalizedMessage(t *testing.T) {
	f := &healFake{sessionDie: true, dieMsg: "会话已失效，请重新登录"}
	srv := f.server(t)
	a := &auth.Auth{UID: "u1", AccessToken: "at-old", RefreshToken: "rt-1", ExpiresAt: 1}
	s := healScheduler(t, srv.URL, a)

	for i := 0; i < pool.SessionDeadThreshold(); i++ {
		s.RunBalanceRefreshNow()
	}
	if st, _ := s.cfg.Pool.Status("u1"); !st.Disabled {
		t.Fatalf("本地化 12153 文案连败达阈应禁用: %+v", st)
	}
}
