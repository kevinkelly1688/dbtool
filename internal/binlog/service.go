// @author: kevinkelly
// @license: Apache Licence
// @file: service.go
// @time: 2026/10/28

package binlog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"dbtool/internal/audit"
	"dbtool/internal/i18n"
	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// Service 是 Binlog 解析的业务逻辑。目标实例、元数据仓库、审计都通过接口注入。
type Service struct {
	repo  Repo
	src   Source
	audit AuditSink
	jobs  *jobStore
	sem   chan struct{} // 同时从目标实例拉取 binlog 的任务数
	now   func() time.Time
	// disabled 返回已在实例管理里停用的实例（平台不再连接它们）；为 nil 或出错时不限制。
	disabled func() (map[uint]bool, error)

	// 结果超过 spillAt 字节就转存到 spillDir 下的临时文件（里面是业务数据：目录 0700、文件 0600，结果过期 / 任务失败时删除）。
	spillDir string
	spillAt  int

	rootMu sync.Mutex
	root   context.Context
}

// DefaultConcurrency 是同时拉取 binlog 的最大任务数（每个任务占用目标实例一条复制连接）。
const DefaultConcurrency = 3

const permQueryAllInsts = "query.admin_run" // 查看 / 查询全部实例

// NewService 创建服务；src 为 nil 时使用真实的 MySQL 实现。
func NewService(repo Repo, src Source, sink AuditSink) *Service {
	if src == nil {
		src = mysqlSource{}
	}
	return &Service{repo: repo, src: src, audit: sink, jobs: newJobStore(), sem: make(chan struct{}, DefaultConcurrency),
		now: time.Now, root: context.Background(), spillDir: filepath.Join(os.TempDir(), "dbtool-binlog"), spillAt: spillThreshold}
}

// CleanSpill 清掉上次运行遗留的结果临时文件（进程被杀时没来得及删）。只删超过两小时的：任务结果最多保留 30 分钟，
// 所以更新的文件可能属于同一台机器上另一个正在运行的 dbtool。
func (s *Service) CleanSpill() {
	old, err := filepath.Glob(filepath.Join(s.spillDir, "binlog-*.spill"))
	if err != nil {
		return
	}
	for _, f := range old {
		if fi, err := os.Stat(f); err == nil && s.now().Sub(fi.ModTime()) > 2*time.Hour {
			_ = os.Remove(f)
		}
	}
}

// SetDisabled 设置“已停用实例”的查询（生产环境来自旁路表 dbt_instance_ext）。
func (s *Service) SetDisabled(f func() (map[uint]bool, error)) { s.disabled = f }

// SetRoot 设置后台任务的根 context（服务退出时取消）。
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
		slog.Warn("写 Binlog 解析审计失败", "action", action, "err", err)
	}
}

func truncRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// ---- 实例范围 ----

// visibleSet 计算用户可见的实例：超级用户、query.admin_run 按钮、或实例属于用户的资源组。
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

func supported(inst *model.Instance) string {
	// 配置了 SSH 隧道的实例经 repo.EngineConn 自动走本地转发器（internal/sshtunnel），与直连实例一样可用。
	if inst.DBType != "mysql" {
		return "仅支持 MySQL 实例"
	}
	return ""
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
	if why := supported(inst); why != "" {
		return nil, newErr(http.StatusBadRequest, "unsupported", why)
	}
	if s.disabled != nil {
		if off, err := s.disabled(); err == nil && off[inst.ID] {
			return nil, newErr(http.StatusConflict, "instance_disabled", "该实例已在实例管理里停用，平台不再连接它")
		}
	}
	return inst, nil
}

// InstanceView 是实例下拉里的一项。
type InstanceView struct {
	ID          uint   `json:"id"`
	Name        string `json:"name"`
	Role        string `json:"role"`
	Unsupported string `json:"unsupported,omitempty"`
}

