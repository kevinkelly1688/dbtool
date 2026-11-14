// @author: kevinkelly
// @license: Apache Licence
// @file: list.go
// @time: 2026/10/28

package privapply

import (
	"strconv"
	"strings"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/model"
)

// 申请列表的页签。
const (
	TabMine = "mine" // 我的申请
	TabTodo = "todo" // 待我审批
	TabAll  = "all"  // 全部（我能看到的）
)

// ListQuery 是权限申请列表的查询条件。
type ListQuery struct {
	Tab    string // mine / todo / all；空串=按可见范围列出（旧行为）
	Status string // 见 ApplyFilter.Status
	Search string // 编号、申请人、实例、库、表、标题
	Offset int
	Limit  int
}

// Progress 是审批进度（待审批的申请才有）。
type Progress struct {
	Level   int    // 当前第几级（从 1 开始）
	Total   int    // 共几级
	Current string // 当前级的权限组名
}

// Counts 是各页签的数量。
type Counts struct {
	Mine int64
	Todo int64
	All  int64
}

// ListView 是新版列表的返回：在 ListResult 之外带页签信息。
type ListView struct {
	ListResult
	Tab      string // 实际生效的页签
	Counts   Counts
	CanTodo  bool // 有审批权限，显示“待我审批”页签
	CanAll   bool // 能看到别人的申请，显示“全部”页签
	Statuses []StatusOpt
}

// StatusOpt 是状态筛选项。
type StatusOpt struct{ Value, Label string }

// StatusOptions 是状态筛选下拉的选项。
func StatusOptions() []StatusOpt {
	return []StatusOpt{{"0", "待审核"}, {"1", "审核通过"}, {"2", "审核不通过"}, {"3", "审核取消"}, {"expired", "已过期"}}
}

func validStatus(s string) bool {
	switch s {
	case "", "0", "1", "2", "3", "expired":
		return true
	}
	return false
}

// MatchApply 判断申请是否满足状态筛选（内存实现与测试用；SQL 实现见 store）。
func (f ApplyFilter) MatchApply(a *model.QueryPrivilegesApply) bool {
	now := f.Today
	if now.IsZero() {
		now = time.Now()
	}
	if f.MineOnly != "" && a.UserName != f.MineOnly {
		return false
	}
	switch f.Status {
	case "":
	case "expired":
		if EffStatus(a.Status, a.ValidDate, now) != StatusExpired {
			return false
		}
	default:
		n, err := strconv.Atoi(f.Status)
		if err != nil || EffStatus(a.Status, a.ValidDate, now) != n {
			return false
		}
	}
	if f.Search != "" {
		q := strings.TrimPrefix(strings.ToLower(f.Search), "#")
		inst := ""
		if a.Instance != nil {
			inst = a.Instance.InstanceName
		}
		hay := strings.ToLower(strings.Join([]string{strconv.Itoa(int(a.ApplyID)), a.Title, a.UserName, a.UserDisplay, a.DBList, a.TableList, inst}, "\n"))
		if !strings.Contains(hay, q) {
			return false
		}
	}
	return true
}

// ListApplies 是权限申请列表页用的查询：页签（我的 / 待我审批 / 全部）、状态筛选、审批进度、可撤回标记。
// 可见范围同 List（管理员全部；有审批权限的看所在资源组内的；其他人只看自己的）。
func (s *Service) ListApplies(p audit.Actor, q ListQuery) (*ListView, error) {
	base, err := s.applyScope(p)
	if err != nil {
		return nil, err
	}
	review, err := s.has(p, permReview)
	if err != nil {
		return nil, err
	}
	if !validStatus(q.Status) {
		q.Status = ""
	}
	v := &ListView{Statuses: StatusOptions(), CanTodo: review, CanAll: base.Scope != ScopeOwn}
	now := s.now()
	base.Today = today(now)
	base.Search = strings.TrimSpace(q.Search)

	// 各页签的数量：不受搜索 / 状态筛选影响。
	count := func(f ApplyFilter) (int64, error) {
		f.Offset, f.Limit = 0, 1
		f.Search, f.Status = "", ""
		_, n, err := s.repo.ListApplies(f)
		return n, err
	}
	mineF := ApplyFilter{Scope: ScopeOwn, UserName: p.Username, MineOnly: p.Username, Today: base.Today}
	if v.Counts.Mine, err = count(mineF); err != nil {
		return nil, err
	}
	if v.Counts.All, err = count(base); err != nil {
		return nil, err
	}
	var todo []model.QueryPrivilegesApply
	if review {
		if todo, err = s.todoApplies(p, base); err != nil {
			return nil, err
		}
		v.Counts.Todo = int64(len(todo))
	}

	tab := q.Tab
	switch tab {
	case TabMine:
	case TabTodo:
		if !review {
			tab = TabMine
		}
	case TabAll:
		if !v.CanAll {
			tab = TabMine
		}
	default:
		tab = TabMine
		if v.Counts.Mine == 0 && v.CanAll {
			tab = TabAll
			if review && v.Counts.Todo > 0 {
				tab = TabTodo
			}
		}
	}
	v.Tab = tab

	var rows []model.QueryPrivilegesApply
	switch tab {
	case TabTodo:
		for i := range todo {
			if (ApplyFilter{Search: base.Search, Status: q.Status, Today: base.Today}).MatchApply(&todo[i]) {
				rows = append(rows, todo[i])
			}
		}
		v.Total = int64(len(rows))
		rows = pageOf(rows, q.Offset, q.Limit)
	default:
		f := base
		if tab == TabMine {
			f = mineF
			f.Search = base.Search
		}
		f.Status, f.Offset, f.Limit = q.Status, q.Offset, q.Limit
		if rows, v.Total, err = s.repo.ListApplies(f); err != nil {
			return nil, err
		}
	}
	if v.Rows, err = s.rowsOf(p, rows, now, tab == TabTodo); err != nil {
		return nil, err
	}
	return v, nil
}

