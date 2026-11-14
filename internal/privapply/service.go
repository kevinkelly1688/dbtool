// @author: kevinkelly
// @license: Apache Licence
// @file: service.go
// @time: 2026/10/28

// Package privapply 实现“查询权限申请”：申请、审批、撤回，以及已授予权限的管理。
// 审批流程复用通用审批引擎 internal/audit。
package privapply

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/i18n"
	"dbtool/internal/model"
	"dbtool/internal/notify"
	"dbtool/internal/query"
)

// 权限码。
const (
	permReview       = "qpriv.review"    // 审批权限申请；也决定能看到组内全部申请
	permApplyPriv    = "qpriv.list"      // 查看“查询权限管理”页面
	permMgtPriv      = "qpriv.manage"    // 修改/删除已授予的权限，并能看到组内权限
	permMenuApplies  = "qpriv.view"      // 打开权限申请页面
	permQueryAllInst = "query.admin_run" // 可查询所有实例
	maxLimitNum      = 1_000_000         // 申请的行数上限的合理上界（防止误填）
	maxNameLen       = 64                // 库名、表名长度（与库表字段一致）
	maxTitleLen      = 50                // 标题长度（与库表字段一致）
)

// Scope 是列表的可见范围。
type Scope int

const (
	ScopeOwn    Scope = iota // 只看自己的
	ScopeGroups              // 所在资源组内的
	ScopeAll                 // 全部
)

type ApplyFilter struct {
	Scope    Scope
	GroupIDs []uint
	UserName string
	Search   string
	Offset   int
	Limit    int
	// 以下为列表页新增的筛选，零值表示不筛选（旧调用方行为不变）。
	MineOnly string    // 只看该账号提交的（“我的申请”页签）
	Status   string    // "0" "1" "2" "3"：按数据库状态；"1" 不含已过期；"expired"：审批通过且权限已过期
	Today    time.Time // 判断“已过期”用的今天（零值=现在）
}

type PrivFilter struct {
	Scope       Scope
	InstanceIDs []uint
	UserName    string
	Search      string
	UserDisplay string
	Offset      int
	Limit       int
	// 以下为查询权限页新增的筛选，零值表示不筛选。
	FilterUser string    // 按账号（精确）筛选
	FilterInst uint      // 按实例筛选
	FilterDB   string    // 按库筛选（精确）
	State      string    // ""=未过期的（有效+即将到期，旧行为）；"soon"=7 天内到期；"expired"=已过期；"all"=全部
	Today      time.Time // 零值=现在
}

// PrivRow 是带实例名的已授予权限。
type PrivRow struct {
	model.QueryPrivilege
	InstanceName string
	// 以下由服务层补全（仓库只负责读出上面两部分）。
	State     string // PrivValid / PrivSoon / PrivExpired
	DaysLeft  int    // 距有效期的天数；已过期为负数
	DaysText  string // 剩余天数的文字：“剩余 30 天” / “今天到期” / “已过期 5 天”
	Source    string // 授予来源 SourceApply / SourceGrant；"" = 未知（旧数据或旁路表没创建）
	ApplyID   uint   // 来源申请单号
	GrantedBy string
	GrantNote string // 直接授予时的原因
}

// Repo 是权限申请服务需要的数据访问。InTx 内部的所有写操作在同一个事务里提交或回滚。
type Repo interface {
	audit.Repo
	InTx(fn func(Repo) error) error

	ListInstances() ([]model.Instance, error)
	UserInstanceIDs(userID uint) (map[uint]bool, error)
	UserResourceGroups(userID uint, all bool) ([]model.ResourceGroup, error)
	ResourceGroupByName(name string) (*model.ResourceGroup, error) // 不存在返回 nil, nil
	ResourceGroupByID(id uint) (*model.ResourceGroup, error)
	ValidQueryPrivileges(username string, instanceID uint) ([]model.QueryPrivilege, error)

	InsertApply(a *model.QueryPrivilegesApply) error
	SaveApply(a *model.QueryPrivilegesApply) error
	GetApply(id uint) (*model.QueryPrivilegesApply, error) // 带 Instance；不存在返回 nil, nil
	ListApplies(f ApplyFilter) ([]model.QueryPrivilegesApply, int64, error)
	WaitingApplies(username string, instanceID uint) ([]model.QueryPrivilegesApply, error)
	// LockUser 在事务内对申请人加行锁，使同一个人的并发提交串行化（用来保证“不重复提交”检查不被竞态绕过）。
	LockUser(username string) error

	AuditByWorkflow(workflowType int, workflowID uint, forUpdate bool) (*model.WorkflowAudit, error)
	AuditLogs(auditID uint) ([]model.WorkflowLog, error)

	InsertPrivileges(ps []model.QueryPrivilege) error
	ListPrivileges(f PrivFilter) ([]PrivRow, int64, error)
	GetPrivilege(id uint) (*model.QueryPrivilege, error)
	SavePrivilege(p *model.QueryPrivilege) error
}

type Service struct {
	repo     Repo
	notifier notify.Notifier
	now      func() time.Time
	async    func(func()) // 发通知用；测试里替换成同步执行
	BaseURL  string       // 对外访问地址，用于通知里的工单链接
}

func NewService(repo Repo, n notify.Notifier) *Service {
	if n == nil {
		n = notify.Noop{}
	}
	return &Service{repo: repo, notifier: n, now: time.Now, async: func(f func()) { go f() }}
}

