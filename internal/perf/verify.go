// @author: kevinkelly
// @license: Apache Licence
// @file: verify.go
// @time: 2026/10/28

package perf

import (
	"fmt"
	"log/slog"
	"time"

	"dbtool/internal/settings"
)

// 修复效果自动验证 & 长期未复现自动关闭（每天一次）。
//
// 研发“标记修复”后观察 N 天：不再出现，或平均耗时/日均耗时下降超过阈值 → 自动关闭（已验证）；
// 否则退回“处理中”并通知负责人——防止“点一下修复就完事”。

// VerifyResult 是一次每日维护的统计。
type VerifyResult struct {
	Pass         int `json:"pass"`
	Fail         int `json:"fail"`
	AbsentClosed int `json:"absent_closed"`
}

// RunVerify 验证所有“待验证”的工单，并关闭长期未复现的问题。
func (s *Service) RunVerify() (VerifyResult, error) {
	today := dateOf(s.now())
	var res VerifyResult
	fixed, err := s.repo.IssuesByStatus([]string{StFixed})
	if err != nil {
		return res, err
	}
	for idx := range fixed {
		r, err := s.verifyIssue(&fixed[idx], today)
		if err != nil {
			slog.Error("验证性能工单失败", "issue", fixed[idx].ID, "err", err)
			continue
		}
		switch r {
		case "pass":
			res.Pass++
		case "fail":
			res.Fail++
		}
	}
	if res.AbsentClosed, err = s.autoCloseAbsent(today); err != nil {
		return res, err
	}
	return res, nil
}

func (s *Service) verifyIssue(i *Issue, today time.Time) (string, error) {
	days := s.cfgInt(settings.KeyPerfVerifyDays, 3)
	ratio := s.cfgFloat(settings.KeyPerfVerifyImproveRatio, 0.5)
	if i.FixedAt == nil || int(today.Sub(dateOf(*i.FixedAt)).Hours()/24+0.5) < days {
		return "", nil
	}
	start := dateOf(*i.FixedAt).AddDate(0, 0, 1) // 修复当天数据混杂，不计
	agg, err := s.repo.SumStats(i.ID, start, today)
	if err != nil {
		return "", err
	}
	nDays := int(today.Sub(start).Hours()/24 + 0.5)
	if nDays < 1 {
		nDays = 1
	}
	prev := i.UpdateTime
	now := s.now()
	if agg.ExecCount == 0 {
		closeIssue(i, CloseVerified, now)
		return s.finishVerify(i, prev, "pass", fmt.Sprintf("[%s] 修复后 %d 天未再出现，自动验证通过", closeLabel[CloseVerified], nDays),
			event{name: evClosed, reason: fmt.Sprintf("修复后 %d 天未再出现，自动验证通过", nDays)})
	}
	avgAfter := agg.TotalTime * 1000 / float64(agg.ExecCount)
	dailyAfter := agg.TotalTime / float64(nDays)
	avgOK := i.BaselineAvgMs > 0 && avgAfter <= i.BaselineAvgMs*(1-ratio)
	dailyOK := i.BaselineDailySec > 0 && dailyAfter <= i.BaselineDailySec*(1-ratio)
	detail := fmt.Sprintf("平均耗时 %.0fms -> %.0fms，日均耗时 %.0fs -> %.0fs", i.BaselineAvgMs, avgAfter, i.BaselineDailySec, dailyAfter)
	if avgOK || dailyOK {
		closeIssue(i, CloseVerified, now)
		return s.finishVerify(i, prev, "pass", fmt.Sprintf("[%s] 自动验证通过：%s", closeLabel[CloseVerified], detail),
			event{name: evClosed, reason: "自动验证通过：" + detail})
	}
	i.Status = StAccepted
	i.ReopenCount++
	reason := fmt.Sprintf("修复后观察 %d 天效果不明显：%s（要求下降≥%.0f%%）", nDays, detail, ratio*100)
	return s.finishVerify(i, prev, "fail", reason, event{name: evVerifyFail, reason: reason})
}

// finishVerify 以乐观锁保存验证结果；和人工操作撞上时放弃（下一天再验证）。
func (s *Service) finishVerify(i *Issue, prev time.Time, result, logText string, ev event) (string, error) {
	if err := s.repo.UpdateIssue(i, prev); err != nil {
		if err == ErrConflict {
			return "", nil
		}
		return "", err
	}
	act := ActClose
	if result == "fail" {
		act = ActVerifyFail
	}
	s.record(i, act, logText, systemWho)
	s.notifyIssue(i.ID, ev, 0)
	return result, nil
}

// autoCloseAbsent 未关闭（还没交给研发验证）的自动采集问题，连续 N 天没有再出现就关闭；再次出现会自动重开。
func (s *Service) autoCloseAbsent(today time.Time) (int, error) {
	n := s.cfgInt(settings.KeyPerfAutoCloseAbsentDays, 14)
	if n <= 0 {
		return 0, nil
	}
	cutoff := today.AddDate(0, 0, -n)
	list, err := s.repo.AbsentIssues([]string{StNew, StAssigned, StAccepted}, "collector", cutoff)
	if err != nil {
		return 0, err
	}
	count := 0
	for idx := range list {
		i := &list[idx]
		prev := i.UpdateTime
		closeIssue(i, CloseDisappeared, s.now())
		if err := s.repo.UpdateIssue(i, prev); err != nil {
			if err == ErrConflict {
				continue
			}
			return count, err
		}
		s.record(i, ActClose, fmt.Sprintf("[%s] 连续 %d 天未出现，自动关闭（再次出现会自动重开）", closeLabel[CloseDisappeared], n), systemWho)
		count++
	}
	return count, nil
}
