package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func testGlobalAuth() *auth.Auth {
	return &auth.Auth{
		UID:         "g1",
		AccessToken: "at-g1",
		Domain:      "www.workbuddy.ai",
		ExpiresAt:   9999999999,
	}
}

func TestBillingSummaryPaidUserMissingIsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		known, paid bool
	}{
		{"missing", ``, false, false},
		{"false", `,"IsPaidUser":false`, true, false},
		{"true", `,"IsPaidUser":true`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/get-user-resource-summary") {
					_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"Packages":[]` + tc.field + `}}`))
					return
				}
				_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"Accounts":[],"TotalCount":0}}`))
			}))
			defer srv.Close()
			c := &Client{HTTP: srv.Client(), BillingBaseGlobal: srv.URL, GlobalEnabled: true}
			a := testGlobalAuth()
			if _, _, _, _, _, _, err := c.UserResourceSnapshot(a, 168*time.Hour); err != nil {
				t.Fatal(err)
			}
			paid, known := c.PaidUser(a.UID)
			if known != tc.known || paid != tc.paid {
				t.Fatalf("PaidUser = paid:%v known:%v, want paid:%v known:%v", paid, known, tc.paid, tc.known)
			}
		})
	}
}

func TestApplyModelTiersExclusive(t *testing.T) {
	badge := func(s string) *struct {
		Label string `json:"label"`
	} {
		return &struct {
			Label string `json:"label"`
		}{Label: s}
	}
	out := map[string]ModelInfo{
		"pro-model":      {ID: "pro-model"},
		"priority-model": {ID: "priority-model"},
	}
	applyModelTiers(out, []v3ModelTier{
		{Enabled: true, Priority: 200, ModelIDs: []string{"pro-model"}, AccessMode: "exclusive", Badge: badge("Pro Only")},
		{Enabled: true, Priority: 100, ModelIDs: []string{"priority-model"}, Badge: badge("订阅优先")},
	})
	if !out["pro-model"].Exclusive {
		t.Error("accessMode=exclusive 的模型应标 Exclusive")
	}
	if out["priority-model"].Exclusive {
		t.Error("只有「订阅优先」文案、没有 accessMode 的不算门控")
	}
}

func TestModelExclusivePerRealm(t *testing.T) {
	c := &Client{}
	c.storeModelRates("global", []ModelInfo{{ID: "pro-model", Exclusive: true}, {ID: "free-model"}})
	c.storeModelRates("cn", []ModelInfo{{ID: "pro-model"}})
	if !c.ModelExclusive("global", "pro-model") {
		t.Error("global:pro-model 应被标为 exclusive")
	}
	if c.ModelExclusive("cn", "pro-model") {
		t.Error("cn 域未标 exclusive，不该串味")
	}
	if c.ModelExclusive("global", "free-model") || c.ModelExclusive("global", "") {
		t.Error("未标 exclusive 的模型 / 空模型名不该被拦")
	}
}

func TestPaidUserUnknownVsFree(t *testing.T) {
	c := &Client{}
	if _, known := c.PaidUser("u1"); known {
		t.Fatal("没记过就该是 unknown")
	}
	c.NotePaidUser("u1", false)
	if paid, known := c.PaidUser("u1"); !known || paid {
		t.Fatalf("PaidUser = %v,%v want false,true", paid, known)
	}
	c.NotePaidUser("u1", true)
	if paid, _ := c.PaidUser("u1"); !paid {
		t.Fatal("后一次观测应覆盖前一次")
	}
}
