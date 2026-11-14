// @author: kevinkelly
// @license: Apache Licence
// @file: ingest.go
// @time: 2026/10/28

package perf

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// ---- 采集结果 ----

// Digest 是采集器统一输出：一个时间窗口内、一个 (库, 指纹) 的聚合指标。
type Digest struct {
	DBName       string
	SampleSQL    string
	ExecCount    int64
	TotalSec     float64
	MaxMs        float64
	RowsExamined float64 // 合计
	RowsSent     float64 // 合计
	FirstSeen    *time.Time
	LastSeen     *time.Time
	Fingerprint  string
	Checksum     string
	SampleReal   bool // false 表示样例是参数化文本，无法 EXPLAIN
}

// Normalize 补全指纹与校验和，库名去掉 doris 老版本的 "default_cluster:" 前缀。
func (d *Digest) Normalize() {
	if i := strings.LastIndex(d.DBName, ":"); i >= 0 {
		d.DBName = d.DBName[i+1:]
	}
	d.DBName = clipRunes(d.DBName, 128) // dbt_perf_issue.db_name 是 varchar(128)
	if d.Fingerprint == "" {
		d.Fingerprint = Fingerprint(d.SampleSQL)
	}
	if d.Checksum == "" {
		d.Checksum = Checksum(d.Fingerprint)
	}
}

// AvgMs 平均耗时。
func (d Digest) AvgMs() float64 {
	if d.ExecCount == 0 {
		return 0
	}
	return d.TotalSec * 1000 / float64(d.ExecCount)
}

// passes 是否达到建单阈值：未命中排除规则/排除库，且平均耗时或窗口总耗时达标。
func passes(cfg *CollectConfig, d *Digest) bool {
	if cfg.ExcludeRegex != "" {
		if re, err := regexp.Compile(cfg.ExcludeRegex); err == nil && re.MatchString(d.SampleSQL) {
			return false
		}
	}
	for _, x := range strings.Split(cfg.ExcludeDBs, ",") {
		if x = strings.TrimSpace(x); x != "" && strings.EqualFold(x, d.DBName) {
			return false
		}
	}
	return d.AvgMs() >= float64(cfg.MinAvgMs) || d.TotalSec >= float64(cfg.MinTotalSec)
}

// ---- 路由 ----

type routed struct {
	teamID     uint
	assigneeID uint
	dbaID      uint
}

// pickDBA DBA 轮值：在配置项 perf.dba_users 的 DBA 中选未关闭工单最少的人。
func (s *Service) pickDBA() (uint, error) {
	names := splitComma(s.cfgStr(settings.KeyPerfDBAUsers, ""))
	if len(names) == 0 {
		return 0, nil
	}
	loads, err := s.repo.DBALoads(names)
	if err != nil || len(loads) == 0 {
		return 0, err
	}
	sort.SliceStable(loads, func(a, b int) bool {
		if loads[a].Open != loads[b].Open {
			return loads[a].Open < loads[b].Open
		}
		return loads[a].User.ID < loads[b].User.ID
	})
	return loads[0].User.ID, nil
}

// route 找到“哪个团队、哪个人”负责。优先级：路由规则 > 库负责人(dbt_instance_database.owner) > 采集配置默认团队/实例的资源组
// > 团队默认接口人；处理方为 DBA 的问题改派给 DBA 值班。
func (s *Service) route(instanceID uint, db, side string, cfg *CollectConfig, tables ...string) (routed, error) {
	r, _, err := s.routeTrace(instanceID, db, side, cfg, tables)
	return r, err
}