// Instances 返回当前用户可见的 MySQL 实例。
func (s *Service) Instances(actor audit.Actor) ([]InstanceView, error) {
	all, ids, err := s.visibleSet(actor)
	if err != nil {
		return nil, err
	}
	list, err := s.repo.ListInstances()
	if err != nil {
		return nil, err
	}
	out := []InstanceView{}
	for i := range list {
		in := &list[i]
		if (!all && !ids[in.ID]) || in.DBType != "mysql" {
			continue
		}
		out = append(out, InstanceView{ID: in.ID, Name: in.InstanceName, Role: in.Type, Unsupported: supported(in)})
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

// ---- 库的可见规则 ----

var systemDBs = map[string]bool{"mysql": true, "information_schema": true, "performance_schema": true, "sys": true}

func anchoredMatcher(expr string) (func(string) bool, error) {
	if strings.TrimSpace(expr) == "" {
		return nil, nil
	}
	re, err := regexp.Compile(`\A(?:` + expr + `)`)
	if err != nil {
		return nil, newErr(http.StatusInternalServerError, "bad_config", "实例的库名过滤正则不合法，请联系管理员")
	}
	return re.MatchString, nil
}

// visibleMatcher 判断库对用户是否可见：去掉系统库，再按实例的 show_db_name_regex / denied_db_name_regex 过滤（与 SQL 优化、在线查询一致）。
func visibleMatcher(inst *model.Instance) (func(string) bool, error) {
	show, err := anchoredMatcher(inst.ShowDBNameRegex)
	if err != nil {
		return nil, err
	}
	denied, err := anchoredMatcher(inst.DeniedDBNameRegex)
	if err != nil {
		return nil, err
	}
	return func(n string) bool {
		return !systemDBs[strings.ToLower(n)] && (show == nil || show(n)) && (denied == nil || !denied(n))
	}, nil
}

// ---- 连接目标实例 ----

func (s *Service) connect(ctx context.Context, inst *model.Instance) (Session, []string, error) {
	conn := s.repo.EngineConn(inst)
	conn.DBName = ""
	sess, err := s.src.Connect(ctx, conn)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, nil, newErr(http.StatusGatewayTimeout, "timeout", fmt.Sprintf("连接实例 %s 超时", inst.InstanceName))
		}
		return nil, nil, newErr(http.StatusBadGateway, "connect_failed", fmt.Sprintf("连接实例 %s 失败：%s", inst.InstanceName, redact(err.Error(), conn.Password)))
	}
	return sess, []string{conn.Password}, nil
}

// redact 把输出里的敏感字符串（实例密码等）替换掉。过短的值不处理，避免误伤。
func redact(s string, secrets ...string) string {
	for _, sec := range secrets {
		if len(sec) >= 4 {
			s = strings.ReplaceAll(s, sec, "******")
		}
	}
	return s
}

func dbErr(ctx context.Context, err error, what string, secrets []string) error {
	var be *Error
	if errors.As(err, &be) {
		return be
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return newErr(http.StatusGatewayTimeout, "timeout", fmt.Sprintf("%s超时", what))
	}
	if errors.Is(err, context.Canceled) {
		return newErr(http.StatusRequestTimeout, "canceled", fmt.Sprintf("%s已取消", what))
	}
	m := redact(err.Error(), secrets...)
	if strings.Contains(m, "REPLICATION CLIENT") || strings.Contains(strings.ToLower(m), "access denied") {
		return newErr(http.StatusBadGateway, "db_error", fmt.Sprintf("%s失败：%s（账号需要 REPLICATION CLIENT 权限才能执行 SHOW BINARY LOGS）", what, m))
	}
	return newErr(http.StatusBadGateway, "db_error", fmt.Sprintf("%s失败：%s", what, m))
}

