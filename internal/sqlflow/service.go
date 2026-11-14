// @author: kevinkelly
// @license: Apache Licence
// @file: service.go
// @time: 2026/10/28

package sqlflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/engine"
	exportpkg "dbtool/internal/export"
	"dbtool/internal/firewall"
	"dbtool/internal/goinception"
	"dbtool/internal/i18n"
	"dbtool/internal/model"
	"dbtool/internal/notify"
	"dbtool/internal/query"
	"dbtool/internal/redisrule"
	"dbtool/internal/review"
	"dbtool/internal/settings"
	"dbtool/internal/sqlx"
	"dbtool/internal/tsql"
)

const (
	maxNameLen   = 50        // 工单名称长度（与库表字段一致）
	maxURLLen    = 500       // 需求链接
	maxDBLen     = 64        // 库名
	maxSQLLength = 2_000_000 // SQL 文本上限（字符）
)

func newErr(status int, code, msg string) error { return query.NewError(status, code, msg) }
func bad(msg string) error                      { return newErr(400, "bad_request", msg) }
func forbid(msg string) error                   { return newErr(403, "forbidden", msg) }
func notFound(msg string) error                 { return newErr(404, "not_found", msg) }
func conflict(msg string) error                 { return newErr(409, "conflict", msg) }

// translate 把审批引擎的业务错误转成对外错误；其他错误原样返回（由上层记日志并返回 500）。
func translate(err error) error {
	var ae *audit.Error
	if errors.As(err, &ae) {
		switch ae.Kind {
		case audit.KindConflict:
			return conflict(ae.Message)
		case audit.KindConfig:
			return newErr(400, "audit_config", ae.Message)
		default:
			return forbid(ae.Message)
		}
	}
	return err
}

// Service 是 SQL 工单的业务入口。
type Service struct {
	repo       Repo
	open       Opener
	notifier   notify.Notifier
	now        func() time.Time
	exportAuth *query.PrivAuthorizer // 数据导出的库表权限校验；nil = 不校验（测试 / 演示）
	async      func(func())          // 发通知用；测试里替换成同步执行
	spawn      func(func())          // 后台执行工单用；测试里替换成同步执行
	wg         sync.WaitGroup
	prog       sync.Map // 工单 ID -> *runProgress：后台执行 / 导出的进度（仅本进程内存）
	runs       sync.Map // 工单 ID -> *runCtl：正在执行的工单的取消句柄（仅本进程内存，见 cancelrun.go）

	recoverEvery time.Duration // 周期扫描“卡在执行中”的工单的间隔；0 = recoverInterval（见 recover.go）

	newInception     func(goinception.Config) Inception
	newBackup        func(goinception.BackupConfig) BackupReader
	requireInception bool
	batchTypes       map[string]bool // 允许批量上线的实例类型
	exportStore      exportpkg.Storage
	exportBase       string           // 导出目录里相对路径的基准目录
	BaseURL          string           // 对外访问地址，用于通知里的工单链接
	redisRules       redisrule.Source // Redis 工单上线规则的来源（见 redis.go）；为空 = 没有规则
	masker           ResultMasker     // 导出结果的数据脱敏（见 export.go）；为空 = 导出不脱敏
	fw               *firewall.Engine // SQL 防火墙（见 firewall.go）；为空 = 不启用
}

// Options 是构造服务的可选项。
type Options struct {
	Notifier         notify.Notifier
	Open             Opener
	RequireInception bool
	BatchDBTypes     []string          // 允许批量上线的实例类型；为空时默认只有 mssql
	ExportStorage    exportpkg.Storage // 导出文件存储；为空时按 dbt_setting 使用本地目录（测试里注入）
	ExportBaseDir    string            // 相对路径的基准目录（环境变量 EXPORT_BASE_DIR），默认当前目录
}

func NewService(repo Repo, o Options) *Service {
	if o.Notifier == nil {
		o.Notifier = notify.Noop{}
	}
	if o.Open == nil {
		o.Open = engine.New
	}
	s := &Service{repo: repo, open: o.Open, notifier: o.Notifier, now: time.Now, requireInception: o.RequireInception, batchTypes: map[string]bool{}, exportStore: o.ExportStorage, exportBase: o.ExportBaseDir}
	if len(o.BatchDBTypes) == 0 {
		o.BatchDBTypes = []string{dbTypeMSSQL}
	}
	for _, t := range o.BatchDBTypes {
		if t = strings.TrimSpace(t); t != "" {
			s.batchTypes[t] = true
		}
	}
	s.async = func(f func()) { go f() }
	s.spawn = func(f func()) {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			f()
		}()
	}
	s.newInception = func(c goinception.Config) Inception { return goinception.New(c) }
	s.newBackup = func(c goinception.BackupConfig) BackupReader { return goinception.NewBackup(c) }
	return s
}

// SetAsync / SetSpawn 让通知与后台执行同步进行，仅测试使用。
func (s *Service) SetAsync(f func(func())) { s.async = f }
func (s *Service) SetSpawn(f func(func())) { s.spawn = f }
func (s *Service) SetClock(f func() time.Time) {
	s.now = f
}

// SetOpener 替换引擎创建函数，仅测试使用。
func (s *Service) SetOpener(o Opener) { s.open = o }

// SetInception 替换 goInception 客户端与备份库读取器，仅测试使用。
func (s *Service) SetInception(i func(goinception.Config) Inception, b func(goinception.BackupConfig) BackupReader) {
	if i != nil {
		s.newInception = i
	}
	if b != nil {
		s.newBackup = b
	}
}

// Wait 等待所有后台执行结束（优雅停机、测试）。
func (s *Service) Wait() { s.wg.Wait() }

func (s *Service) has(p audit.Actor, code string) (bool, error) {
	if p.IsSuperuser {
		return true, nil
	}
	return s.repo.HasButton(p.ID, code)
}

// ---- 权限判断 ----

type perms struct {
	submit, batchSubmit, execute, executeRG, audit, queryAll     bool
	viewGroup, approve, reject                                   bool
	exportSubmit, exportBatch, download, menuExport, menuExportB bool
	groups                                                       map[uint]bool
}

// groupView 能看到所在资源组内的全部工单：持有“查看资源组内所有工单”、审批、驳回或“立即执行（资源组内）”任一按钮。
func (pm perms) groupView() bool { return pm.viewGroup || pm.approve || pm.reject || pm.executeRG }