// routeTrace 与 route 相同，另外返回每一步是怎么定下来的（路由测试用）。
// tables 是语句涉及的表：路由规则设置了表名正则时才会用到；没有设置表名正则的规则只看库名，行为与表名正则功能上线前一致。
func (s *Service) routeTrace(instanceID uint, db, side string, cfg *CollectConfig, tables []string) (routed, routeTrace, error) {
	var r routed
	var tr routeTrace
	routes, err := s.repo.ListRoutes()
	if err != nil {
		return r, tr, err
	}
	tblRe := s.routeTblRegexes()
	for _, rt := range routes {
		if rt.InstanceID != nil && *rt.InstanceID != instanceID {
			continue
		}
		re, err := regexp.Compile("^(?:" + rt.DBRegex + ")$")
		if err != nil {
			slog.Warn("性能工单路由规则正则非法，已跳过", "route", rt.ID, "regex", rt.DBRegex)
			continue
		}
		if re.MatchString(db) && tblMatches(tblRe[rt.ID], tables) {
			r.teamID, r.assigneeID = rt.TeamID, derefUint(rt.AssigneeID)
			tr.Rule = &rt
			tr.TeamSrc = "route"
			if r.assigneeID != 0 {
				tr.AssigneeSrc = "route"
			}
			break
		}
	}
	if r.assigneeID == 0 && db != "" {
		owner, err := s.repo.DatabaseOwner(instanceID, db)
		if err != nil {
			return r, tr, err
		}
		if owner != "" {
			if us, err := s.repo.UsersByNames([]string{owner}); err == nil && len(us) > 0 {
				r.assigneeID = us[0].ID
				tr.AssigneeSrc = "db_owner"
			}
		}
	}
	if r.teamID == 0 && cfg != nil && cfg.TeamID != nil {
		r.teamID = *cfg.TeamID
		tr.TeamSrc = "collect_config"
	}
	if r.teamID == 0 {
		groups, err := s.repo.InstanceGroupIDs(instanceID)
		if err != nil {
			return r, tr, err
		}
		if r.assigneeID != 0 {
			mine, _ := s.repo.UserGroupIDs(r.assigneeID)
			for _, g := range groups {
				for _, m := range mine {
					if g == m && r.teamID == 0 {
						r.teamID = g
					}
				}
			}
		}
		if r.teamID == 0 && len(groups) > 0 {
			r.teamID = groups[0]
		}
		if r.teamID != 0 {
			tr.TeamSrc = "instance_group"
		}
	}
	if r.assigneeID == 0 && r.teamID != 0 {
		if t, err := s.repo.GetTeamByGroup(r.teamID); err == nil && t != nil && t.DefaultAssigneeID != nil {
			r.assigneeID = *t.DefaultAssigneeID
			tr.AssigneeSrc = "team_default"
		}
	}
	dba, err := s.pickDBA()
	if err != nil {
		return r, tr, err
	}
	r.dbaID = dba
	if side == SideDBA && dba != 0 {
		r.assigneeID = dba
		tr.AssigneeSrc = "dba_rotation"
	}
	return r, tr, nil
}

// routeTrace 记录路由每一步的来源（路由测试页展示）。
type routeTrace struct {
	Rule        *Route // 命中的路由规则，nil 表示没有命中
	TeamSrc     string // route / collect_config / instance_group，空表示没有找到团队
	AssigneeSrc string // route / db_owner / team_default / dba_rotation，空表示没有负责人
}

func (s *Service) reroute(i *Issue) (routed, error) {
	inst, _ := s.repo.GetInstance(i.InstanceID)
	return s.route(i.InstanceID, i.DBName, i.OwnerSide, s.collectConfigFor(i.InstanceID, inst), splitComma(i.Tables)...)
}

// group 取资源组；不存在报 400。
func (s *Service) group(id uint) (*GroupBrief, error) {
	gs, err := s.repo.ListGroups()
	if err != nil {
		return nil, err
	}
	for i := range gs {
		if gs[i].ID == id {
			return &gs[i], nil
		}
	}
	return nil, bad("所选团队不存在")
}

// collectConfigFor 返回实例的采集配置；没有配置时按实例类型给出默认值（手工建单、诊断用）。
func (s *Service) collectConfigFor(instanceID uint, inst *model.Instance) *CollectConfig {
	if c, err := s.repo.GetCollectConfigByInstance(instanceID); err == nil && c != nil {
		return c
	}
	dbType := ""
	if inst != nil {
		dbType = inst.DBType
	}
	return defaultCollectConfig(instanceID, dbType)
}

func defaultCollectConfig(instanceID uint, dbType string) *CollectConfig {
	d, ok := dbTypeDefaults[dbType]
	if !ok {
		d = [2]string{ProdOther, CollCustomSQL}
	}
	return &CollectConfig{InstanceID: instanceID, Product: d[0], Collector: d[1], MinAvgMs: 1000, MinTotalSec: 10,
		ExcludeRegex: defaultExcludeRegex, ExcludeDBs: defaultExcludeDBs, Enabled: true}
}