// Info 返回实例的 binlog 文件列表与格式设置。
func (s *Service) Info(ctx context.Context, actor audit.Actor, instID uint) (*ServerInfo, error) {
	inst, err := s.instanceFor(actor, instID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	sess, secrets, err := s.connect(ctx, inst)
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	info, err := sess.Info(ctx)
	if err != nil {
		return nil, dbErr(ctx, err, "读取 binlog 信息", secrets)
	}
	finishInfo(info)
	return info, nil
}

func finishInfo(info *ServerInfo) {
	info.RollbackReason = info.rollbackProblem()
	info.CanRollback = info.RollbackReason == ""
	if info.Files == nil {
		info.Files = []LogFile{}
	}
}

// Databases 返回实例上用户可见的库。
func (s *Service) Databases(ctx context.Context, actor audit.Actor, instID uint) ([]string, error) {
	inst, err := s.instanceFor(actor, instID)
	if err != nil {
		return nil, err
	}
	vis, err := visibleMatcher(inst)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	sess, secrets, err := s.connect(ctx, inst)
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	all, err := sess.Databases(ctx)
	if err != nil {
		return nil, dbErr(ctx, err, "读取数据库列表", secrets)
	}
	out := []string{}
	for _, d := range all {
		if vis(d) {
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ---- 参数校验 ----

var fileNameRe = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,255}$`)

func parseServerTime(v string, loc *time.Location) (int64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, v, loc); err == nil {
			return t.Unix(), nil
		}
	}
	return 0, bad(fmt.Sprintf("时间格式应为 YYYY-MM-DD HH:MM:SS：%s", truncRunes(v, 40)))
}

func cleanList(in []string, what string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, v := range in {
		for _, p := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '，' || r == ';' || r == '\n' || r == ' ' }) {
			p = strings.TrimSpace(p)
			if p == "" || seen[strings.ToLower(p)] {
				continue
			}
			if len([]rune(p)) > maxFilterLen {
				return nil, bad(fmt.Sprintf("%s名称过长", what))
			}
			for _, r := range p {
				if r < 0x20 || r == 0x7f {
					return nil, bad(fmt.Sprintf("%s名称含有非法字符", what))
				}
			}
			seen[strings.ToLower(p)] = true
			out = append(out, p)
		}
	}
	if len(out) > maxFilterItems {
		return nil, bad(fmt.Sprintf("%s过滤条件最多 %d 个", what, maxFilterItems))
	}
	return out, nil
}

// plan 是校验后的任务参数。
type plan struct {
	req    Req
	spec   scanSpec
	limits Limits
	desc   string
	files  []LogFile // 任务开始时实例上的 binlog 文件列表：回滚分段的续接位置要按它规整
}

// HardLimits 返回页面里“最多条数”和“结果体积上限（MB）”能填的最大值：取系统设置 binlog.max_rows / binlog.max_result_mb。
// 读不到或不是正整数时用默认值；超出登记范围的值（比如直接改库写进去的）夹回范围内，不会失控。
func (s *Service) HardLimits() (rows, mb int) {
	return hardLimit(s.repo, settings.KeyBinlogMaxRows, DefaultHardRows), hardLimit(s.repo, settings.KeyBinlogMaxResultMB, DefaultHardMB)
}

func hardLimit(r Repo, key string, def int) int {
	v, ok := r.ConfigValue(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return def
	}
	if d := settings.Find(key); d != nil && d.Ranged && float64(n) > d.Max {
		return int(d.Max)
	}
	return n
}

func clampInt(v, lo, hi, def int) int {
	if v == 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func fileIndex(files []LogFile, name string) int {
	for i, f := range files {
		if f.Name == name {
			return i
		}
	}
	return -1
}

// buildPlan 校验请求并生成扫描参数。info 是目标实例当前的 binlog 信息；hardRows / hardMB 是“最多条数 / 结果体积（MB）”的硬上限（见 HardLimits）。
func buildPlan(req Req, info *ServerInfo, inst *model.Instance, hardRows, hardMB int) (*plan, error) {
	vis, err := visibleMatcher(inst)
	if err != nil {
		return nil, err
	}
	if req.Mode == "" {
		req.Mode = ModeSQL
	}
	if req.Mode != ModeSQL && req.Mode != ModeRollback {
		return nil, bad("模式只能是 sql 或 rollback")
	}
	if !info.LogBin {
		return nil, newErr(http.StatusBadRequest, "no_binlog", "该实例没有开启 binlog（log_bin=OFF）")
	}
	if req.Mode == ModeRollback {
		if why := info.rollbackProblem(); why != "" {
			return nil, newErr(http.StatusBadRequest, "rollback_unsupported", fmt.Sprintf("无法生成回滚 SQL：%s。仍可使用“还原 SQL”模式解析。", why))
		}
	}
	req.StartFile = strings.TrimSpace(req.StartFile)
	req.EndFile = strings.TrimSpace(req.EndFile)
	if req.StartFile == "" {
		return nil, bad("请选择起始 binlog 文件")
	}
	for _, f := range []string{req.StartFile, req.EndFile} {
		if f != "" && !fileNameRe.MatchString(f) {
			return nil, bad("binlog 文件名不合法")
		}
	}
	si := fileIndex(info.Files, req.StartFile)
	if si < 0 {
		return nil, bad(fmt.Sprintf("起始文件 %s 不在实例当前的 binlog 列表里（可能已被清理）", req.StartFile))
	}
	if req.StartPos == 0 {
		req.StartPos = 4
	}
	if req.StartPos < 4 {
		return nil, bad("起始位置不能小于 4（binlog 文件头占 4 字节）")
	}
	if sz := info.Files[si].Size; sz > 0 && int64(req.StartPos) >= sz {
		return nil, bad(fmt.Sprintf("起始位置 %d 超出文件 %s 的大小 %d", req.StartPos, req.StartFile, sz))
	}
	if req.EndFile == "" && req.EndPos != 0 {
		return nil, bad("指定结束位置时请同时选择结束文件")
	}
	if req.EndFile != "" {
		ei := fileIndex(info.Files, req.EndFile)
		if ei < 0 {
			return nil, bad(fmt.Sprintf("结束文件 %s 不在实例当前的 binlog 列表里", req.EndFile))
		}
		if ei < si {
			return nil, bad("结束文件不能早于起始文件")
		}
		if ei == si && req.EndPos != 0 && req.EndPos <= req.StartPos {
			return nil, bad("结束位置必须大于起始位置")
		}
	}
	loc := time.FixedZone("server", info.TZOffset)
	startTS, err := parseServerTime(req.StartTime, loc)
	if err != nil {
		return nil, err
	}
	stopTS, err := parseServerTime(req.StopTime, loc)
	if err != nil {
		return nil, err
	}
	if startTS > 0 && stopTS > 0 && stopTS <= startTS {
		return nil, bad("结束时间必须晚于开始时间")
	}
	dbs, err := cleanList(req.Databases, "数据库")
	if err != nil {
		return nil, err
	}
	tbls, err := cleanList(req.Tables, "表")
	if err != nil {
		return nil, err
	}
	req.Databases, req.Tables = dbs, tbls

	types := map[string]bool{}
	if len(req.SQLTypes) == 0 {
		types[KindInsert], types[KindUpdate], types[KindDelete] = true, true, true
	}
	for _, t := range req.SQLTypes {
		switch k := strings.ToUpper(strings.TrimSpace(t)); k {
		case KindInsert, KindUpdate, KindDelete, KindDDL:
			types[k] = true
		case "":
		default:
			return nil, bad(fmt.Sprintf("不支持的语句类型：%s", truncRunes(t, 20)))
		}
	}
	var typeList []string
	for _, k := range []string{KindInsert, KindUpdate, KindDelete, KindDDL} {
		if types[k] {
			typeList = append(typeList, strings.ToLower(k))
		}
	}
	req.SQLTypes = typeList
	req.Segment = clampInt(req.Segment, 1, maxSegment, 1)

	lim := Limits{
		MaxRows:    clampInt(req.MaxRows, 1, hardRows, min(DefaultMaxRows, hardRows)),
		MaxBytes:   clampInt(req.MaxMB, 1, hardMB, min(DefaultMaxMB, hardMB)) << 20,
		MaxScan:    MaxScanEvents,
		TimeoutSec: clampInt(req.TimeoutSec, 10, MaxTimeoutSec, DefaultTimeoutSec),
	}
	req.MaxRows, req.MaxMB, req.TimeoutSec = lim.MaxRows, lim.MaxBytes>>20, lim.TimeoutSec

	spec := scanSpec{Mode: req.Mode, StartFile: req.StartFile, StartPos: req.StartPos, EndFile: req.EndFile, EndPos: req.EndPos, StartTS: startTS, StopTS: stopTS,
		Visible: vis, Types: types, MaxRows: lim.MaxRows, MaxBytes: lim.MaxBytes, MaxScan: lim.MaxScan,
		genOpt: genOpt{NoDBPrefix: req.NoDBPrefix, FullColumns: req.FullColumns}, ExtraInfo: req.ExtraInfo, Loc: loc}
	if n := len(info.Files); n > 0 {
		spec.LastFile, spec.LastSize = info.Files[n-1].Name, info.Files[n-1].Size
	}
	if req.EndFile != "" && req.EndPos == 0 {
		// 读到结束文件的末尾：用它的当前大小做边界，这样结束文件是正在写的最后一个文件时也能确定地停下来
		if i := fileIndex(info.Files, req.EndFile); i >= 0 && info.Files[i].Size > 0 {
			spec.EndPos = uint32(info.Files[i].Size)
		}
	}
	if len(dbs) > 0 {
		spec.Schemas = map[string]bool{}
		for _, d := range dbs {
			spec.Schemas[strings.ToLower(d)] = true
		}
	}
	for _, t := range tbls {
		f := tableFilter{Table: strings.ToLower(t)}
		if i := strings.IndexByte(t, '.'); i > 0 && i < len(t)-1 {
			f.Schema, f.Table = strings.ToLower(t[:i]), strings.ToLower(t[i+1:])
		}
		spec.Tables = append(spec.Tables, f)
	}
	return &plan{req: req, spec: spec, limits: lim, desc: describe(req), files: append([]LogFile(nil), info.Files...)}, nil
}

// describe 生成范围的文字描述（审计与任务里存的中文原文）。
func describe(r Req) string { return describeIn(i18n.ZH, r) }

// describeIn 同 describe，按 lang 生成（页面展示用；中文与 describe 一致）。
func describeIn(lang string, r Req) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s:%d", r.StartFile, r.StartPos)
	b.WriteString(" → ")
	switch {
	case r.EndFile == "":
		b.WriteString(i18n.T(lang, "当前末尾"))
	case r.EndPos > 0:
		fmt.Fprintf(&b, "%s:%d", r.EndFile, r.EndPos)
	default:
		b.WriteString(i18n.T(lang, "{file} 末尾", "file", r.EndFile))
	}
	sep := i18n.T(lang, "；")
	if r.StartTime != "" || r.StopTime != "" {
		b.WriteString(sep + i18n.T(lang, "时间 {from} ~ {to}", "from", nonEmpty(r.StartTime, i18n.T(lang, "不限")), "to", nonEmpty(r.StopTime, i18n.T(lang, "不限"))))
	}
	if len(r.Databases) > 0 {
		b.WriteString(sep + i18n.T(lang, "库 {list}", "list", strings.Join(r.Databases, ",")))
	}
	if len(r.Tables) > 0 {
		b.WriteString(sep + i18n.T(lang, "表 {list}", "list", strings.Join(r.Tables, ",")))
	}
	if len(r.SQLTypes) > 0 {
		b.WriteString(sep + i18n.T(lang, "类型 {list}", "list", strings.Join(r.SQLTypes, ",")))
	}
	if r.Segment > 1 {
		b.WriteString(sep + i18n.T(lang, "第 {n} 段", "n", r.Segment))
	}
	return truncRunes(b.String(), 600)
}

func nonEmpty(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// Start 校验参数、连接实例读取 binlog 列表与变量，然后在后台开始解析。返回任务视图（此时通常还在排队 / 运行）。
// 调用方负责在调用前校验 binlog.parse（回滚模式还要 binlog.rollback）。
func (s *Service) Start(ctx context.Context, actor audit.Actor, req Req) (*JobView, error) {
	inst, err := s.instanceFor(actor, req.InstanceID)
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	sess, secrets, err := s.connect(sctx, inst)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = sess.Close()
		}
	}()
	info, err := sess.Info(sctx)
	if err != nil {
		return nil, dbErr(sctx, err, "读取 binlog 信息", secrets)
	}
	hardRows, hardMB := s.HardLimits()
	p, err := buildPlan(req, info, inst, hardRows, hardMB)
	if err != nil {
		return nil, err
	}
	p.spec.Lang = i18n.FromContext(ctx) // 结果行里的说明性注释（-- [无法回滚的…]）按发起人的语言生成
	p.spec.SpillDir, p.spec.SpillAt = s.spillDir, s.spillAt
	j := &Job{ID: newJobID(), Mode: p.req.Mode, UserID: actor.ID, Username: actor.Username, InstID: inst.ID, InstName: inst.InstanceName,
		Created: s.now(), Status: StatusQueued, Req: p.req, Range: p.desc, Limits: p.limits, Warnings: []string{}}
	if err := s.jobs.add(j, s.now(), storeBudget(hardMB)); err != nil {
		return nil, err
	}
	act := actParse
	if p.req.Mode == ModeRollback {
		act = actRollback
	}
	s.record(actor, act, fmt.Sprintf("实例:%s(#%d) 任务:%s 范围:%s", inst.InstanceName, inst.ID, j.ID, p.desc)) // i18n-ignore 审计，存库的文本不翻译
	keep = true
	s.launch(j, sess, p, secrets)
	return j.view(actor.ID), nil
}

// ---- 查询任务 ----

func (s *Service) ownedJob(actor audit.Actor, id string) (*Job, error) {
	j := s.jobs.get(id)
	if j == nil || (j.UserID != actor.ID && !actor.IsSuperuser) {
		return nil, newErr(http.StatusNotFound, "not_found", "任务不存在或已过期（结果只保留 30 分钟，服务重启后也会丢失）")
	}
	return j, nil
}

// Job 返回任务状态。只有创建者和超级用户能看。
func (s *Service) Job(actor audit.Actor, id string) (*JobView, error) {
	j, err := s.ownedJob(actor, id)
	if err != nil {
		return nil, err
	}
	return j.view(actor.ID), nil
}

// Jobs 返回仍保留的任务（新的在前）：普通用户只能看到自己的，超级用户能看到所有人的。
func (s *Service) Jobs(actor audit.Actor) []JobView {
	s.jobs.mu.Lock()
	s.jobs.gc(s.now())
	all := make([]*Job, 0, len(s.jobs.jobs))
	for _, j := range s.jobs.jobs {
		if j.UserID == actor.ID || actor.IsSuperuser {
			all = append(all, j)
		}
	}
	s.jobs.mu.Unlock()
	sort.Slice(all, func(a, b int) bool { return all[a].Created.After(all[b].Created) })
	out := make([]JobView, 0, len(all))
	for _, j := range all {
		out = append(out, *j.view(actor.ID))
		if len(out) >= 30 {
			break
		}
	}
	return out
}

// Rows 返回一页结果。
func (s *Service) Rows(actor audit.Actor, id string, offset, limit int) (*RowsPage, error) {
	j, err := s.ownedJob(actor, id)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > RowsPageMax {
		limit = RowsPageMax
	}
	if offset < 0 {
		offset = 0
	}
	j.mu.Lock()
	res, rollback := j.res, j.Mode == ModeRollback
	j.mu.Unlock()
	if res == nil {
		return &RowsPage{Rows: []Stmt{}}, nil
	}
	// 回滚结果按“后发生的先撤销”的顺序给出：存放的是正序，倒着读（大结果在临时文件里，读一页不需要把整个结果装进内存）
	rows, err := res.Stmts.Page(offset, limit, rollback)
	if err != nil {
		if errors.Is(err, errStoreClosed) {
			return nil, newErr(http.StatusNotFound, "not_found", "任务不存在或已过期（结果只保留 30 分钟，服务重启后也会丢失）")
		}
		return nil, newErr(http.StatusInternalServerError, "internal", fmt.Sprintf("读取结果失败：%v", err))
	}
	return &RowsPage{Total: res.Stmts.Len(), Offset: offset, Rows: rows}, nil
}

// Cancel 取消任务。已结束的任务返回 409。
func (s *Service) Cancel(actor audit.Actor, id string) error {
	j, err := s.ownedJob(actor, id)
	if err != nil {
		return err
	}
	j.mu.Lock()
	if !j.active() {
		j.mu.Unlock()
		return newErr(http.StatusConflict, "conflict", "任务已经结束")
	}
	j.Status = StatusCanceled
	j.Err = "已取消"
	if j.Finished.IsZero() {
		j.Finished = s.now()
	}
	cancel := j.cancel
	j.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.record(actor, actCancel, fmt.Sprintf("实例:%s(#%d) 任务:%s", j.InstName, j.InstID, j.ID)) // i18n-ignore 审计，存库的文本不翻译
	return nil
}

// Download 生成结果文件（SQL 文本，中文说明）。只有创建者和超级用户能下载；每次下载写审计。
func (s *Service) Download(actor audit.Actor, id string) (name string, body []byte, err error) {
	return s.DownloadIn(i18n.ZH, actor, id)
}

// DownloadIn 同 Download，文件头部的说明注释按 lang 生成（语句本身不受影响）。整个文件放在内存里返回：只给测试和小结果用，
// HTTP 下载走 OpenDownload，边读边写，不受结果大小限制。
func (s *Service) DownloadIn(lang string, actor audit.Actor, id string) (name string, body []byte, err error) {
	d, err := s.OpenDownload(lang, actor, id)
	if err != nil {
		return "", nil, err
	}
	var buf bytes.Buffer
	if _, err := d.WriteTo(&buf); err != nil {
		return "", nil, err
	}
	return d.Name, buf.Bytes(), nil
}

// Download 是一次待发送的结果文件：Name 是建议的文件名，WriteTo 把内容写出去。
type Download struct {
	Name  string
	svc   *Service
	actor audit.Actor
	lang  string
	meta  fileMeta
	n     int // 语句数
	inst  string
	instN uint
	jobID string
}

// OpenDownload 校验权限和任务状态，返回待发送的结果文件。只有创建者和超级用户能下载。
// 结果可能很大（几百 MB 到几 GB），所以不在这里生成内容：由 WriteTo 边读边写；审计也在写完之后记（大小是实际写出的字节数）。
func (s *Service) OpenDownload(lang string, actor audit.Actor, id string) (*Download, error) {
	j, err := s.ownedJob(actor, id)
	if err != nil {
		return nil, err
	}
	j.mu.Lock()
	if j.Status != StatusDone || j.res == nil {
		j.mu.Unlock()
		return nil, newErr(http.StatusConflict, "not_ready", "任务尚未成功完成，没有可下载的结果")
	}
	m := j.fileMetaLocked()
	d := &Download{Name: fmt.Sprintf("binlog_%d_%s_%s.sql", j.InstID, j.Mode, j.ID[:8]), svc: s, actor: actor, lang: lang, meta: m,
		n: m.store.Len(), inst: j.InstName, instN: j.InstID, jobID: j.ID}
	j.mu.Unlock()
	return d, nil
}

// WriteTo 把结果文件写到 w，然后写下载审计。读取期间结果不会被清理；写出中途失败（客户端断开等）也会记审计，并标明没传完。
func (d *Download) WriteTo(w io.Writer) (int64, error) {
	if !d.meta.store.Acquire() {
		return 0, newErr(http.StatusNotFound, "not_found", "任务不存在或已过期（结果只保留 30 分钟，服务重启后也会丢失）")
	}
	defer d.meta.store.Release()
	n, err := writeFile(w, d.meta, d.svc.now(), d.lang)
	extra := fmt.Sprintf("实例:%s(#%d) 任务:%s 模式:%s 语句数:%d 大小:%d字节", d.inst, d.instN, d.jobID, d.meta.mode, d.n, n) // i18n-ignore 审计，存库的文本不翻译
	if err != nil {
		extra += " 未传完" // i18n-ignore 审计，存库的文本不翻译
	}
	d.svc.record(d.actor, actDownload, extra)
	return n, err
}
