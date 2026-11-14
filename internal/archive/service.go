// @author: kevinkelly
// @license: Apache Licence
// @file: service.go
// @time: 2026/10/28

package archive

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dbtool/internal/archive/engine"
	"dbtool/internal/audit"
	ieng "dbtool/internal/engine"
	"dbtool/internal/model"
	"dbtool/internal/notify"
	"dbtool/internal/query"
)

// Source 是元数据：实例与解密后的连接信息。server.Repo 满足它。
type Source interface {
	ListInstances() ([]model.Instance, error)
	EngineConn(inst *model.Instance) ieng.Conn
}

// Access 是实例可见范围（query.Authorizer 满足它）。
type Access interface {
	FilterInstances(p query.Principal, list []model.Instance) ([]model.Instance, error)
	CanUseInstance(p query.Principal, inst *model.Instance) error
}

// OpenFunc 按 Spec 打开一次运行（或试运行）用到的连接 / 存储。生产用 engine.Opener.Open。
type OpenFunc func(ctx context.Context, spec engine.Spec) (*engine.Env, error)

// StoreTester 对备份存储做测试连接（写入 / 读取 / 删除探测对象）；返回耗时与可用空间（ok=false 表示未知）。
type StoreTester func(ctx context.Context, cfg engine.StoreConfig) (took time.Duration, free int64, freeOK bool, err error)

// Options 是 Service 的可选依赖，零值取生产默认。
type Options struct {
	Open      OpenFunc
	Catalog   CatalogFunc
	TestStore StoreTester
	Notifier  notify.Notifier
	Now       func() time.Time
	Spawn     func(func()) // 异步执行运行；测试里可替换成同步 / 受控
	NotifyGo  func(func()) // 异步发通知；测试里替换成同步执行
	BaseURL   string
	FileKey   func() ([]byte, error)
	Owner     string                                                                       // 租约持有者标识；空则 host:pid
	Disk      func(ctx context.Context, instanceID uint) (pct float64, ok bool, err error) // 实例磁盘剩余 %（巡检指标）；nil = 恒为“未检测”
}

// Caller 是发起操作的用户及其权限判断。Can 判断按钮权限码（archive.*、query.admin_run）。
type Caller struct {
	query.Principal
	Can  func(code string) bool
	Lang string // 请求的界面语言；空 / zh-CN 都按中文处理（后台调用没有语言）
}

func (c Caller) has(code string) bool {
	if c.IsSuperuser {
		return true
	}
	return c.Can != nil && c.Can(code)
}

func (c Caller) need(code, what string) error {
	if !c.has(code) {
		return forbidden("没有权限%s（需要 %s）", what, code)
	}
	return nil
}

// viewAll 超级用户与拥有 query.admin_run（查看 / 查询全部实例）的用户看全部资源组的任务。
func (c Caller) viewAll() bool { return c.has("query.admin_run") }

func (c Caller) actor() audit.Actor {
	return audit.Actor{ID: c.ID, Username: c.Username, Display: c.Display, IsSuperuser: c.IsSuperuser}
}

// systemCaller 是调度器 / 后台的“操作人”。
var systemCaller = Caller{Principal: query.Principal{Username: "system", Display: "数据归档调度", IsSuperuser: true}} // i18n-ignore：写进审计表的操作人名

type Service struct {
	repo     Repo
	meta     Source
	access   Access
	eng      *engine.Engine
	open     OpenFunc
	catalog  CatalogFunc
	testSt   StoreTester
	notifier notify.Notifier
	now      func() time.Time
	spawn    func(func())
	notifyGo func(func())
	baseURL  string
	fileKey  func() ([]byte, error)
	owner    string
	disk     func(ctx context.Context, instanceID uint) (float64, bool, error)

	base    context.Context // Worker 的 ctx：运行随服务关闭而停止；测试 / 未启动 Worker 时为 nil（用 Background）
	startMu sync.Mutex      // 串行化“并发上限 / 互斥检查 + 插入运行记录”
	mu      sync.Mutex
	live    map[uint]*liveRun
	lastGC  time.Time
}