// ---- 指标刷新 ----

// refreshMetrics 用近 7 天的日指标重算影响量化（日均浪费 DB 时间、平均耗时、日均次数…）。
func (s *Service) refreshMetrics(i *Issue) error {
	today := dateOf(s.now())
	since := today.AddDate(0, 0, -6)
	agg, err := s.repo.SumStats(i.ID, since, time.Time{})
	if err != nil {
		return err
	}
	first := today
	if i.FirstSeen != nil {
		first = dateOf(*i.FirstSeen)
	}
	from := first
	if since.After(from) {
		from = since
	}
	days := int(today.Sub(from).Hours()/24+0.5) + 1
	if days > 7 {
		days = 7
	}
	if days < 1 {
		days = 1
	}
	i.ExecCount7d = agg.ExecCount
	i.TotalTime7d = round(agg.TotalTime, 3)
	i.AvgTimeMs, i.RowsExaminedAvg, i.RowsSentAvg = 0, 0, 0
	if agg.ExecCount > 0 {
		i.AvgTimeMs = round(agg.TotalTime*1000/float64(agg.ExecCount), 2)
		i.RowsExaminedAvg = round(agg.RowsExamined/float64(agg.ExecCount), 1)
		i.RowsSentAvg = round(agg.RowsSent/float64(agg.ExecCount), 1)
	}
	i.MaxTimeMs = agg.MaxTimeMs
	i.ImpactScore = round(agg.TotalTime/float64(days), 2)
	i.PerDayCount = round(float64(agg.ExecCount)/float64(days), 1)
	return s.repo.UpdateIssueMetrics(i)
}

// ---- 入库 ----

const batchNotifyThreshold = 5 // 一次新建超过这个数量时，按团队汇总推送，避免首次接入刷屏

// IngestResult 是一次入库的统计。
type IngestResult struct {
	Created int
	Updated int
}

type touched struct {
	id     uint
	isNew  bool
	isReal bool
}

// Ingest 把采集结果入库：新问题建单，已有问题累加当天指标、刷新影响量化，再按需升级级别/回归重开。
//
// 去重键是 (instance_id, db_name, checksum)，库里有唯一索引：并发采集也不会建出重复工单；
// 但重复累加同一窗口的指标会重复计数，所以同一窗口只应被采集一次。
func (s *Service) Ingest(ctx context.Context, cfg *CollectConfig, inst *model.Instance, digests []Digest, windowEnd time.Time) (IngestResult, error) {
	var res IngestResult
	var done []touched
	for idx := range digests {
		d := &digests[idx]
		d.Normalize()
		if d.ExecCount <= 0 || d.Fingerprint == "" || !passes(cfg, d) {
			continue
		}
		day := windowEnd
		if d.LastSeen != nil {
			day = *d.LastSeen
		}
		i, err := s.repo.FindIssue(cfg.InstanceID, d.DBName, d.Checksum)
		if err != nil {
			return res, err
		}
		isNew := i == nil
		if isNew {
			first, last := windowEnd, windowEnd
			if d.FirstSeen != nil {
				first = *d.FirstSeen
			}
			if d.LastSeen != nil {
				last = *d.LastSeen
			}
			now := s.now()
			i = &Issue{InstanceID: cfg.InstanceID, DBName: d.DBName, Checksum: d.Checksum, Fingerprint: clipRunes(d.Fingerprint, 65535),
				SampleSQL: clipRunes(d.SampleSQL, 65535), Tables: clipRunes(strings.Join(ExtractTables(d.SampleSQL, 10), ","), 500),
				Source: "collector", FirstSeen: &first, LastSeen: &last, Category: CatOther, OwnerSide: SideDev, Severity: SevP3, Status: StNew,
				CreateTime: now, UpdateTime: now}
			if err := s.repo.InsertIssue(i); err == ErrDuplicate {
				// 并发刚建了同一个问题：按已有工单累加
				if i, err = s.repo.FindIssue(cfg.InstanceID, d.DBName, d.Checksum); err != nil || i == nil {
					return res, fmt.Errorf("工单已存在但读取失败: %v", err)
				}
				isNew = false
			} else if err != nil {
				return res, err
			}
		}
		if !isNew {
			last := d.LastSeen
			if last == nil {
				last = &windowEnd
			}
			if i.LastSeen == nil || last.After(*i.LastSeen) {
				i.LastSeen = last
			}
			if d.SampleReal && d.MaxMs >= i.MaxTimeMs {
				i.SampleSQL = clipRunes(d.SampleSQL, 65535)
			}
		}
		if err := s.repo.AddStat(i.ID, dateOf(day), StatDelta{ExecCount: d.ExecCount, TotalTime: d.TotalSec, MaxTimeMs: d.MaxMs,
			RowsExamined: d.RowsExamined, RowsSent: d.RowsSent}); err != nil {
			return res, err
		}
		if err := s.refreshMetrics(i); err != nil {
			return res, err
		}
		done = append(done, touched{i.ID, isNew, d.SampleReal})
		if isNew {
			res.Created++
		} else {
			res.Updated++
		}
	}
	if len(done) == 0 {
		return res, nil
	}
	if err := s.repo.RefreshLoadShare(cfg.InstanceID); err != nil {
		slog.Error("刷新占比失败", "instance", cfg.InstanceID, "err", err)
	}
	batch := res.Created > batchNotifyThreshold
	var created []uint
	for _, t := range done {
		i, err := s.repo.GetIssue(t.id)
		if err != nil || i == nil {
			continue
		}
		if t.isNew {
			if err := s.openNewIssue(ctx, i, cfg, inst, t.isReal, systemWho, !batch); err != nil {
				slog.Error("新建工单后处理失败", "issue", t.id, "err", err)
				continue
			}
			created = append(created, t.id)
		} else if err := s.onRecurrence(i); err != nil {
			slog.Error("处理再次出现的问题失败", "issue", t.id, "err", err)
		}
	}
	if batch && len(created) > 0 {
		s.notifyBatchCreated(created)
	}
	return res, nil
}

