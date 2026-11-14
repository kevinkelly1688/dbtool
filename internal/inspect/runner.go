// @author: kevinkelly
// @license: Apache Licence
// @file: runner.go
// @time: 2026/10/28

package inspect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"time"

	"dbtool/internal/model"
)

const (
	defaultTimeout = 10 * time.Minute // 手动巡检 SQL Server 的单实例预算
	mysqlTimeout   = 30 * time.Minute // 手动巡检 MySQL 系的单实例时限：大实例的 information_schema 查询可能很慢
	staleManual    = 45 * time.Minute // 手动巡检创建报告时，超过这个时间仍是 running 的视为进程中断
	graceAfterTime = 2 * time.Minute
)

var (
	errUserCancel = errors.New("用户取消")
	errShutdown   = errors.New("服务退出")
)

// RunOptions 是单个实例一次巡检的选项。
type RunOptions struct {
	Timeout      time.Duration // 单实例预算；SQL Server 超出预算的检查项记为“未执行”
	Databases    []string      // SQL Server 每库检查项的库范围；空表示全部用户库
	RedisBigKeys int           // Redis 大 key 抽样的 key 数；0 表示用服务的默认值（默认关闭）
}

// Outcome 是一次巡检的结果摘要。
type Outcome struct {
	Ref       ReportRef
	Status    string // finish / failed / cancelled
	Alert     int
	Attention int
	Failed    int
	Score     *int
	Err       string
}

// interrupted 判断巡检是否应该停下：本进程被取消 / 超时，或报告已被别处（取消接口）改成了非 running。
func (s *Service) interrupted(ctx context.Context, ref ReportRef) bool {
	if ctx.Err() != nil {
		return true
	}
	st, err := s.repo.ReportStatus(ref.Kind, ref.ID)
	return err == nil && st != StatusRunning
}

// stopMessage 返回因超时 / 退出而中止时写进报告的原因；用户取消返回空串。
func stopMessage(ctx context.Context) string {
	switch cause := context.Cause(ctx); {
	case errors.Is(cause, errUserCancel):
		return ""
	case errors.Is(cause, errShutdown):
		return "服务退出，巡检被中断"
	case errors.Is(cause, context.DeadlineExceeded):
		return "巡检超时"
	}
	return "巡检被中止"
}

