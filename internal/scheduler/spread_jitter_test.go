package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestJitterOffsetDeterministicBoundedAndDistinct(t *testing.T) {
	nominal := time.Date(2026, 11, 26, 9, 0, 0, 0, cstZone)
	if off := jitterOffset(taskCheckin, nominal, 0); off != 0 {
		t.Fatalf("jitter=0 got %v want 0", off)
	}
	a := jitterOffset(taskCheckin, nominal, 15)
	b := jitterOffset(taskCheckin, nominal, 15)
	if a != b {
		t.Fatalf("non-deterministic jitter: %v vs %v", a, b)
	}
	if a < 0 || a >= 15*time.Minute {
		t.Fatalf("jitter out of [0,15m): %v", a)
	}
	// 超出 59 分钟应钳位，绝不跨小时。
	if clamped := jitterOffset(taskCheckin, nominal, 120); clamped < 0 || clamped >= 59*time.Minute {
		t.Fatalf("clamped jitter out of [0,59m): %v", clamped)
	}
	// 不同任务族在同一名义整点应得到不同偏移，避免 checkin 与 travel 同秒撞车。
	tr := jitterOffset(taskTravel, nominal, 15)
	if tr == a {
		t.Fatalf("expected checkin and travel to stagger at same nominal hour, both got %v", a)
	}
}

func TestNextFireKindAndNextWakeWithJitter(t *testing.T) {
	loc := cstZone
	nominal := time.Date(2026, 11, 26, 9, 0, 0, 0, loc)
	off := jitterOffset(taskCheckin, nominal, 15)
	if off <= time.Second {
		t.Fatalf("unexpected tiny offset %v for test nominal", off)
	}
	// 名义整点已过但仍在今日抖动点之前：必须落到今日 nominal+off，不能跳去明天。
	now := nominal.Add(off - time.Second)
	got := nextFireKind(now, []int{9}, taskCheckin, 15)
	want := nominal.Add(off)
	if !got.Equal(want) {
		t.Fatalf("nextFireKind(%v) = %v want %v", now, got, want)
	}
	// 刚好到达抖动点后，应滚到次日（并重新按次日哈希算偏移）。
	nextDay := nextFireKind(want, []int{9}, taskCheckin, 15)
	if nextDay.Day() != 27 || nextDay.Hour() != 9 {
		t.Fatalf("after fire expected next day 09:xx, got %v", nextDay)
	}

	// 同配 9 点的 checkin 与 travel，开启 jitter 后不会合并在同一个唤醒时刻。
	s := New(Config{
		CheckinHours:      []int{9},
		TravelHours:       []int{9},
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		BlackcatDisabled:  true,
		GrowthDisabled:    true,
		JitterMinutes:     15,
	})
	at1, kinds1 := s.nextWake(time.Date(2026, 11, 26, 8, 50, 0, 0, loc))
	if len(kinds1) != 1 {
		t.Fatalf("expected checkin & travel separated by jitter, got at=%v kinds=%v", at1, kinds1)
	}
	at2, kinds2 := s.nextWake(at1)
	if len(kinds2) != 1 || kinds2[0] == kinds1[0] || at2.Hour() != 9 {
		t.Fatalf("expected second 09:xx wake for the other kind, got at=%v kinds=%v (first=%v)", at2, kinds2, kinds1)
	}
}

func TestSetJitterAndSpreadHotRearm(t *testing.T) {
	s := New(Config{
		CheckinHours:      []int{9},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		BlackcatDisabled:  true,
		GrowthDisabled:    true,
	})
	s.SetJitterAndSpread(15, 5, 15)
	select {
	case <-s.rearmSchedule:
	default:
		t.Fatal("changing jitter must signal rearmSchedule")
	}
	w, g := s.spreadParams()
	if w != 5*time.Minute || g != 15*time.Second {
		t.Fatalf("spreadParams = (%v, %v) want (5m, 15s)", w, g)
	}
}

func TestAccountSpreadSpacingOrderAndCancel(t *testing.T) {
	now := time.Date(2026, 11, 26, 9, 0, 0, 0, cstZone)
	uids := []string{"u1", "u2", "u3", "u4", "u5", "u6", "u7"}
	p1 := newAccountSpread(taskCheckin, uids, 5*time.Minute, 15*time.Second, now)
	p2 := newAccountSpread(taskCheckin, uids, 5*time.Minute, 15*time.Second, now)
	if p1 == nil {
		t.Fatal("expected non-nil spread plan")
	}
	// 同日确定性 + 落于 [now, now+window)。
	for _, u := range uids {
		if !p1.at[u].Equal(p2.at[u]) {
			t.Fatalf("uid %s non-deterministic: %v vs %v", u, p1.at[u], p2.at[u])
		}
		if p1.at[u].Before(now) || !p1.at[u].Before(now.Add(5*time.Minute)) {
			t.Fatalf("uid %s out of window: %v", u, p1.at[u])
		}
	}
	// order 按计划时刻升序，且除窗口末尾钳位外均满足 minGap。
	list := make([]pool.Status, len(uids))
	for i, u := range uids {
		list[i] = pool.Status{UID: u}
	}
	ordered := p1.order(list)
	for i := 1; i < len(ordered); i++ {
		prev := p1.at[ordered[i-1].UID]
		cur := p1.at[ordered[i].UID]
		if cur.Before(prev) {
			t.Fatalf("order not monotonic at %d: %v < %v", i, cur, prev)
		}
		if cur.Before(now.Add(5*time.Minute - time.Second)) && cur.Sub(prev) < 15*time.Second {
			t.Fatalf("gap too small between %s and %s: %v", ordered[i-1].UID, ordered[i].UID, cur.Sub(prev))
		}
	}

	// wait 响应 ctx 取消，不睡满。
	future := &accountSpread{at: map[string]time.Time{"u1": time.Now().Add(time.Hour)}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if future.wait(ctx, "u1") {
		t.Fatal("wait should return false on canceled ctx")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("wait did not exit immediately on cancel: %v", time.Since(start))
	}
}

func TestRunBatchSpreadReentryGuardPerInstance(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		entered <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"access_token":"at2","refresh_token":"rt2","expires_in":3600}}`))
	}))
	defer srv.Close()

	makeSched := func() *Scheduler {
		p := pool.New("")
		p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
		up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
		return New(Config{
			Pool:                 p,
			Upstream:             up,
			AccountSpreadMinutes: 1, // >0 启用 spread 分支
		})
	}
	s1 := makeSched()
	s2 := makeSched()
	// 用已过点的计划加速测试：仅将 spreadRunning 占住并验证同实例拦截、跨实例不互相干扰。
	s1.spreadRunning[taskKeepalive].Store(true)
	s1.runBatch(context.Background(), []taskKind{taskKeepalive})
	if calls.Load() != 0 {
		t.Fatalf("s1 should skip reentrant keepalive, got %d calls", calls.Load())
	}
	s1.spreadRunning[taskKeepalive].Store(false)

	// s2 独立实例不应受 s1 标记影响。
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s2.runKeepalive(context.Background(), false)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("s2 keepalive did not reach server")
	}
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("expected 1 call on s2, got %d", calls.Load())
	}
}
