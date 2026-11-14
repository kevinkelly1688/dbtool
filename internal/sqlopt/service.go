// @author: kevinkelly
// @license: Apache Licence
// @file: service.go
// @time: 2026/10/28

package sqlopt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"dbtool/internal/audit"
	"dbtool/internal/model"
)

// Service 是 SQL 分析 / SQL 优化的业务逻辑。外部程序、目标库、元数据仓库都通过接口注入。
//
// 并发控制：后台任务里的每一条 SQL 逐条获取一个名额（全局同时最多 3 条在占用目标库连接 / 外部程序）；
// 同步接口（执行计划、内置建议、库列表）另有一组 3 个名额，等不到 5 秒就返回“系统繁忙”；
// 每人最多 2 个、全局最多 16 个进行中的任务。
type Service struct {
	repo    Repo
	opener  Opener
	runner  Runner
	audit   AuditSink
	sem     chan struct{} // 后台任务的并发名额
	syncSem chan struct{} // 同步接口（执行计划、内置建议、库列表）的并发名额，与后台任务分开，避免被长任务饿死
	jobs    *jobStore
	now     func() time.Time

	rootMu sync.Mutex
	root   context.Context
}

// DefaultConcurrency 是同时占用目标库连接 / 外部程序的最大并发数。
const DefaultConcurrency = 3

// NewService 创建服务。opener、runner 为 nil 时使用真实实现（database/sql、os/exec）。
func NewService(repo Repo, opener Opener, runner Runner, sink AuditSink) *Service {
	if opener == nil {
		opener = mysqlOpener{}
	}
	if runner == nil {
		runner = osRunner{}
	}
	return &Service{repo: repo, opener: opener, runner: runner, audit: sink, sem: make(chan struct{}, DefaultConcurrency),
		syncSem: make(chan struct{}, DefaultConcurrency), jobs: newJobStore(), now: time.Now, root: context.Background()}
}

// SetRoot 设置后台任务的根 context（服务退出时取消，进行中的任务会被终止）。
func (s *Service) SetRoot(ctx context.Context) {
	s.rootMu.Lock()
	s.root = ctx
	s.rootMu.Unlock()
}

func (s *Service) rootCtx() context.Context {
	s.rootMu.Lock()
	defer s.rootMu.Unlock()
	return s.root
}

// gc 清理过期任务（由后台 worker 定时调用；每次访问任务时也会顺带清理）。
func (s *Service) gc() {
	s.jobs.mu.Lock()
	s.jobs.gc(s.now())
	s.jobs.mu.Unlock()
}

func (s *Service) record(actor audit.Actor, action, extra string) {
	if s.audit == nil {
		return
	}
	e := &model.AuditEntry{UserID: actor.ID, UserName: actor.Username, UserDisplay: actor.Display, Action: action, ExtraInfo: truncRunes(extra, 900)}
	if err := s.audit.Record(e); err != nil {
		slog.Warn("写操作审计失败", "action", action, "err", err)
	}
}

// ---- 实例范围 ----

// visibleSet 计算用户可见的实例：超级用户、query.admin_run 权限、或实例属于用户的资源组。
func (s *Service) visibleSet(actor audit.Actor) (all bool, ids map[uint]bool, err error) {
	if actor.IsSuperuser {
		return true, nil, nil
	}
	ok, err := s.repo.HasButton(actor.ID, permQueryAllInsts)
	if err != nil {
		return false, nil, err
	}
	if ok {
		return true, nil, nil
	}
	ids, err = s.repo.UserInstanceIDs(actor.ID)
	return false, ids, err
}

func (s *Service) instanceFor(actor audit.Actor, id uint) (*model.Instance, error) {
	if id == 0 {
		return nil, bad("请选择实例")
	}
	inst, err := s.repo.GetInstance(id)
	if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && inst == nil) {
		return nil, newErr(http.StatusNotFound, "not_found", "实例不存在")
	}
	if err != nil {
		return nil, err
	}
	all, ids, err := s.visibleSet(actor)
	if err != nil {
		return nil, err
	}
	if !all && !ids[inst.ID] {
		return nil, newErr(http.StatusForbidden, "no_instance", "你所在组未关联该实例！")
	}
	return inst, nil
}

