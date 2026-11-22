// egress.go 出口级瞬时失败计数：同一出口共用计数器，一次链路抖动仅触发秒级退避，
// 不把偶发网络抖动错记到单个账号头上触发降权出池。运行态，不持久化。
package pool

import "time"

// egressWindow 距上次失败超过该窗口后，连续计数归零重计新一轮。
const egressWindow = 60 * time.Second

// maxEgressKeys 出口计数表上限；超限时整体清空重计。
const maxEgressKeys = 128

type egressStat struct {
	streak int
	last   time.Time
}

// NoteEgressFailure 记录一次出口级瞬时失败，返回包含本次在内的连续次数（从 1 起）。
// 空 key 一律忽略并返回 0。
func (p *Pool) NoteEgressFailure(key string, now time.Time) int {
	if key == "" {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.egress == nil {
		p.egress = make(map[string]egressStat)
	}
	st := p.egress[key]
	if st.last.IsZero() || now.Sub(st.last) > egressWindow {
		st.streak = 0
	}
	st.streak++
	st.last = now
	if len(p.egress) > maxEgressKeys {
		if _, exists := p.egress[key]; !exists {
			p.egress = make(map[string]egressStat, 1)
		}
	}
	p.egress[key] = st
	return st.streak
}

// EgressStreak 返回该出口当前的连续失败次数；超过窗口未再失败返回 0。
func (p *Pool) EgressStreak(key string, now time.Time) int {
	if key == "" {
		return 0
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	st, ok := p.egress[key]
	if !ok || st.last.IsZero() || now.Sub(st.last) > egressWindow {
		return 0
	}
	return st.streak
}