// PermLister 是仓库可选实现的优化：一次取出用户的全部权限码。没有实现时逐个权限查询。
type PermLister interface {
	AllowedButtons(userID uint) (map[string]bool, error)
}

func (s *Service) loadPerms(p audit.Actor) (perms, error) {
	var out perms
	codes := map[string]*bool{permSubmit: &out.submit, permBatchSubmit: &out.batchSubmit, permExportSubmit: &out.exportSubmit, permExportBatch: &out.exportBatch,
		permDownload: &out.download, permMenuExport: &out.menuExport, permMenuExportB: &out.menuExportB, permExecute: &out.execute,
		permViewGroup: &out.viewGroup, permApprove: &out.approve, permReject: &out.reject,
		permExecuteForRG: &out.executeRG, permAuditUser: &out.audit, permQueryAllInsts: &out.queryAll}
	if pl, ok := s.repo.(PermLister); ok && !p.IsSuperuser {
		have, err := pl.AllowedButtons(p.ID)
		if err != nil {
			return out, err
		}
		for c, dst := range codes {
			*dst = have[c]
		}
	} else {
		for c, dst := range codes {
			var err error
			if *dst, err = s.has(p, c); err != nil {
				return out, err
			}
		}
	}
	gs, err := s.repo.UserResourceGroups(p.ID, p.IsSuperuser)
	if err != nil {
		return out, err
	}
	out.groups = map[uint]bool{}
	for _, g := range gs {
		out.groups[g.GroupID] = true
	}
	return out, nil
}

// CanAccessPage 判断能否打开工单列表（sqlworkflow.view_own / export.view / export.view_batch 任一）。
func (s *Service) CanAccessPage(p audit.Actor) bool {
	for _, code := range []string{permMenu, permMenuExport, permMenuExportB} {
		if ok, err := s.has(p, code); err == nil && ok {
			return true
		}
	}
	return false
}

// CanSubmit 判断能否提交工单。
func (s *Service) CanSubmit(p audit.Actor) bool {
	for _, code := range []string{permSubmit, permBatchSubmit} {
		if ok, err := s.has(p, code); err == nil && ok {
			return true
		}
	}
	return false
}

// CanExport 判断能否提交数据导出工单（单实例或批量）。
func (s *Service) CanExport(p audit.Actor) bool {
	for _, code := range []string{permExportSubmit, permExportBatch} {
		if ok, err := s.has(p, code); err == nil && ok {
			return true
		}
	}
	return false
}

func (pm perms) canView(p audit.Actor, w *model.SqlWorkflow) bool {
	switch {
	case p.IsSuperuser || pm.audit:
		return true
	case w.Engineer == p.Username:
		return true // 提交人始终能看自己提交的工单（含导出工单）
	case w.IsOfflineExport != 0 && !w.IsBatch() && !pm.menuExport:
		return false // 他人的导出工单：单实例导出需要 export.view
	case w.IsOfflineExport != 0 && w.IsBatch() && !pm.menuExportB:
		return false // 批量导出需要 export.view_batch
	case pm.groupView():
		return pm.groups[w.GroupID]
	}
	return false
}

func (pm perms) canExecute(p audit.Actor, w *model.SqlWorkflow) bool {
	if w.Status != model.WfReviewPass && w.Status != model.WfTimingTask {
		return false
	}
	if pm.groups[w.GroupID] && pm.executeRG {
		return true
	}
	return w.Engineer == p.Username && pm.execute
}

// ---- 提交 ----

type SubmitReq struct {
	Name             string
	DemandURL        string
	InstanceID       uint
	BatchInstanceIDs []uint // 非空表示批量上线：同一段 SQL 依次在这些实例上执行；第一个实例作为工单的主实例，检查只在它上面做
	DBName           string
	GroupName        string
	SQL              string
	IsBackup         bool
	SkipCheck        bool        // 跳过检查、原生执行（仅超级用户；批量工单不支持）
	Export           *ExportSpec // 非空表示数据导出工单
	RunStart         *time.Time
	RunEnd           *time.Time
	PlanAt           *time.Time // 提交时设定的“定时执行 / 定时导出”时间：审批全部通过后自动转入定时执行（需要 dbt_sql_schedule 表）
}

// SubmitResult 是提交后的结果。
type SubmitResult struct {
	WorkflowID uint   `json:"workflow_id"`
	Status     string `json:"status"`
	Message    string `json:"message"`
}

const maxBatchInstances = 100 // 单个批量工单最多的实例数（受 dbt_sql_workflow.batch_instance_ids 字段长度限制）

// mysqlProtocolTypes 是走 MySQL 协议、能用 goInception / 原生执行的实例类型。显式列出，不借用 query.SupportsQuery：
// 在线查询已经接入 PostgreSQL / MongoDB / ClickHouse，它们不能因此进入 SQL 工单。
var mysqlProtocolTypes = map[string]bool{"mysql": true, "polardb": true, "polardb-x": true, "polardbx": true, "selectdb": true, "doris": true, "adb": true}

// workflowSupports 哪些类型的实例能走 SQL 工单：MySQL 协议系（goInception/原生）、SQL Server 与 Redis。
func workflowSupports(dbType string) bool {
	// Redis：提交的是命令，由管理员定义的规则检查（见 redis.go）。
	return dbType == dbTypeMSSQL || dbType == dbTypeRedis || mysqlProtocolTypes[dbType]
}