// Instances 返回当前用户可见的、至少支持执行计划的实例。
func (s *Service) Instances(actor audit.Actor) ([]instanceView, error) {
	all, ids, err := s.visibleSet(actor)
	if err != nil {
		return nil, err
	}
	list, err := s.repo.ListInstances()
	if err != nil {
		return nil, err
	}
	out := []instanceView{}
	for i := range list {
		if (all || ids[list[i].ID]) && explainDBTypes[list[i].DBType] {
			out = append(out, viewOf(&list[i]))
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

// ---- 目标库会话 ----

// target 是已连接的目标实例：会话 + 该用户可见的库（按实例的库名过滤规则）。
type target struct {
	inst    *model.Instance
	sess    Session
	db      string
	visible map[string]bool // 小写库名
	dbs     []string        // 可见库（原样大小写，已排序）
	dbStat  DBStats         // 库列表的来源统计（列表为空时用来向用户说明原因）
	secrets []string
}

func (t *target) close() { _ = t.sess.Close() }

// dbError 把目标库的错误转成给用户看的业务错误。
func (t *target) dbError(ctx context.Context, err error, what string) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return newErr(http.StatusGatewayTimeout, "timeout", fmt.Sprintf("%s超时", what))
	}
	if errors.Is(err, context.Canceled) {
		return newErr(http.StatusRequestTimeout, "canceled", fmt.Sprintf("%s已取消", what))
	}
	return newErr(http.StatusBadRequest, "db_error", fmt.Sprintf("%s失败：%s", what, redact(err.Error(), t.secrets...)))
}

var errBadConfig = newErr(http.StatusInternalServerError, "bad_config", "实例的库名过滤正则不合法，请联系管理员")

func anchoredMatcher(expr string) (func(string) bool, error) {
	if strings.TrimSpace(expr) == "" {
		return nil, nil
	}
	re, err := regexp.Compile(`\A(?:` + expr + `)`)
	if err != nil {
		return nil, errBadConfig
	}
	return re.MatchString, nil
}

// DBStats 说明实例上的库为什么被隐藏：账号 SHOW DATABASES 看到的总数，以及被系统库、实例的显示 / 屏蔽规则各排除了几个。
type DBStats struct {
	Total        int `json:"total"`
	HiddenSystem int `json:"hidden_system"`
	HiddenShow   int `json:"hidden_show"`   // 不符合实例的“库名显示规则”（show_db_name_regex）
	HiddenDenied int `json:"hidden_denied"` // 命中实例的“库名屏蔽规则”（denied_db_name_regex）
}

// visibleDBs 列出实例上用户可见的库：去掉系统库，再按实例的 show_db_name_regex / denied_db_name_regex 过滤。
func visibleDBs(ctx context.Context, sess Session, inst *model.Instance) ([]string, DBStats, error) {
	var st DBStats
	show, err := anchoredMatcher(inst.ShowDBNameRegex)
	if err != nil {
		return nil, st, err
	}
	denied, err := anchoredMatcher(inst.DeniedDBNameRegex)
	if err != nil {
		return nil, st, err
	}
	t, err := sess.Query(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, st, err
	}
	var out []string
	for _, r := range t.Rows {
		if len(r) == 0 {
			continue
		}
		n := cellString(r[0])
		st.Total++
		switch {
		case systemDBs[strings.ToLower(n)]:
			st.HiddenSystem++
		case show != nil && !show(n):
			st.HiddenShow++
		case denied != nil && denied(n):
			st.HiddenDenied++
		default:
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, st, nil
}

// connect 连接实例；db 非空时校验它在可见库里并 USE。
func (s *Service) connect(ctx context.Context, inst *model.Instance, db string) (*target, error) {
	if db != "" && !validDBName(db) {
		return nil, bad("数据库名不合法")
	}
	conn := connOf(s.repo, inst)
	sess, err := s.opener.Open(ctx, conn, "")
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, newErr(http.StatusGatewayTimeout, "timeout", fmt.Sprintf("连接实例 %s 超时", inst.InstanceName))
		}
		return nil, newErr(http.StatusBadGateway, "connect_failed", fmt.Sprintf("连接实例 %s 失败：%s", inst.InstanceName, redact(err.Error(), conn.Password)))
	}
	t := &target{inst: inst, sess: sess, db: db, secrets: []string{conn.Password}, visible: map[string]bool{}}
	list, stat, err := visibleDBs(ctx, sess, inst)
	if err != nil {
		t.close()
		var be *Error
		if errors.As(err, &be) {
			return nil, be
		}
		return nil, t.dbError(ctx, err, "读取数据库列表")
	}
	t.dbs, t.dbStat = list, stat
	for _, d := range list {
		t.visible[strings.ToLower(d)] = true
	}
	if db != "" {
		if !t.visible[strings.ToLower(db)] {
			t.close()
			return nil, newErr(http.StatusForbidden, "no_database", fmt.Sprintf("数据库 %s 不存在，或你没有权限访问它", db))
		}
		if _, err := sess.Query(ctx, "USE "+quoteIdent(db)); err != nil {
			t.close()
			return nil, t.dbError(ctx, err, "切换数据库")
		}
	}
	return t, nil
}

// checkRefs 语句里带库名引用的表必须落在用户可见的库内（防止借 EXPLAIN / 表结构探测被隐藏的库）。
func (t *target) checkRefs(p *pstmt) error {
	for _, r := range p.tableRefs() {
		if r.Schema != "" && !t.visible[strings.ToLower(r.Schema)] {
			return newErr(http.StatusForbidden, "no_database", fmt.Sprintf("语句涉及的数据库 %s 不存在，或你没有权限访问它", r.Schema))
		}
	}
	return nil
}

// Databases 返回实例上用户可见的库。
func (s *Service) Databases(ctx context.Context, actor audit.Actor, instID uint) ([]string, error) {
	d, err := s.DatabasesDetail(ctx, actor, instID)
	if err != nil {
		return nil, err
	}
	return d.Databases, nil
}

// DBList 是库列表加上它的来源统计：页面在列表为空时据此说明原因，而不是给一个空下拉。
type DBList struct {
	Databases []string `json:"databases"`
	DBStats
}

// DatabasesDetail 同 Databases，另带统计。
func (s *Service) DatabasesDetail(ctx context.Context, actor audit.Actor, instID uint) (*DBList, error) {
	inst, err := s.instanceFor(actor, instID)
	if err != nil {
		return nil, err
	}
	if !explainDBTypes[inst.DBType] {
		return nil, newErr(http.StatusBadRequest, "unsupported", fmt.Sprintf("暂不支持 %s 类型的实例", inst.DBType))
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	rel, err := s.acquire(ctx, acquireWait, s.syncSem)
	if err != nil {
		return nil, err
	}
	defer rel()
	t, err := s.connect(ctx, inst, "")
	if err != nil {
		return nil, err
	}
	defer t.close()
	return &DBList{Databases: append([]string{}, t.dbs...), DBStats: t.dbStat}, nil
}

// acquire 获取一个并发名额；wait<=0 表示一直等到 ctx 结束。
func (s *Service) acquire(ctx context.Context, wait time.Duration, sem chan struct{}) (func(), error) {
	var timeout <-chan time.Time
	if wait > 0 {
		tm := time.NewTimer(wait)
		defer tm.Stop()
		timeout = tm.C
	}
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, newErr(http.StatusRequestTimeout, "canceled", "请求已取消")
	case <-timeout:
		return nil, newErr(http.StatusTooManyRequests, "busy", "系统当前分析请求较多，请稍后再试")
	}
}

// ---- 执行计划 ----

type ExplainReq struct {
	InstanceID uint   `json:"instance_id"`
	DB         string `json:"db"`
	SQL        string `json:"sql"`
	Format     string `json:"format"` // "" / traditional / json
}

type ExplainResult struct {
	Statement string    `json:"statement"`
	Format    string    `json:"format"`
	Plan      *Table    `json:"plan,omitempty"`
	JSON      string    `json:"json,omitempty"`
	Findings  []Finding `json:"findings"`
}

// Explain 在目标实例上执行 EXPLAIN（不会执行语句本身）。同步接口，整体超时 30 秒。
func (s *Service) Explain(ctx context.Context, actor audit.Actor, req ExplainReq) (*ExplainResult, error) {
	stmt, _, err := prepareExplain(req.SQL)
	if err != nil {
		return nil, err
	}
	format := strings.ToLower(strings.TrimSpace(req.Format))
	if format != "" && format != "traditional" && format != "json" {
		return nil, bad("不支持的执行计划格式")
	}
	if format == "" {
		format = "traditional"
	}
	inst, err := s.instanceFor(actor, req.InstanceID)
	if err != nil {
		return nil, err
	}
	if !explainDBTypes[inst.DBType] {
		return nil, newErr(http.StatusBadRequest, "unsupported", fmt.Sprintf("暂不支持 %s 类型实例的执行计划", inst.DBType))
	}
	if format == "json" && !toolDBTypes[inst.DBType] {
		return nil, bad("该类型的实例不支持 JSON 格式的执行计划")
	}
	ctx, cancel := context.WithTimeout(ctx, explainTimeout)
	defer cancel()
	rel, err := s.acquire(ctx, acquireWait, s.syncSem)
	if err != nil {
		return nil, err
	}
	defer rel()
	t, err := s.connect(ctx, inst, req.DB)
	if err != nil {
		return nil, err
	}
	defer t.close()
	if err := t.checkRefs(parseStmt(stmt)); err != nil {
		return nil, err
	}
	s.record(actor, "SQL优化-执行计划", fmt.Sprintf("实例:%s 库:%s SQL:%s", inst.InstanceName, req.DB, stmt))
	prefix := "EXPLAIN "
	if format == "json" {
		prefix = "EXPLAIN FORMAT=JSON "
	}
	plan, err := t.sess.Query(ctx, prefix+stmt)
	if err != nil {
		return nil, t.dbError(ctx, err, "获取执行计划")
	}
	res := &ExplainResult{Statement: stmt, Format: format, Plan: plan, Findings: []Finding{}}
	if format == "json" {
		res.Plan = nil
		if len(plan.Rows) > 0 && len(plan.Rows[0]) > 0 {
			res.JSON = prettyJSON(cellString(plan.Rows[0][0]))
		}
	} else {
		res.Findings = append(res.Findings, planFindings(plan)...)
	}
	return res, nil
}

func prettyJSON(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return s
	}
	return string(b)
}

