// @author: kevinkelly
// @license: Apache Licence
// @file: list.go
// @time: 2026/10/28

package sqlflow

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"dbtool/internal/audit"
	"dbtool/internal/model"
	"dbtool/internal/review"
)

// 列表页的页签、排序、统计与待办计数。

// 列表页签。
const (
	TabAll     = ""        // 全部（可见范围内）
	TabMine    = "mine"    // 我提交的
	TabApprove = "approve" // 待我审批
	TabExec    = "exec"    // 待我执行
)

// maxTodoScan 是“待我审批 / 待我执行”最多扫描的工单数（取最新的）。这两类工单都是流程中间态，通常很少；
// 超过时只统计最新的这么多，避免一次请求扫全表。
const maxTodoScan = 500

// orderKeys 是列表允许的排序键（白名单）；其余值一律按默认的 id 倒序。
var orderKeys = map[string]bool{"id": true, "create_time": true, "status": true, "name": true, "instance": true, "db": true, "engineer": true, "group": true}

// NormalizeOrder 校验排序键：不在白名单里返回空（表示默认的 id 倒序）。
func NormalizeOrder(by string) string {
	by = strings.TrimSpace(by)
	if orderKeys[by] {
		return by
	}
	return ""
}

// SearchID 把搜索词识别成工单号：纯数字，或 #数字。
func SearchID(s string) (uint, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil || n == 0 {
		return 0, false
	}
	return uint(n), true
}

// auditCache 在一次请求里缓存审批判断用到的只读查询：列表上有几十行待审批工单时，权限、权限组与成员的查询只做一次。
// 非并发安全，只能在单个请求的单个 goroutine 里用。
type auditCache struct {
	audit.Repo
	perm  map[string]bool
	group map[uint]*model.AuthGroup
	in    map[[2]uint]bool
	users map[string][]model.User
}

func newAuditCache(r audit.Repo) *auditCache {
	return &auditCache{Repo: r, perm: map[string]bool{}, group: map[uint]*model.AuthGroup{}, in: map[[2]uint]bool{}, users: map[string][]model.User{}}
}

func (c *auditCache) HasButton(userID uint, code string) (bool, error) {
	k := strconv.Itoa(int(userID)) + "/" + code
	if v, ok := c.perm[k]; ok {
		return v, nil
	}
	v, err := c.Repo.HasButton(userID, code)
	if err == nil {
		c.perm[k] = v
	}
	return v, err
}

func (c *auditCache) AuthGroup(id uint) (*model.AuthGroup, error) {
	if g, ok := c.group[id]; ok {
		return g, nil
	}
	g, err := c.Repo.AuthGroup(id)
	if err == nil {
		c.group[id] = g
	}
	return g, err
}

func (c *auditCache) UserInAuthGroup(userID, groupID uint) (bool, error) {
	k := [2]uint{userID, groupID}
	if v, ok := c.in[k]; ok {
		return v, nil
	}
	v, err := c.Repo.UserInAuthGroup(userID, groupID)
	if err == nil {
		c.in[k] = v
	}
	return v, err
}

func (c *auditCache) AuthGroupUsers(names []string, rg uint) ([]model.User, error) {
	k := strings.Join(names, "\x00") + "/" + strconv.Itoa(int(rg))
	if v, ok := c.users[k]; ok {
		return v, nil
	}
	v, err := c.Repo.AuthGroupUsers(names, rg)
	if err == nil {
		c.users[k] = v
	}
	return v, err
}

// baseFilter 是用户的可见范围（管理员/审计员全部；能看组内工单 / 审批 / 驳回 / 组内执行的看所在资源组；其他人只看自己的）。
func (s *Service) baseFilter(p audit.Actor, pm perms) WorkflowFilter {
	var f WorkflowFilter
	switch {
	case p.IsSuperuser || pm.audit:
		f.Scope = ScopeAll
	case pm.groupView():
		f.Scope = ScopeGroups
		for id := range pm.groups {
			f.GroupIDs = append(f.GroupIDs, id)
		}
		sort.Slice(f.GroupIDs, func(i, j int) bool { return f.GroupIDs[i] < f.GroupIDs[j] })
	default:
		f.Scope, f.Username = ScopeOwn, p.Username
	}
	return f
}