func bad(msg string) error      { return query.NewError(400, "bad_request", msg) }
func forbid(msg string) error   { return query.NewError(403, "forbidden", msg) }
func notFound(msg string) error { return query.NewError(404, "not_found", msg) }

// translate 把审批引擎的业务错误转成对外错误；其他错误原样返回（由上层记日志并返回 500）。
func translate(err error) error {
	var ae *audit.Error
	if errors.As(err, &ae) {
		switch ae.Kind {
		case audit.KindConflict:
			return query.NewError(409, "conflict", ae.Message)
		case audit.KindConfig:
			return query.NewError(400, "audit_config", ae.Message)
		default:
			return query.NewError(403, "forbidden", ae.Message)
		}
	}
	return err
}

func (s *Service) has(p audit.Actor, code string) (bool, error) {
	if p.IsSuperuser {
		return true, nil
	}
	return s.repo.HasButton(p.ID, code)
}

// CanAccessPage 判断用户能否打开权限申请页面（qpriv.view）。
func (s *Service) CanAccessPage(p audit.Actor) bool {
	ok, err := s.has(p, permMenuApplies)
	return err == nil && ok
}

// ---- 申请时可选的实例与资源组 ----

// ApplyInstances 返回用户可以申请权限的实例：用户能访问、且开启了查询（can_read 标签）。
// 标签过滤对管理员同样生效。
func (s *Service) ApplyInstances(p audit.Actor) ([]model.Instance, error) {
	list, err := s.repo.ListInstances()
	if err != nil {
		return nil, err
	}
	all, err := s.has(p, permQueryAllInst)
	if err != nil {
		return nil, err
	}
	var ids map[uint]bool
	if !all {
		if ids, err = s.repo.UserInstanceIDs(p.ID); err != nil {
			return nil, err
		}
	}
	var out []model.Instance
	for i := range list {
		if (all || ids[list[i].ID]) && query.SupportsQuery(list[i].DBType) && query.HasReadTag(&list[i]) {
			out = append(out, list[i])
		}
	}
	return out, nil
}

// ApplyGroups 返回用户可以选作“审批所属资源组”的资源组。
func (s *Service) ApplyGroups(p audit.Actor) ([]model.ResourceGroup, error) {
	return s.repo.UserResourceGroups(p.ID, p.IsSuperuser)
}

// ---- 提交申请 ----

type CreateReq struct {
	Title      string
	InstanceID uint
	GroupName  string
	PrivType   int // 1 库权限；2 表权限
	DBName     string
	DBList     []string
	TableList  []string
	ValidDate  string // YYYY-MM-DD
	LimitNum   int
	Reason     string // 申请理由（旁路表就绪时必填，至少 5 个字）
}