// ---- 内置规则建议 ----

// Report 是对一条 SQL 的分析报告：内置规则的建议 + （选了实例时）表结构与执行计划相关的建议 + （配置了 SOAR 时）SOAR 报告。
type Report struct {
	Type        string    `json:"type"`
	Fingerprint string    `json:"fingerprint"`
	FPID        string    `json:"fp_id"`
	Tables      []string  `json:"tables"`
	Findings    []Finding `json:"findings"`
	Score       int       `json:"score"`
	Highest     string    `json:"highest,omitempty"`
	Plan        *Table    `json:"plan,omitempty"`
	PlanError   string    `json:"plan_error,omitempty"`
	IndexDDL    []string  `json:"index_ddl,omitempty"`
	// 现有索引清单、建议索引上线前后的执行计划对比（后者是估算值，见 indexextra.go）
	ExistingIndexes []IndexInfo   `json:"existing_indexes,omitempty"`
	PlanCompare     []PlanCompare `json:"plan_compare,omitempty"`
	Notes           []string      `json:"notes,omitempty"`
	Soar            string        `json:"soar,omitempty"`
	SoarError       string        `json:"soar_error,omitempty"`
}

type AdviseReq struct {
	InstanceID uint   `json:"instance_id"`
	DB         string `json:"db"`
	SQL        string `json:"sql"`
}