// todoIDs 返回“待我审批 / 待我执行”的工单 ID（最新的在前，最多扫描 maxTodoScan 张）。
func (s *Service) todoIDs(p audit.Actor, pm perms, tab, kind, batch string) ([]uint, error) {
	f := s.baseFilter(p, pm)
	f.Kind, f.Batch = kind, batch
	f.OrderBy, f.Desc, f.Limit = "id", true, maxTodoScan
	var ids []uint
	switch tab {
	case TabApprove:
		if !pm.approve {
			return nil, nil
		}
		f.Status = model.WfManReviewing
		rows, _, err := s.repo.ListWorkflows(f)
		if err != nil {
			return nil, err
		}
		if p.IsSuperuser { // 超级用户能审批所有待审核的工单（审批引擎对超级用户直接放行），不用逐张查审批记录
			for _, r := range rows {
				ids = append(ids, r.ID)
			}
			break
		}
		ac := newAuditCache(s.repo)
		for _, r := range rows {
			au, err := s.repo.AuditByWorkflow(audit.TypeSQLReview, r.ID, false)
			if err != nil {
				return nil, err
			}
			if au != nil && audit.CanReview(ac, au, p) {
				ids = append(ids, r.ID)
			}
		}
	case TabExec:
		if !pm.execute && !pm.executeRG {
			return nil, nil
		}
		f.Statuses = []string{model.WfReviewPass, model.WfTimingTask}
		rows, _, err := s.repo.ListWorkflows(f)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if pm.canExecute(p, &r.SqlWorkflow) {
				ids = append(ids, r.ID)
			}
		}
	}
	return ids, nil
}

// TodoCounts 是侧栏角标、首页等用的待办数。
type TodoCounts struct {
	Approve int `json:"approve"` // 待我审批
	Exec    int `json:"exec"`    // 待我执行
}

// TodoCounts 返回当前用户的待办数（上线 + 导出工单合计）。
func (s *Service) TodoCounts(p audit.Actor) (TodoCounts, error) {
	pm, err := s.loadPerms(p)
	if err != nil {
		return TodoCounts{}, err
	}
	var out TodoCounts
	a, err := s.todoIDs(p, pm, TabApprove, "", "")
	if err != nil {
		return out, err
	}
	x, err := s.todoIDs(p, pm, TabExec, "", "")
	if err != nil {
		return out, err
	}
	out.Approve, out.Exec = len(a), len(x)
	return out, nil
}

// ListStats 是列表页顶部的状态卡片与页签计数。
type ListStats struct {
	ByStatus map[string]int64 `json:"by_status"`
	Total    int64            `json:"total"`
	Mine     int64            `json:"mine"`
	Approve  int64            `json:"approve"`
	Exec     int64            `json:"exec"`
}

func (s *Service) statusCounts(f WorkflowFilter) (map[string]int64, error) {
	if sc, ok := s.repo.(StatusCounter); ok {
		return sc.StatusCounts(f)
	}
	f.Status, f.Statuses, f.Limit, f.Offset = "", nil, 1, 0
	out := map[string]int64{}
	for _, o := range StatusOptions() {
		f.Status = o.Value
		_, n, err := s.repo.ListWorkflows(f)
		if err != nil {
			return nil, err
		}
		out[o.Value] = n
	}
	return out, nil
}

// Stats 返回可见范围内各状态的工单数与三个页签的计数。kind 为 "export" / "change" 时只统计该种类。
func (s *Service) Stats(p audit.Actor, kind string) (*ListStats, error) {
	pm, err := s.loadPerms(p)
	if err != nil {
		return nil, err
	}
	return s.stats(p, pm, kind, "")
}

func (s *Service) stats(p audit.Actor, pm perms, kind, batch string) (*ListStats, error) {
	f := s.baseFilter(p, pm)
	f.Kind, f.Batch = normKind(kind), batch
	by, err := s.statusCounts(f)
	if err != nil {
		return nil, err
	}
	st := &ListStats{ByStatus: by}
	for _, n := range by {
		st.Total += n
	}
	f.Engineer = p.Username
	mine, err := s.statusCounts(f)
	if err != nil {
		return nil, err
	}
	for _, n := range mine {
		st.Mine += n
	}
	a, err := s.todoIDs(p, pm, TabApprove, f.Kind, batch)
	if err != nil {
		return nil, err
	}
	x, err := s.todoIDs(p, pm, TabExec, f.Kind, batch)
	if err != nil {
		return nil, err
	}
	st.Approve, st.Exec = int64(len(a)), int64(len(x))
	return st, nil
}

