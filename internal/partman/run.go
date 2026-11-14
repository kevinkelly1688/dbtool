// @author: kevinkelly
// @license: Apache Licence
// @file: run.go
// @time: 2026/10/28

package partman

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"dbtool/internal/model"
)

// ---- 预览 ----

type PartitionView struct {
	Name    string `json:"name"`
	Bound   string `json:"bound"`             // 上边界；MAXVALUE 分区为 MAXVALUE；读不到上界（Doris）为“未知”
	Unknown bool   `json:"unknown,omitempty"` // 读不到上界：不参与续建与清理，永不删除
}

// InitRequest 是预览 / 执行里“初始化为分区表”的起点选择。
type InitRequest struct {
	StartMode string `json:"start_mode"`
	StartDate string `json:"start_date"`
}

// ConfirmInit 是执行初始化时要求的确认值：必须与服务端重新计算的结果逐项一致，且 table 为用户键入的“库.表”。
type ConfirmInit struct {
	Table          string `json:"table"`
	PartitionCount int    `json:"partition_count"`
	FirstPartition string `json:"first_partition"`
	LastPartition  string `json:"last_partition"`
}

// InitView 是初始化计划的展示形式。
type InitView struct {
	StartMode      string      `json:"start_mode"`
	StartDate      string      `json:"start_date"`
	Start          string      `json:"start"`
	PartitionCount int         `json:"partition_count"`
	FirstPartition string      `json:"first_partition"`
	LastPartition  string      `json:"last_partition"`
	LastBound      string      `json:"last_bound"`
	EarliestData   string      `json:"earliest_data"`
	CanExecute     bool        `json:"can_execute"`
	Checks         []InitCheck `json:"checks"`
	Notes          []string    `json:"notes"`
	Confirm        ConfirmInit `json:"confirm"`
}

// PlanView 是展示给前端的计划。
type PlanView struct {
	Kind       string          `json:"kind"` // maintain / init
	Dialect    string          `json:"dialect"`
	Timezone   string          `json:"timezone"`
	Table      string          `json:"table"`
	Now        string          `json:"now"`
	Method     string          `json:"method"`
	Expression string          `json:"expression"`
	Existing   []PartitionView `json:"existing"`
	Statements []Statement     `json:"statements"`
	Drops      []string        `json:"drops"`      // 将被删除的分区名（执行时要二次确认）
	Reorganize bool            `json:"reorganize"` // 需要 REORGANIZE（重写 MAXVALUE 分区）
	Warnings   []string        `json:"warnings"`
	Init       *InitView       `json:"init,omitempty"`
	Output     *OutputView     `json:"output"` // “检查过程”：只读预览的检查 / 决策输出（不落库）
}

func planView(p *Plan, at time.Time) *PlanView {
	v := &PlanView{Kind: p.Kind, Dialect: p.Dialect, Timezone: p.Timezone, Table: tableLabel(&p.Config), Now: p.Now.Format(timeLayout),
		Method: p.Method, Expression: p.Expression,
		Existing: []PartitionView{}, Statements: p.Statements(), Drops: p.DropNames(), Reorganize: p.Reorganize != nil, Warnings: p.Warnings}
	if v.Statements == nil {
		v.Statements = []Statement{}
	}
	if v.Drops == nil {
		v.Drops = []string{}
	}
	if v.Warnings == nil {
		v.Warnings = []string{}
	}
	for _, e := range p.Existing {
		b := "MAXVALUE"
		switch {
		case e.Unknown:
			b = "未知"
		case !e.MaxValue:
			b = e.Bound.Format(timeLayout)
		}
		v.Existing = append(v.Existing, PartitionView{Name: e.Name, Bound: b, Unknown: e.Unknown})
	}
	if in := p.Init; in != nil {
		iv := &InitView{StartMode: in.StartMode, StartDate: in.StartDate, Start: in.Start.Format(timeLayout), PartitionCount: in.PartitionCount,
			FirstPartition: in.First, LastPartition: in.Last, CanExecute: in.CanExecute, Checks: in.Checks, Notes: in.Notes,
			Confirm: ConfirmInit{Table: tableLabel(&p.Config), PartitionCount: in.PartitionCount, FirstPartition: in.First, LastPartition: in.Last}}
		if iv.Checks == nil {
			iv.Checks = []InitCheck{}
		}
		if iv.Notes == nil {
			iv.Notes = []string{}
		}
		if !in.LastBound.IsZero() {
			iv.LastBound = in.LastBound.Format(timeLayout)
		}
		if !in.Earliest.IsZero() {
			iv.EarliestData = in.Earliest.Format(timeLayout)
		}
		v.Init = iv
	}
	tr := newTracer(func() time.Time { return at })
	tr.addPlan(p)
	v.Output = outputView(capTrace(tr.lines, maxTraceLines, maxTraceBytes))
	return v
}