// execute 执行一份已创建（status=running）的报告。结果写进报告，不向调用方返回错误。
func (s *Service) execute(inst model.Instance, ref ReportRef, opt RunOptions) (out Outcome) {
	out = Outcome{Ref: ref, Status: StatusFailed}
	if opt.Timeout <= 0 {
		opt.Timeout = defaultTimeout
		if ref.Kind == KindMySQL {
			opt.Timeout = mysqlTimeout
		}
	}
	ctx, cancel := context.WithCancelCause(s.base)
	key := ref.String()
	s.mu.Lock()
	s.cancels[key] = cancel
	s.mu.Unlock()
	defer func() {
		cancel(nil)
		s.mu.Lock()
		delete(s.cancels, key)
		s.mu.Unlock()
	}()
	tctx, tcancel := context.WithTimeout(ctx, opt.Timeout+graceAfterTime)
	defer tcancel()

	fail := func(msg string) {
		out.Status, out.Err = StatusFailed, msg
		f := Finish{Status: StatusFailed, Error: msg, At: s.now()}
		if applied, err := s.repo.FinishReport(ref.Kind, ref.ID, f); err != nil {
			slog.Error("巡检报告收尾失败", "report", key, "err", err)
		} else if !applied {
			out.Status, out.Err = StatusCancelled, ""
		}
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("巡检协程异常退出", "report", key, "panic", r, "stack", string(debug.Stack()))
			fail(fmt.Sprintf("巡检异常：%v", r))
		}
	}()

	conn := s.repo.Conn(&inst)
	var target Target
	var rt RedisTarget
	var err error
	if ref.Kind == KindRedis {
		rt, err = s.openRedis(tctx, conn)
	} else {
		target, err = s.open(tctx, inst.DBType, conn)
	}
	if err != nil {
		if tctx.Err() != nil {
			if m := stopMessage(tctx); m != "" {
				fail(m)
			} else {
				out.Status = StatusCancelled
			}
			return out
		}
		fail(truncate(err.Error(), 500))
		return out
	}
	if ref.Kind == KindRedis {
		defer rt.Close()
	} else {
		defer target.Close()
	}

	var fin Finish
	switch ref.Kind {
	case KindRedis:
		fin, err = s.runRedis(tctx, rt, inst, ref, opt, conn)
	case KindMSSQL:
		fin, err = s.runMSSQL(tctx, target, inst, ref, opt)
	default:
		fin, err = s.runMySQL(tctx, target, inst, ref)
	}
	switch {
	case errors.Is(err, errUserCancel):
		// 取消接口已经把报告置为 cancelled，这里只确认状态
		if applied, _ := s.repo.FinishReport(ref.Kind, ref.ID, Finish{Status: StatusCancelled, Error: "巡检已取消", At: s.now()}); applied {
			slog.Info("巡检已取消", "report", key)
		}
		out.Status = StatusCancelled
		return out
	case err != nil:
		fail(truncate(err.Error(), 500))
		return out
	}
	applied, ferr := s.repo.FinishReport(ref.Kind, ref.ID, fin)
	switch {
	case ferr != nil:
		slog.Error("保存巡检结果失败", "report", key, "err", ferr)
		fail(fmt.Sprintf("巡检已完成但保存结果失败：%s", truncate(ferr.Error(), 300)))
	case !applied:
		out.Status = StatusCancelled
	default:
		out.Status, out.Alert, out.Attention, out.Failed = StatusFinish, fin.Alert, fin.Attention, fin.Failed
		if ref.Kind == KindMySQL || ref.Kind == KindRedis {
			score := fin.HealthScore
			out.Score = &score
		}
	}
	return out
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// ---- MySQL 系 ----

func (s *Service) runMySQL(ctx context.Context, t Target, inst model.Instance, ref ReportRef) (Finish, error) {
	col := &mysqlCollector{ctx: ctx, t: t, instID: inst.ID, slow: s.repo.SlowTop, now: s.now}
	result := jmap{"inspection_time": s.now().Format("2006-01-02T15:04:05"), "instance_id": inst.ID, "engine": "mysql"}
	total := len(mysqlSteps)
	if err := s.repo.UpdateProgress(ref.Kind, ref.ID, 0, total); err != nil {
		return Finish{}, err
	}
	for i, st := range mysqlSteps {
		if s.interrupted(ctx, ref) {
			return Finish{}, s.stopError(ctx)
		}
		result[st.Key] = st.Run(col)
		if err := s.repo.UpdateProgress(ref.Kind, ref.ID, i+1, total); err != nil {
			slog.Warn("更新巡检进度失败", "report", ref.String(), "err", err)
		}
	}
	if s.interrupted(ctx, ref) {
		return Finish{}, s.stopError(ctx)
	}
	// 连基本信息都采不到：多半是连接中途断了，整份报告没有意义，记为失败
	if e1, e2 := sub(result, "server"), sub(result, "connection"); has(e1, "error") && has(e2, "error") {
		return Finish{}, fmt.Errorf("无法采集实例信息：%s", sv(e1, "error"))
	}
	// 先走一遍 JSON，统一成 JSON 数据类型（评分和分区都按它计算，保存的也是它）
	norm, err := normalizeJSON(result)
	if err != nil {
		return Finish{}, fmt.Errorf("巡检结果无法序列化：%v", err)
	}
	health := MySQLHealth(norm)
	norm["inspection_report"] = health
	norm, err = normalizeJSON(norm)
	if err != nil {
		return Finish{}, fmt.Errorf("巡检结果无法序列化：%v", err)
	}
	secs := BuildMySQLSections(norm)
	alert, attention, failed := countSections(secs)
	raw, _ := json.Marshal(norm)
	return Finish{Status: StatusFinish, HealthScore: int(i64(health, "health_score")), Alert: alert, Attention: attention, Failed: failed,
		Total: total, Done: total, Result: string(raw), At: s.now()}, nil
}

