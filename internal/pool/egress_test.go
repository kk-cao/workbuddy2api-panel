package pool

import (
	"strconv"
	"testing"
	"time"
)

func TestEgressStreakIncrementsAndResets(t *testing.T) {
	p := New("")
	now := time.Now()
	for i := 1; i <= 3; i++ {
		if got := p.NoteEgressFailure("direct:cn", now); got != i {
			t.Fatalf("第 %d 次连续失败 got=%d want %d", i, got, i)
		}
		if got := p.EgressStreak("direct:cn", now); got != i {
			t.Fatalf("EgressStreak 第 %d 次 got=%d want %d", i, got, i)
		}
		now = now.Add(time.Second)
	}
	now = now.Add(egressWindow - time.Second)
	if got := p.NoteEgressFailure("direct:cn", now); got != 4 {
		t.Fatalf("窗口内应接着上一轮: got=%d want 4", got)
	}
	now = now.Add(egressWindow + time.Second)
	if got := p.NoteEgressFailure("direct:cn", now); got != 1 {
		t.Fatalf("超过窗口应重新计数: got=%d want 1", got)
	}
}

func TestEgressStreakExpiresWithoutTraffic(t *testing.T) {
	p := New("")
	now := time.Now()
	p.NoteEgressFailure("direct:global", now)
	p.NoteEgressFailure("direct:global", now)
	if got := p.EgressStreak("direct:global", now); got != 2 {
		t.Fatalf("got=%d want 2", got)
	}
	if got := p.EgressStreak("direct:global", now.Add(egressWindow+time.Second)); got != 0 {
		t.Fatalf("过期后应报 0: got=%d", got)
	}
}

func TestEgressKeysAreIndependent(t *testing.T) {
	p := New("")
	now := time.Now()
	for range 3 {
		p.NoteEgressFailure("direct:cn", now)
	}
	if got := p.EgressStreak("direct:global", now); got != 0 {
		t.Fatalf("global 不该受 cn 影响: got=%d", got)
	}
	if got := p.NoteEgressFailure("direct:global", now); got != 1 {
		t.Fatalf("global 应从 1 起: got=%d", got)
	}
}

func TestEgressEmptyKeyIgnored(t *testing.T) {
	p := New("")
	now := time.Now()
	if got := p.NoteEgressFailure("", now); got != 0 {
		t.Fatalf("空键应返回 0: got=%d", got)
	}
	if got := p.EgressStreak("", now); got != 0 {
		t.Fatalf("空键应返回 0: got=%d", got)
	}
	if len(p.egress) != 0 {
		t.Fatalf("空键不应写入计数表: %v", p.egress)
	}
}

func TestEgressTableBounded(t *testing.T) {
	p := New("")
	now := time.Now()
	for i := 0; i < maxEgressKeys+20; i++ {
		p.NoteEgressFailure("p:node-"+strconv.Itoa(i), now)
	}
	if len(p.egress) > maxEgressKeys+1 {
		t.Fatalf("计数表超限未回收: %d 项", len(p.egress))
	}
}
