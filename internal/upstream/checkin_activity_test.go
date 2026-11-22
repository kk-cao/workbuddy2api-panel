package upstream

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestGetCheckinActivityStatusShape(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Method != http.MethodPost {
			t.Errorf("method=%s want POST", r.Method)
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"active":true,"today_checked_in":true,` +
			`"streak_days":17,"daily_credit":100,"today_credit":100,"is_streak_day":false,` +
			`"checkin_dates":["2026-10-16"],"week_checkin_days":5,"week_progress":[true,false],` +
			`"total_credits":1700,"start_time":"2026-10-01 00:00:00","end_time":"2026-10-31 23:59:59"}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	st, err := c.GetCheckinActivityStatus(&auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.codebuddy.cn"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if gotPath != "/v2/billing/meter/checkin-activity-status" {
		t.Errorf("path=%s want /v2/billing/meter/checkin-activity-status", gotPath)
	}
	if !st.Active || !st.TodayCheckedIn || st.StreakDays != 17 || st.DailyCredit != 100 ||
		st.TodayCredit != 100 || st.WeekCheckinDays != 5 || st.TotalCredits != 1700 ||
		st.EndTime != "2026-10-31 23:59:59" {
		t.Fatalf("parsed=%+v", st)
	}
}

func TestGetCheckinActivityStatusGlobalFallsBack(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/billing/meter/checkin-activity-status" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"active":true,"today_checked_in":false}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL,
		ChatBaseGlobal: srv.URL, BillingBaseGlobal: srv.URL, GlobalEnabled: true}
	a, err := auth.Parse([]byte(`{"uid":"g1","accessToken":"at","realm":"global","domain":"www.workbuddy.ai","expiresAt":9999999999}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetCheckinActivityStatus(a); err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(paths) != 2 || paths[0] != "/billing/meter/checkin-activity-status" || paths[1] != "/v2/billing/meter/checkin-activity-status" {
		t.Fatalf("global fallback paths=%v", paths)
	}
}
