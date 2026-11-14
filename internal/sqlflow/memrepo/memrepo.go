// @author: kevinkelly
// @license: Apache Licence
// @file: memrepo.go
// @time: 2026/10/28

// Package memrepo 是 SQL 工单数据访问的内存实现：在权限申请的内存仓库之上增加工单、内容与定时计划，带事务快照回滚。
// 仅用于单元测试和本地演示（cmd/demo），不在生产代码路径里使用。
package memrepo

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dbtool/internal/btnperm"
	"dbtool/internal/engine"
	"dbtool/internal/model"
	"dbtool/internal/privapply"
	base "dbtool/internal/privapply/memrepo"
	"dbtool/internal/sqlflow"
)

// Repo 在 base.Repo 上叠加 SQL 工单的数据。
type Repo struct {
	*base.Repo

	Workflows map[uint]model.SqlWorkflow
	Contents  map[uint]model.SqlWorkflowContent
	Schedules map[uint]model.SqlSchedule
	NoSchedTb bool // 模拟尚未创建 dbt_sql_schedule 表

	ExportExts        map[uint]model.SqlExportExt // 数据导出扩展（模拟 dbt_sql_export_ext）
	NoExportExtTb     bool                        // 模拟尚未创建 dbt_sql_export_ext 表
	NoExportMergeCols bool                        // 模拟 dbt_sql_export_ext 是旧版脚本建的（没有 merge_files 列）

	InstEnv   map[uint]string                    // 实例环境（模拟 dbt_instance_ext.env）
	NoInstExt bool                               // 模拟尚未创建 dbt_instance_ext 表
	EnvCalls  int                                // InstanceEnvs 被调用的次数（断言不出现 N+1）
	MaskCols  map[uint][]model.DataMaskingColumn // 实例的脱敏字段配置（模拟 dbt_masking_policy）

	FailSaveContent error // 注入故障：保存执行结果失败
	AuditEntries    []model.AuditEntry
	LifecycleAudits []model.AuditEntry // 审批类动作的系统审计（audit.Lifecycle）；与 AuditEntries 分开，不影响下载 / 回滚语句审计的断言
	FailLifecycle   error              // 注入故障：写审批审计失败
	nextID          uint

	lockMu  sync.Mutex
	locks   map[string]bool // 假的 GET_LOCK：名字 -> 是否被占用
	LockErr error           // 注入故障：取锁出错
}

func New(b *base.Repo) *Repo {
	return &Repo{Repo: b, Workflows: map[uint]model.SqlWorkflow{}, Contents: map[uint]model.SqlWorkflowContent{}, Schedules: map[uint]model.SqlSchedule{}}
}

var (
	_ sqlflow.Repo       = (*Repo)(nil)
	_ sqlflow.PermLister = (*Repo)(nil)
	_ sqlflow.Locker     = (*Repo)(nil)

	_ sqlflow.ExportUnmaskedLoader = (*Repo)(nil)
	_ sqlflow.ExportMergeGate      = (*Repo)(nil)

	_ sqlflow.InstanceEnvLoader = (*Repo)(nil)
)

// InstanceEnvs 返回一批实例的环境；NoInstExt 时返回空集合。
func (r *Repo) InstanceEnvs(ids []uint) (map[uint]string, error) {
	r.EnvCalls++
	out := map[uint]string{}
	if r.NoInstExt {
		return out, nil
	}
	for _, id := range ids {
		if e := r.InstEnv[id]; e != "" {
			out[id] = e
		}
	}
	return out, nil
}

// AllowedButtons 一次取出用户的全部按钮权限码：Perms 里放按钮码，加上对所有人开放的基础按钮（登记表里标了 Open）。
func (r *Repo) AllowedButtons(userID uint) (map[string]bool, error) {
	out := map[string]bool{}
	for _, code := range btnperm.BaselineButtons() {
		out[code] = true
	}
	for c, ok := range r.Perms[userID] {
		if ok {
			out[c] = true
		}
	}
	return out, nil
}

func (r *Repo) InTx(fn func(sqlflow.Repo) error) error {
	ws := map[uint]model.SqlWorkflow{}
	for k, v := range r.Workflows {
		ws[k] = v
	}
	cs := map[uint]model.SqlWorkflowContent{}
	for k, v := range r.Contents {
		cs[k] = v
	}
	ss := map[uint]model.SqlSchedule{}
	for k, v := range r.Schedules {
		ss[k] = v
	}
	next := r.nextID
	err := r.Repo.InTx(func(privapply.Repo) error { return fn(r) })
	if err != nil {
		r.Workflows, r.Contents, r.Schedules, r.nextID = ws, cs, ss, next
	}
	return err
}

