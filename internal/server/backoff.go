// backoff.go 轮转退避与抖动的单一事实来源（WAF 403 修复 P0-2/P0-1 共用）：
// 指数基数/封顶/抖动比例一处定义，chatCompletions 轮转退避与 WAF 软冷却基数
// （handler.wafCooldownBase）共用同一 jitterDur，不在 handler 与 upstream 各写一份。
package server

import (
	"context"
	"math/rand/v2"
	"time"
)

// rotateBackoffBase 轮转退避基数（对齐官方 intl CLI computeRequestRetryDelayMs
// 的 500ms 形态，WAF 403 并发研究报告 §2.1/§6 P0-2）。测试可置 0 跳过等待
// （TestMain 已置 0 加速轮转测试；退避界断言测试临时恢复）。
var rotateBackoffBase = 500 * time.Millisecond

const (
	// rotateBackoffCap 轮转退避封顶（报告 P0-2 建议 8s：轮转上限默认 3 次，
	// 实际等待序列 500ms/1s，封顶只约束极端配置下的 MaxRotate）。
	rotateBackoffCap = 8 * time.Second
	// egressBackoffCap 出口级传输失败退避封顶（秒级，防 2^n 溢出）。
	egressBackoffCap = 15 * time.Second
	// egressGiveUpStreak 同一出口连续传输失败达此阈值时放弃继续换号重试。
	egressGiveUpStreak = 5
	// jitterFraction 抖动比例（±25%，对齐 intl CLI delay×(1±0.25) 形态）。
	jitterFraction = 0.25
)

var (
	// egressBackoffBase 出口级退避基数（TestMain 中置 0，单测退避阶梯时显式传入 time.Second）。
	egressBackoffBase = time.Second
	// busyWaitMax 6020 排队等待上限（单测可临时覆盖以加速）。
	busyWaitMax = 60 * time.Second
)

// egressBackoff 计算出口级第 streak 次（1 基）传输失败的退避时长：base·2^(streak-1)
// 封顶 egressBackoffCap 并施加 ±25% 抖动；streak<=0 或 base<=0 返回 0。
func egressBackoff(streak int, base time.Duration) time.Duration {
	if streak <= 0 || base <= 0 {
		return 0
	}
	d := base
	for k := 1; k < streak && d < egressBackoffCap; k++ {
		if d > egressBackoffCap/2 {
			d = egressBackoffCap
			break
		}
		d *= 2
	}
	if d > egressBackoffCap {
		d = egressBackoffCap
	}
	return jitterDur(d)
}

// jitterDur 给时长施加 ±jitterFraction 的均匀抖动，返回 [d·(1-f), d·(1+f)] 区间值。
// d<=0 原样返回（零等待不抖动）。抖动目的是打散多请求同相位重试（WAF 频控按
// 密度判罚，齐步走的退避会以固定周期再次聚团）。
func jitterDur(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	f := 1 + (rand.Float64()*2-1)*jitterFraction
	out := time.Duration(float64(d) * f)
	if out < 0 {
		return 0
	}
	return out
}

// backoffAfter 返回第 n 次轮转（0 基：首次失败换号前 n=0）前应等待的退避时长：
// base·2^n 封顶 rotateBackoffCap，再施加 ±25% 抖动。base 置 0（测试）时恒 0。
// 用逐次翻倍而非位移：base 调整后无需同步维护移位上限，溢出由封顶比较兜底。
func backoffAfter(n int) time.Duration {
	d := rotateBackoffBase
	if d <= 0 {
		return 0
	}
	for k := 0; k < n && d < rotateBackoffCap; k++ {
		d *= 2
		if d <= 0 { // 翻倍溢出成非正数：直接按封顶处理
			return jitterDur(rotateBackoffCap)
		}
	}
	if d > rotateBackoffCap {
		d = rotateBackoffCap
	}
	return jitterDur(d)
}

// sleepCtx 可取消的等待：ctx 取消立即返回 false（客户端断连/优雅停机不必等退避
// 睡醒），等满返回 true。d<=0 立即放行。与 scheduler.sleepCtx 同模式（该函数未
// 导出且 scheduler 不宜被 server 反向依赖，按任务书「等价物」口径在消费侧实现）。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