// openNewIssue 给新问题做诊断、评估级别、路由到负责人、套 SLA，并写“建单”日志、发通知。
func (s *Service) openNewIssue(ctx context.Context, i *Issue, cfg *CollectConfig, inst *model.Instance, sampleReal bool, w who, notifyNow bool) error {
	prev := i.UpdateTime
	diag := Diagnose(ctx, s.explainer, DiagnoseInput{Instance: inst, Product: cfg.Product, MinAvgMs: cfg.MinAvgMs, DB: i.DBName, SQL: i.SampleSQL,
		Metrics: Metrics{i.AvgTimeMs, i.RowsSentAvg, i.RowsExaminedAvg, i.PerDayCount}, SampleReal: sampleReal,
		AutoExplain: s.cfgBool(settings.KeyPerfAutoExplain, true), TimeoutMs: s.cfgInt(settings.KeyPerfExplainTimeoutMs, 3000)})
	i.Category, i.OwnerSide, i.Diagnosis, i.Suggestion, i.SuggestedDDL, i.ExplainResult = diag.Category, diag.OwnerSide, diag.Diagnosis, diag.Suggestion, diag.SuggestedDDL, diag.ExplainResult
	i.Severity = s.evaluateSeverity(i)
	r, err := s.route(i.InstanceID, i.DBName, i.OwnerSide, cfg, splitComma(i.Tables)...)
	if err != nil {
		return err
	}
	i.TeamID, i.AssigneeID, i.DBAID = optUint(r.teamID), optUint(r.assigneeID), optUint(r.dbaID)
	i.Status = StNew
	if r.assigneeID != 0 {
		i.Status = StAssigned
	}
	setDeadlines(i, s.policy(i.Severity), s.now(), false)
	return s.finishCreate(i, prev, w, notifyNow, "自动建单")
}

// finishCreate 保存新工单的诊断/路由结果，写日志与审计，发通知。
func (s *Service) finishCreate(i *Issue, prev time.Time, w who, notifyNow bool, how string) error {
	if err := s.repo.UpdateIssue(i, prev); err != nil {
		return err
	}
	name := "未匹配(待DBA分派)"
	if i.AssigneeID != nil {
		if us, _ := s.repo.GetUsers([]uint{*i.AssigneeID}); len(us) > 0 {
			name = us[*i.AssigneeID].Name()
		}
	}
	s.record(i, ActCreate, fmt.Sprintf("%s：%s / %s / %s，日均浪费DB时间 %gs，平均 %.0fms，负责人 %s",
		how, labelOf(Categories, i.Category), sideLabel[i.OwnerSide], i.Severity, i.ImpactScore, i.AvgTimeMs, name), w)
	if notifyNow {
		s.notifyIssue(i.ID, event{name: evCreated}, 0)
	}
	return nil
}