// MinReader 是 Target 的可选能力：读分区字段的最小值（初始化时“从最早数据开始”用）。
type MinReader interface {
	MinTime(ctx context.Context, db, table, column, colType string, loc *time.Location) (t time.Time, has bool, err error)
}

// plan 读取目标表并计算计划。init 为 nil 按“从当前窗口开始”。
func (s *Service) plan(ctx context.Context, inst *model.Instance, cfg Config, init *InitRequest) (*Plan, error) {
	tgt, err := s.dial(ctx, inst, s.meta.EngineConn(inst))
	if err != nil {
		return nil, targetErr("连接目标实例失败：%v", err)
	}
	defer tgt.Close()
	st, err := tgt.Inspect(ctx, cfg.DBName, cfg.TbName, cfg.Column)
	if err != nil {
		return nil, targetErr("读取目标表的分区信息失败：%v", err)
	}
	st.Dialect = DialectOf(inst.DBType)
	var opt InitOptions
	if init != nil {
		opt.StartMode, opt.StartDate = init.StartMode, init.StartDate
	}
	unpartitioned := st.Exists && st.IsBase && (st.Method == "" || len(st.Partitions) == 0)
	if unpartitioned && st.Dialect == DialectMySQL && opt.StartMode == StartEarliest && strings.EqualFold(st.ColumnType, cfg.ColType) {
		tz, err := resolveTimezone(cfg.Timezone, &st)
		if err != nil {
			return nil, err
		}
		mr, ok := tgt.(MinReader)
		if !ok {
			return nil, conflict("earliest_unavailable", "该连接不支持读取最早数据时间，请改用“指定起始日期”")
		}
		t, has, err := mr.MinTime(ctx, cfg.DBName, cfg.TbName, cfg.Column, cfg.ColType, tz.Loc)
		if err != nil {
			return nil, conflict("earliest_unavailable", "读取最早数据时间失败或超时（%s），请改用“指定起始日期”", clipMsg(err.Error(), 150))
		}
		opt.EarliestRead, opt.HasEarliest, opt.Earliest = true, has, t
	}
	return BuildPlanWith(cfg, st, s.now(), opt)
}

// targetErr 把连接 / 读取目标库的失败包成 502。format 是带一个 %v（放底层错误）的完整中文格式串，
// 整句写出来才能按目录翻译（如 "连接目标实例失败：%v"）。
func targetErr(format string, err error) error {
	var pe *Error
	if errors.As(err, &pe) {
		return pe
	}
	return newErr(502, "target_error", format, err)
}