func NewService(repo Repo, meta Source, access Access, opt Options) *Service {
	s := &Service{repo: repo, meta: meta, access: access, eng: engine.New(), open: opt.Open, catalog: opt.Catalog, testSt: opt.TestStore,
		notifier: opt.Notifier, now: opt.Now, spawn: opt.Spawn, notifyGo: opt.NotifyGo, baseURL: strings.TrimRight(opt.BaseURL, "/"), fileKey: opt.FileKey, owner: opt.Owner, disk: opt.Disk,
		live: map[uint]*liveRun{}}
	if s.now == nil {
		s.now = time.Now
	}
	if s.notifier == nil {
		s.notifier = notify.Noop{}
	}
	if s.spawn == nil {
		s.spawn = func(f func()) { go f() }
	}
	if s.notifyGo == nil {
		s.notifyGo = func(f func()) { go f() }
	}
	if s.owner == "" {
		host, _ := os.Hostname()
		s.owner = fmt.Sprintf("%s:%d:%d", host, os.Getpid(), time.Now().Unix())
	}
	if s.open == nil {
		s.open = func(ctx context.Context, spec engine.Spec) (*engine.Env, error) {
			return engine.Opener{Conn: s.connFor, Store: s.storeFor, FileKey: s.fileKeyOrErr, TempDir: spoolDir(), SlaveByHost: s.slaveByHost, Disk: s.disk, Lenient: s.lenientInstance}.Open(ctx, spec)
		}
	}
	if s.catalog == nil {
		s.catalog = s.defaultCatalog
	}
	if s.testSt == nil {
		s.testSt = defaultStoreTest
	}
	return s
}

// Ready 表示元数据表已创建。
func (s *Service) Ready() bool { return s.repo.Ready() }

func (s *Service) fileKeyOrErr() ([]byte, error) {
	if s.fileKey == nil {
		return nil, errors.New("未配置文件加密密钥")
	}
	return s.fileKey()
}

func defaultStoreTest(ctx context.Context, cfg engine.StoreConfig) (time.Duration, int64, bool, error) {
	fs, err := engine.NewFileStore(cfg)
	if err != nil {
		return 0, 0, false, err
	}
	took, err := fs.Test(ctx)
	if err != nil {
		return took, 0, false, err
	}
	free, ok, ferr := fs.Free(ctx)
	if ferr != nil {
		ok = false
	}
	return took, free, ok, nil
}

// ---- 实例 ----

// supportedDBTypes 是能做归档来源 / 库表备份目标的实例类型：MySQL 协议、InnoDB 事务语义的 MySQL 系。
// Doris / SelectDB / ADB 没有按主键分批删除的事务语义，不在其中。
var supportedDBTypes = map[string]bool{"mysql": true, "mariadb": true, "polardb": true, "polardb-x": true, "polardbx": true}

// lenientDBTypes 是“兼容实现”：information_schema / 会话变量 / 复制状态可能不完整，引擎遇到这类缺口时降级继续。
var lenientDBTypes = map[string]bool{"polardb-x": true, "polardbx": true}

// eligibleInstance 判断实例能否作为归档的来源 / 库表备份目标：MySQL / MariaDB / PolarDB / PolarDB-X 主库且已启用。
func eligibleInstance(in *model.Instance) error {
	switch {
	case !supportedDBTypes[strings.ToLower(in.DBType)]:
		return badReq("实例 %s 的类型（%s）不支持数据归档，目前只支持 MySQL / MariaDB / PolarDB / PolarDB-X", in.InstanceName, in.DBType)
	case in.Type != "master":
		return badReq("实例 %s 不是主库，数据归档只能连主库", in.InstanceName)
	case !in.IsEnable:
		return badReq("实例 %s 已停用", in.InstanceName)
	}
	return nil
}

func (s *Service) instances() ([]model.Instance, error) { return s.meta.ListInstances() }

func (s *Service) instanceByID(id uint) (*model.Instance, error) {
	list, err := s.instances()
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].ID == id {
			return &list[i], nil
		}
	}
	return nil, notFound("实例不存在")
}

// instanceFor 取调用者可用的实例（存在、可访问）。
func (s *Service) instanceFor(c Caller, id uint) (*model.Instance, error) {
	in, err := s.instanceByID(id)
	if err != nil {
		return nil, err
	}
	if err := s.access.CanUseInstance(c.Principal, in); err != nil {
		var qe *query.Error
		if errors.As(err, &qe) {
			return nil, newErr(qe.HTTP, qe.Code, "%s", qe.Message)
		}
		return nil, err
	}
	return in, nil
}

// lenientInstance 是 engine.Opener.Lenient：实例是兼容实现（PolarDB-X）时引擎对缺失的会话变量 / 状态降级处理。
func (s *Service) lenientInstance(_ context.Context, id uint) bool {
	in, err := s.instanceByID(id)
	return err == nil && lenientDBTypes[strings.ToLower(in.DBType)]
}

// connFor 是 engine.Opener 的 ConnResolver：实例不存在 / 已停用 / 不是主库时返回错误。
func (s *Service) connFor(_ context.Context, id uint) (ieng.Conn, string, error) {
	in, err := s.instanceByID(id)
	if err != nil {
		return ieng.Conn{}, "", err
	}
	if err := eligibleInstance(in); err != nil {
		return ieng.Conn{}, "", err
	}
	return s.meta.EngineConn(in), in.InstanceName, nil
}