func cleanNames(in []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, n := range in {
		n = strings.TrimSpace(n)
		if n == "" || seen[strings.ToLower(n)] {
			continue
		}
		if len([]rune(n)) > maxNameLen {
			return nil, bad(fmt.Sprintf("名称过长（最多 %d 个字符）：%s", maxNameLen, n))
		}
		if strings.ContainsAny(n, ",\r\n") {
			return nil, bad("库名、表名不能包含逗号或换行")
		}
		seen[strings.ToLower(n)] = true
		out = append(out, n)
	}
	return out, nil
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func today(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
}

// Create 提交一条权限申请并创建审批流。校验包括：
// 申请人必须属于所选资源组（否则申请人可以任选资源组从而选择审批人）、有效期不能早于今天、
// 行数上限必须为正、不允许提交完全相同的待审批申请。
func (s *Service) Create(ctx context.Context, p audit.Actor, req CreateReq) (*model.QueryPrivilegesApply, error) {
	now := s.now()
	if req.PrivType != 1 && req.PrivType != 2 {
		return nil, bad("权限类型错误")
	}
	if req.InstanceID == 0 {
		return nil, bad("请填写完整：请选择实例")
	}
	if req.LimitNum <= 0 || req.LimitNum > maxLimitNum {
		return nil, bad(fmt.Sprintf("返回行数上限需在 1 到 %d 之间", maxLimitNum))
	}
	valid, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(req.ValidDate), now.Location())
	if err != nil {
		return nil, bad("请填写完整：有效期格式应为 YYYY-MM-DD")
	}
	if valid.Before(today(now)) {
		return nil, bad("有效期不能早于今天")
	}
	reason := ""
	if reasonEnabled(s.repo) {
		if reason, err = checkReason(req.Reason, "请填写申请理由（至少 5 个字）"); err != nil {
			return nil, err
		}
	}

	insts, err := s.ApplyInstances(p)
	if err != nil {
		return nil, err
	}
	var inst *model.Instance
	for i := range insts {
		if insts[i].ID == req.InstanceID {
			inst = &insts[i]
		}
	}
	if inst == nil {
		return nil, forbid("你所在组未关联该实例！")
	}

	if req.PrivType == 2 && inst.DBType == "redis" {
		return nil, bad("Redis 没有表，只能申请库（db 编号）级的查询权限")
	}

	group, err := s.pickGroup(p, req.GroupName)
	if err != nil {
		return nil, err
	}
	// 实例必须属于所选资源组：审批流跟着资源组走，不允许拿别的组的宽松审批流申请这个实例的权限（同 SQL 上线的做法）。
	if gi := groupInstances(s.repo); gi != nil {
		ids, err := gi.GroupInstanceIDs(group.GroupID)
		if err != nil {
			return nil, err
		}
		if !ids[inst.ID] {
			return nil, bad(fmt.Sprintf("所选资源组未关联实例 %s", inst.InstanceName))
		}
	}

	var dbList, tableList string
	var reqDBs, reqTables []string // 规整后的库 / 表，用来按重叠判断重复的待审批申请
	privs, err := s.repo.ValidQueryPrivileges(p.Username, inst.ID)
	if err != nil {
		return nil, err
	}
	switch req.PrivType {
	case 1:
		dbs, err := cleanNames(req.DBList)
		if err != nil {
			return nil, err
		}
		if len(dbs) == 0 {
			return nil, bad("请填写完整：请选择要申请的数据库")
		}
		for _, db := range dbs {
			if query.HasDBPriv(privs, db) {
				return nil, bad(fmt.Sprintf("你已拥有%s实例%s库权限，不能重复申请", inst.InstanceName, db))
			}
		}
		dbList = strings.Join(dbs, ",")
		reqDBs = dbs
	case 2:
		db := strings.TrimSpace(req.DBName)
		tbs, err := cleanNames(req.TableList)
		if err != nil {
			return nil, err
		}
		if db == "" || len(tbs) == 0 {
			return nil, bad("请填写完整：请选择数据库和要申请的表")
		}
		if len([]rune(db)) > maxNameLen || strings.ContainsAny(db, ",\r\n") {
			return nil, bad("数据库名不合法")
		}
		if query.HasDBPriv(privs, db) {
			return nil, bad(fmt.Sprintf("你已拥有%s实例%s库的全部权限，不能重复申请", inst.InstanceName, db))
		}
		for _, tb := range tbs {
			if query.HasTablePriv(privs, db, tb) {
				return nil, bad(fmt.Sprintf("你已拥有%s实例%s.%s表的查询权限，不能重复申请", inst.InstanceName, db, tb))
			}
		}
		dbList, tableList = db, strings.Join(tbs, ",")
		reqDBs, reqTables = []string{db}, tbs
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		if req.PrivType == 1 {
			title = fmt.Sprintf("申请 %s %s 库查询权限", inst.InstanceName, dbList) // i18n-ignore 默认标题存库，属于用户数据
		} else {
			title = fmt.Sprintf("申请 %s %s.%s 查询权限", inst.InstanceName, dbList, tableList) // i18n-ignore 默认标题存库，属于用户数据
		}
	}
	title = truncRunes(title, maxTitleLen)

	apply := &model.QueryPrivilegesApply{
		GroupID: group.GroupID, GroupName: group.GroupName, Title: title,
		UserName: p.Username, UserDisplay: p.Display, InstanceID: inst.ID,
		DBList: dbList, TableList: tableList, ValidDate: valid, LimitNum: req.LimitNum,
		PrivType: req.PrivType, Status: audit.StatusWaiting,
	}
	var a *model.WorkflowAudit
	err = s.repo.InTx(func(tx Repo) error {
		// 先锁住申请人，再检查是否已有相同内容的待审批申请：并发的重复提交会在这里排队，后到的看到先到的结果。
		if err := tx.LockUser(p.Username); err != nil {
			return err
		}
		waiting, err := tx.WaitingApplies(p.Username, inst.ID)
		if err != nil {
			return err
		}
		lang := i18n.FromContext(ctx)
		for _, w := range waiting {
			if what := overlapWaiting(lang, &w, req.PrivType, reqDBs, reqTables); what != "" {
				return bad(fmt.Sprintf("已有包含 %s 的待审批申请（#%d），请勿重复提交", what, w.ApplyID))
			}
		}
		if err := tx.InsertApply(apply); err != nil {
			return err
		}
		if reason != "" {
			if e := ext(tx); e != nil {
				if err := e.SaveApplyExt(ApplyExt{ApplyID: apply.ApplyID, Reason: reason, CreatedAt: now}); err != nil {
					return err
				}
			}
		}
		a, err = audit.Create(tx, now, audit.Spec{
			WorkflowType: audit.TypeQueryPrivilege, WorkflowID: apply.ApplyID, GroupID: group.GroupID, GroupName: group.GroupName,
			Title: title, CreateUser: p.Username, CreateUserDisplay: p.Display,
		})
		if err != nil {
			return err
		}
		apply.AuditAuthGroups = a.AuditAuthGroups
		return tx.SaveApply(apply)
	})
	if err != nil {
		return nil, translate(err)
	}
	apply.Instance = inst
	s.notifyAsync(apply, a, "")
	s.sysAudit(p, "提交", applyAuditInfo(apply, "")+"，对象："+truncRunes(dbList+tableAuditText(tableList), 200)) // i18n-ignore 系统审计原文入库
	return apply, nil
}