// Preview 预览将执行的语句，不修改任何东西（不写审计、不写日志）。id 非 0 时用已保存的配置；否则用 draft（编辑弹窗里还没保存的内容）。
func (s *Service) Preview(ctx context.Context, c Caller, id uint, draft *ConfigInput, init *InitRequest) (*PlanView, error) {
	var cfg Config
	var inst *model.Instance
	var warns []string
	if id != 0 {
		saved, in, err := s.load(c, id)
		if err != nil {
			return nil, err
		}
		cfg, inst = *saved, in
	} else {
		if draft == nil {
			return nil, badReq("缺少配置内容")
		}
		in := *draft
		if err := in.validate(); err != nil {
			return nil, err
		}
		warns = in.warnings
		var err error
		if inst, err = s.checkTarget(c, &in); err != nil {
			return nil, err
		}
		cfg = Config{InstanceID: in.InstanceID, DBName: in.DBName, TbName: in.TbName, Column: in.Column, ColType: in.ColType, Period: in.Period,
			Interval: in.Interval, Retain: in.Retain, PreCreate: in.PreCreate, Timezone: in.Timezone, IsEnable: in.IsEnable, Env: in.Env}
	}
	if !SupportedDBType(inst.DBType) {
		return nil, unsupportedErr(inst.DBType)
	}
	if err := dbAllowed(inst, cfg.DBName); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	p, err := s.plan(ctx, inst, cfg, init)
	if err != nil {
		return nil, err
	}
	for _, w := range warns {
		if !contains(p.Warnings, w) {
			p.Warnings = append(p.Warnings, w)
		}
	}
	return planView(p, s.now()), nil
}

// ---- 执行 ----

// RunOptions 是手动执行时的确认项。
type RunOptions struct {
	ConfirmDrops      []string `json:"confirm_drops"`      // 用户在预览里看过并确认删除的分区名，必须与服务端重新计算出的完全一致
	ConfirmReorganize bool     `json:"confirm_reorganize"` // 确认执行 REORGANIZE（重写 MAXVALUE 分区）

	// 未分区的 MySQL 表“初始化为分区表”：起点选择与确认值（必须与服务端重算的结果逐项一致，table 为键入的“库.表”）。
	Init        *InitRequest `json:"init"`
	ConfirmInit *ConfirmInit `json:"confirm_init"`
}

type StatementResult struct {
	Kind      string `json:"kind"`
	SQL       string `json:"sql"`
	Status    string `json:"status"` // success / failed / skipped
	Error     string `json:"error,omitempty"`
	ElapsedMs int64  `json:"elapsed_ms"`
}