func pageOf(rows []model.QueryPrivilegesApply, off, lim int) []model.QueryPrivilegesApply {
	if off > 0 {
		if off >= len(rows) {
			return nil
		}
		rows = rows[off:]
	}
	if lim > 0 && len(rows) > lim {
		rows = rows[:lim]
	}
	return rows
}

// todoApplies 返回当前用户能审批的待审批申请（新的在前）。
func (s *Service) todoApplies(p audit.Actor, base ApplyFilter) ([]model.QueryPrivilegesApply, error) {
	f := base
	f.Status, f.Search, f.Offset, f.Limit = "0", "", 0, 0
	list, _, err := s.repo.ListApplies(f)
	if err != nil {
		return nil, err
	}
	var out []model.QueryPrivilegesApply
	for i := range list {
		au, err := s.repo.AuditByWorkflow(audit.TypeQueryPrivilege, list[i].ApplyID, false)
		if err != nil {
			return nil, err
		}
		if au != nil && audit.CanReview(s.repo, au, p) {
			out = append(out, list[i])
		}
	}
	return out, nil
}

// rowsOf 把申请转成列表行：显示状态、审批进度、申请理由、可撤回。
func (s *Service) rowsOf(p audit.Actor, list []model.QueryPrivilegesApply, now time.Time, todo bool) ([]ApplyRow, error) {
	names := map[string]string{}
	groupName := func(id string) string {
		if n, ok := names[id]; ok {
			return n
		}
		n := id
		if gid, err := strconv.ParseUint(id, 10, 32); err == nil {
			if g, err := s.repo.AuthGroup(uint(gid)); err == nil && g != nil {
				n = g.Name
			}
		}
		names[id] = n
		return n
	}
	var reasons map[uint]ApplyExt
	if e := ext(s.repo); e != nil && e.ApplyExtReady() && len(list) > 0 {
		ids := make([]uint, 0, len(list))
		for _, a := range list {
			ids = append(ids, a.ApplyID)
		}
		if m, err := e.ApplyExts(ids); err == nil {
			reasons = m
		}
	}
	out := make([]ApplyRow, 0, len(list))
	for _, a := range list {
		r := ApplyRow{QueryPrivilegesApply: a}
		if a.Instance != nil {
			r.InstanceName = a.Instance.InstanceName
		}
		r.EffStatus = EffStatus(a.Status, a.ValidDate, now)
		r.StatusLabel = EffStatusLabel(r.EffStatus)
		r.Reason = reasons[a.ApplyID].Reason
		r.CanAbort = a.Status == audit.StatusWaiting && a.UserName == p.Username
		r.CanReview = todo
		if a.Status == audit.StatusWaiting {
			au, err := s.repo.AuditByWorkflow(audit.TypeQueryPrivilege, a.ApplyID, false)
			if err != nil {
				return nil, err
			}
			if au != nil {
				r.Progress = progressOf(au, groupName)
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func splitIDs(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// progressOf 计算待审批工单的审批进度；审批流为空或当前节点不在流程里时返回 nil。
func progressOf(au *model.WorkflowAudit, groupName func(string) string) *Progress {
	ids := splitIDs(au.AuditAuthGroups)
	for i, id := range ids {
		if id == au.CurrentAudit {
			return &Progress{Level: i + 1, Total: len(ids), Current: groupName(id)}
		}
	}
	return nil
}

// MatchPriv 判断一条未回收的权限是否满足筛选条件（内存实现与测试用；SQL 实现见 store）。范围（Scope）由调用方判断。
func (f PrivFilter) MatchPriv(p *model.QueryPrivilege) bool {
	now := f.Today
	if now.IsZero() {
		now = time.Now()
	}
	d := DaysLeft(p.ValidDate, now)
	switch f.State {
	case "all":
	case "soon":
		if d < 0 || d > soonDays {
			return false
		}
	case "expired":
		if d >= 0 {
			return false
		}
	default:
		if d < 0 {
			return false
		}
	}
	if f.FilterUser != "" && p.UserName != f.FilterUser {
		return false
	}
	if f.FilterInst != 0 && p.InstanceID != f.FilterInst {
		return false
	}
	if f.FilterDB != "" && !strings.EqualFold(p.DBName, f.FilterDB) {
		return false
	}
	if f.UserDisplay != "" && f.UserDisplay != "all" && p.UserDisplay != f.UserDisplay {
		return false
	}
	if f.Search != "" {
		q := strings.ToLower(f.Search)
		hay := strings.ToLower(p.UserDisplay + "\n" + p.DBName + "\n" + p.TbName)
		if !strings.Contains(hay, q) {
			return false
		}
	}
	return true
}
