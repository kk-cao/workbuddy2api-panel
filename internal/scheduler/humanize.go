// humanize.go 周期任务人味化抖动：让余额刷新与任务补领清扫按稳定随机种子错开固定整点节拍。
package scheduler

import (
	"hash/fnv"
	"sync/atomic"
	"time"
)

var jitterSeed atomic.Int64

func init() { jitterSeed.Store(time.Now().UnixNano()) }

// setJitterSeed 固定抖动种子（测试专用；0 = 恢复当前时刻）。
func setJitterSeed(n int64) {
	if n == 0 {
		n = time.Now().UnixNano()
	}
	jitterSeed.Store(n)
}

func jitterFrac(key string) float64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	_, _ = h.Write([]byte{0})
	var b [8]byte
	n := jitterSeed.Load()
	b[0] = byte(n)
	b[1] = byte(n >> 8)
	b[2] = byte(n >> 16)
	b[3] = byte(n >> 24)
	b[4] = byte(n >> 32)
	b[5] = byte(n >> 40)
	b[6] = byte(n >> 48)
	b[7] = byte(n >> 56)
	_, _ = h.Write(b[:])
	return float64(h.Sum64()>>11) / float64(1<<53)
}

// periodicJitter 返回周期 interval 的 ±frac 稳定抖动偏移。
func periodicJitter(interval time.Duration, frac float64, key string) time.Duration {
	if frac <= 0 || interval <= 0 {
		return 0
	}
	return time.Duration((jitterFrac(key) - 0.5) * 2 * float64(interval) * frac)
}

func spreadIntervalStagger(interval time.Duration, frac float64) time.Duration {
	if frac <= 0 || interval <= 0 {
		return 0
	}
	return time.Duration(float64(interval) * frac)
}

func spreadStaggerDelay(window time.Duration, uid string) time.Duration {
	if window <= 0 {
		return 0
	}
	return time.Duration(jitterFrac("bal|"+uid) * float64(window))
}