type RunResult struct {
	Statements []StatementResult `json:"statements"`
	Failed     bool              `json:"failed"`
	LogID      uint              `json:"log_id,omitempty"`
	Notice     string            `json:"notice,omitempty"`
	Output     *OutputView       `json:"output,omitempty"` // 本次运行输出（与 dbt_partman_log 里存的一致）
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func (s *Service) acquire(cnfID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight[cnfID] {
		return false
	}
	s.inFlight[cnfID] = true
	return true
}

func (s *Service) release(cnfID int64) {
	s.mu.Lock()
	delete(s.inFlight, cnfID)
	s.mu.Unlock()
}

func lockName(cnfID int64) string { return "dbt_partman_" + strconv.FormatInt(cnfID, 10) }

// startLine 是运行输出的起始行（同原程序的 “=== reconcile start | … ===”）：带全部参数。
func startLine(cfg *Config, inst *model.Instance, source, operator, tzDesc string) string {
	return fmt.Sprintf("=== 分区维护开始 | 实例=%s(#%d) 表=%s.%s col=%s coltype=%s period=%s interval=%d retain=%d preCreate=%d tz=%s 来源=%s 操作人=%s ===",
		inst.InstanceName, inst.ID, cfg.DBName, cfg.TbName, cfg.Column, cfg.ColType, cfg.Period, cfg.Interval, cfg.Retain, cfg.PreCreate, tzDesc, source, operator)
}

func tzDescOf(cfg *Config, p *Plan) string {
	if p == nil {
		if cfg.Timezone == "" {
			return "跟随目标库"
		}
		return cfg.Timezone
	}
	return fmt.Sprintf("%s(%s)", p.Timezone, offsetClock(zoneOffset(p.Now)))
}

// Run 手动执行一次维护。流程：重新读取目标表并计算计划（不信任任何前端传来的语句）→ 校验权限与确认项 →
// 写审计日志（写不进去就不执行）→ 逐条执行 → 推送 → 写 dbt_partman_log（含运行输出）。
func (s *Service) Run(ctx context.Context, c Caller, id uint, opt RunOptions) (*RunResult, error) {
	if !c.canRun() {
		return nil, newErr(403, "forbidden", "没有权限执行分区维护（需要 %s）", btnRun)
	}
	cfg, inst, err := s.load(c, id)
	if err != nil {
		return nil, err
	}
	if !SupportedDBType(inst.DBType) {
		return nil, unsupportedErr(inst.DBType)
	}
	if !cfg.IsEnable {
		return nil, newErr(409, "disabled", "该配置的维护开关是关闭的，请先在编辑里开启")
	}
	if err := dbAllowed(inst, cfg.DBName); err != nil {
		return nil, err
	}
	if !s.acquire(cfg.CnfID) {
		return nil, newErr(409, "busy", "该配置正在执行分区维护，请稍后再试")
	}
	defer s.release(cfg.CnfID)
	release, ok, err := s.repo.TryLock(ctx, lockName(cfg.CnfID))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, newErr(409, "busy", "该配置正在被另一个进程维护，请稍后再试")
	}
	defer release()

	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	started := s.now()
	p, err := s.plan(ctx, inst, *cfg, opt.Init)
	if err != nil {
		return nil, err
	}
	stmts := p.Statements()
	if p.Kind == KindInit {
		if err := c.need(permDelete, "初始化为分区表（重建整张表）"); err != nil {
			return nil, err
		}
		if !p.Init.CanExecute {
			e := newErr(409, "init_precheck_failed", "不能初始化为分区表：%s", firstInitError(p.Init))
			e.Data = planView(p, s.now())
			return nil, e
		}
		ci := opt.ConfirmInit
		want := ConfirmInit{Table: tableLabel(cfg), PartitionCount: p.Init.PartitionCount, FirstPartition: p.Init.First, LastPartition: p.Init.Last}
		if ci == nil || *ci != want {
			e := newErr(409, "confirm_required", "初始化会重建整张表，需要先预览并键入 %s 确认；或表在预览之后发生了变化（将创建 %d 个分区 %s ~ %s），请重新预览",
				want.Table, want.PartitionCount, want.FirstPartition, want.LastPartition)
			e.Data = planView(p, s.now())
			return nil, e
		}
	}
	if len(stmts) == 0 {
		return &RunResult{Statements: []StatementResult{}, Notice: "分区已经是最新的，没有需要执行的语句"}, nil
	}
	if p.Kind != KindInit {
		if drops := p.DropNames(); len(drops) > 0 {
			if err := c.need(permDelete, "删除分区（DROP PARTITION）"); err != nil {
				return nil, err
			}
			if !sameSet(drops, opt.ConfirmDrops) {
				e := newErr(409, "confirm_required", "将删除分区 %s，需要先预览并确认；或表的分区在预览之后发生了变化，请重新预览", strings.Join(drops, "、"))
				e.Data = planView(p, s.now())
				return nil, e
			}
		}
		if p.Reorganize != nil && !opt.ConfirmReorganize {
			e := newErr(409, "confirm_required", "续建需要 REORGANIZE PARTITION（会重写 MAXVALUE 分区里的数据），请确认后再执行")
			e.Data = planView(p, s.now())
			return nil, e
		}
	}
	tr := newTracer(s.now)
	tr.addAt(started, "INFO", 0, "%s", startLine(cfg, inst, SourceManual, c.Username, tzDescOf(cfg, p)))
	tr.addPlan(p)
	if p.Kind == KindInit {
		tr.add("INFO", "已按键入的 %s 确认初始化：%d 个分区 %s ~ %s", tableLabel(cfg), p.Init.PartitionCount, p.Init.First, p.Init.Last)
	} else {
		if drops := p.DropNames(); len(drops) > 0 {
			tr.add("INFO", "已确认删除分区：%s", strings.Join(drops, ", "))
		}
		if p.Reorganize != nil {
			tr.add("INFO", "已确认执行 REORGANIZE PARTITION")
		}
	}
	return s.execute(ctx, c, cfg, inst, stmts, SourceManual, tr, started)
}

