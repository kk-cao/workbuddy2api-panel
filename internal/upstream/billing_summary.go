// billing_summary.go 国际版（global realm）计费口径：summary + paid/free packages。
//
// summary（get-user-resource-summary）提供当前周期权威聚合与 IsPaidUser 付费状态；
// paid/free-packages 仅用于补齐各批次的到期时刻与余量。明细失败时仍保留总额。
package upstream

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

var estimatedExpiryWarnEvery = time.Hour

var estimatedExpiryWarned sync.Map // uid -> time.Time

func estimatedExpiryWarnDue(uid string, now time.Time) bool {
	for {
		v, loaded := estimatedExpiryWarned.LoadOrStore(uid, now)
		if !loaded {
			return true
		}
		if now.Sub(v.(time.Time)) < estimatedExpiryWarnEvery {
			return false
		}
		if estimatedExpiryWarned.CompareAndSwap(uid, v, now) {
			return true
		}
	}
}

const (
	billingSummaryPath = "/billing/meter/get-user-resource-summary"
	billingPaidPath    = "/billing/meter/get-user-resource-paid-packages"
	billingFreePath    = "/billing/meter/get-user-resource-free-packages"
)

// flexFloat 解析「可能是 JSON 数字也可能是字符串」的容量字段；脏值按 0 兜底。
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(raw []byte) error {
	s := strings.TrimSpace(string(raw))
	if s == "null" || s == `""` {
		*f = 0
		return nil
	}
	s = strings.Trim(s, `"`)
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		*f = 0
		return nil
	}
	*f = flexFloat(v)
	return nil
}

type summaryPackage struct {
	PackageCode         string    `json:"PackageCode"`
	CycleTotalCapacity  flexFloat `json:"CycleTotalCapacity"`
	CycleRemainCapacity flexFloat `json:"CycleRemainCapacity"`
	CycleUsedCapacity   flexFloat `json:"CycleUsedCapacity"`
	CapacityUnit        string    `json:"CapacityUnit"`
}

type packageDetail struct {
	PackageCode                  string    `json:"PackageCode"`
	PackageName                  string    `json:"PackageName"`
	DeductionEndTime             int64     `json:"DeductionEndTime"`
	ExpiredTime                  string    `json:"ExpiredTime"`
	CycleEndTime                 string    `json:"CycleEndTime"`
	CapacitySize                 flexFloat `json:"CapacitySize"`
	CapacityRemain               flexFloat `json:"CapacityRemain"`
	CapacityUsed                 flexFloat `json:"CapacityUsed"`
	CycleCapacitySize            flexFloat `json:"CycleCapacitySize"`
	CycleCapacityRemain          flexFloat `json:"CycleCapacityRemain"`
	CycleCapacityUsed            flexFloat `json:"CycleCapacityUsed"`
	CapacitySizePrecise          flexFloat `json:"CapacitySizePrecise"`
	CapacityRemainPrecise        flexFloat `json:"CapacityRemainPrecise"`
	CycleCapacitySizePrecise     flexFloat `json:"CycleCapacitySizePrecise"`
	CycleCapacityRemainPrecise   flexFloat `json:"CycleCapacityRemainPrecise"`
}

type summaryReply struct {
	Packages                []summaryPackage `json:"Packages"`
	SubscriptionPackageName string           `json:"SubscriptionPackageName"`
	IsPaidUser              *bool            `json:"IsPaidUser"`
}

type packagesReply struct {
	Accounts   []packageDetail `json:"Accounts"`
	TotalCount int             `json:"TotalCount"`
}

// packageEndAtEx 返回套餐失效时刻并报告是否只能由 CycleEndTime 推算。
func packageEndAtEx(deductionMS int64, expired, cycleEnd string) (end time.Time, fromCycle, ok bool) {
	if deductionMS > 0 {
		return time.UnixMilli(deductionMS).In(softRateResetLoc), false, true
	}
	if t, ok := parsePackageEndTime(expired); ok {
		return t, false, true
	}
	t, ok := parsePackageEndTime(cycleEnd)
	return t, ok, ok
}

