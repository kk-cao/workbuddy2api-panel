package scheduler

// balance_401.go 余额探测 401 的登录态求证：通过 RefreshToken 区分会话真死（12153）与
// 降级 token（billing 401 但 refresh 可换新），仅前者的过期 AT 才喂 NoteSessionDead。

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const authHealBackoff = 30 * time.Minute

func (s *Scheduler) noteBalanceAuthFailure(a *auth.Auth, uid string, probeErr error) {
	var ue *upstream.Error
	if !errors.As(probeErr, &ue) || ue.Status != http.StatusUnauthorized {
		return
	}
	if a.RefreshTokenValue() == "" {
		return
	}
	label := logfmt.Label(uid, a.Nickname)
	// 仅在 refresh 成功时记退避时刻，真死号每轮继续试以便按阈值及时禁用。
	if !s.allowAuthHeal(uid) {
		log.Printf("balance %s: 401（%s 内已求证过登录态，本轮跳过 refresh）", label, authHealBackoff)
		return
	}
	rerr := s.cfg.Upstream.RefreshToken(a)
	if rerr == nil {
		if serr := a.SaveAtomic(); serr != nil {
			log.Printf("balance %s: 401 后刷新成功但落盘失败: %v", label, serr)
		}
		s.noteAuthHeal(uid)
		s.cfg.Pool.ClearSessionDead(uid)
		log.Printf("balance %s: 401 经 refresh 求证为降级 token（会话仍活），已换新 token 并清 session dead 计数", label)
		return
	}
	var re *upstream.Error
	if errors.As(rerr, &re) && (re.Kind == upstream.ErrSessionDead || strings.Contains(re.Msg, "12153")) {
		if a.AccessTokenValue() != "" && !a.NeedsRefresh(0) {
			log.Printf("balance %s: refresh token 已失效但 access token 仍有效，不计 session dead", label)
			return
		}
		if s.cfg.Pool.NoteSessionDead(uid) {
			log.Printf("balance %s: 连续 %d 次 401 且 refresh 判死 — 禁用（需重新登录）", label, pool.SessionDeadThreshold())
			return
		}
		log.Printf("balance %s: 401 且 refresh 判死（计入 session dead 计数，连续 %d 次将禁用）", label, pool.SessionDeadThreshold())
		return
	}
	log.Printf("balance %s: 401 但 refresh 失败（非 session dead，不计罚）: %v", label, rerr)
}

func (s *Scheduler) allowAuthHeal(uid string) bool {
	s.authHealMu.Lock()
	defer s.authHealMu.Unlock()
	if t, ok := s.authHealAt[uid]; ok && time.Since(t) < authHealBackoff {
		return false
	}
	return true
}

func (s *Scheduler) noteAuthHeal(uid string) {
	s.authHealMu.Lock()
	defer s.authHealMu.Unlock()
	if s.authHealAt == nil {
		s.authHealAt = make(map[string]time.Time)
	}
	s.authHealAt[uid] = time.Now()
}