// Advise 内置规则的优化建议（同步，整体超时 45 秒）。不选实例时只做静态规则；选了实例会读取表结构并分析执行计划。
func (s *Service) Advise(ctx context.Context, actor audit.Actor, req AdviseReq) (*Report, error) {
	stmt, err := normalizeSingle(req.SQL)
	if err != nil {
		return nil, err
	}
	if req.InstanceID == 0 {
		return s.buildReport(ctx, nil, stmt), nil
	}
	inst, err := s.instanceFor(actor, req.InstanceID)
	if err != nil {
		return nil, err
	}
	if !toolDBTypes[inst.DBType] {
		return nil, newErr(http.StatusBadRequest, "unsupported", fmt.Sprintf("内置建议暂不支持 %s 类型的实例，请使用“执行计划”", inst.DBType))
	}
	ctx, cancel := context.WithTimeout(ctx, adviseTimeout)
	defer cancel()
	rel, err := s.acquire(ctx, acquireWait, s.syncSem)
	if err != nil {
		return nil, err
	}
	defer rel()
	t, err := s.connect(ctx, inst, req.DB)
	if err != nil {
		return nil, err
	}
	defer t.close()
	if err := t.checkRefs(parseStmt(stmt)); err != nil {
		return nil, err
	}
	s.record(actor, "SQL优化-内置建议", fmt.Sprintf("实例:%s 库:%s SQL:%s", inst.InstanceName, req.DB, stmt))
	return s.buildReport(ctx, t, stmt), nil
}