// ---- 实例 ----

func (r *Repo) GetInstance(id uint) (*model.Instance, error) {
	for i := range r.Instances {
		if r.Instances[i].ID == id {
			in := r.Instances[i]
			return &in, nil
		}
	}
	return nil, nil
}

func (r *Repo) EngineConn(inst *model.Instance) engine.Conn {
	return engine.Conn{Host: inst.Host, Port: inst.Port, User: "u", Password: "p", DBName: inst.DBName}
}

func (r *Repo) GroupInstances(groupID uint) ([]model.Instance, error) {
	var out []model.Instance
	for _, in := range r.Instances {
		for _, g := range r.InstRGs[in.ID] {
			if g == groupID {
				out = append(out, in)
			}
		}
	}
	return out, nil
}

// AuditByWorkflow 与 store.AuditByWorkflow 一致：SQL 上线类审批记录带出“是否数据导出工单”（model.WorkflowAudit.IsExport）。
func (r *Repo) AuditByWorkflow(t int, id uint, forUpdate bool) (*model.WorkflowAudit, error) {
	a, err := r.Repo.AuditByWorkflow(t, id, forUpdate)
	if a != nil && t == 2 {
		a.IsExport = r.Workflows[id].IsOfflineExport != 0
	}
	return a, err
}

// ---- 工单 ----

func (r *Repo) withInstance(w model.SqlWorkflow) *model.SqlWorkflow {
	if in, _ := r.GetInstance(w.InstanceID); in != nil {
		w.Instance = in
	}
	return &w
}

func (r *Repo) InsertWorkflow(w *model.SqlWorkflow, c *model.SqlWorkflowContent) error {
	r.nextID++
	w.ID = r.nextID
	if w.CreateTime.IsZero() {
		w.CreateTime = time.Now()
	}
	r.Workflows[w.ID] = *w
	c.WorkflowID, c.ID = w.ID, w.ID
	r.Contents[w.ID] = *c
	return nil
}

func (r *Repo) GetWorkflow(id uint, _ bool) (*model.SqlWorkflow, error) {
	w, ok := r.Workflows[id]
	if !ok {
		return nil, nil
	}
	return r.withInstance(w), nil
}

func (r *Repo) SaveWorkflow(w *model.SqlWorkflow) error {
	c := *w
	c.Instance = nil
	r.Workflows[w.ID] = c
	return nil
}

func (r *Repo) GetContent(id uint) (*model.SqlWorkflowContent, error) {
	c, ok := r.Contents[id]
	if !ok {
		return nil, nil
	}
	return &c, nil
}

func (r *Repo) SaveContent(c *model.SqlWorkflowContent) error {
	if r.FailSaveContent != nil {
		return r.FailSaveContent
	}
	r.Contents[c.WorkflowID] = *c
	return nil
}

func (r *Repo) ListWorkflows(f sqlflow.WorkflowFilter) ([]sqlflow.WorkflowRow, int64, error) {
	rows := r.filtered(f)
	switch f.OrderBy {
	case "", "id":
		desc := f.OrderBy == "" || f.Desc
		sort.Slice(rows, func(i, j int) bool {
			if desc {
				return rows[i].ID > rows[j].ID
			}
			return rows[i].ID < rows[j].ID
		})
	default:
		key := func(w sqlflow.WorkflowRow) string {
			switch f.OrderBy {
			case "create_time":
				return w.CreateTime.Format("20060102150405.000000000")
			case "status":
				return w.Status
			case "name":
				return w.WorkflowName
			case "instance":
				return w.InstanceName
			case "db":
				return w.DBName
			case "engineer":
				return w.Engineer
			case "group":
				return w.GroupName
			}
			return ""
		}
		sort.Slice(rows, func(i, j int) bool {
			a, b := key(rows[i]), key(rows[j])
			if a != b {
				return (a < b) != f.Desc
			}
			return rows[i].ID > rows[j].ID
		})
	}
	total := int64(len(rows))
	if f.Offset > len(rows) {
		f.Offset = len(rows)
	}
	rows = rows[f.Offset:]
	if f.Limit > 0 && f.Limit < len(rows) {
		rows = rows[:f.Limit]
	}
	return rows, total, nil
}

