package pool

import (
	"fmt"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func mkPool(n int) *Pool {
	p := New("")
	for i := 0; i < n; i++ {
		uid := fmt.Sprintf("u%d", i)
		p.Add(&auth.Auth{UID: uid, AccessToken: "at-" + uid, ExpiresAt: 9999999999})
		p.SetCredits(uid, 1000, 0)
	}
	return p
}

func TestPickSkipsKnownFreeForExclusiveModel(t *testing.T) {
	p := mkPool(2)
	paid := map[string]struct {
		paid  bool
		known bool
	}{
		"u0": {paid: false, known: true},
		"u1": {paid: true, known: true},
	}
	p.SetExclusiveModels(
		func(_, model string) bool { return model == "pro-model" },
		func(uid string) (bool, bool) { s := paid[uid]; return s.paid, s.known },
	)
	for i := 0; i < 20; i++ {
		if a := p.PickExcludingForModel(nil, "pro-model"); a == nil || a.UID != "u1" {
			t.Fatalf("exclusive 模型应只派给已知付费号，第 %d 次拿到 %v", i, a)
		}
	}
	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		if a := p.PickExcludingForModel(nil, "free-model"); a != nil {
			seen[a.UID] = true
		}
	}
	if !seen["u0"] || !seen["u1"] {
		t.Fatalf("非 exclusive 模型不该被闸门影响，实际只用到 %v", seen)
	}
}

func TestPickExclusiveFallsBackWhenAllFree(t *testing.T) {
	p := mkPool(2)
	p.SetExclusiveModels(
		func(_, model string) bool { return model == "pro-model" },
		func(string) (bool, bool) { return false, true },
	)
	if a := p.PickExcludingForModel(nil, "pro-model"); a == nil {
		t.Fatal("全是免费号时也要给一个号，不能报错无号可用")
	}
}

func TestPickExclusiveAllowsUnknownPaid(t *testing.T) {
	p := mkPool(2)
	p.SetExclusiveModels(
		func(_, model string) bool { return model == "pro-model" },
		func(string) (bool, bool) { return false, false },
	)
	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		if a := p.PickExcludingForModel(nil, "pro-model"); a != nil {
			seen[a.UID] = true
		}
	}
	if !seen["u0"] || !seen["u1"] {
		t.Fatalf("未知付费状态应照旧参与选号，实际只用到 %v", seen)
	}
}