func optUint(v uint) *uint {
	if v == 0 {
		return nil
	}
	return &v
}

// onRecurrence 已存在的问题再次出现：未关闭的看影响是否扩大（升级级别并收紧时限）；已关闭的看是否回归（自动重开）。
func (s *Service) onRecurrence(i *Issue) error {
	now := s.now()
	if isOpen(i.Status) {
		if i.SeverityLocked {
			return nil
		}
		newSev := s.evaluateSeverity(i)
		if severityRank[newSev] >= severityRank[i.Severity] {
			return nil
		}
		prev, old := i.UpdateTime, i.Severity
		i.Severity = newSev
		if isSLARunning(i.Status) {
			setDeadlines(i, s.policy(newSev), now, true)
		}
		if err := s.repo.UpdateIssue(i, prev); err != nil {
			if err == ErrConflict {
				return nil // 刚被别人改过，下一轮采集再评估
			}
			return err
		}
		s.record(i, ActSeverity, fmt.Sprintf("影响扩大，级别自动由 %s 升为 %s", old, newSev), systemWho)
		s.notifyIssue(i.ID, event{name: evSeverityUp, oldSev: old}, 0)
		return nil
	}
	if i.CloseReason == CloseDuplicate {
		return nil
	}
	if i.CloseReason == CloseWaived && s.evaluateSeverity(i) != SevP0 {
		return nil
	}
	if i.ImpactScore < s.cfgFloat(settings.KeyPerfReopenMinScore, 20) {
		return nil
	}
	prev := i.UpdateTime
	s.reopenIssue(i, now)
	if err := s.repo.UpdateIssue(i, prev); err != nil {
		if err == ErrConflict {
			return nil
		}
		return err
	}
	reason := fmt.Sprintf("关闭后再次出现，日均浪费DB时间 %gs，自动重开", i.ImpactScore)
	s.record(i, ActReopen, reason, systemWho)
	s.notifyIssue(i.ID, event{name: evReopened, reason: reason}, 0)
	return nil
}

// ---- 手工建单 ----

// CreateInput 是手工建单（含慢日志页面“生成性能工单”）的输入。
type CreateInput struct {
	InstanceID uint   `json:"instance_id"`
	DBName     string `json:"db_name"`
	SQL        string `json:"sql"`
	Severity   string `json:"severity"`    // 为空表示自动评估
	AssigneeID uint   `json:"assignee_id"` // 为 0 表示自动路由
	Remark     string `json:"remark"`
}

// ExistsError 表示同一 (实例, 库, 指纹) 已有工单。
type ExistsError struct{ ID uint }

func (e *ExistsError) Error() string { return fmt.Sprintf("该SQL已存在工单 #%d", e.ID) }

// visibleInstance 取用户能建单的实例：DBA 只能对自己有权限的实例建单（超级用户或 query.admin_run 可选全部）。
func (s *Service) visibleInstance(v Viewer, queryAll bool, id uint) (*model.Instance, error) {
	inst, err := s.repo.GetInstance(id)
	if err != nil || inst == nil {
		return nil, bad("实例不存在或无权限")
	}
	if !contains(SupportedDBTypes, inst.DBType) {
		return nil, bad(fmt.Sprintf("暂不支持 %s 类型的实例", inst.DBType))
	}
	if v.IsSuperuser || queryAll {
		return inst, nil
	}
	ids, err := s.repo.UserInstanceIDs(v.ID)
	if err != nil {
		return nil, err
	}
	if !ids[id] {
		return nil, bad("实例不存在或无权限")
	}
	return inst, nil
}

const maxSQLLen = 200000