// StatusCounts 按状态统计（忽略 Status / Statuses / 排序 / 分页）。
func (r *Repo) StatusCounts(f sqlflow.WorkflowFilter) (map[string]int64, error) {
	f.Status, f.Statuses = "", nil
	out := map[string]int64{}
	for _, w := range r.filtered(f) {
		out[w.Status]++
	}
	return out, nil
}

// DownloadCount 统计导出文件的成功下载次数（dbt_audit_log 里“离线下载”且不带 error 的记录）。
func (r *Repo) DownloadCount(id uint) (int, error) {
	prefix := fmt.Sprintf("工单id：%d，文件：", id)
	n := 0
	for _, e := range r.AuditEntries {
		if e.Action == "离线下载" && strings.HasPrefix(e.ExtraInfo, prefix) && !strings.Contains(e.ExtraInfo, "error:") {
			n++
		}
	}
	return n, nil
}

// filtered 按筛选条件取出符合的工单（未排序、未分页）。
func (r *Repo) filtered(f sqlflow.WorkflowFilter) []sqlflow.WorkflowRow {
	var rows []sqlflow.WorkflowRow
	for _, w := range r.Workflows {
		if f.Engineer != "" && w.Engineer != f.Engineer {
			continue
		}
		if len(f.IDs) > 0 {
			in := false
			for _, id := range f.IDs {
				in = in || id == w.ID
			}
			if !in {
				continue
			}
		}
		if len(f.Statuses) > 0 {
			in := false
			for _, st := range f.Statuses {
				in = in || st == w.Status
			}
			if !in {
				continue
			}
		}
		switch f.Scope {
		case sqlflow.ScopeOwn:
			if w.Engineer != f.Username {
				continue
			}
		case sqlflow.ScopeGroups:
			in := false
			for _, g := range f.GroupIDs {
				in = in || g == w.GroupID
			}
			if !in {
				continue
			}
		}
		if f.Status != "" && w.Status != f.Status || f.GroupID != 0 && w.GroupID != f.GroupID {
			continue
		}
		if f.Kind == "export" && w.IsOfflineExport == 0 || f.Kind == "change" && w.IsOfflineExport != 0 {
			continue
		}
		isBatch := w.BatchInstanceIDs != nil && *w.BatchInstanceIDs != ""
		if f.Batch == "1" && !isBatch || f.Batch == "0" && isBatch {
			continue
		}
		if f.InstanceID != 0 {
			hit := w.InstanceID == f.InstanceID
			if isBatch && f.Batch != "0" {
				for _, tok := range strings.Split(*w.BatchInstanceIDs, ",") {
					hit = hit || tok == strconv.Itoa(int(f.InstanceID))
				}
			}
			if !hit {
				continue
			}
		}
		if len(f.SyntaxTypes) > 0 {
			ok := false
			for _, t := range f.SyntaxTypes {
				ok = ok || t == w.SyntaxType
			}
			if !ok {
				continue
			}
		}
		if f.StartDate != nil && w.CreateTime.Before(*f.StartDate) || f.EndDate != nil && !w.CreateTime.Before(*f.EndDate) {
			continue
		}
		row := sqlflow.WorkflowRow{SqlWorkflow: w}
		if in, _ := r.GetInstance(w.InstanceID); in != nil {
			row.InstanceName = in.InstanceName
		}
		if f.Search != "" {
			hit := false
			for _, s := range []string{w.EngineerDisplay, w.WorkflowName, w.Engineer, w.DBName, row.InstanceName} {
				hit = hit || strings.Contains(s, f.Search)
			}
			if id, ok := sqlflow.SearchID(f.Search); ok && id == w.ID {
				hit = true
			}
			if !hit {
				continue
			}
		}
		rows = append(rows, row)
	}
	return rows
}

func (r *Repo) ExecutingDDL(db string, exclude uint) ([]model.SqlWorkflow, error) {
	var out []model.SqlWorkflow
	for _, w := range r.Workflows {
		if w.Status == model.WfExecuting && w.DBName == db && w.SyntaxType == sqlflow.SyntaxDDL && w.ID != exclude {
			out = append(out, w)
		}
	}
	return out, nil
}

