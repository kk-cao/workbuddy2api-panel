package upstream

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestTasksMPDropsDesktopClientIdentityHeaders(t *testing.T) {
	var seen []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Clone())
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[],"already_claimed":false,"credit":10,"energy":1}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BillingBaseCN: srv.URL, ChatBaseCN: srv.URL}
	a := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.workbuddy.cn"}

	if _, err := c.ListTasksMP(a); err != nil {
		t.Fatalf("ListTasksMP: %v", err)
	}
	if _, _, err := c.ClaimRewardMP(a, "daily_checkin"); err != nil {
		t.Fatalf("ClaimRewardMP: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(seen))
	}
	for i, hdr := range seen {
		if got := hdr.Get("X-Client-Platform"); got != mpPlatform {
			t.Errorf("req[%d] X-Client-Platform=%q want %q", i, got, mpPlatform)
		}
		if got := hdr.Get("X-Client-Version"); got != "" {
			t.Errorf("req[%d] X-Client-Version=%q must be dropped on MP endpoints", i, got)
		}
		if got := hdr.Get("X-Client-Product"); got != "" {
			t.Errorf("req[%d] X-Client-Product=%q must be dropped on MP endpoints", i, got)
		}
	}
}
