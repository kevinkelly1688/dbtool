// @author: kevinkelly
// @license: Apache Licence
// @file: service.go
// @time: 2026/10/28

package dict

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"dbtool/internal/engine"
	"dbtool/internal/model"
	"dbtool/internal/query"
)

// Source 是元数据：实例列表与连接信息。server.Repo 满足它。
type Source interface {
	ListInstances() ([]model.Instance, error)
	EngineConn(inst *model.Instance) engine.Conn
}

// Access 是实例可见范围（query.Authorizer 满足它）：超级用户 / query.admin_run 看全部，其余按资源组。
type Access interface {
	FilterInstances(p query.Principal, list []model.Instance) ([]model.Instance, error)
	CanUseInstance(p query.Principal, inst *model.Instance) error
}

type Service struct {
	meta   Source
	access Access
	open   Opener
	mask   MaskSource // 敏感列规则来源；nil 表示不标敏感列
	now    func() time.Time
}

func NewService(meta Source, access Access, open Opener) *Service {
	if open == nil {
		open = DefaultOpener
	}
	return &Service{meta: meta, access: access, open: open, now: time.Now}
}

// ---- 实例 ----

type InstanceView struct {
	ID        uint   `json:"id"`
	Name      string `json:"name"`
	DBType    string `json:"db_type"`
	Supported bool   `json:"supported"`
	Kinds     []Kind `json:"kinds"` // 这个实例上能看的对象类别
}

func (s *Service) Instances(p query.Principal) ([]InstanceView, error) {
	list, err := s.meta.ListInstances()
	if err != nil {
		return nil, err
	}
	list, err = s.access.FilterInstances(p, list)
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]InstanceView, 0, len(list))
	for i := range list {
		in := &list[i]
		d := dialectFor(in.DBType)
		kinds := []Kind{}
		if d != nil {
			kinds = d.Kinds()
		}
		out = append(out, InstanceView{ID: in.ID, Name: in.InstanceName, DBType: in.DBType, Supported: d != nil, Kinds: kinds})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func wrap(err error) error {
	var qe *query.Error
	if errors.As(err, &qe) {
		return &Error{Status: qe.HTTP, Code: qe.Code, Message: qe.Message}
	}
	return err
}

func (s *Service) lookup(p query.Principal, id uint) (*model.Instance, dialect, error) {
	list, err := s.meta.ListInstances()
	if err != nil {
		return nil, nil, err
	}
	for i := range list {
		if list[i].ID != id {
			continue
		}
		in := &list[i]
		if err := s.access.CanUseInstance(p, in); err != nil {
			return nil, nil, wrap(err)
		}
		d := dialectFor(in.DBType)
		if d == nil {
			return nil, nil, newErr(501, "unsupported_engine", "该类型（%s）的实例暂不支持数据字典，目前支持 MySQL 系、SQL Server、PostgreSQL、MongoDB、ClickHouse、Oracle 与达梦", in.DBType)
		}
		return in, d, nil
	}
	return nil, nil, newErr(404, "not_found", "实例不存在")
}

// ---- 库名与对象名校验 ----

func anchored(expr string) (*regexp.Regexp, error) {
	if strings.TrimSpace(expr) == "" {
		return nil, nil
	}
	re, err := regexp.Compile(`\A(?:` + expr + `)`) // 从开头匹配
	if err != nil {
		return nil, newErr(500, "bad_config", "实例的库名过滤正则不合法：%s", expr)
	}
	return re, nil
}

// dbVisible 按实例配置判断库能不能展示：匹配 show_db_name_regex（若配置）且不匹配 denied_db_name_regex。正则非法时拒绝而不是放行。
func dbVisible(inst *model.Instance, db string) (bool, error) {
	show, err := anchored(inst.ShowDBNameRegex)
	if err != nil {
		return false, err
	}
	denied, err := anchored(inst.DeniedDBNameRegex)
	if err != nil {
		return false, err
	}
	if show != nil && !show.MatchString(db) {
		return false, nil
	}
	if denied != nil && denied.MatchString(db) {
		return false, nil
	}
	return true, nil
}

func cleanName(label, v string) error {
	if strings.TrimSpace(v) == "" {
		return badReq("请指定%s", label)
	}
	if len([]rune(v)) > 128 {
		return badReq("%s过长（最多 128 个字符）", label)
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return badReq("%s含有不允许的字符", label)
		}
	}
	return nil
}