// overlapWaiting 判断一张待审批申请 w 与新申请（类型 privType、库 dbs、表 tables）是否重叠，返回重叠的对象描述，不重叠返回空串。
// 库权限：库名有交集即重叠；表权限：同库且表名有交集即重叠；待审批的库权限覆盖该库的表权限申请。均不区分大小写。
// 描述文字按 lang 翻译（它会被代入“已有包含 %s 的待审批申请”这条错误提示）；中文下与原来逐字一致。
func overlapWaiting(lang string, w *model.QueryPrivilegesApply, privType int, dbs, tables []string) string {
	split := func(s string) []string {
		var out []string
		for _, x := range strings.Split(s, ",") {
			if x = strings.TrimSpace(x); x != "" {
				out = append(out, x)
			}
		}
		return out
	}
	wdbs, wtbs := split(w.DBList), split(w.TableList)
	inList := func(l []string, x string) bool {
		for _, y := range l {
			if strings.EqualFold(x, y) {
				return true
			}
		}
		return false
	}
	switch {
	case w.PrivType == 1 && privType == 1:
		for _, d := range dbs {
			if inList(wdbs, d) {
				return i18n.T(lang, "{db}库", "db", d)
			}
		}
	case w.PrivType == 1 && privType == 2: // 待审批的是整库，新申请的是该库的表
		for _, d := range dbs {
			if inList(wdbs, d) {
				return i18n.T(lang, "{db}库的全部权限", "db", d)
			}
		}
	case w.PrivType == 2 && privType == 2:
		for _, d := range dbs {
			if !inList(wdbs, d) {
				continue
			}
			for _, t := range tables {
				if inList(wtbs, t) {
					return d + "." + t
				}
			}
		}
	}
	return ""
}

// pickGroup 确定审批所属的资源组：必须是申请人所在的资源组（超级用户不限）。
func (s *Service) pickGroup(p audit.Actor, name string) (*model.ResourceGroup, error) {
	groups, err := s.repo.UserResourceGroups(p.ID, p.IsSuperuser)
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		if len(groups) == 1 {
			return &groups[0], nil
		}
		return nil, bad("请填写完整：请选择资源组")
	}
	for i := range groups {
		if groups[i].GroupName == name {
			return &groups[i], nil
		}
	}
	return nil, forbid("你不属于该资源组，不能以它的审批流提交申请")
}

// ---- 列表与详情 ----

type ApplyRow struct {
	model.QueryPrivilegesApply
	InstanceName string
	StatusLabel  string // 显示状态的文字（审批通过但权限已过期时为“已过期”）
	EffStatus    int    // 显示状态：同数据库状态，另有 StatusExpired
	Reason       string // 申请理由（旁路表没创建或老数据为空）
	Progress     *Progress
	CanAbort     bool // 申请人本人且待审批：可撤回
	CanReview    bool // 在“待我审批”页签里：当前用户可审批
}

type ListResult struct {
	Rows  []ApplyRow
	Total int64
}

func (s *Service) applyScope(p audit.Actor) (ApplyFilter, error) {
	if p.IsSuperuser {
		return ApplyFilter{Scope: ScopeAll}, nil
	}
	review, err := s.has(p, permReview)
	if err != nil {
		return ApplyFilter{}, err
	}
	if !review {
		return ApplyFilter{Scope: ScopeOwn, UserName: p.Username}, nil
	}
	groups, err := s.repo.UserResourceGroups(p.ID, false)
	if err != nil {
		return ApplyFilter{}, err
	}
	f := ApplyFilter{Scope: ScopeGroups, UserName: p.Username}
	for _, g := range groups {
		f.GroupIDs = append(f.GroupIDs, g.GroupID)
	}
	return f, nil
}

// List 返回权限申请列表。可见范围：管理员全部；有审批权限的看所在资源组内的；其他人只看自己的。
func (s *Service) List(p audit.Actor, search string, offset, limit int) (*ListResult, error) {
	f, err := s.applyScope(p)
	if err != nil {
		return nil, err
	}
	f.Search, f.Offset, f.Limit = strings.TrimSpace(search), offset, limit
	rows, total, err := s.repo.ListApplies(f)
	if err != nil {
		return nil, err
	}
	res := &ListResult{Total: total}
	for _, r := range rows {
		name := ""
		if r.Instance != nil {
			name = r.Instance.InstanceName
		}
		eff := EffStatus(r.Status, r.ValidDate, s.now())
		res.Rows = append(res.Rows, ApplyRow{QueryPrivilegesApply: r, InstanceName: name, StatusLabel: EffStatusLabel(eff), EffStatus: eff})
	}
	return res, nil
}

type Detail struct {
	Apply            model.QueryPrivilegesApply
	InstanceName     string
	StatusLabel      string
	FlowText         string
	Nodes            []audit.Node
	CurrentReviewers []model.User
	Logs             []model.WorkflowLog
	CanReview        bool
	CanAbort         bool
	LastRemark       string // 被驳回时的驳回原因

	// 以下为详情页新增的信息（旁路表没创建时对应部分为空）。
	EffStatus int       // 显示状态（审批通过且权限已过期 = StatusExpired）
	Reason    string    // 申请理由
	Dept      string    // 申请人部门
	Granted   []Granted // 这张申请授予的权限
	Timeline  []TLStep  // 各级审批时间线
	Reapply   string    // “重新申请”预填参数（URL 查询串，不含 ?）；被驳回 / 已撤回 / 已过期时才有
}