// CreateIssue DBA 手工建单。digests 为该 SQL 近 7 天的按天指标（慢日志页面建单时传入），有则按真实数据评估影响和级别；
// 没有时影响指标为 0，级别默认 P2。queryAll 表示该用户有 query.admin_run 权限。
func (s *Service) CreateIssue(ctx context.Context, v Viewer, queryAll bool, in CreateInput, digests []Digest, source string, sampleReal bool) (*Issue, error) {
	if !v.IsDBA() {
		return nil, forbid("需要 DBA(性能工单管理) 权限")
	}
	sql := strings.TrimSpace(in.SQL)
	if sql == "" {
		return nil, bad("请填写SQL")
	}
	if len(sql) > maxSQLLen {
		return nil, bad("SQL 过长")
	}
	if source == "" || source == "manual" { // 慢日志页面建单的 SQL 来自真实慢日志（可能是 INSERT 等），不在此限
		if err := validateSampleSQL(sql); err != nil {
			return nil, bad(err.Error())
		}
	}
	if len(in.DBName) > 128 {
		return nil, bad("库名过长")
	}
	if in.Severity != "" {
		if _, ok := severityRank[in.Severity]; !ok {
			return nil, bad("级别不合法")
		}
	}
	if len([]rune(in.Remark)) > maxRemarkLen {
		return nil, bad("备注过长")
	}
	inst, err := s.visibleInstance(v, queryAll, in.InstanceID)
	if err != nil {
		return nil, err
	}
	var assigneeID uint
	if in.AssigneeID != 0 {
		us, err := s.repo.GetUsers([]uint{in.AssigneeID})
		if err != nil {
			return nil, err
		}
		if u, ok := us[in.AssigneeID]; !ok || !u.Active {
			return nil, bad("所选负责人不存在或已停用")
		}
		assigneeID = in.AssigneeID
	}
	fp := Fingerprint(sql)
	ck := Checksum(fp)
	if old, err := s.repo.FindIssue(inst.ID, in.DBName, ck); err != nil {
		return nil, err
	} else if old != nil {
		return nil, &ExistsError{old.ID}
	}
	cfg := s.collectConfigFor(inst.ID, inst)
	now := s.now()
	var ds []Digest
	var first, last *time.Time
	for _, d := range digests {
		if d.ExecCount <= 0 {
			continue
		}
		ds = append(ds, d)
		for _, t := range []*time.Time{d.FirstSeen, d.LastSeen} {
			if t == nil {
				continue
			}
			if first == nil || t.Before(*first) {
				first = t
			}
			if last == nil || t.After(*last) {
				last = t
			}
		}
	}
	if first == nil {
		first, last = &now, &now
	}
	if source == "" {
		source = "manual"
	}
	i := &Issue{InstanceID: inst.ID, DBName: in.DBName, Checksum: ck, Fingerprint: clipRunes(fp, 65535), SampleSQL: sql,
		Tables: clipRunes(strings.Join(ExtractTables(sql, 10), ","), 500), Source: source, FirstSeen: first, LastSeen: last,
		Category: CatOther, OwnerSide: SideDev, Severity: SevP2, Status: StNew, CreateTime: now, UpdateTime: now}
	if err := s.repo.InsertIssue(i); err != nil {
		if err == ErrDuplicate {
			if old, _ := s.repo.FindIssue(inst.ID, in.DBName, ck); old != nil {
				return nil, &ExistsError{old.ID}
			}
		}
		return nil, err
	}
	for _, d := range ds {
		day := now
		if d.LastSeen != nil {
			day = *d.LastSeen
		}
		if err := s.repo.AddStat(i.ID, dateOf(day), StatDelta{ExecCount: d.ExecCount, TotalTime: d.TotalSec, MaxTimeMs: d.MaxMs,
			RowsExamined: d.RowsExamined, RowsSent: d.RowsSent}); err != nil {
			return nil, err
		}
	}
	if len(ds) > 0 {
		if err := s.refreshMetrics(i); err != nil {
			return nil, err
		}
		if err := s.repo.RefreshLoadShare(inst.ID); err != nil {
			slog.Error("刷新占比失败", "instance", inst.ID, "err", err)
		}
	}
	// 重新读出（指标刷新写过库），再诊断/路由
	if i, err = s.repo.GetIssue(i.ID); err != nil || i == nil {
		return nil, fmt.Errorf("读取新建的工单失败: %v", err)
	}
	prev := i.UpdateTime
	diag := Diagnose(ctx, s.explainer, DiagnoseInput{Instance: inst, Product: cfg.Product, MinAvgMs: cfg.MinAvgMs, DB: i.DBName, SQL: i.SampleSQL,
		Metrics: Metrics{i.AvgTimeMs, i.RowsSentAvg, i.RowsExaminedAvg, i.PerDayCount}, SampleReal: sampleReal,
		AutoExplain: s.cfgBool(settings.KeyPerfAutoExplain, true), TimeoutMs: s.cfgInt(settings.KeyPerfExplainTimeoutMs, 3000)})
	i.Category, i.OwnerSide, i.Diagnosis, i.Suggestion, i.SuggestedDDL, i.ExplainResult = diag.Category, diag.OwnerSide, diag.Diagnosis, diag.Suggestion, diag.SuggestedDDL, diag.ExplainResult
	if len(ds) > 0 {
		i.Severity = s.evaluateSeverity(i)
	}
	r, err := s.route(inst.ID, i.DBName, i.OwnerSide, cfg, splitComma(i.Tables)...)
	if err != nil {
		return nil, err
	}
	i.TeamID, i.AssigneeID, i.DBAID = optUint(r.teamID), optUint(r.assigneeID), optUint(r.dbaID)
	if assigneeID != 0 {
		i.AssigneeID = &assigneeID
		if i.TeamID == nil {
			if groups, _ := s.repo.UserGroupIDs(assigneeID); len(groups) > 0 {
				i.TeamID = ptr(groups[0])
			}
		}
	}
	if in.Severity != "" {
		i.Severity, i.SeverityLocked = in.Severity, true
	}
	i.Status = StNew
	if i.AssigneeID != nil {
		i.Status = StAssigned
	}
	setDeadlines(i, s.policy(i.Severity), now, false)
	if err := s.finishCreate(i, prev, whoOf(v.Actor), false, "手工建单"); err != nil {
		return nil, err
	}
	if remark := strings.TrimSpace(in.Remark); remark != "" {
		s.addLog(i.ID, ActComment, remark, whoOf(v.Actor))
	}
	s.notifyIssue(i.ID, event{name: evCreated}, 0)
	return i, nil
}