func firstInitError(in *InitInfo) string {
	for _, ch := range in.Checks {
		if ch.Level == "error" {
			return ch.Message
		}
	}
	return "前置检查未通过"
}

// execute 写审计 → 执行 → 推送 → 写日志（含运行输出）。审计写失败则不执行（同在线查询：宁可失败，也不留下没有审计记录的目标库变更）。
// tr 里已经有起始行与检查 / 决策输出。
func (s *Service) execute(ctx context.Context, c Caller, cfg *Config, inst *model.Instance, stmts []Statement, source string, tr *tracer, started time.Time) (*RunResult, error) {
	sqls := make([]string, len(stmts))
	for i, st := range stmts {
		sqls[i] = st.SQL
	}
	action := "分区维护-执行" // i18n-ignore：审计动作名，不翻译
	switch {
	case source == SourceAuto:
		action = "分区维护-定时执行" // i18n-ignore：审计动作名，不翻译
	case len(stmts) == 1 && stmts[0].Kind == "init":
		action = "分区维护-执行初始化" // i18n-ignore：审计动作名，不翻译
	}
	if err := s.repo.InsertAudit(s.auditEntry(c, action, fmt.Sprintf("配置 #%d，实例 %s：%s", cfg.CnfID, inst.InstanceName, strings.Join(sqls, "; ")))); err != nil { // i18n-ignore：审计日志正文，不翻译
		slog.Error("分区维护审计日志写入失败，已取消执行", "cnf_id", cfg.CnfID, "user", c.Username, "err", err)
		return nil, newErr(500, "audit_failed", "审计日志写入失败，已取消执行，请联系管理员")
	}
	tgt, err := s.dial(ctx, inst, s.meta.EngineConn(inst))
	if err != nil {
		return nil, targetErr("连接目标实例失败：%v", err)
	}
	defer tgt.Close()
	results := tgt.Exec(ctx, sqls)
	s.invalidateStatus(cfg.ID) // 分区变了：状态缓存作废

	res := &RunResult{Statements: make([]StatementResult, 0, len(stmts))}
	items := make([]logItem, 0, len(stmts))
	at := s.now()
	ev := maintEvent{Env: cfg.Env, DB: cfg.DBName, Table: cfg.TbName}
	var okN, failN, skipN int
	for i, r := range results {
		sr := StatementResult{Kind: stmts[i].Kind, SQL: r.SQL, Status: "success", ElapsedMs: r.ElapsedMs}
		switch {
		case r.Skipped:
			sr.Status = "skipped"
			skipN++
			tr.add("WARN", "跳过（前面的语句失败）: %s", oneLine(r.SQL))
		case r.Err != "":
			sr.Status, sr.Error = "failed", r.Err
			res.Failed = true
			failN++
			tr.add("INFO", "执行 SQL: %s", oneLine(r.SQL))
			tr.add("ERROR", "执行失败（%d ms）: %s", r.ElapsedMs, r.Err)
			ev.Records = append(ev.Records, pushRecord{SQL: oneLine(r.SQL), Result: "failed:" + r.Err})
		default:
			okN++
			tr.add("INFO", "执行 SQL: %s", oneLine(r.SQL))
			tr.add("INFO", "执行成功（%d ms）", r.ElapsedMs)
			ev.Records = append(ev.Records, pushRecord{SQL: oneLine(r.SQL), Result: "success"})
		}
		res.Statements = append(res.Statements, sr)
		items = append(items, logItem{SQLText: sr.SQL, Kind: sr.Kind, Status: sr.Status, Error: sr.Error, ElapsedMs: sr.ElapsedMs,
			Operator: c.Username, Source: source, ExecAt: at.Format(timeLayout)})
	}
	ev.Skipped = skipN
	for _, l := range s.pushMaintenance(ctx, ev) {
		tr.lines = append(tr.lines, l)
	}
	total := s.now().Sub(started).Milliseconds()
	tr.add("INFO", "=== 分区维护结束 | 成功 %d / 失败 %d / 跳过 %d，耗时 %d ms ===", okN, failN, skipN, total)

	body := buildLogBody(items, tr.lines, logItem{Operator: c.Username, Source: source, ExecAt: at.Format(timeLayout)})
	logID, err := s.repo.InsertLog(cfg.CnfID, body, at)
	if err != nil {
		slog.Error("写入 dbt_partman_log 失败", "cnf_id", cfg.CnfID, "err", err)
		res.Notice = "语句已执行，但维护日志写入失败，请以审计日志为准"
	}
	res.LogID = logID
	res.Output = outputView(capTrace(tr.lines, maxTraceLines, maxTraceBytes))
	return res, nil
}

