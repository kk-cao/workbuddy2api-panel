// restrict.go 11140 风控连坐保护闸门：
//  1. 命中 11140 账号级限制 → 禁用该号、本请求立刻停手不轮转；
//  2. 同域（cn / global）窗口内不同被限制账号达到阈值 max(2, healthy/10) → 暂停该域派发（30s 起 2 倍退避至 10m），
//     互不牵连另一域。
package server

import (
	"log"
	"sync"
	"time"
)

var (
	defaultRestrictionHold = 10 * time.Minute
	restrictionWaveWindow  = 15 * time.Minute
	restrictionWaveMin     = 2
	restrictionWaveDiv     = 10
	restrictionHoldBase    = 30 * time.Second
	restrictionHoldMax     = 10 * time.Minute
	restrictionCalmReset   = 30 * time.Minute
)

func restrictionThreshold(healthy int) int {
	if restrictionWaveDiv <= 0 {
		return restrictionWaveMin
	}
	if n := healthy / restrictionWaveDiv; n > restrictionWaveMin {
		return n
	}
	return restrictionWaveMin
}

type restrictionState struct {
	hits   map[string]time.Time
	until  time.Time
	streak int
	last   time.Time
}

// restrictionGate 按 realm 隔离的 11140 连坐防护状态机（零值可用）。
type restrictionGate struct {
	mu sync.Mutex
	m  map[string]*restrictionState
}

// paused 返回 realm 当前是否处于连坐暂停期及剩余时间。
func (g *restrictionGate) paused(realm string) (time.Duration, bool) {
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		return 0, false
	}
	st := g.m[realm]
	if st == nil || st.until.IsZero() {
		return 0, false
	}
	rem := st.until.Sub(now)
	if rem <= 0 {
		st.until = time.Time{}
		return 0, false
	}
	return rem, true
}

// note 记录一次 realm 上 uid 命中 11140 账号限制，达到阈值时激活该域暂停。
func (g *restrictionGate) note(realm, uid string, hold time.Duration, healthy int) bool {
	if hold <= 0 {
		return false
	}
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		g.m = map[string]*restrictionState{}
	}
	st := g.m[realm]
	if st == nil {
		st = &restrictionState{hits: map[string]time.Time{}}
		g.m[realm] = st
	}
	for u, t := range st.hits {
		if now.Sub(t) > restrictionWaveWindow {
			delete(st.hits, u)
		}
	}
	st.hits[uid] = now
	need := restrictionThreshold(healthy)
	if len(st.hits) < need {
		return false
	}
	if !st.last.IsZero() && now.Sub(st.last) > restrictionCalmReset {
		st.streak = 0
	}
	st.streak++
	st.last = now
	pause := restrictionHoldBase
	for i := 1; i < st.streak; i++ {
		pause *= 2
		if pause >= restrictionHoldMax {
			break
		}
	}
	if pause > restrictionHoldMax {
		pause = restrictionHoldMax
	}
	if pause > hold {
		pause = hold
	}
	st.until = now.Add(pause)
	st.hits = map[string]time.Time{}
	log.Printf("WARN: [server] upstream restriction wave realm=%q: %d accounts restricted within %s (threshold %d, %d healthy), dispatch paused %s (until %s, streak=%d)",
		realm, need, restrictionWaveWindow, need, healthy, pause, st.until.Format(time.RFC3339), st.streak)
	return true
}