// storeFor 是 engine.Opener 的 StoreResolver。
func (s *Service) storeFor(_ context.Context, id uint) (engine.StoreConfig, error) {
	row, err := s.repo.GetStore(id)
	if err != nil {
		return engine.StoreConfig{}, err
	}
	if row == nil {
		return engine.StoreConfig{}, errors.New("备份存储不存在")
	}
	if !row.Enabled {
		return engine.StoreConfig{}, fmt.Errorf("备份存储「%s」已停用", row.Name)
	}
	return storeConfigOf(row), nil
}

func storeConfigOf(r *StoreRow) engine.StoreConfig {
	return engine.StoreConfig{ID: r.ID, Name: r.Name, Type: r.Type, Path: r.Path, Endpoint: r.Endpoint, Region: r.Region, Bucket: r.Bucket,
		Prefix: r.Prefix, AK: r.AK, SK: r.SK, PathStyle: r.PathStyle, RetentionDays: r.RetentionDays}
}

// ---- 资源组 / 可见范围 ----

// callerGroups 返回调用者所属的资源组（all=true 时是全部）。
func (s *Service) callerGroups(c Caller) (map[uint]bool, error) {
	gs, err := s.repo.UserResourceGroups(c.ID, c.viewAll())
	if err != nil {
		return nil, err
	}
	m := make(map[uint]bool, len(gs))
	for _, g := range gs {
		m[g.GroupID] = true
	}
	return m, nil
}

// scope 返回按调用者限定的资源组范围。
func (s *Service) scope(c Caller) (all bool, ids []uint, err error) {
	if c.viewAll() {
		return true, nil, nil
	}
	m, err := s.callerGroups(c)
	if err != nil {
		return false, nil, err
	}
	for id := range m {
		ids = append(ids, id)
	}
	return false, ids, nil
}

func (s *Service) canSeeGroup(c Caller, rg uint) (bool, error) {
	if c.viewAll() {
		return true, nil
	}
	m, err := s.callerGroups(c)
	return m[rg], err
}

// ---- 审计 / 通知 ----

const auditPrefix = "数据归档-" // i18n-ignore：写进审计表的动作前缀

func (s *Service) auditAt(c Caller, action, extra string) {
	if r := []rune(extra); len(r) > 900 {
		extra = string(r[:900]) + "…"
	}
	e := &model.AuditEntry{UserID: c.ID, UserName: truncRunes(c.Username, 30), UserDisplay: truncRunes(c.Display, 50),
		Action: truncRunes(auditPrefix+action, 128), ExtraInfo: extra, ActionTime: s.now()}
	if err := s.repo.WriteAudit(e); err != nil {
		slog.Error("写入数据归档审计失败", "action", action, "user", c.Username, "err", err)
	}
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func (s *Service) taskLink(t *Task) string {
	if s.baseURL == "" {
		return ""
	}
	// 前端路由是 #task/ID（web/static/js/archive/main.js 的 parseHash），打开任务详情抽屉
	return fmt.Sprintf("%s/archive#task/%d", s.baseURL, t.ID)
}

func (s *Service) notifyAsync(ev notify.Event) {
	n := s.notifier
	s.notifyGo(func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("数据归档通知异常", "panic", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		n.Notify(ctx, ev)
	})
}

// instNameOf 返回实例名称（通知正文的“来源”行用）；查不到时退回 “实例 #实例ID”。
func (s *Service) instNameOf(id uint) string {
	if list, err := s.instances(); err == nil {
		for i := range list {
			if list[i].ID == id {
				return list[i].InstanceName
			}
		}
	}
	return fmt.Sprintf("实例 #%d", id) // i18n-ignore：通知正文，语言属于接收方
}

// taskContent 是通知正文的开头四行（任务、来源 = 实例名称、库表、模式），lines 是后面追加的行。inst 是实例名称（或 “实例 #实例ID”）。
func taskContent(t *Task, inst string, lines ...string) string {
	b := &strings.Builder{}
	fmt.Fprintf(b, "任务：%s\n来源：%s\n库表：%s.%s\n模式：%s", t.Name, inst, t.DBName, t.TblName, modeLabel(t.Mode)) // i18n-ignore：通知正文，语言属于接收方
	for _, l := range lines {
		if l != "" {
			b.WriteString("\n" + l)
		}
	}
	return b.String()
}

// spoolDir 是文件备份的本地暂存目录（先落盘 fsync，滚动 / 收尾时再上传到备份存储）。
// 删源发生在落盘之后、上传之前，所以它必须是持久目录，不能用系统临时目录：进程被强杀时，未上传的备份留在这里供人工补传。
// 环境变量 ARCHIVE_SPOOL_DIR 指定；缺省为 EXPORT_BASE_DIR（缺省 .）下的 archive-spool。
func spoolDir() string {
	if d := strings.TrimSpace(os.Getenv("ARCHIVE_SPOOL_DIR")); d != "" {
		return d
	}
	base := strings.TrimSpace(os.Getenv("EXPORT_BASE_DIR"))
	if base == "" {
		base = "."
	}
	return filepath.Join(base, "archive-spool")
}