// globalUserResource 国际版个人额度查询：summary 给总额与付费身份，paid/free 明细补到期批次。
func (c *Client) globalUserResource(a *auth.Auth, soon time.Duration) (remain, total, expiring int64, earliestAt time.Time, earliestRemaining int64, buckets []ExpiryBucket, err error) {
	raw, err := c.billingJSON(a, "POST", billingSummaryPath, map[string]any{})
	if err != nil {
		return 0, 0, 0, time.Time{}, 0, nil, err
	}
	var sum summaryReply
	if uerr := json.Unmarshal(raw, &sum); uerr != nil {
		return 0, 0, 0, time.Time{}, 0, nil, fmt.Errorf("billing summary parse: %w", uerr)
	}
	if a != nil && sum.IsPaidUser != nil {
		c.NotePaidUser(a.UID, *sum.IsPaidUser)
	}
	now := time.Now()
	remainByCode := make(map[string]int64, len(sum.Packages))
	for _, p := range sum.Packages {
		r := int64(p.CycleRemainCapacity)
		s := int64(p.CycleTotalCapacity)
		if r < 0 {
			r = 0
		}
		if r > s && s > 0 {
			r = s
		}
		remain += r
		total += s
		if p.PackageCode != "" {
			remainByCode[p.PackageCode] = r
		}
	}

	codes := make([]string, 0, len(sum.Packages))
	for _, p := range sum.Packages {
		if p.PackageCode != "" {
			codes = append(codes, p.PackageCode)
		}
	}
	details := c.globalPackageDetails(a, codes)

	type pick struct {
		d   packageDetail
		end time.Time
	}
	type cycleKey struct {
		code string
		end  int64
	}
	merged := make(map[cycleKey]pick, len(details))
	cycleFallback := 0
	for _, d := range details {
		end, fromCycle, ok := packageEndAtEx(d.DeductionEndTime, d.ExpiredTime, d.CycleEndTime)
		if !ok || !end.After(now) {
			continue
		}
		if fromCycle {
			cycleFallback++
		}
		k := cycleKey{d.PackageCode, end.Unix()}
		if cur, seen := merged[k]; seen {
			if d.balanceRemain() > cur.d.balanceRemain() {
				merged[k] = pick{d: d, end: end}
			}
			continue
		}
		merged[k] = pick{d: d, end: end}
	}
	if cycleFallback > 0 && estimatedExpiryWarnDue(a.UID, now) {
		log.Printf("WARN: [billing] %d 个积分类的到期日只能按计量周期（CycleEndTime）推算，非真实扣费截止（acct=%s）",
			cycleFallback, logfmt.Label(a.UID, a.Nickname))
	}

	futurePerCode := make(map[string]int, len(merged))
	for k := range merged {
		futurePerCode[k.code]++
	}
	byDate := make(map[string]*ExpiryBucket)
	for k, p := range merged {
		r := int64(p.d.balanceRemain())
		if r <= 0 && futurePerCode[k.code] == 1 {
			if sr, ok := remainByCode[k.code]; ok {
				r = sr
			}
		}
		if r <= 0 {
			continue
		}
		end := p.end
		if earliestAt.IsZero() || end.Before(earliestAt) {
			earliestAt = end
		}
		day := end.In(softRateResetLoc).Format("2006-01-02")
		b, seen := byDate[day]
		if !seen {
			b = &ExpiryBucket{Date: day, ExpireAt: end}
			byDate[day] = b
		} else if end.Before(b.ExpireAt) {
			b.ExpireAt = end
		}
		b.Remain += r
		if soon > 0 && !end.After(now.Add(soon)) {
			expiring += r
		}
	}
	dates := make([]string, 0, len(byDate))
	for d := range byDate {
		dates = append(dates, d)
	}
	sort.Strings(dates)
	if len(dates) > 0 {
		buckets = make([]ExpiryBucket, 0, len(dates))
		for _, d := range dates {
			buckets = append(buckets, *byDate[d])
		}
		earliestRemaining = buckets[0].Remain
	}
	c.notifyCredits(a, remain, nil)
	return remain, total, expiring, earliestAt, earliestRemaining, buckets, nil
}

func (c *Client) globalPackageDetails(a *auth.Auth, codes []string) []packageDetail {
	day := time.Now().In(softRateResetLoc).Format("2006-01-02")
	paidBody := map[string]any{
		"PageNumber":    1,
		"PageSize":      200,
		"PackageCodes":  codes,
		"Status":        []int{0, 3},
		"NeedRenewInfo": true,
	}
	freeBody := map[string]any{
		"PageNumber":           1,
		"PageSize":             200,
		"PackageCodes":         codes,
		"Status":               []int{0, 3},
		"SlicePeriodStartTime": day + " 00:00:00",
		"SlicePeriodEndTime":   day + " 23:59:59",
	}
	var out []packageDetail
	for _, req := range []struct {
		path string
		body map[string]any
	}{
		{billingPaidPath, paidBody},
		{billingFreePath, freeBody},
	} {
		raw, err := c.billingJSON(a, "POST", req.path, req.body)
		if err != nil {
			continue
		}
		var rep packagesReply
		if json.Unmarshal(raw, &rep) != nil {
			continue
		}
		out = append(out, rep.Accounts...)
	}
	return out
}

func (d packageDetail) balanceRemain() float64 {
	prefer := func(precise, plain, preciseCycle, plainCycle flexFloat) float64 {
		if preciseCycle > 0 || plainCycle > 0 {
			if preciseCycle > 0 {
				return float64(preciseCycle)
			}
			return float64(plainCycle)
		}
		if precise > 0 {
			return float64(precise)
		}
		return float64(plain)
	}
	return prefer(d.CapacityRemainPrecise, d.CapacityRemain,
		d.CycleCapacityRemainPrecise, d.CycleCapacityRemain)
}