// buildReport 生成一条语句的报告。t 为 nil 时只做静态分析；与目标库交互的失败只记录在报告里，不让整个分析失败。
func (s *Service) buildReport(ctx context.Context, t *target, stmt string) *Report {
	p := parseStmt(stmt)
	fp := Fingerprint(stmt)
	rep := &Report{Type: p.kind, Fingerprint: fp, FPID: FingerprintID(fp), Tables: p.tableNames(), Findings: StaticFindings(stmt)}
	if rep.Tables == nil {
		rep.Tables = []string{}
	}
	if t != nil {
		s.schemaAnalysis(ctx, t, p, stmt, rep)
	}
	if rep.Findings == nil { // 没有建议时也要序列化成 []：页面直接读 findings.length，null 会让报告弹窗报错
		rep.Findings = []Finding{}
	}
	sort.SliceStable(rep.Findings, func(a, b int) bool { return levelRank[rep.Findings[a].Level] > levelRank[rep.Findings[b].Level] })
	rep.Score = scoreOf(rep.Findings)
	rep.Highest = highestLevel(rep.Findings)
	return rep
}

func (s *Service) schemaAnalysis(ctx context.Context, t *target, p *pstmt, stmt string, rep *Report) {
	if err := t.checkRefs(p); err != nil {
		rep.Notes = append(rep.Notes, err.Error())
		return
	}
	refs := p.tableRefs()
	if len(refs) > 8 {
		refs = refs[:8]
		rep.Notes = append(rep.Notes, "语句涉及的表较多，只读取了前 8 张表的结构")
	}
	ms := &metaSet{refs: refs, metas: make([]*tableMeta, len(refs))}
	for i, r := range refs {
		schema := r.Schema
		if schema == "" {
			schema = t.db
		}
		if schema == "" {
			rep.Notes = append(rep.Notes, fmt.Sprintf("没有选择数据库，无法读取表 %s 的结构", r.Name))
			continue
		}
		m, err := loadTableMeta(ctx, t.sess, schema, r.Name)
		if err != nil {
			rep.Notes = append(rep.Notes, fmt.Sprintf("读取表 %s 的结构失败：%s", r.Name, redact(err.Error(), t.secrets...)))
			if ctx.Err() != nil {
				return
			}
			continue
		}
		if m == nil {
			rep.Notes = append(rep.Notes, fmt.Sprintf("表 %s.%s 不存在（或是 CTE / 派生表），跳过结构分析", schema, r.Name))
		}
		ms.metas[i] = m
	}
	rep.Findings = append(rep.Findings, implicitConversion(p, ms)...)
	sugg := adviseIndex(p, ms)
	if sugg != nil {
		rep.Findings = append(rep.Findings, sugg.Finding)
		rep.IndexDDL = []string{sugg.DDL}
	}
	rep.ExistingIndexes = existingIndexes(ms)
	budget, seenTbl := maxRedundantFindings, map[*tableMeta]bool{}
	for _, m := range ms.metas {
		if m == nil || seenTbl[m] {
			continue
		}
		seenTbl[m] = true
		fs := redundantIndexFindings(m, budget)
		rep.Findings = append(rep.Findings, fs...)
		budget -= len(fs)
	}

	// 执行计划
	one, _, err := prepareExplain(stmt)
	if err != nil {
		if p.kind != TypeDDL && p.kind != TypeOther {
			rep.PlanError = err.Error()
		}
		return
	}
	plan, err := t.sess.Query(ctx, "EXPLAIN "+one)
	if err != nil {
		rep.PlanError = redact(err.Error(), t.secrets...)
		return
	}
	rep.Plan = plan
	rep.Findings = append(rep.Findings, planFindings(plan)...)
	if sugg != nil && len(ms.refs) == 1 {
		if pc := buildPlanCompare(sugg, plan, ms.refs[0]); pc != nil {
			rep.PlanCompare = []PlanCompare{*pc}
		}
	}
}
