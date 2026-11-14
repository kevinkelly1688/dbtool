// @author: kevinkelly
// @license: Apache Licence
// @file: throttle.go
// @time: 2026/10/28

package engine

import (
	"fmt"
	"time"
)

// adaptor 实现批耗时自适应：某批耗时超过目标则批量减半、休眠加倍；连续 okStreakToGrow 批正常后逐步恢复（不超过设定值）。
type adaptor struct {
	baseBatch, baseSleep int
	batch, sleep         int
	okStreak             int
	target               time.Duration
}

const minAdaptBatch = 100

func newAdaptor(batch, sleepMs int, target time.Duration) *adaptor {
	return &adaptor{baseBatch: batch, baseSleep: sleepMs, batch: batch, sleep: sleepMs, target: target}
}

// observe 登记一批的耗时，返回是否调整了参数以及一句说明。slow=true 表示这是一次“因慢而限流”。
func (a *adaptor) observe(d time.Duration) (changed, slow bool, msg string) {
	if d > a.target {
		a.okStreak = 0
		nb := a.batch / 2
		floor := minAdaptBatch
		if a.baseBatch < floor {
			floor = a.baseBatch
		}
		if nb < floor {
			nb = floor
		}
		ns := a.sleep
		if ns <= 0 {
			ns = minDeleteSleepMs
		}
		ns *= 2
		if ns > maxSleepMs {
			ns = maxSleepMs
		}
		if nb == a.batch && ns == a.sleep {
			return false, false, ""
		}
		msg = fmt.Sprintf("批耗时 %d ms > 目标 %d ms：批大小 %d → %d，休眠 %d → %d ms", d.Milliseconds(), a.target.Milliseconds(), a.batch, nb, a.sleep, ns)
		a.batch, a.sleep = nb, ns
		return true, true, msg
	}
	if a.batch >= a.baseBatch && a.sleep <= a.baseSleep {
		a.okStreak = 0
		return false, false, ""
	}
	a.okStreak++
	if a.okStreak < okStreakToGrow {
		return false, false, ""
	}
	a.okStreak = 0
	nb := (a.batch*3 + 1) / 2
	if nb > a.baseBatch {
		nb = a.baseBatch
	}
	ns := a.sleep / 2
	if ns < a.baseSleep {
		ns = a.baseSleep
	}
	msg = fmt.Sprintf("连续 %d 批正常，逐步恢复：批大小 %d → %d，休眠 %d → %d ms", okStreakToGrow, a.batch, nb, a.sleep, ns)
	a.batch, a.sleep = nb, ns
	return true, false, msg
}
