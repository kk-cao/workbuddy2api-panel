// spread.go 账号级摊开：排程触发的账号遍历型任务不再同一秒打全池，
// 每个账号在窗口内按确定性偏移错开执行，缓解同 IP 多号并发触发 429。
package scheduler

import (
	"context"
	"fmt"
	"hash/fnv"
	"sort"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// accountSpread 本轮各账号的执行时刻（绝对时间，以计划生成时刻为起点）。
type accountSpread struct {
	at map[string]time.Time
}

// newAccountSpread 生成账号错峰计划：偏移 = FNV(任务|uid|CST自然日) % 窗口秒数，
// 相邻账号间距小于 minGap 时将靠后者顺延推开。
//
// shortcut: 推开后超过窗口末尾的偏移钳到窗口最后一秒，账号数 × minGap 接近窗口时末尾会重叠；
// 池规模扩大到数十号以上时应调大 account_spread_minutes 或调小 minGap。
func newAccountSpread(kind taskKind, uids []string, window, minGap time.Duration, now time.Time) *accountSpread {
	secs := int64(window / time.Second)
	if secs <= 0 || len(uids) == 0 {
		return nil
	}
	gap := int64(minGap / time.Second)
	if gap < 0 {
		gap = 0
	}
	day := travelDay(now)
	type entry struct {
		uid string
		off int64
	}
	plan := make([]entry, 0, len(uids))
	h := fnv.New32a()
	for _, uid := range uids {
		h.Reset()
		_, _ = fmt.Fprintf(h, "%s|%s|%s", kind, uid, day)
		plan = append(plan, entry{uid: uid, off: int64(h.Sum32()) % secs})
	}
	sort.Slice(plan, func(i, j int) bool {
		if plan[i].off != plan[j].off {
			return plan[i].off < plan[j].off
		}
		return plan[i].uid < plan[j].uid
	})
	for i := 1; i < len(plan); i++ {
		if minOff := plan[i-1].off + gap; plan[i].off < minOff {
			plan[i].off = minOff
		}
		if plan[i].off >= secs {
			plan[i].off = secs - 1
		}
	}
	e := &accountSpread{at: make(map[string]time.Time, len(plan))}
	for _, p := range plan {
		e.at[p.uid] = now.Add(time.Duration(p.off) * time.Second)
	}
	return e
}

// wait 等到该账号的计划时刻；计划为 nil（关闭/手动入口）或账号不在计划中时立即放行。
// 返回 false 表示 ctx 已取消（优雅停机）。
func (a *accountSpread) wait(ctx context.Context, uid string) bool {
	if a == nil {
		return ctx.Err() == nil
	}
	at, ok := a.at[uid]
	if !ok {
		return ctx.Err() == nil
	}
	return sleepCtx(ctx, time.Until(at))
}

// order 按计划时刻升序返回账号列表，避免晚时刻账号堵住早时刻账号导致全部塌缩到同一秒。
func (a *accountSpread) order(list []pool.Status) []pool.Status {
	if a == nil || len(list) == 0 {
		return list
	}
	out := append([]pool.Status(nil), list...)
	sort.SliceStable(out, func(i, j int) bool {
		ti, iok := a.at[out[i].UID]
		tj, jok := a.at[out[j].UID]
		if !iok || !jok {
			return iok && !jok
		}
		return ti.Before(tj)
	})
	return out
}

// spreadParams 返回当前生效的账号摊开窗口与最小间隔（在 schedMu 下快照）。
func (s *Scheduler) spreadParams() (window, minGap time.Duration) {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return time.Duration(s.cfg.AccountSpreadMinutes) * time.Minute,
		time.Duration(s.cfg.AccountSpreadMinGapSeconds) * time.Second
}

// spreadEnabled 是否启用了账号级摊开。
func (s *Scheduler) spreadEnabled() bool {
	w, _ := s.spreadParams()
	return w > 0
}

// spreadPlan 生成本轮错峰计划；on=false（面板手动触发或测试直调）恒返回 nil。
func (s *Scheduler) spreadPlan(kind taskKind, on bool, now time.Time) *accountSpread {
	if !on {
		return nil
	}
	window, gap := s.spreadParams()
	if window <= 0 {
		return nil
	}
	list := s.cfg.Pool.List()
	uids := make([]string, len(list))
	for i, st := range list {
		uids[i] = st.UID
	}
	return newAccountSpread(kind, uids, window, gap, now)
}