func normKind(k string) string {
	if k == "export" || k == "change" {
		return k
	}
	return ""
}

// ---- 数据导出列表（独立页面）----

// ExportView 描述当前用户能看到哪些导出工单：export.view（单实例）/ export.view_batch（批量）。
type ExportView struct {
	Single bool // 能看单实例导出工单
	Batch  bool // 能看批量导出工单
}

func (v ExportView) any() bool { return v.Single || v.Batch }

// batchFilter 把可见性换算成列表的 Batch 过滤："0" 只看单实例，"1" 只看批量，空不过滤。
func (v ExportView) batchFilter() string {
	switch {
	case v.Single && !v.Batch:
		return "0"
	case v.Batch && !v.Single:
		return "1"
	}
	return ""
}

// ExportViewOf 返回用户的导出工单可见性（超级用户两者皆可）。
func (s *Service) ExportViewOf(p audit.Actor) (ExportView, error) {
	pm, err := s.loadPerms(p)
	if err != nil {
		return ExportView{}, err
	}
	return ExportView{Single: pm.menuExport, Batch: pm.menuExportB}, nil
}

// ListExports 是数据导出工单的独立列表：只含导出工单，并按 export.view / export.view_batch 过滤——
// 只有“查看导出工单”的人看不到批量导出工单，反之亦然；两者都没有则拒绝。
func (s *Service) ListExports(p audit.Actor, q ListQuery) ([]WorkflowItem, int64, error) {
	pm, err := s.loadPerms(p)
	if err != nil {
		return nil, 0, err
	}
	v := ExportView{Single: pm.menuExport, Batch: pm.menuExportB}
	if !v.any() {
		return nil, 0, forbid("你没有查看数据导出工单的权限（缺少 export.view / export.view_batch）")
	}
	q.Kind = "export"
	if bf := v.batchFilter(); bf != "" {
		q.Batch = bf // 受限时覆盖页面传来的值
	}
	return s.list(p, pm, q)
}

// ExportStats 是导出列表页的 KPI 与页签计数，口径同 ListExports。
func (s *Service) ExportStats(p audit.Actor) (*ListStats, error) {
	pm, err := s.loadPerms(p)
	if err != nil {
		return nil, err
	}
	v := ExportView{Single: pm.menuExport, Batch: pm.menuExportB}
	if !v.any() {
		return nil, forbid("你没有查看数据导出工单的权限（缺少 export.view / export.view_batch）")
	}
	return s.stats(p, pm, "export", v.batchFilter())
}

// ---- 列表行的补充信息 ----

func splitAuditIDs(csv string) []string { return splitComma(csv) }

// approvalLevel 返回审批进度文本。待审核的工单读审批记录得到“第 n / m 级 · 组名”，同时判断当前用户能否审批。
func (s *Service) approvalLevel(ac *auditCache, p audit.Actor, w *model.SqlWorkflow) (text string, waitingForMe bool) {
	n := len(splitAuditIDs(w.AuditAuthGroups))
	switch w.Status {
	case model.WfAutoReviewWrong:
		return "系统自动驳回", false
	case model.WfAbort:
		return "已终止", false
	case model.WfManReviewing:
		au, err := s.repo.AuditByWorkflow(audit.TypeSQLReview, w.ID, false)
		if err != nil || au == nil {
			return "待审批", false
		}
		ids := splitAuditIDs(au.AuditAuthGroups)
		for i, id := range ids {
			if id == au.CurrentAudit {
				name := id
				if gid, err := strconv.ParseUint(id, 10, 32); err == nil {
					if g, _ := ac.AuthGroup(uint(gid)); g != nil {
						name = g.Name
					}
				}
				return fmt.Sprintf("第 %d / %d 级 · %s", i+1, len(ids), name), audit.CanReview(ac, au, p)
			}
		}
		return "待审批", audit.CanReview(ac, au, p)
	}
	if n == 0 {
		return "无需审批", false
	}
	return fmt.Sprintf("%d 级审批完成", n), false
}

// rowsAffected 汇总结果行里的影响行数（导出工单即导出行数）；失败的行不计，批量汇总行不计。
func rowsAffected(rows []review.Row) (total int64, any bool) {
	for _, r := range rows {
		if r.IsSummary || r.ErrLevel == review.LevelError {
			continue
		}
		total += int64(r.AffectedRows)
		any = true
	}
	return total, any
}