// ---- 定时计划 ----

func (r *Repo) ScheduleTableReady() bool { return !r.NoSchedTb }

func (r *Repo) UpsertSchedule(s *model.SqlSchedule) error { r.Schedules[s.WorkflowID] = *s; return nil }

func (r *Repo) GetSchedule(id uint) (*model.SqlSchedule, error) {
	if s, ok := r.Schedules[id]; ok {
		return &s, nil
	}
	return nil, nil
}

func (r *Repo) DeleteSchedule(id uint) error { delete(r.Schedules, id); return nil }

func (r *Repo) ClaimSchedule(id uint) (bool, error) {
	_, ok := r.Schedules[id]
	delete(r.Schedules, id)
	return ok, nil
}

func (r *Repo) DueSchedules(now time.Time, limit int) ([]model.SqlSchedule, error) {
	var out []model.SqlSchedule
	for _, s := range r.Schedules {
		if !s.RunAt.After(now) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunAt.Before(out[j].RunAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *Repo) InsertAuditEntry(e *model.AuditEntry) error {
	r.AuditEntries = append(r.AuditEntries, *e)
	return nil
}

func (r *Repo) WriteLifecycleAudit(e *model.AuditEntry) error {
	if r.FailLifecycle != nil {
		return r.FailLifecycle
	}
	r.LifecycleAudits = append(r.LifecycleAudits, *e)
	return nil
}

// TryLock 是假的 GET_LOCK：同名锁被占用时返回 ok=false。测试用 Hold 模拟“别的进程正在执行”。
func (r *Repo) TryLock(_ context.Context, name string) (func(), bool, error) {
	if r.LockErr != nil {
		return nil, false, r.LockErr
	}
	r.lockMu.Lock()
	defer r.lockMu.Unlock()
	if r.locks == nil {
		r.locks = map[string]bool{}
	}
	if r.locks[name] {
		return nil, false, nil
	}
	r.locks[name] = true
	return func() {
		r.lockMu.Lock()
		delete(r.locks, name)
		r.lockMu.Unlock()
	}, true, nil
}

// Hold 占住某把锁（模拟另一个进程在执行），返回释放函数。
func (r *Repo) Hold(name string) func() {
	rel, ok, _ := r.TryLock(context.Background(), name)
	if !ok {
		return func() {}
	}
	return rel
}

// Locked 报告某把锁当前是否被占用。
func (r *Repo) Locked(name string) bool {
	r.lockMu.Lock()
	defer r.lockMu.Unlock()
	return r.locks[name]
}

// ActiveMaskingColumns 返回实例的脱敏字段配置（工单文本脱敏用）。
func (r *Repo) ActiveMaskingColumns(instanceID uint) ([]model.DataMaskingColumn, error) {
	return r.MaskCols[instanceID], nil
}

// ---- 数据导出扩展：导出原值（不脱敏）----

func (r *Repo) ExportExtReady() bool { return !r.NoExportExtTb }

// ExportMergeReady 实现 sqlflow.ExportMergeGate。
func (r *Repo) ExportMergeReady() bool { return !r.NoExportExtTb && !r.NoExportMergeCols }

func (r *Repo) UpsertExportExt(e *model.SqlExportExt) error {
	if r.ExportExts == nil {
		r.ExportExts = map[uint]model.SqlExportExt{}
	}
	v := *e
	if r.NoExportMergeCols { // 旧表没有这一列：与真实仓库一样不写
		v.MergeFiles = false
	}
	r.ExportExts[e.WorkflowID] = v
	return nil
}

func (r *Repo) GetExportExt(id uint) (*model.SqlExportExt, error) {
	if r.NoExportExtTb {
		return nil, nil
	}
	if e, ok := r.ExportExts[id]; ok {
		return &e, nil
	}
	return nil, nil
}

func (r *Repo) ExportUnmaskedIDs(ids []uint) (map[uint]bool, error) {
	out := map[uint]bool{}
	for _, id := range ids {
		if e, ok := r.ExportExts[id]; ok && e.Unmasked && !r.NoExportExtTb {
			out[id] = true
		}
	}
	return out, nil
}

func (r *Repo) UserIsSuperuser(username string) (bool, error) {
	for _, u := range r.Users {
		if u.Username == username {
			return u.IsActive && u.IsSuperuser, nil
		}
	}
	return false, nil
}
