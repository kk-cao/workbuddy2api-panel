// checkin_activity.go 签到活动状态查询（POST /billing/meter/checkin-activity-status）。
package upstream

import (
	"encoding/json"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	checkinActivityPath   = "/billing/meter/checkin-activity-status"
	checkinActivityPathV2 = "/v2/billing/meter/checkin-activity-status"
)

// CheckinActivity 签到活动状态。
type CheckinActivity struct {
	Active          bool   `json:"active"`
	TodayCheckedIn  bool   `json:"today_checked_in"`
	StreakDays      int64  `json:"streak_days"`
	DailyCredit     int64  `json:"daily_credit"`
	TodayCredit     int64  `json:"today_credit"`
	WeekCheckinDays int64  `json:"week_checkin_days"`
	TotalCredits    int64  `json:"total_credits"`
	EndTime         string `json:"end_time"`
}

func (c *Client) checkinActivityPaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{checkinActivityPath, checkinActivityPathV2}
	}
	return []string{checkinActivityPathV2}
}

// GetCheckinActivityStatus 只读查询签到活动状态（不参与任何账号惩罚）。
func (c *Client) GetCheckinActivityStatus(a *auth.Auth) (CheckinActivity, error) {
	data, err := c.billingMeterJSON(a, c.checkinActivityPaths(a), http.MethodPost, map[string]any{})
	if err != nil {
		return CheckinActivity{}, err
	}
	var st CheckinActivity
	if err := json.Unmarshal(data, &st); err != nil {
		return CheckinActivity{}, err
	}
	return st, nil
}