// hasOSC 检查结果里是否有需要在线改表（pt-osc / gh-ost）的语句：goInception 对这类语句会返回 sqlsha1。
func hasOSC(rows []review.Row) bool {
	for _, r := range rows {
		if strings.TrimSpace(r.SQLSha1) != "" && strings.ToLower(strings.TrimSpace(r.SQLSha1)) != "none" {
			return true
		}
	}
	return false
}

// decorate 给列表行补上审批级别、待我审批、OSC / 定时 / 导出行数等信息。每项都是尽力而为：取不到就留空，不影响列表本身。
func (s *Service) decorate(p audit.Actor, rows []WorkflowRow, items []WorkflowItem, kind string) {
	ac := newAuditCache(s.repo)
	if ld, ok := s.repo.(ExportUnmaskedLoader); ok { // 导出原值：一次查出这一页里选了它的导出工单，列表上标出来
		var ids []uint
		for i := range rows {
			if rows[i].IsOfflineExport != 0 {
				ids = append(ids, rows[i].ID)
			}
		}
		if len(ids) > 0 {
			if m, err := ld.ExportUnmaskedIDs(ids); err == nil {
				for i := range rows {
					items[i].ExportUnmasked = m[rows[i].ID]
				}
			}
		}
	}
	schedOK := false
	needSched := false
	for i := range rows {
		if rows[i].Status == model.WfTimingTask || rows[i].Status == model.WfManReviewing {
			needSched = true
			break
		}
	}
	if needSched {
		schedOK = s.repo.ScheduleTableReady()
	}
	for i := range rows {
		w, it := &rows[i].SqlWorkflow, &items[i]
		it.Level, it.WaitingForYour = s.approvalLevel(ac, p, w)
		if schedOK && (w.Status == model.WfTimingTask || w.Status == model.WfManReviewing) {
			if sc, err := s.repo.GetSchedule(w.ID); err == nil && sc != nil {
				at := sc.RunAt
				it.ScheduleAt = &at
			}
		}
		switch {
		case w.IsOfflineExport != 0:
			if kind != "export" {
				continue
			}
			c, err := s.repo.GetContent(w.ID)
			if err != nil || c == nil {
				continue
			}
			raw, est := c.ExecuteResult, false
			if w.Status != model.WfFinish && w.Status != model.WfException {
				raw, est = c.ReviewContent, true
			}
			if rs, err := review.ParseRows(raw); err == nil {
				if n, ok := rowsAffected(rs); ok {
					it.ExportRows, it.ExportRowsEstimate = &n, est
				}
			}
		case w.SyntaxType == SyntaxDDL:
			if c, err := s.repo.GetContent(w.ID); err == nil && c != nil {
				if rs, err := review.ParseRows(c.ReviewContent); err == nil {
					it.OSC = hasOSC(rs)
				}
			}
		}
	}
}

// ListOptions 是列表页筛选下拉用的数据：当前用户可见范围内的资源组与实例。
type ListOptions struct {
	Groups    []model.ResourceGroup
	Instances []InstanceOption
}

// ListFilterOptions 返回列表筛选用的资源组与实例（超级用户 / 审计员 / 可查询全部实例的人看到全部，其余人只看自己资源组关联的）。
func (s *Service) ListFilterOptions(p audit.Actor) (*ListOptions, error) {
	pm, err := s.loadPerms(p)
	if err != nil {
		return nil, err
	}
	gs, err := s.repo.UserResourceGroups(p.ID, p.IsSuperuser || pm.audit)
	if err != nil {
		return nil, err
	}
	all, err := s.repo.ListInstances()
	if err != nil {
		return nil, err
	}
	var visible map[uint]bool
	if !(p.IsSuperuser || pm.audit || pm.queryAll) {
		if visible, err = s.repo.UserInstanceIDs(p.ID); err != nil {
			return nil, err
		}
	}
	out := &ListOptions{Groups: gs}
	for _, in := range all {
		if visible != nil && !visible[in.ID] {
			continue
		}
		out.Instances = append(out.Instances, InstanceOption{ID: in.ID, Name: in.InstanceName, DBType: in.DBType})
	}
	sort.Slice(out.Instances, func(i, j int) bool { return out.Instances[i].Name < out.Instances[j].Name })
	return out, nil
}