// checkDB 校验库名：不是系统库，并且通过该实例的库名过滤规则。数据字典永远不展示系统库，即使知道它的名字。
func (s *Service) checkDB(inst *model.Instance, d dialect, db string) error {
	if err := cleanName("库名", db); err != nil {
		return err
	}
	if d.IsSystemDB(db) {
		return newErr(403, "db_not_allowed", "不允许查看系统库 %s", db)
	}
	ok, err := dbVisible(inst, db)
	if err != nil {
		return err
	}
	if !ok {
		return newErr(403, "db_not_allowed", "库 %s 不在该实例允许展示的库范围内", db)
	}
	return nil
}

func (s *Service) checkKind(inst *model.Instance, d dialect, kind Kind) error {
	if !validKind(kind) {
		return badReq("未知的对象类别 %q", kind)
	}
	supported := false
	for _, k := range d.Kinds() {
		supported = supported || k == kind
	}
	if !supported {
		return newErr(501, "unsupported_kind", "该类型（%s）的实例暂不支持查看%s", inst.DBType, kindLabel(kind))
	}
	return nil
}

// ---- 连接与错误 ----

// statusTargetError 是“目标库 / 引擎返回了错误或超时”的 HTTP 状态。不用 502 / 504：反向代理 / 网关常常拦截 5xx 并替换响应体，
// 页面就只剩“请求失败（HTTP 502）”，看不到目标库真正的报错。与在线查询（query.statusTargetError）保持一致。
const statusTargetError = 422

func (s *Service) connect(ctx context.Context, inst *model.Instance, db string) (Querier, error) {
	q, err := s.open(ctx, inst.DBType, s.meta.EngineConn(inst), db)
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			return nil, e
		}
		return nil, newErr(statusTargetError, "engine_error", "连接实例失败：%v", err)
	}
	return q, nil
}

func engineErr(what string, err error) error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return newErr(statusTargetError, "timeout", "%s超时，请稍后重试", what)
	}
	return newErr(statusTargetError, "engine_error", "%s失败：%v", what, err)
}

// ---- 库 / 对象 / 详情 ----