// BatchTypes 返回允许批量上线的实例类型（默认只有 SQL Server）。
func (s *Service) BatchTypes() []string {
	out := make([]string, 0, len(s.batchTypes))
	for t := range s.batchTypes {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func dedupeIDs(in []uint) []uint {
	seen := map[uint]bool{}
	var out []uint
	for _, id := range in {
		if id != 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// checkTarget 校验一个目标实例：存在、带 can_write、属于所选资源组、用户能访问、类型受支持。
func (s *Service) checkTarget(p audit.Actor, pm perms, inGroup map[uint]bool, visible map[uint]bool, inst *model.Instance, batch, export bool) error {
	if inst == nil {
		return bad("不存在该实例")
	}
	if export {
		// 导出只读数据：需要开启“查询”（导出只需读权限，上线变更需要写权限）
		if !query.HasReadTag(inst) {
			return bad(fmt.Sprintf("实例 %s 未开启“查询”权限（缺少 can_read 标签），不能导出", inst.InstanceName))
		}
	} else if !query.HasActiveTag(inst, "can_write") {
		return bad(fmt.Sprintf("实例 %s 未开启“上线”权限（缺少 can_write 标签）", inst.InstanceName))
	}
	if !workflowSupports(inst.DBType) {
		return bad(fmt.Sprintf("暂不支持 %s 类型的实例（%s）提交 SQL 工单", inst.DBType, inst.InstanceName))
	}
	if err := redisModeError(inst, batch, export); err != nil {
		return err
	}
	if batch && !s.batchTypes[inst.DBType] {
		return bad(fmt.Sprintf("实例 %s 的类型是 %s，批量%s只支持：%s", inst.InstanceName, inst.DBType, kindWord(export), strings.Join(s.BatchTypes(), "、")))
	}
	// 实例必须属于所选资源组：审批流跟着资源组走，不允许拿别的组的宽松审批流去上线这个实例。
	if !inGroup[inst.ID] {
		return bad(fmt.Sprintf("所选资源组未关联实例 %s", inst.InstanceName))
	}
	if !pm.queryAll && !visible[inst.ID] {
		return forbid(fmt.Sprintf("你所在组未关联实例 %s！", inst.InstanceName))
	}
	return nil
}

// resolveTargets 校验并载入目标实例（顺序同提交时选择的顺序）。批量时要求同一类型。
func (s *Service) resolveTargets(p audit.Actor, pm perms, g *model.ResourceGroup, ids []uint, batch, export bool) ([]*model.Instance, error) {
	ins, err := s.repo.GroupInstances(g.GroupID)
	if err != nil {
		return nil, err
	}
	inGroup := map[uint]bool{}
	for _, i := range ins {
		inGroup[i.ID] = true
	}
	var visible map[uint]bool
	if !pm.queryAll {
		if visible, err = s.repo.UserInstanceIDs(p.ID); err != nil {
			return nil, err
		}
	}
	var out []*model.Instance
	for _, id := range ids {
		inst, err := s.repo.GetInstance(id)
		if err != nil {
			return nil, err
		}
		if err := s.checkTarget(p, pm, inGroup, visible, inst, batch, export); err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	if batch {
		types := map[string]bool{}
		for _, i := range out {
			types[i.DBType] = true
		}
		if len(types) > 1 {
			return nil, bad(fmt.Sprintf("所选实例的数据库类型不一致，批量%s要求所有实例为同一类型", kindWord(export)))
		}
	}
	return out, nil
}

func (s *Service) validateSubmit(p audit.Actor, pm perms, r *SubmitReq) (*model.ResourceGroup, []*model.Instance, error) {
	r.BatchInstanceIDs = dedupeIDs(r.BatchInstanceIDs)
	batch := len(r.BatchInstanceIDs) > 0
	export := r.Export != nil
	switch {
	case export && batch && !pm.exportBatch:
		return nil, nil, forbid("你没有提交批量数据导出工单的权限（缺少 export.submit_batch）")
	case export && !batch && !pm.exportSubmit:
		return nil, nil, forbid("你没有提交数据导出工单的权限（缺少 export.submit）")
	case !export && batch && !pm.batchSubmit:
		return nil, nil, forbid("你没有提交批量上线工单的权限（缺少 sqlworkflow.submit_batch）")
	case !export && !batch && !pm.submit:
		return nil, nil, forbid("你没有提交 SQL 工单的权限")
	}
	if export && !exportpkg.ValidFormat(r.Export.Format) {
		return nil, nil, bad("请选择导出格式（csv / xlsx / json / xml / sql）")
	}
	r.Name, r.DemandURL, r.DBName, r.GroupName = strings.TrimSpace(r.Name), strings.TrimSpace(r.DemandURL), strings.TrimSpace(r.DBName), strings.TrimSpace(r.GroupName)
	switch {
	case r.Name == "":
		return nil, nil, bad("请填写工单名称")
	case len([]rune(r.Name)) > maxNameLen:
		return nil, nil, bad(fmt.Sprintf("工单名称过长（最多 %d 个字符）", maxNameLen))
	case len([]rune(r.DemandURL)) > maxURLLen:
		return nil, nil, bad("需求链接过长")
	case r.DemandURL != "" && !strings.HasPrefix(strings.ToLower(r.DemandURL), "http://") && !strings.HasPrefix(strings.ToLower(r.DemandURL), "https://"):
		return nil, nil, bad("需求链接必须以 http:// 或 https:// 开头")
	case r.DBName == "" || len([]rune(r.DBName)) > maxDBLen:
		return nil, nil, bad("请选择数据库")
	case strings.TrimSpace(r.SQL) == "":
		return nil, nil, bad("SQL 内容不能为空")
	case len([]rune(r.SQL)) > maxSQLLength:
		return nil, nil, bad("SQL 内容过长")
	case batch && len(r.BatchInstanceIDs) > maxBatchInstances:
		return nil, nil, bad(fmt.Sprintf("单个批量工单最多支持 %d 个实例，当前选择了 %d 个", maxBatchInstances, len(r.BatchInstanceIDs)))
	case batch && export && len(r.BatchInstanceIDs) < 2:
		return nil, nil, bad("批量导出至少选择 2 个实例；只导出一个实例请使用单实例导出")
	case batch && r.SkipCheck:
		return nil, nil, bad("批量工单不支持跳过语法检查")
	case export && r.SkipCheck:
		return nil, nil, bad("数据导出工单不支持跳过检查")
	}
	if r.SkipCheck && !p.IsSuperuser {
		return nil, nil, forbid("仅管理员可以跳过语法检查提交！")
	}
	if export && r.Export.Unmasked {
		r.Export.UnmaskReason = strings.TrimSpace(r.Export.UnmaskReason)
		switch n := len([]rune(r.Export.UnmaskReason)); {
		case !p.IsSuperuser:
			return nil, nil, forbid("仅超级用户可以选择导出原值（不脱敏）！")
		case !s.repo.ExportExtReady():
			return nil, nil, newErr(500, "export_ext_table", "导出原值需要先创建 dbt_sql_export_ext 表（见 deploy/sql/290_export_unmasked.sql），请联系管理员。")
		case !s.exportMaskingOn():
			return nil, nil, bad("当前导出不会做数据脱敏（系统脱敏开关没有开启），不需要选择导出原值")
		case n < unmaskReasonMin:
			return nil, nil, bad(fmt.Sprintf("导出原值必须填写理由（至少 %d 个字），审批人会看到", unmaskReasonMin))
		case n > unmaskReasonMax:
			return nil, nil, bad(fmt.Sprintf("导出原值的理由最多 %d 个字", unmaskReasonMax))
		}
	} else if export {
		r.Export.UnmaskReason = ""
	}
	if export && r.Export.MergeFiles {
		switch {
		case !batch:
			return nil, nil, bad("只有批量导出可以合并成一个文件")
		case !s.mergeReady():
			return nil, nil, newErr(500, "export_ext_table", "合并成一个文件需要先更新 dbt_sql_export_ext 表（重新执行 deploy/sql/290_export_unmasked.sql），请联系管理员。")
		}
	}
	if r.RunStart != nil && r.RunEnd != nil && !r.RunEnd.After(*r.RunStart) {
		return nil, nil, bad("可执行时间的结束时间必须晚于开始时间")
	}
	if r.PlanAt != nil {
		switch {
		case !r.PlanAt.After(s.now()):
			return nil, nil, bad("定时执行时间必须晚于当前时间")
		case r.RunStart != nil && r.PlanAt.Before(*r.RunStart), r.RunEnd != nil && r.PlanAt.After(*r.RunEnd):
			return nil, nil, bad("定时执行时间必须在可执行时间范围内")
		case !s.repo.ScheduleTableReady():
			return nil, nil, newErr(500, "schedule_table", "定时执行需要先创建 dbt_sql_schedule 表（见 deploy/sql/002_sql_schedule.sql），请联系管理员。")
		}
	}
	g, err := s.repo.ResourceGroupByName(r.GroupName)
	if err != nil {
		return nil, nil, err
	}
	if g == nil || g.IsDeleted != 0 {
		return nil, nil, bad(fmt.Sprintf("不存在该资源组：%s", r.GroupName))
	}
	if !pm.groups[g.GroupID] {
		return nil, nil, forbid("你不在该资源组内，无权向该组提交工单")
	}
	ids := r.BatchInstanceIDs
	if !batch {
		ids = []uint{r.InstanceID}
	}
	insts, err := s.resolveTargets(p, pm, g, ids, batch, export)
	if err != nil {
		return nil, nil, err
	}
	if !export && insts[0].DBType != dbTypeRedis { // 导出是只读的，没有 DDL/DML 之分；Redis 命令没有 DDL/DML 之分
		if err := checkGroupKind(g.GroupName, splitFor(insts[0].DBType, r.SQL)); err != nil {
			return nil, nil, err
		}
	}
	return g, insts, nil
}

// splitFor 按实例类型切分语句：SQL Server 按 GO 批与 T-SQL 规则，其余按 MySQL 规则。
func splitFor(dbType, sqlText string) []string {
	if dbType == dbTypeMSSQL {
		us := tsql.Units(sqlText)
		out := make([]string, len(us))
		for i, u := range us {
			out[i] = u.SQL
		}
		return out
	}
	return sqlx.Split(sqlText)
}

// checkGroupKind 校验 SQL 类型与所选资源组类型一致：组名含 DDL/DML 关键字时，DDL 语句必须选 DDL 组、DML 语句必须选 DML 组；
// 组名不含关键字时不做区分。逐条语句判定，DELETE 归为 DDL。
func checkGroupKind(groupName string, stmts []string) error {
	up := strings.ToUpper(groupName)
	isDDL, isDML := strings.Contains(up, "DDL"), strings.Contains(up, "DML")
	if !isDDL && !isDML {
		return nil
	}
	hasDDL, hasDML := false, false
	for _, st := range stmts {
		switch StatementType(st) {
		case SyntaxDDL:
			hasDDL = true
		case SyntaxDML:
			hasDML = true
		}
	}
	if hasDDL && !isDDL {
		return bad("您提交的是DDL语句，请选择DDL资源组！")
	}
	if hasDML && !isDML {
		return bad("您提交的是DML语句，请选择DML资源组！")
	}
	return nil
}

// InstanceCheck 是批量上线时对每个目标实例的预检结果（能连上、库存在）。
type InstanceCheck struct {
	ID    uint   `json:"id"`
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// precheckInstances 并发（最多 8 路）检查每个实例能否连接、目标库是否存在。批量上线提交前做，
// 避免“提交了才发现某个实例连不上”；失败的实例会列出，整张工单不允许提交。
func (s *Service) precheckInstances(ctx context.Context, insts []*model.Instance, db string) []InstanceCheck {
	lang := i18n.FromContext(ctx) // 预检结果只在本次响应里显示（不入库），按请求语言生成
	out := make([]InstanceCheck, len(insts))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, inst := range insts {
		wg.Add(1)
		go func(i int, inst *model.Instance) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = InstanceCheck{ID: inst.ID, Name: inst.InstanceName}
			e, err := s.open(inst.DBType, s.repo.EngineConn(inst))
			if err != nil {
				out[i].Error = i18n.T(lang, "无法连接：{err}", "err", err.Error())
				return
			}
			defer e.Close()
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			dbs, err := e.AllDatabases(cctx)
			if err != nil {
				out[i].Error = i18n.T(lang, "无法连接：{err}", "err", err.Error())
				return
			}
			for _, d := range dbs {
				if strings.EqualFold(d, db) {
					out[i].OK = true
					return
				}
			}
			out[i].Error = i18n.T(lang, "没有找到数据库 {db}", "db", db)
		}(i, inst)
	}
	wg.Wait()
	return out
}

func failedChecks(cs []InstanceCheck) string {
	var parts []string
	for _, c := range cs {
		if !c.OK {
			parts = append(parts, fmt.Sprintf("%s（%s）", c.Name, c.Error))
		}
	}
	return strings.Join(parts, "；")
}

// Preview 在提交前检查 SQL，返回逐条结果（页面的“检查”按钮）。
func (s *Service) Preview(ctx context.Context, p audit.Actor, instID uint, db, sqlText string, skip bool) (*CheckResult, error) {
	return s.previewTargets(ctx, p, []uint{instID}, false, false, db, sqlText, skip)
}

// PreviewBatch 批量上线的检查：语法与规则只在第一个实例上做，另外预检每个实例能否连接、库是否存在。
func (s *Service) PreviewBatch(ctx context.Context, p audit.Actor, instIDs []uint, db, sqlText string) (*CheckResult, error) {
	return s.previewTargets(ctx, p, dedupeIDs(instIDs), true, false, db, sqlText, false)
}

func (s *Service) previewTargets(ctx context.Context, p audit.Actor, ids []uint, batch, export bool, db, sqlText string, skip bool) (*CheckResult, error) {
	pm, err := s.loadPerms(p)
	if err != nil {
		return nil, err
	}
	switch {
	case export && batch && !pm.exportBatch:
		return nil, forbid("你没有提交批量数据导出工单的权限（缺少 export.submit_batch）")
	case export && !batch && !pm.exportSubmit:
		return nil, forbid("你没有提交数据导出工单的权限（缺少 export.submit）")
	case !export && batch && !pm.batchSubmit:
		return nil, forbid("你没有提交批量上线工单的权限（缺少 sqlworkflow.submit_batch）")
	case !export && !batch && !pm.submit:
		return nil, forbid("你没有提交 SQL 工单的权限")
	}
	if skip && !p.IsSuperuser {
		return nil, forbid("仅管理员可以跳过语法检查！")
	}
	if len(ids) == 0 {
		return nil, bad("请选择实例")
	}
	if len(ids) > maxBatchInstances {
		return nil, bad(fmt.Sprintf("单个批量工单最多支持 %d 个实例", maxBatchInstances))
	}
	var insts []*model.Instance
	var visible map[uint]bool
	if !pm.queryAll {
		if visible, err = s.repo.UserInstanceIDs(p.ID); err != nil {
			return nil, err
		}
	}
	for _, id := range ids {
		inst, err := s.repo.GetInstance(id)
		if err != nil {
			return nil, err
		}
		if inst == nil {
			return nil, bad("不存在该实例")
		}
		if !workflowSupports(inst.DBType) {
			return nil, bad(fmt.Sprintf("暂不支持 %s 类型的实例提交 SQL 工单", inst.DBType))
		}
		if err := redisModeError(inst, batch, export); err != nil {
			return nil, err
		}
		if batch && !s.batchTypes[inst.DBType] {
			return nil, bad(fmt.Sprintf("实例 %s 的类型是 %s，批量%s只支持：%s", inst.InstanceName, inst.DBType, kindWord(export), strings.Join(s.BatchTypes(), "、")))
		}
		if !pm.queryAll && !visible[inst.ID] {
			return nil, forbid(fmt.Sprintf("你所在组未关联实例 %s！", inst.InstanceName))
		}
		insts = append(insts, inst)
	}
	if db = strings.TrimSpace(db); db == "" {
		return nil, bad("请选择数据库")
	}
	if len([]rune(sqlText)) > maxSQLLength {
		return nil, bad("SQL 内容过长")
	}
	if export {
		return s.checkExport(ctx, p, insts, db, sqlText)
	}
	res, err := s.Check(ctx, insts[0], db, sqlText, skip)
	if err != nil {
		return nil, err
	}
	s.applyFirewall(ctx, p, insts, db, res)
	if res.Source == SourceRedis {
		s.redisAudit(p, "检查", redisCheckSummary(insts[0], normalizeRedisDB(db), res)) // i18n-ignore 审计动作名
	}
	if batch {
		res.Instances = s.precheckInstances(ctx, insts, db)
		if msg := failedChecks(res.Instances); msg != "" {
			res.Notice = strings.TrimSpace(res.Notice + " " + i18n.T(i18n.FromContext(ctx), "有实例无法使用：{msg}", "msg", msg))
		}
	}
	return res, nil
}

// Submit 提交工单：服务端重新检查（不信任页面的预览结果）、创建工单与审批流。
func (s *Service) Submit(ctx context.Context, p audit.Actor, req SubmitReq) (*SubmitResult, error) {
	pm, err := s.loadPerms(p)
	if err != nil {
		return nil, err
	}
	g, insts, err := s.validateSubmit(p, pm, &req)
	if err != nil {
		return nil, err
	}
	inst := insts[0]
	batch := len(req.BatchInstanceIDs) > 0
	var chk *CheckResult
	if req.Export != nil {
		cctx := ctx
		if req.Export.MergeFiles {
			cctx = withMergeCheck(ctx) // 合并成一个文件：服务端也核对各实例的列（不信任页面的预检）
		}
		chk, err = s.checkExport(cctx, p, insts, req.DBName, req.SQL)
		if err == nil && req.Export.MergeFiles && req.Export.Format == exportpkg.XLSX {
			var total int64
			for _, row := range chk.Rows {
				total += int64(row.AffectedRows)
			}
			if total+1 > exportpkg.MaxXLSXRows { // 加上表头一行
				return nil, bad(fmt.Sprintf("合并后的行数（约 %d）超过 Excel 的上限（%d 行），请改用 CSV 或缩小范围", total, exportpkg.MaxXLSXRows))
			}
		}
	} else {
		chk, err = s.Check(ctx, inst, req.DBName, req.SQL, req.SkipCheck)
		if err == nil {
			s.applyFirewall(ctx, p, insts, req.DBName, chk)
		}
	}
	if err != nil {
		return nil, err
	}
	if chk.Source == SourceRedis {
		// Redis：服务端再检查一次，只要有一行被拒绝就不允许提交（不走“自动审核不通过”那条路径，不产生工单）
		req.DBName = normalizeRedisDB(req.DBName)
		s.redisAudit(p, "提交检查", redisCheckSummary(inst, req.DBName, chk)) // i18n-ignore 审计动作名
		if chk.ErrorCount > 0 {
			return nil, bad(fmt.Sprintf("有命令被拒绝，不能提交：%s", firstError(&chk.Set)))
		}
	}
	if batch && req.Export == nil {
		// 批量上线：每个实例都要能连上、库存在，否则提交了也只会在执行时失败
		if msg := failedChecks(s.precheckInstances(ctx, insts, req.DBName)); msg != "" {
			return nil, bad(fmt.Sprintf("以下实例无法使用，请处理后再提交：%s", msg))
		}
	}
	if chk.Denied != "" {
		// 数据导出：语句涉及的库表没有查询权限，直接拒绝提交（不产生工单）；检查按钮会给出同样的错误行
		return nil, forbid(chk.Denied)
	}
	chk.Count()
	if chk.ErrorCount > 0 && !s.isAutoReject(&chk.Set) {
		// 配置为“错误不自动驳回”时，带错误的工单交给审批人决定
		slog.Info("工单带检查错误提交，进入人工审批", "user", p.Username)
	}

	// 备份：没有开启“备份开关”且检查来自 goInception 时强制备份；原生检查不支持备份。
	backup := req.IsBackup
	if chk.Source == SourceGoInception && !cfgBool(s.repo, settings.KeyReviewBackupEnabled) {
		backup = true
	}
	if chk.Source != SourceGoInception {
		backup = false
	}

	w := &model.SqlWorkflow{
		WorkflowName: req.Name, DemandURL: req.DemandURL, GroupID: g.GroupID, GroupName: g.GroupName,
		InstanceID: inst.ID, DBName: req.DBName, SyntaxType: chk.SyntaxType, IsBackup: backup,
		Engineer: p.Username, EngineerDisplay: p.Display, Status: model.WfManReviewing,
		RunDateStart: req.RunStart, RunDateEnd: req.RunEnd, CreateTime: s.now(),
	}
	if req.SkipCheck {
		w.IsManual = 1
	}
	if req.Export != nil {
		w.IsOfflineExport, w.IsBackup, w.SyntaxType = 1, false, SyntaxExport
		f := req.Export.Format
		w.ExportFormat = &f
	}
	if batch {
		ids := make([]string, len(insts))
		for i, in := range insts {
			ids[i] = strconv.FormatUint(uint64(in.ID), 10)
		}
		csv := strings.Join(ids, ",")
		w.BatchInstanceIDs = &csv
	}
	content := &model.SqlWorkflowContent{SQLContent: strings.TrimSpace(req.SQL), ReviewContent: chk.JSON(), ExecuteResult: ""}

	spec := audit.Spec{WorkflowType: audit.TypeSQLReview, GroupID: g.GroupID, GroupName: g.GroupName, Title: req.Name,
		CreateUser: p.Username, CreateUserDisplay: p.Display}
	spec.AutoReject = s.isAutoReject(&chk.Set)
	spec.AutoPass = !spec.AutoReject && s.isAutoPass(w, inst, chk)

	var au *model.WorkflowAudit
	err = s.repo.InTx(func(tx Repo) error {
		if err := tx.LockUser(p.Username); err != nil {
			return err
		}
		if err := tx.InsertWorkflow(w, content); err != nil {
			return err
		}
		if req.Export != nil && (req.Export.Unmasked || req.Export.MergeFiles) {
			// 导出扩展选项（导出原值、合并成一个文件）：与工单同一个事务，工单创建失败就不会留下这条记录
			ext := &model.SqlExportExt{WorkflowID: w.ID, MergeFiles: req.Export.MergeFiles, CreatedAt: s.now()}
			if req.Export.Unmasked {
				ext.Unmasked, ext.Reason, ext.SetBy = true, req.Export.UnmaskReason, p.Username
			}
			if err := tx.UpsertExportExt(ext); err != nil {
				return err
			}
		}
		spec.WorkflowID = w.ID
		a, err := audit.Create(tx, s.now(), spec)
		if err != nil {
			return err
		}
		au = a
		w.AuditAuthGroups = a.AuditAuthGroups
		switch a.CurrentStatus {
		case audit.StatusRejected:
			w.Status = model.WfAutoReviewWrong
		case audit.StatusPassed:
			w.Status = model.WfReviewPass
		}
		if req.PlanAt != nil && w.Status != model.WfAutoReviewWrong {
			// 提交时设定的定时：先把计划存进 dbt_sql_schedule（调度器对待审批的工单会跳过它）；
			// 不需要审批的工单（审核直接通过）当场转入“定时执行”，需要审批的在最后一级通过时转入（见 Pass）。
			sc := &model.SqlSchedule{WorkflowID: w.ID, RunAt: *req.PlanAt, CreatedBy: p.Username}
			if req.Export != nil {
				sc.Executor = ExecNative
			}
			if err := tx.UpsertSchedule(sc); err != nil {
				return err
			}
			if w.Status == model.WfReviewPass {
				w.Status = model.WfTimingTask
				if err := s.addLog(tx, a.AuditID, opSchedule, "定时执行", fmt.Sprintf("按提交时设定的计划转入定时执行，时间：%s", req.PlanAt.Format("2006-01-02 15:04")), &p); err != nil {
					return err
				}
			}
		}
		return tx.SaveWorkflow(w)
	})
	if err != nil {
		return nil, translate(err)
	}
	w.Instance = inst
	// 提示语每种情形各写一整句（带参数的用格式串），不拼接碎片：界面按整句查英文目录。
	exportMsg := req.Export != nil && w.Status == model.WfManReviewing
	res := &SubmitResult{WorkflowID: w.ID, Status: w.Status, Message: "工单已正常提交"}
	if exportMsg {
		res.Message = "导出工单已正常提交"
	}
	switch w.Status {
	case model.WfAutoReviewWrong:
		res.Message = "检查结果有错误，系统直接审核不通过，请按提示修改后重新提交"
		s.notifyAudit(w, au, "系统自动审核不通过："+firstError(&chk.Set)) // i18n-ignore 通知正文（语言属于接收方）
	case model.WfReviewPass:
		res.Message = "无需审批，系统直接审核通过"
		s.notifyAudit(w, au, "")
	case model.WfTimingTask:
		res.Message = fmt.Sprintf("无需审批，系统直接审核通过，已按设定转入定时执行：%s", req.PlanAt.Format("2006-01-02 15:04"))
		s.notifyAudit(w, au, "")
		s.notifyScheduled(w, au, *req.PlanAt)
	default:
		if req.PlanAt != nil {
			at := req.PlanAt.Format("2006-01-02 15:04")
			if exportMsg {
				res.Message = fmt.Sprintf("导出工单已正常提交，审批全部通过后将于 %s 自动定时执行", at)
			} else {
				res.Message = fmt.Sprintf("工单已正常提交，审批全部通过后将于 %s 自动定时执行", at)
			}
		}
		s.notifyAudit(w, au, "")
	}
	info := wfAuditInfo(w) + "，状态：" + StatusLabel(w.Status) // i18n-ignore 写进审计库的存档文本
	if req.SkipCheck {                                      // 跳过语法检查提交（仅超级用户，见 validateSubmit）：留痕，提示这张单只能原生执行
		info += "，跳过语法检查（未经 goInception / 规则检查，只能原生执行）" // i18n-ignore 写进审计库的存档文本
	}
	if req.Export != nil && req.Export.Unmasked { // 导出原值：留痕，写明理由
		info += "，选择导出原值（不脱敏），理由：" + truncate(req.Export.UnmaskReason, unmaskReasonMax) // i18n-ignore 写进审计库的存档文本
	}
	if req.Export != nil && req.Export.MergeFiles { // 合并成一个文件：留痕
		info += "，批量导出合并成一个文件" // i18n-ignore 写进审计库的存档文本
	}
	s.sysAudit(p, "提交", info) // i18n-ignore 审计动作名
	return res, nil
}

func firstError(r *review.Set) string {
	for _, row := range r.Rows {
		if row.ErrLevel == review.LevelError && row.ErrorMessage != "" && row.ErrorMessage != "None" {
			return row.ErrorMessage
		}
	}
	return "检查未通过"
}

// ---- 列表 ----

// ListQuery 是列表筛选（来自页面）。
type ListQuery struct {
	Status      string
	InstanceID  uint
	GroupID     uint
	SyntaxTypes []int
	Search      string // 工单名称 / 提交人 / 库 / 实例名；数字（可带 #）按工单号
	StartDate   string // YYYY-MM-DD
	EndDate     string
	Batch       string // "1" 只看批量上线工单，"0" 只看单实例工单，空不过滤
	Kind        string // "export" 只看数据导出工单，"change" 只看上线（变更）工单
	Tab         string // 页签：空=全部，mine=我提交的，approve=待我审批，exec=待我执行
	OrderBy     string // 排序键（白名单，见 NormalizeOrder）；空=按工单号倒序
	Desc        bool   // 排序方向（OrderBy 非空时有效）
	Offset      int
	Limit       int
}

// WorkflowItem 是列表行。
type WorkflowItem struct {
	ID                 uint       `json:"id"`
	Name               string     `json:"workflow_name"`
	Engineer           string     `json:"engineer_display"`
	EngineerName       string     `json:"engineer"` // 提交人登录名
	Status             string     `json:"status"`
	StatusLabel        string     `json:"status_label"`
	IsBackup           bool       `json:"is_backup"`
	CreateTime         time.Time  `json:"create_time"`
	InstanceName       string     `json:"instance_name"`
	DBName             string     `json:"db_name"`
	GroupID            uint       `json:"group_id"`
	GroupName          string     `json:"group_name"`
	SyntaxType         int        `json:"syntax_type"`
	SyntaxLabel        string     `json:"syntax_label"`
	CanBeExecuted      bool       `json:"can_execute"`
	BatchCount         int        `json:"batch_count,omitempty"` // 批量上线工单的实例数；单实例工单为 0
	IsExport           bool       `json:"is_export"`
	ExportFormat       string     `json:"export_format,omitempty"`
	Level              string     `json:"approval_level,omitempty"` // 审批进度：第 n / m 级 · 权限组
	WaitingForYour     bool       `json:"waiting_for_you"`          // 当前轮到我审批
	OSC                bool       `json:"osc,omitempty"`            // 含需要在线改表（pt-osc / gh-ost）的语句
	SkipCheck          bool       `json:"skip_check,omitempty"`     // 提交时跳过了语法检查
	ScheduleAt         *time.Time `json:"schedule_at,omitempty"`    // 定时执行时间（含提交时设定、审批通过后才生效的计划）
	ExportRows         *int64     `json:"export_rows,omitempty"`    // 导出工单：导出（或预计导出）的行数；仅导出列表计算
	ExportRowsEstimate bool       `json:"export_rows_estimate,omitempty"`
	ExportUnmasked     bool       `json:"export_unmasked,omitempty"` // 导出工单：提交人（超级用户）选择了导出原值（不脱敏）
}

// List 按用户权限返回可见的工单（管理员/审计员全部；有审批或组内执行权限的看所在资源组；其他人只看自己的）。
func (s *Service) List(p audit.Actor, q ListQuery) ([]WorkflowItem, int64, error) {
	pm, err := s.loadPerms(p)
	if err != nil {
		return nil, 0, err
	}
	return s.list(p, pm, q)
}

func (s *Service) list(p audit.Actor, pm perms, q ListQuery) ([]WorkflowItem, int64, error) {
	f := s.baseFilter(p, pm)
	f.Status, f.InstanceID, f.GroupID, f.SyntaxTypes = q.Status, q.InstanceID, q.GroupID, q.SyntaxTypes
	f.Search, f.Batch, f.Kind, f.Offset, f.Limit = strings.TrimSpace(q.Search), q.Batch, q.Kind, q.Offset, q.Limit
	f.OrderBy, f.Desc = NormalizeOrder(q.OrderBy), q.Desc
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 20
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	switch q.Tab {
	case TabMine:
		f.Engineer = p.Username
	case TabApprove, TabExec:
		ids, err := s.todoIDs(p, pm, q.Tab, normKind(q.Kind), q.Batch)
		if err != nil {
			return nil, 0, err
		}
		if len(ids) == 0 {
			return []WorkflowItem{}, 0, nil
		}
		f.IDs = ids
	}
	if q.StartDate != "" && q.EndDate != "" {
		st, err1 := time.ParseInLocation("2006-01-02", q.StartDate, time.Local)
		en, err2 := time.ParseInLocation("2006-01-02", q.EndDate, time.Local)
		if err1 != nil || err2 != nil {
			return nil, 0, bad("日期格式应为 YYYY-MM-DD")
		}
		e := en.AddDate(0, 0, 1)
		f.StartDate, f.EndDate = &st, &e
	}
	rows, total, err := s.repo.ListWorkflows(f)
	if err != nil {
		return nil, 0, err
	}
	out := make([]WorkflowItem, 0, len(rows))
	for _, r := range rows {
		it := WorkflowItem{ID: r.ID, Name: r.WorkflowName, Engineer: r.EngineerDisplay, EngineerName: r.Engineer, Status: r.Status, StatusLabel: StatusLabel(r.Status),
			IsBackup: r.IsBackup, CreateTime: r.CreateTime, InstanceName: r.InstanceName, DBName: r.DBName, GroupID: r.GroupID, GroupName: r.GroupName,
			SyntaxType: r.SyntaxType, SyntaxLabel: SyntaxLabel(r.SyntaxType), CanBeExecuted: pm.canExecute(p, &r.SqlWorkflow), SkipCheck: r.IsManual != 0}
		if r.IsOfflineExport != 0 {
			it.IsExport, it.ExportFormat = true, exportFormat(&r.SqlWorkflow)
		}
		if ids := parseBatchIDs(r.BatchInstanceIDs); len(ids) > 0 {
			it.BatchCount = len(ids)
			it.InstanceName = fmt.Sprintf("%s 等 %d 个实例", r.InstanceName, len(ids))
		}
		out = append(out, it)
	}
	s.decorate(p, rows, out, q.Kind)
	return out, total, nil
}

// ---- 提交页的选项 ----

// SubmitOptions 是提交页需要的下拉数据：用户所在资源组，以及每个资源组下可上线（can_write）的实例。
type SubmitOptions struct {
	Groups          []model.ResourceGroup
	Instances       map[uint][]InstanceOption // 资源组 ID -> 实例
	Inception       bool                      // 是否配置了 goInception
	Native          bool                      // 未配置 goInception 时是否允许降级为原生检查
	Batch           BatchOptions              // 批量上线
	Export          ExportOptions             // 数据导出
	ExportInstances map[uint][]InstanceOption // 资源组 ID -> 可导出（开启查询）的实例
}

// ExportOptions 描述数据导出能力。
type ExportOptions struct {
	Allowed      bool     `json:"allowed"`       // 能提交单实例导出
	BatchAllowed bool     `json:"batch_allowed"` // 能提交批量导出
	Formats      []Option `json:"formats"`
	MaxRows      int      `json:"max_rows"`
	Seconds      int      `json:"seconds"`
	// CanUnmask：当前用户能在提交时选择“导出原值（不脱敏）”——仅超级用户，且导出本来会脱敏、扩展表已创建。
	CanUnmask bool `json:"can_unmask"`
	// CanMerge：当前用户能在提交批量导出时选择“合并成一个文件”——有批量导出权限，且扩展表已有 merge_files 列。
	CanMerge bool `json:"can_merge"`
}

// BatchOptions 描述批量上线能力：当前用户有没有权限、允许的实例类型、实例数上限。
type BatchOptions struct {
	Allowed bool     `json:"allowed"`
	Types   []string `json:"types"`
	Max     int      `json:"max"`
}

type InstanceOption struct {
	ID     uint   `json:"id"`
	Name   string `json:"name"`
	DBType string `json:"db_type"`
}

func (s *Service) SubmitOptions(p audit.Actor) (*SubmitOptions, error) {
	pm, err := s.loadPerms(p)
	if err != nil {
		return nil, err
	}
	gs, err := s.repo.UserResourceGroups(p.ID, p.IsSuperuser)
	if err != nil {
		return nil, err
	}
	var visible map[uint]bool
	if !pm.queryAll {
		if visible, err = s.repo.UserInstanceIDs(p.ID); err != nil {
			return nil, err
		}
	}
	out := &SubmitOptions{Groups: gs, Instances: map[uint][]InstanceOption{}, Inception: s.InceptionConfigured(), Native: !s.requireInception,
		Batch:           BatchOptions{Allowed: pm.batchSubmit, Types: s.BatchTypes(), Max: maxBatchInstances},
		ExportInstances: map[uint][]InstanceOption{}}
	exportScopes := map[uint]bool{}
	maxRows, secs := s.exportCaps()
	out.Export = ExportOptions{Allowed: pm.exportSubmit, BatchAllowed: pm.exportBatch, MaxRows: maxRows, Seconds: secs,
		CanUnmask: p.IsSuperuser && s.exportMaskingOn() && s.repo.ExportExtReady(),
		CanMerge:  pm.exportBatch && s.mergeReady()}
	for _, f := range exportpkg.Formats {
		out.Export.Formats = append(out.Export.Formats, Option{Value: f, Label: exportpkg.FormatLabel(f)})
	}
	for _, g := range gs {
		list, err := s.repo.GroupInstances(g.GroupID)
		if err != nil {
			return nil, err
		}
		for i := range list {
			in := &list[i]
			if !workflowSupports(in.DBType) || (visible != nil && !visible[in.ID]) {
				continue
			}
			opt := InstanceOption{ID: in.ID, Name: in.InstanceName, DBType: in.DBType}
			if query.HasActiveTag(in, "can_write") {
				out.Instances[g.GroupID] = append(out.Instances[g.GroupID], opt)
			}
			if in.DBType != dbTypeRedis && query.HasReadTag(in) { // Redis 不能导出数据
				// 数据导出只列出用户有查询权限（库权限或表权限）的实例；超级用户 / 查询全部实例 / 资源组管理员不受限
				if ok, err := s.exportVisible(p, in, exportScopes); err != nil {
					return nil, err
				} else if !ok {
					continue
				}
				out.ExportInstances[g.GroupID] = append(out.ExportInstances[g.GroupID], opt)
			}
		}
	}
	return out, nil
}

// Databases 返回实例上可选的库（上线只允许选所在资源组实例上的库）。
func (s *Service) Databases(ctx context.Context, p audit.Actor, instID uint) ([]string, error) {
	pm, err := s.loadPerms(p)
	if err != nil {
		return nil, err
	}
	if !pm.submit && !pm.batchSubmit && !pm.exportSubmit && !pm.exportBatch {
		return nil, forbid("你没有提交 SQL 工单的权限")
	}
	inst, err := s.repo.GetInstance(instID)
	if err != nil {
		return nil, err
	}
	if inst == nil || !(query.HasActiveTag(inst, "can_write") || query.HasReadTag(inst)) {
		return nil, bad("不存在该实例或未开启上线/查询权限")
	}
	if !pm.queryAll {
		ids, err := s.repo.UserInstanceIDs(p.ID)
		if err != nil {
			return nil, err
		}
		if !ids[inst.ID] {
			return nil, forbid("你所在组未关联该实例！")
		}
	}
	e, err := s.open(inst.DBType, s.repo.EngineConn(inst))
	if err != nil {
		return nil, err
	}
	defer e.Close()
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	dbs, err := e.AllDatabases(cctx)
	if err == nil && inst.DBType == dbTypeRedis {
		dbs = redisDBNumbers(dbs) // Redis 的库是编号：工单里保存纯数字（0、1…），页面显示成 db0、db1…
	}
	return dbs, err
}