// canView 判断用户能否查看一张申请：超级用户、申请人、或有审批权限且在该申请所属资源组内的用户。
func (s *Service) canView(p audit.Actor, a *model.QueryPrivilegesApply) (bool, error) {
	if p.IsSuperuser || p.Username == a.UserName {
		return true, nil
	}
	review, err := s.has(p, permReview)
	if err != nil || !review {
		return false, err
	}
	groups, err := s.repo.UserResourceGroups(p.ID, false)
	if err != nil {
		return false, err
	}
	for _, g := range groups {
		if g.GroupID == a.GroupID {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) Detail(p audit.Actor, id uint) (*Detail, error) {
	a, err := s.repo.GetApply(id)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, notFound("工单不存在")
	}
	ok, err := s.canView(p, a)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, notFound("工单不存在") // 不区分“不存在”和“无权限”，避免被用来探测编号
	}
	au, err := s.repo.AuditByWorkflow(audit.TypeQueryPrivilege, a.ApplyID, false)
	if err != nil {
		return nil, err
	}
	if au == nil {
		return nil, query.NewError(500, "data_error", "工单缺少审批流记录，请联系管理员")
	}
	eff := EffStatus(a.Status, a.ValidDate, s.now())
	d := &Detail{Apply: *a, StatusLabel: EffStatusLabel(eff), EffStatus: eff}
	if a.Instance != nil {
		d.InstanceName = a.Instance.InstanceName
	}
	if d.FlowText, err = audit.FlowText(s.repo, au); err != nil {
		return nil, err
	}
	d.Nodes = audit.ReviewNodes(s.repo, au)
	for _, n := range d.Nodes {
		if n.Current {
			users, err := s.repo.AuthGroupUsers([]string{n.Name}, a.GroupID)
			if err != nil {
				return nil, err
			}
			d.CurrentReviewers = users
		}
	}
	if d.Logs, err = s.repo.AuditLogs(au.AuditID); err != nil {
		return nil, err
	}
	actor := p
	d.CanReview = audit.CanReview(s.repo, au, actor)
	d.CanAbort = au.CurrentStatus == audit.StatusWaiting && p.Username == au.CreateUser
	if a.Status == audit.StatusRejected && len(d.Logs) > 0 {
		d.LastRemark = d.Logs[len(d.Logs)-1].OperationInfo
	}
	s.fillDetailExt(d, au)
	return d, nil
}

// ---- 审批 / 撤回 ----

// Review 审批（通过或驳回）。同一个事务里完成：审批主表与明细、业务表状态、通过时写入权限；提交后再发通知。
func (s *Service) Review(ctx context.Context, p audit.Actor, id uint, action int, remark string) error {
	if action != audit.ActionPass && action != audit.ActionReject {
		return bad("审批动作错误")
	}
	remark = strings.TrimSpace(remark)
	if action == audit.ActionReject && remark == "" {
		return bad("驳回时请填写驳回原因")
	}
	return s.operate(ctx, p, id, action, remark)
}

// Abort 提交人撤回自己的待审批申请。
func (s *Service) Abort(ctx context.Context, p audit.Actor, id uint, remark string) error {
	return s.operate(ctx, p, id, audit.ActionAbort, strings.TrimSpace(remark))
}

func (s *Service) operate(ctx context.Context, p audit.Actor, id uint, action int, remark string) error {
	var apply *model.QueryPrivilegesApply
	var au *model.WorkflowAudit
	var detail *model.WorkflowAuditDetail
	err := s.repo.InTx(func(tx Repo) error {
		var err error
		if apply, err = tx.GetApply(id); err != nil {
			return err
		}
		if apply == nil {
			return notFound("工单不存在")
		}
		// 先取行锁：两个审批人同时操作同一张工单时，后到的会看到最新状态而不是重复审批。
		if au, err = tx.AuditByWorkflow(audit.TypeQueryPrivilege, apply.ApplyID, true); err != nil {
			return err
		}
		if au == nil {
			return query.NewError(500, "data_error", "工单缺少审批流记录，请联系管理员")
		}
		if action != audit.ActionAbort {
			// 审批入口需要 qpriv.review 按钮（超级用户除外）。
			if ok, err := s.has(p, permReview); err != nil {
				return err
			} else if !ok {
				return forbid("用户无相关审批权限，请合理配置权限")
			}
		} else if ok, err := s.canView(p, apply); err != nil {
			return err
		} else if !ok {
			return notFound("工单不存在")
		}
		if detail, err = audit.Operate(tx, s.now(), au, action, p, remark); err != nil {
			return err
		}
		return s.callback(tx, apply, au.CurrentStatus)
	})
	if err != nil {
		return translate(err)
	}
	s.notifyAsync(apply, au, detail.Remark)
	switch action {
	case audit.ActionPass:
		info := applyAuditInfo(apply, detail.Remark)
		if au.CurrentStatus == audit.StatusPassed {
			info += "，审批已全部通过，权限已授予" // i18n-ignore 系统审计原文入库
		} else {
			info += "，进入下一级审批" // i18n-ignore 系统审计原文入库
		}
		s.sysAudit(p, "审批通过", info) // i18n-ignore 审计动作名入库，audit.Classify 按它归类
	case audit.ActionReject:
		s.sysAudit(p, "驳回", applyAuditInfo(apply, detail.Remark)) // i18n-ignore 同上
	case audit.ActionAbort:
		s.sysAudit(p, "撤销", applyAuditInfo(apply, detail.Remark)) // i18n-ignore 同上
	}
	return nil
}