// logItem 是写进 dbt_partman_log.exec_result 的一项。外部程序写的项只有 sql_text 等基础字段，其余字段是本系统的补充。
// kind=trace 的项带运行输出（trace），没有 sql_text。
type logItem struct {
	SQLText   string        `json:"sql_text"`
	Kind      string        `json:"kind,omitempty"`
	Status    string        `json:"status"`
	Error     string        `json:"error,omitempty"`
	ElapsedMs int64         `json:"elapsed_ms"`
	Operator  string        `json:"operator"`
	Source    string        `json:"source"` // manual / auto
	ExecAt    string        `json:"exec_at"`
	Audience  string        `json:"audience,omitempty"` // admin：系统记录里只给有 change_partmanconfig 的人看
	Event     string        `json:"event,omitempty"`    // 系统记录的种类：auto_round / webhook_test
	Round     *RoundSummary `json:"round,omitempty"`    // 定时轮次摘要（event=auto_round）
	Trace     []traceLine   `json:"trace,omitempty"`
}

// RoundItem 是定时轮次摘要里的一个配置。Result：executed / failed / skipped / nothing。
type RoundItem struct {
	CnfID  int64  `json:"cnf_id"`
	Table  string `json:"table"`
	Result string `json:"result"`
	Reason string `json:"reason"`
}

// RoundSummary 是一轮定时维护的摘要（“跳过了谁、为什么”）。Items 不含“无需维护且没有说明”的配置（只计入 Nothing）。
type RoundSummary struct {
	Checked      int         `json:"checked"`
	Executed     int         `json:"executed"`
	Nothing      int         `json:"nothing"`
	Skipped      int         `json:"skipped"`
	Failed       int         `json:"failed"`
	GuardMinutes int         `json:"guard_minutes"`
	AutoDrop     bool        `json:"auto_drop"`
	NotifyOn     string      `json:"notify_on"`
	Items        []RoundItem `json:"items"`
	ItemsCut     int         `json:"items_cut,omitempty"` // 因条数上限省略的项数
}

const audienceAdmin = "admin"

// writeSystemLog 写一条不属于具体配置的“系统记录”（cnf_id=0）：定时轮次摘要、推送测试。失败只记服务日志。返回日志 ID。
func (s *Service) writeSystemLog(lines []traceLine, head logItem) uint {
	body := buildLogBody(nil, lines, head)
	id, err := s.repo.InsertLog(0, body, s.now())
	if err != nil {
		slog.Warn("写入分区维护系统记录失败", "err", err)
		return 0
	}
	return id
}

func marshalJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