func (s *Service) stopError(ctx context.Context) error {
	if ctx.Err() == nil {
		return errUserCancel // 报告在别处被取消
	}
	if m := stopMessage(ctx); m != "" {
		return errors.New(m)
	}
	return errUserCancel
}

func normalizeJSON(m jmap) (jmap, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out jmap
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func countSections(secs []Section) (alert, attention, failed int) {
	for _, s := range secs {
		alert += s.Alert
		attention += s.Attention
		failed += s.Failed
	}
	return
}

// ---- SQL Server ----

type msPlanItem struct {
	check *msCheck
	db    string
}

func (s *Service) runMSSQL(ctx context.Context, t Target, inst model.Instance, ref ReportRef, opt RunOptions) (Finish, error) {
	started := s.now()
	c := &msContext{ctx: ctx, t: t, now: s.now}
	if err := c.loadServerInfo(); err != nil {
		return Finish{}, fmt.Errorf("读取实例信息失败：%s", truncate(err.Error(), 300))
	}
	var prevReport *PrevRaw
	if p, err := s.repo.PrevMSSQLRaw(inst.ID, ref.ID); err != nil {
		slog.Warn("读取上一份 SQL Server 巡检失败，累计型指标按启动以来统计", "report", ref.String(), "err", err)
	} else if p != nil && p.ServerStart != nil && !c.start.IsZero() && abs(p.ServerStart.Sub(c.start)) < 5*time.Second {
		// 只有实例没重启，累计计数器才能做差
		prevReport = p
		c.prevRaw = p.Raw
	}
	if err := c.loadDatabases(opt.Databases); err != nil {
		return Finish{}, fmt.Errorf("读取库清单失败：%s", truncate(err.Error(), 300))
	}
	var skipped []string
	dbs := c.databases
	if len(dbs) > msMaxDatabases {
		skipped = append(skipped, fmt.Sprintf("库数量 %d 超过 %d，每库检查只跑前 %d 个", len(dbs), msMaxDatabases, msMaxDatabases))
		dbs = dbs[:msMaxDatabases]
	}
	if len(opt.Databases) > 0 && len(dbs) == 0 {
		skipped = append(skipped, "指定的库都不存在或不在线，每库检查项没有执行")
	}
	var plan []msPlanItem
	for i := range msChecks {
		ck := &msChecks[i]
		if c.version > 0 && c.version < max(ck.MinVersion, 11) {
			skipped = append(skipped, fmt.Sprintf("%s：需 SQL Server 主版本 ≥%d", ck.Name, ck.MinVersion))
			continue
		}
		if ck.Scope == scopeDatabase {
			for _, d := range dbs {
				plan = append(plan, msPlanItem{ck, d})
			}
		} else {
			plan = append(plan, msPlanItem{ck, ""})
		}
	}
	total := len(plan)
	if err := s.repo.UpdateProgress(ref.Kind, ref.ID, 0, total); err != nil {
		return Finish{}, err
	}
	budget := opt.Timeout
	if budget <= 0 {
		budget = msTotalBudget
	}

	var items []MSSQLItem
	stopped := error(nil)
	for _, p := range plan {
		if s.interrupted(ctx, ref) {
			stopped = s.stopError(ctx)
			break
		}
		t0 := s.now()
		it := MSSQLItem{Section: p.check.Section, Key: p.check.Key, Name: p.check.Name, DB: p.db}
		if t0.Sub(started) > budget {
			it.Level, it.Value = LevelFailed, fmt.Sprintf("超出 %d 分钟时间预算未执行", int(budget.Minutes()))
		} else {
			c.db = p.db
			res, err := runCheck(ctx, c, p.check)
			switch {
			case err != nil && isPermissionError(err):
				it.Level, it.Value = LevelFailed, fmt.Sprintf("无权限：%s", truncate(err.Error(), 300))
			case err != nil:
				it.Level, it.Value = LevelFailed, fmt.Sprintf("采集失败：%s", truncate(err.Error(), 300))
				slog.Warn("SQL Server 巡检项失败", "report", ref.String(), "item", p.check.Key, "db", p.db, "err", err)
			default:
				it.Level, it.Value, it.Threshold = res.Level, clip(res.Value, 500), clip(res.Threshold, 200)
				if res.Detail != nil {
					if b, e := json.Marshal(res.Detail); e == nil {
						it.Detail = string(b)
					}
				}
				if res.Raw != nil {
					if b, e := json.Marshal(res.Raw); e == nil {
						it.RawCounter = string(b)
					}
				}
			}
		}
		it.ElapsedMs = int(s.now().Sub(t0) / time.Millisecond)
		items = append(items, it)
		if err := s.repo.UpdateProgress(ref.Kind, ref.ID, len(items), total); err != nil {
			slog.Warn("更新巡检进度失败", "report", ref.String(), "err", err)
		}
	}
	// 已采集的部分无论是否被中止都保留（取消的报告在列表里显示“已取消 N / 总数 项”）
	if len(items) > 0 {
		if err := s.repo.InsertMSSQLItems(ref.ID, items); err != nil {
			return Finish{}, fmt.Errorf("保存检查项失败：%s", truncate(err.Error(), 300))
		}
	}
	if stopped != nil {
		return Finish{}, stopped
	}
	counts := map[int]int{}
	secCounts := map[string]map[string]int{}
	for _, sec := range msSections {
		secCounts[sec[0]] = map[string]int{"alert": 0, "attention": 0, "normal": 0, "failed": 0}
	}
	nameOf := map[string]string{}
	for _, sec := range msSections {
		nameOf[sec[0]] = sec[1]
	}
	for _, it := range items {
		counts[it.Level]++
		m := secCounts[it.Section]
		if m == nil {
			m = map[string]int{"alert": 0, "attention": 0, "normal": 0, "failed": 0}
			secCounts[it.Section] = m
		}
		m[[]string{"normal", "attention", "alert", "failed"}[it.Level]]++
	}
	rank := func(section string) int {
		for i, c := range msSectionPriority {
			if c == section {
				return i
			}
		}
		return len(msSectionPriority)
	}
	var alerts []MSSQLItem
	for _, it := range items {
		if it.Level == LevelAlert {
			alerts = append(alerts, it)
		}
	}
	sort.SliceStable(alerts, func(i, j int) bool {
		pi, pj := rank(alerts[i].Section), rank(alerts[j].Section)
		if pi != pj {
			return pi < pj
		}
		return alerts[i].Key < alerts[j].Key
	})
	top := []any{}
	for i, it := range alerts {
		if i >= 3 {
			break
		}
		sn := nameOf[it.Section]
		if sn == "" {
			sn = it.Section
		}
		top = append(top, map[string]any{"section": sn, "name": it.Name, "db_name": it.DB, "value": it.Value})
	}
	summary := map[string]any{"top": top, "sections": secCounts, "server": c.server, "compare_with": nil, "compare_time": "", "skipped": append([]string{}, skipped...)}
	if prevReport != nil {
		summary["compare_with"] = prevReport.ReportID
		summary["compare_time"] = prevReport.StartTime.Format("2006-01-02 15:04")
	}
	raw, err := json.Marshal(summary)
	if err != nil {
		return Finish{}, fmt.Errorf("巡检摘要无法序列化：%v", err)
	}
	st := c.start
	return Finish{Status: StatusFinish, Alert: counts[LevelAlert], Attention: counts[LevelAttention], Normal: counts[LevelNormal], Failed: counts[LevelFailed],
		Total: total, Done: len(items), Summary: string(raw), ServerStart: &st, At: s.now()}, nil
}

// runCheck 执行一项检查；panic 转成该项失败，不影响其他项。
func runCheck(ctx context.Context, c *msContext, ck *msCheck) (res msResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("检查项异常：%v", r)
		}
	}()
	if err := ctx.Err(); err != nil {
		return msResult{}, err
	}
	return ck.Run(c)
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