// callback 是审批后的业务回调：更新申请状态，通过时写入权限。
func (s *Service) callback(tx Repo, a *model.QueryPrivilegesApply, status int) error {
	a.Status = status
	if err := tx.SaveApply(a); err != nil {
		return err
	}
	if status != audit.StatusPassed {
		return nil
	}
	var rows []model.QueryPrivilege
	mk := func(db, table string) model.QueryPrivilege {
		return model.QueryPrivilege{UserName: a.UserName, UserDisplay: a.UserDisplay, InstanceID: a.InstanceID,
			DBName: db, TbName: table, ValidDate: a.ValidDate, LimitNum: a.LimitNum, PrivType: a.PrivType}
	}
	switch a.PrivType {
	case 1:
		for _, db := range strings.Split(a.DBList, ",") {
			rows = append(rows, mk(db, a.TableList))
		}
	case 2:
		for _, tb := range strings.Split(a.TableList, ",") {
			rows = append(rows, mk(a.DBList, tb))
		}
	default:
		return fmt.Errorf("申请 #%d 的权限类型 %d 无法授权", a.ApplyID, a.PrivType)
	}
	if err := tx.InsertPrivileges(rows); err != nil {
		return err
	}
	// 旁路记录授予来源（表没创建时跳过）。
	if e := ext(tx); e != nil && e.PrivExtReady() {
		exts := make([]PrivExt, 0, len(rows))
		for _, r := range rows {
			if r.PrivilegeID != 0 {
				exts = append(exts, PrivExt{PrivilegeID: r.PrivilegeID, Source: SourceApply, ApplyID: a.ApplyID, CreatedAt: s.now()})
			}
		}
		if len(exts) > 0 {
			// 授予来源只是展示用的旁路信息：写失败不能让审批回滚。
			if err := e.SavePrivExts(exts); err != nil {
				slog.Warn("记录权限授予来源失败", "apply_id", a.ApplyID, "err", err)
			}
		}
	}
	return nil
}

// ---- 通知 ----

func (s *Service) workflowURL(applyID uint) string {
	return fmt.Sprintf("%s/query/applies/%d", strings.TrimRight(s.BaseURL, "/"), applyID)
}

func privContent(a *model.QueryPrivilegesApply) string {
	var b strings.Builder
	if a.PrivType == 1 {
		fmt.Fprintf(&b, "数据库清单：%s\n", a.DBList) // i18n-ignore 通知正文（语言属于接收方，不翻译）
	} else {
		fmt.Fprintf(&b, "数据库：%s\n表清单：%s\n", a.DBList, a.TableList) // i18n-ignore 通知正文
	}
	fmt.Fprintf(&b, "授权截止时间：%s\n结果集：%d\n", a.ValidDate.Format("2006-01-02 15:04:05"), a.LimitNum) // i18n-ignore 通知正文
	return b.String()
}

// buildEvent 生成通知事件。
func (s *Service) buildEvent(a *model.QueryPrivilegesApply, au *model.WorkflowAudit, remark string) notify.Event {
	label := "查询权限申请" // i18n-ignore 通知用的工单类型名
	instance := ""
	if a.Instance != nil {
		instance = a.Instance.InstanceName
	}
	flow := s.readableFlow(au)
	created := a.CreateTime.Format("2006-01-02 15:04:05")
	ev := notify.Event{Kind: notify.KindQueryPrivilege, GroupID: a.GroupID, Applicant: a.UserName}
	applicant := au.CreateUserDisplay
	if applicant == "" {
		applicant = a.UserName
	}
	// 消息模板用的变量（见 notify.Vars）；库 / 表清单、授权截止时间、结果集上限在 a 上
	ev.Vars = map[string]string{
		"ticket_id": fmt.Sprint(au.AuditID), "kind": label, "title": au.WorkflowTitle, "created_at": created, "url": s.workflowURL(a.ApplyID),
		"applicant": applicant, "applicant_user": a.UserName, "group": au.GroupName, "instance": instance, "flow": flow,
		"db_list": a.DBList, "table_list": a.TableList, "valid_date": a.ValidDate.Format("2006-01-02 15:04:05"), "limit_num": fmt.Sprint(a.LimitNum),
	}
	switch au.CurrentStatus {
	case audit.StatusWaiting:
		current := ""
		for _, n := range audit.ReviewNodes(s.repo, au) {
			if n.Current {
				current = n.Name
				ev.ReviewerAuthGroupID = n.GroupID
			}
		}
		ev.Name = notify.EventPending
		ev.Vars["current_step"] = current
		if notify.IsFirstAuditNode(au.AuditAuthGroups, au.CurrentAudit) {
			ev.Specific = notify.EventSubmitted // 新工单提交（同时仍属于“待处理”）
		}
		ev.Title = fmt.Sprintf("[%s]新的工单申请#%d", label, au.AuditID)                                                        // i18n-ignore 通知正文
		ev.Content = fmt.Sprintf("发起时间：%s\n发起人：%s\n组：%s\n目标实例：%s\n数据库：%s\n审批流程：%s\n当前审批：%s\n工单名称：%s\n工单地址：%s\n工单详情预览：%s", // i18n-ignore 通知正文
			created, au.CreateUserDisplay, au.GroupName, instance, a.DBList, flow, current, au.WorkflowTitle, s.workflowURL(a.ApplyID), privContent(a))
	case audit.StatusPassed:
		ev.Name = notify.EventPassed
		ev.Title = fmt.Sprintf("[%s]工单审核通过#%d", label, au.AuditID)                                                 // i18n-ignore 通知正文
		ev.Content = fmt.Sprintf("发起时间：%s\n发起人：%s\n组：%s\n目标实例：%s\n数据库：%s\n审批流程：%s\n工单名称：%s\n工单地址：%s\n工单详情预览：%s\n", // i18n-ignore 通知正文
			created, au.CreateUserDisplay, au.GroupName, instance, a.DBList, flow, au.WorkflowTitle, s.workflowURL(a.ApplyID), privContent(a))
	case audit.StatusRejected:
		ev.Name = notify.EventRejected
		ev.Vars["remark"] = strings.TrimSpace(remark)
		ev.Title = fmt.Sprintf("[%s]工单被驳回#%d", label, au.AuditID)                                                  // i18n-ignore 通知正文
		ev.Content = fmt.Sprintf("发起时间：%s\n目标实例：%s\n数据库：%s\n工单名称：%s\n工单地址：%s\n驳回原因：%s\n提醒：此工单被审核不通过，请按照驳回原因进行修改！", // i18n-ignore 通知正文
			created, instance, a.DBList, au.WorkflowTitle, s.workflowURL(a.ApplyID), remark)
	default: // 已撤回
		ev.Name = notify.EventAborted
		ev.Vars["remark"] = strings.TrimSpace(remark)
		ev.Title = fmt.Sprintf("[%s]提交人主动终止工单#%d", label, au.AuditID)                                 // i18n-ignore 通知正文
		ev.Content = fmt.Sprintf("发起时间：%s\n发起人：%s\n组：%s\n目标实例：%s\n数据库：%s\n工单名称：%s\n工单地址：%s\n终止原因：%s", // i18n-ignore 通知正文
			created, au.CreateUserDisplay, au.GroupName, instance, a.DBList, au.WorkflowTitle, s.workflowURL(a.ApplyID), remark)
	}
	return ev
}

