// checkin_global.go 国际版（global realm）每日签到：只读先行查活动状态，仅 active 且未签才发写请求。
package scheduler

import (
	"log"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func (s *Scheduler) checkinGlobal(a *auth.Auth) {
	if a.IsEnterprise() {
		return
	}
	label := logfmt.Label(a.UID, a.Nickname)
	act, err := s.cfg.Upstream.GetCheckinActivityStatus(a)
	if err != nil {
		log.Printf("WARN: checkin %s: 国际版活动状态查询失败（不发签到请求）: %v", label, err)
		return
	}
	if !act.Active {
		log.Printf("checkin %s: 国际版签到活动未开启（每日 %d 积分），跳过", label, act.DailyCredit)
		return
	}
	if act.TodayCheckedIn {
		s.cfg.Pool.NoteCheckinDone(a.UID)
		log.Printf("checkin %s: 国际版今天已签到（连登 %d 天，每日 %d 积分）", label, act.StreakDays, act.DailyCredit)
		return
	}
	if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
		if upstream.IsAlreadyCheckin(err) {
			s.cfg.Pool.NoteCheckinDone(a.UID)
			log.Printf("checkin %s: 国际版今天已签到（幂等）", label)
			return
		}
		log.Printf("WARN: checkin %s: 国际版签到失败: %v", label, err)
		return
	}
	s.cfg.Pool.NoteCheckinDone(a.UID)
	log.Printf("checkin %s: 国际版签到成功（每日 %d 积分）", label, act.DailyCredit)
}