func (s *Service) visibleDatabases(ctx context.Context, inst *model.Instance, d dialect, q Querier) ([]string, error) {
	all, err := d.Databases(ctx, q)
	if err != nil {
		return nil, engineErr("读取库列表", err)
	}
	out := make([]string, 0, len(all))
	for _, db := range all {
		ok, err := dbVisible(inst, db)
		if err != nil {
			return nil, err
		}
		if ok && !d.IsSystemDB(db) {
			out = append(out, db)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (s *Service) Databases(ctx context.Context, p query.Principal, id uint) ([]string, error) {
	inst, d, err := s.lookup(p, id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	q, err := s.connect(ctx, inst, "")
	if err != nil {
		return nil, err
	}
	defer q.Close()
	return s.visibleDatabases(ctx, inst, d, q)
}

// maxItems 是单次返回的对象数上限。
const maxItems = 20000

// noter 是 dialect 的可选能力：返回列表 / 详情附带的说明（PostgreSQL / MongoDB / ClickHouse）。
type noter interface{ Notes() []string }

func notesOf(d dialect) []string {
	if n, ok := d.(noter); ok {
		return n.Notes()
	}
	return nil
}

func (s *Service) Objects(ctx context.Context, p query.Principal, id uint, db string, kind Kind) ([]Item, error) {
	items, _, err := s.ObjectsEx(ctx, p, id, db, kind)
	return items, err
}

// ObjectsEx 同 Objects，另返回对象列表附带的说明（仅 PostgreSQL / MongoDB / ClickHouse 有）。
func (s *Service) ObjectsEx(ctx context.Context, p query.Principal, id uint, db string, kind Kind) ([]Item, []string, error) {
	items, d, err := s.objects(ctx, p, id, db, kind)
	if err != nil {
		return nil, nil, err
	}
	return items, notesOf(d), nil
}

func (s *Service) objects(ctx context.Context, p query.Principal, id uint, db string, kind Kind) ([]Item, dialect, error) {
	inst, d, err := s.lookup(p, id)
	if err != nil {
		return nil, nil, err
	}
	if err := s.checkKind(inst, d, kind); err != nil {
		return nil, nil, err
	}
	if err := s.checkDB(inst, d, db); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	q, err := s.connect(ctx, inst, db)
	if err != nil {
		return nil, nil, err
	}
	defer q.Close()
	items, err := d.List(ctx, q, db, kind)
	if err != nil {
		return nil, nil, engineErr("读取对象列表", err)
	}
	if len(items) > maxItems {
		items = items[:maxItems]
	}
	if items == nil {
		items = []Item{}
	}
	if kind == KindTable || kind == KindView {
		s.annotateSensitive(ctx, q, inst, d, db, items)
	}
	return items, d, nil
}

// listWhat 是“读取某类对象列表”这个动作的说明（拼进 engineErr 的错误里；整句写出来，翻译才不用拼接名词）。
func listWhat(k Kind) string {
	switch k {
	case KindTable:
		return "读取表列表"
	case KindView:
		return "读取视图列表"
	case KindTrigger:
		return "读取触发器列表"
	case KindProcedure:
		return "读取存储过程列表"
	case KindFunction:
		return "读取函数列表"
	case KindEvent:
		return "读取定时任务列表"
	}
	return "读取对象列表"
}

// ProgrammableGroup 是一类可编程对象（视图 / 函数 / 存储过程 / 触发器 / 定时任务）及其对象。
type ProgrammableGroup struct {
	Kind  Kind   `json:"kind"`
	Label string `json:"label"`
	Items []Item `json:"items"`
	Error string `json:"error,omitempty"` // 这一类取不到时的原因（如没有查看权限）；不影响其他类别
}

// programmableKinds 是在线查询“可编程对象”页签里的类别与顺序（表不在其中：表走库表树的“表”页签）。
var programmableKinds = []Kind{KindView, KindFunction, KindProcedure, KindTrigger, KindEvent}

// Programmables 在一次连接里列出库里的可编程对象：实例支持哪些类别就返回哪些，某一类取失败只在该类里写原因。
func (s *Service) Programmables(ctx context.Context, p query.Principal, id uint, db string) ([]ProgrammableGroup, error) {
	inst, d, err := s.lookup(p, id)
	if err != nil {
		return nil, err
	}
	if err := s.checkDB(inst, d, db); err != nil {
		return nil, err
	}
	supported := map[Kind]bool{}
	for _, k := range d.Kinds() {
		supported[k] = true
	}
	var kinds []Kind
	for _, k := range programmableKinds {
		if supported[k] {
			kinds = append(kinds, k)
		}
	}
	out := []ProgrammableGroup{}
	if len(kinds) == 0 {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	q, err := s.connect(ctx, inst, db)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	for _, k := range kinds {
		g := ProgrammableGroup{Kind: k, Label: kindLabel(k), Items: []Item{}}
		items, err := d.List(ctx, q, db, k)
		if err != nil {
			g.Error = engineErr(listWhat(k), err).Error()
		} else {
			if len(items) > maxItems {
				items = items[:maxItems]
			}
			if items != nil {
				g.Items = items
			}
		}
		out = append(out, g)
	}
	return out, nil
}

// DetailRequest 是对象详情的请求。
type DetailRequest struct {
	InstanceID uint
	DB         string
	Kind       Kind
	Schema     string // 仅 SQL Server
	Name       string
}

func (s *Service) Detail(ctx context.Context, p query.Principal, r DetailRequest) (*Detail, error) {
	inst, d, err := s.lookup(p, r.InstanceID)
	if err != nil {
		return nil, err
	}
	if err := s.checkKind(inst, d, r.Kind); err != nil {
		return nil, err
	}
	if err := s.checkDB(inst, d, r.DB); err != nil {
		return nil, err
	}
	if err := cleanName("对象名", r.Name); err != nil {
		return nil, err
	}
	if r.Schema != "" {
		if err := cleanName("架构名", r.Schema); err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	q, err := s.connect(ctx, inst, r.DB)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	det, err := d.Detail(ctx, q, r.DB, r.Kind, r.Schema, r.Name)
	if err != nil {
		return nil, engineErr("读取对象详情", err)
	}
	s.annotateDetail(inst, r, det)
	return det, nil
}