// readableFlow 形如 “组长(passed) -> DBA(current)”。
func (s *Service) readableFlow(au *model.WorkflowAudit) string {
	var parts []string
	for _, n := range audit.ReviewNodes(s.repo, au) {
		switch {
		case n.Current:
			parts = append(parts, n.Name+"(current)")
		case n.Passed:
			parts = append(parts, n.Name+"(passed)")
		default:
			parts = append(parts, n.Name)
		}
	}
	return strings.Join(parts, " -> ")
}

func (s *Service) notifyAsync(a *model.QueryPrivilegesApply, au *model.WorkflowAudit, remark string) {
	ev := s.buildEvent(a, au, remark)
	n := s.notifier
	s.async(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		n.Notify(ctx, ev)
	})
}

// ---- 已授予权限的查看与管理 ----

type PrivQuery struct {
	Search      string
	UserDisplay string
	Offset      int
	Limit       int
	// 以下为查询权限页新增的筛选。
	User       string // 按账号筛选
	InstanceID uint   // 按实例筛选
	DB         string // 按库筛选
	State      string // ""=未过期的；"soon"；"expired"；"all"
}

type PrivList struct {
	Rows      []PrivRow
	Total     int64
	CanManage bool
	Stats     PrivStats // 顶部 KPI 与筛选项（仓库不支持统计时为零值）
	HasStats  bool
}

func (s *Service) CanAccessPrivPage(p audit.Actor) bool {
	ok, err := s.has(p, permApplyPriv)
	return err == nil && ok
}

func (s *Service) privScope(p audit.Actor) (PrivFilter, bool, error) {
	if p.IsSuperuser {
		return PrivFilter{Scope: ScopeAll}, true, nil
	}
	mgt, err := s.has(p, permMgtPriv)
	if err != nil {
		return PrivFilter{}, false, err
	}
	if !mgt {
		return PrivFilter{Scope: ScopeOwn, UserName: p.Username}, false, nil
	}
	// 管理者：看所在资源组关联的实例上的权限。
	ids, err := s.repo.UserInstanceIDs(p.ID)
	if err != nil {
		return PrivFilter{}, false, err
	}
	f := PrivFilter{Scope: ScopeGroups}
	for id := range ids {
		f.InstanceIDs = append(f.InstanceIDs, id)
	}
	sort.Slice(f.InstanceIDs, func(i, j int) bool { return f.InstanceIDs[i] < f.InstanceIDs[j] })
	return f, true, nil
}

// ListPrivileges 返回当前有效（未删除、未过期）的权限。需要 qpriv.list 权限。
func (s *Service) ListPrivileges(p audit.Actor, q PrivQuery) (*PrivList, error) {
	if !s.CanAccessPrivPage(p) {
		return nil, forbid("你没有查看查询权限的权限（缺少 qpriv.list）")
	}
	f, manage, err := s.privScope(p)
	if err != nil {
		return nil, err
	}
	f.Search, f.UserDisplay, f.Offset, f.Limit = strings.TrimSpace(q.Search), q.UserDisplay, q.Offset, q.Limit
	f.FilterUser, f.FilterInst, f.FilterDB = strings.TrimSpace(q.User), q.InstanceID, strings.TrimSpace(q.DB)
	switch q.State {
	case "soon", "expired", "all":
		f.State = q.State
	}
	now := s.now()
	f.Today = today(now)
	rows, total, err := s.repo.ListPrivileges(f)
	if err != nil {
		return nil, err
	}
	s.decoratePrivs(rows, now)
	res := &PrivList{Rows: rows, Total: total, CanManage: manage}
	if e := ext(s.repo); e != nil {
		scope := f
		scope.Search, scope.UserDisplay, scope.FilterUser, scope.FilterDB, scope.State, scope.Offset, scope.Limit = "", "", "", "", "", 0, 0
		st, err := e.PrivStats(scope, f.Today)
		if err != nil {
			slog.Warn("统计查询权限失败", "err", err)
		} else {
			res.Stats, res.HasStats = st, true
		}
	}
	return res, nil
}