// CreateFromSlowLog 慢日志页面“生成性能工单”：按这条 SQL 近 7 天的慢日志计算影响并建单。
// sqlID 是慢日志页面传来的 SQL 指纹 MD5（slow_logs.sql_md5）。找不到明细时用页面上的 SQL 文本建单，影响指标为 0。
type SlowLogInput struct {
	InstanceID uint   `json:"instance_id"`
	SQLID      string `json:"sql_id"`
	SQLText    string `json:"sql_text"`
	DBName     string `json:"db_name"`
	Severity   string `json:"severity"`
	AssigneeID uint   `json:"assignee_id"`
	Remark     string `json:"remark"`
}

func (s *Service) CreateFromSlowLog(ctx context.Context, v Viewer, queryAll bool, in SlowLogInput) (*Issue, error) {
	if !v.IsDBA() {
		return nil, forbid("需要 DBA(性能工单管理) 权限")
	}
	if _, err := s.visibleInstance(v, queryAll, in.InstanceID); err != nil {
		return nil, err
	}
	sql, db := strings.TrimSpace(in.SQLText), strings.TrimSpace(in.DBName)
	var digests []Digest
	real := !paramPlaceholder.MatchString(sql)
	label := "慢日志页面SQL文本"
	if id := strings.TrimSpace(in.SQLID); id != "" {
		since := dateOf(s.now()).AddDate(0, 0, -7)
		sample, days, err := s.repo.SlowLogByMD5(in.InstanceID, id, db, since)
		if err != nil {
			return nil, err
		}
		if sample != nil {
			sql, db, real, label = sample.SQLText, sample.DBName, true, "平台慢日志 dbt_slow_logs"
			digests = days
		}
	}
	if sql == "" {
		return nil, bad("未找到这条慢SQL的明细，无法建单")
	}
	remark := strings.TrimSpace(in.Remark)
	if remark == "" {
		remark = fmt.Sprintf("由慢日志页面生成（%s）", label)
	}
	return s.CreateIssue(ctx, v, queryAll, CreateInput{InstanceID: in.InstanceID, DBName: db, SQL: sql, Severity: in.Severity,
		AssigneeID: in.AssigneeID, Remark: remark}, digests, "slowlog", real)
}

// 统计页展示的是参数化指纹（字面量为 ?），不能拿去 EXPLAIN。
var paramPlaceholder = regexp.MustCompile(`(^|[^\w'"])\?([^\w'"]|$)`)