// decoratePrivs 补全每条权限的状态、剩余天数与授予来源。
func (s *Service) decoratePrivs(rows []PrivRow, now time.Time) {
	var exts map[uint]PrivExt
	if e := ext(s.repo); e != nil && e.PrivExtReady() && len(rows) > 0 {
		ids := make([]uint, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.PrivilegeID)
		}
		if m, err := e.PrivExts(ids); err == nil {
			exts = m
		} else {
			slog.Warn("读取授予来源失败", "err", err)
		}
	}
	for i := range rows {
		r := &rows[i]
		r.State, r.DaysLeft = PrivState(r.ValidDate, now), DaysLeft(r.ValidDate, now)
		r.DaysText = DaysText(r.DaysLeft)
		if x, ok := exts[r.PrivilegeID]; ok {
			r.Source, r.ApplyID, r.GrantedBy, r.GrantNote = x.Source, x.ApplyID, x.GrantedBy, x.Reason
		}
	}
}

// managePrivilege 取出用户有权管理的权限；不在管理范围内的按“不存在”处理。
func (s *Service) managePrivilege(p audit.Actor, id uint) (*model.QueryPrivilege, error) {
	ok, err := s.has(p, permMgtPriv)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, forbid("你没有管理查询权限的权限（缺少 qpriv.manage）")
	}
	priv, err := s.repo.GetPrivilege(id)
	if err != nil {
		return nil, err
	}
	if priv == nil || priv.IsDeleted != 0 {
		return nil, notFound("待操作权限不存在")
	}
	if !p.IsSuperuser {
		// 非超级用户限定在自己资源组关联的实例内。
		ids, err := s.repo.UserInstanceIDs(p.ID)
		if err != nil {
			return nil, err
		}
		if !ids[priv.InstanceID] {
			return nil, notFound("待操作权限不存在")
		}
	}
	return priv, nil
}

// DeletePrivilege 回收一条权限（软删除）。
func (s *Service) DeletePrivilege(p audit.Actor, id uint) error {
	priv, err := s.managePrivilege(p, id)
	if err != nil {
		return err
	}
	priv.IsDeleted = 1
	if err := s.repo.SavePrivilege(priv); err != nil {
		return err
	}
	s.afterPrivChange(p, priv, privChange{action: auditActRevoke, verb: "回收", evName: notify.EventAborted, // i18n-ignore 审计 / 通知 / 申请单日志原文入库
		changes: fmt.Sprintf("有效期 %s，行数上限 %d（已回收）", priv.ValidDate.Format("2006-01-02"), priv.LimitNum)}) // i18n-ignore 同上
	slog.Info("查询权限已回收", "operator", p.Username, "privilege_id", id, "user", priv.UserName, "instance_id", priv.InstanceID, "db", priv.DBName, "table", priv.TbName)
	return nil
}

// ModifyPrivilege 修改一条权限的有效期与行数上限。
func (s *Service) ModifyPrivilege(p audit.Actor, id uint, validDate string, limit int) error {
	now := s.now()
	valid, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(validDate), now.Location())
	if err != nil {
		return bad("有效期格式应为 YYYY-MM-DD")
	}
	if valid.Before(today(now)) {
		return bad("有效期不能早于今天")
	}
	if limit <= 0 || limit > maxLimitNum {
		return bad(fmt.Sprintf("返回行数上限需在 1 到 %d 之间", maxLimitNum))
	}
	priv, err := s.managePrivilege(p, id)
	if err != nil {
		return err
	}
	oldDate, oldLimit := priv.ValidDate, priv.LimitNum
	priv.ValidDate, priv.LimitNum = valid, limit
	if err := s.repo.SavePrivilege(priv); err != nil {
		return err
	}
	s.afterPrivChange(p, priv, privChange{action: auditActModify, verb: "修改", evName: notify.EventPassed, // i18n-ignore 审计 / 通知 / 申请单日志原文入库
		changes: fmt.Sprintf("有效期 %s→%s，行数上限 %d→%d", oldDate.Format("2006-01-02"), valid.Format("2006-01-02"), oldLimit, limit)}) // i18n-ignore 同上
	slog.Info("查询权限已修改", "operator", p.Username, "privilege_id", id, "user", priv.UserName,
		"valid_date", fmt.Sprintf("%s -> %s", oldDate.Format("2006-01-02"), valid.Format("2006-01-02")), "limit", fmt.Sprintf("%d -> %d", oldLimit, limit))
	return nil
}

// SetAsync 替换发送通知的调度方式（默认每条通知起一个 goroutine）；测试里换成同步执行以便断言。
func (s *Service) SetAsync(f func(func())) { s.async = f }
