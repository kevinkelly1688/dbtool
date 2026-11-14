// @author: kevinkelly
// @license: Apache Licence
// @file: service.go
// @time: 2026/10/28

package inspect

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"dbtool/internal/model"
	"dbtool/internal/notify"
)

// Actor 是执行操作的用户（只用于审计与报告里的“操作人”）。
type Actor struct {
	ID          uint
	Username    string
	Display     string
	IsSuperuser bool
}

// Scope 是用户可见的实例范围：超级用户 / query.admin_run 看全部，其余按资源组关联的实例。
type Scope struct {
	All bool
	IDs map[uint]bool
}

func (s Scope) Has(id uint) bool { return s.All || s.IDs[id] }

// IDList 返回可见实例 ID（All 为 true 时返回 nil，调用方应看 All）。
func (s Scope) IDList() []uint {
	out := make([]uint, 0, len(s.IDs))
	for id := range s.IDs {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ChannelOption 是计划里可选的通知渠道。
type ChannelOption struct {
	ID      uint   `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Label   string `json:"label"`
	GroupID uint   `json:"group_id"`
	Enabled bool   `json:"enabled"`
}

// Notifier 把计划的汇总消息发到选定的通知渠道（复用 internal/notify 的渠道与 @ 规则）。
type Notifier interface {
	Channels() ([]ChannelOption, error)
	// Send 向每个渠道发送；返回成功的数量。渠道不存在或已停用的跳过。
	Send(ctx context.Context, channelIDs []uint, ev notify.Event) (int, error)
}

// Options 是构造服务的可选项。
type Options struct {
	Open         Opener   // 打开目标库；默认 OpenSQL
	Notifier     Notifier // 为空则计划不能配置通知
	BaseURL      string   // 对外访问地址，用于通知里的报告链接
	Now          func() time.Time
	Sleep        func(ctx context.Context, d time.Duration) // 错峰、重试等待；测试里替换
	Manual       int                                        // 手动巡检同时执行的实例数，默认 3
	Tick         time.Duration                              // 调度器检查间隔，默认 15 秒
	RetryDelay   time.Duration                              // “失败后自动重试”的等待，默认 5 分钟
	BusyPoll     time.Duration                              // 不跳过进行中巡检时的轮询间隔，默认 10 秒
	OpenRedis    RedisOpener                                // 打开 Redis 实例；为空表示不巡检 Redis（单测默认）。生产环境由 http.go 传入 OpenRedis
	RedisBigKeys int                                        // Redis 大 key 抽样的默认 key 数（环境变量 REDIS_INSPECT_BIGKEY_SAMPLE），0 表示关闭
}

// Service 是性能巡检的业务入口。
type Service struct {
	repo         Repo
	open         Opener
	notifier     Notifier
	baseURL      string
	now          func() time.Time
	sleep        func(ctx context.Context, d time.Duration)
	tick         time.Duration
	retryDelay   time.Duration
	busyPoll     time.Duration
	openRedisFn  RedisOpener
	redisBigKeyN int

	base       context.Context
	baseCancel context.CancelCauseFunc
	wg         sync.WaitGroup
	sem        chan struct{} // 手动巡检的并发限制

	mu       sync.Mutex
	cancels  map[string]context.CancelCauseFunc
	planRuns map[uint]bool // 本进程正在跑的计划
}

func NewService(repo Repo, o Options) *Service {
	s := &Service{repo: repo, open: o.Open, notifier: o.Notifier, baseURL: strings.TrimRight(o.BaseURL, "/"), now: o.Now, sleep: o.Sleep,
		tick: o.Tick, retryDelay: o.RetryDelay, busyPoll: o.BusyPoll, cancels: map[string]context.CancelCauseFunc{}, planRuns: map[uint]bool{},
		openRedisFn: o.OpenRedis, redisBigKeyN: min(max(o.RedisBigKeys, 0), redisBigKeyMax)}
	if s.open == nil {
		s.open = OpenSQL
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.sleep == nil {
		s.sleep = func(ctx context.Context, d time.Duration) {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
			case <-t.C:
			}
		}
	}
	if s.tick <= 0 {
		s.tick = 15 * time.Second
	}
	if s.retryDelay <= 0 {
		s.retryDelay = 5 * time.Minute
	}
	if s.busyPoll <= 0 {
		s.busyPoll = 10 * time.Second
	}
	n := o.Manual
	if n <= 0 {
		n = 3
	}
	s.sem = make(chan struct{}, n)
	s.base, s.baseCancel = context.WithCancelCause(context.Background())
	return s
}

// Shutdown 等待在跑的巡检结束；超过 wait 仍未结束的被中止（报告记为失败“服务退出”）。
func (s *Service) Shutdown(wait time.Duration) {
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return
	case <-time.After(wait):
	}
	slog.Warn("巡检在退出等待时间内没有结束，正在中止", "wait", wait)
	s.baseCancel(errShutdown)
	<-done
}

func (s *Service) audit(a Actor, action, extra string) {
	e := &model.AuditEntry{UserID: a.ID, UserName: a.Username, UserDisplay: a.Display, Action: "性能巡检·" + action, ExtraInfo: extra, ActionTime: s.now()}
	if err := s.repo.InsertAudit(e); err != nil {
		slog.Error("写审计日志失败", "action", action, "err", err)
	}
}

// ---- 实例 ----

// InstanceOption 是可巡检实例的选项（立即巡检、计划编辑里用）。
type InstanceOption struct {
	ID     uint     `json:"id"`
	Name   string   `json:"name"`
	DBType string   `json:"db_type"`
	Kind   string   `json:"kind"`
	Label  string   `json:"label"`
	Host   string   `json:"host"`
	Port   int      `json:"port"`
	Tags   []string `json:"tags"`
	Groups []string `json:"groups"`
}

// TagOption 是实例标签。
type TagOption struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// Instances 返回用户可见、且支持巡检的实例，按“SQL Server / MySQL 系”分组由前端处理。
func (s *Service) Instances(scope Scope) ([]InstanceOption, []TagOption, error) {
	list, err := s.repo.ListInstances()
	if err != nil {
		return nil, nil, err
	}
	var out []InstanceOption
	tagSeen := map[string]bool{}
	var tags []TagOption
	disabled := s.disabledIDs()
	for _, in := range list {
		kind, ok := s.kindOf(in.DBType)
		if !ok || !scope.Has(in.ID) || disabled[in.ID] {
			continue
		}
		o := InstanceOption{ID: in.ID, Name: in.InstanceName, DBType: in.DBType, Kind: kind, Label: KindLabel(in.DBType), Host: in.Host, Port: in.Port, Tags: []string{}, Groups: []string{}}
		for _, t := range in.Tags {
			if t.Active {
				o.Tags = append(o.Tags, t.TagCode)
				if !tagSeen[t.TagCode] {
					tagSeen[t.TagCode] = true
					tags = append(tags, TagOption{Code: t.TagCode, Name: t.TagName})
				}
			}
		}
		for _, g := range in.ResourceGroups {
			o.Groups = append(o.Groups, g.GroupName)
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	sort.Slice(tags, func(i, j int) bool { return tags[i].Code < tags[j].Code })
	return out, tags, nil
}

// ---- 报告列表与详情 ----

// ReportQuery 是列表查询参数。
type ReportQuery struct {
	InstanceID uint
	Level      string
	Trigger    string
	Search     string
	Start, End *time.Time
	Running    bool
	Latest     bool // 每个实例只取最近一次结束的报告（首页“我可见的实例”、巡检列表的“每个实例最近一次”）
	PlanID     uint
	RunID      uint
	Offset     int
	Limit      int
}

// maxReportLimit 是一次最多取多少份报告；Latest 时每个实例至多一份，上限和首页能带的实例数（2000）一致。
func maxReportLimit(latest bool) int {
	if latest {
		return 2000
	}
	return 100
}

var validLevels = map[string]bool{"": true, "alert": true, "attention": true, "clean": true, "failed": true, "cancelled": true}

func (s *Service) ListReports(scope Scope, q ReportQuery) ([]ReportSummary, int64, error) {
	if !validLevels[q.Level] {
		return nil, 0, badInput("级别筛选取值不合法")
	}
	if q.Trigger != "" && q.Trigger != TriggerManual && q.Trigger != TriggerSchedule {
		return nil, 0, badInput("触发方式筛选取值不合法")
	}
	if q.Limit <= 0 || q.Limit > maxReportLimit(q.Latest) {
		q.Limit = 20
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	f := ReportFilter{InstanceID: q.InstanceID, Level: q.Level, Trigger: q.Trigger, Search: strings.TrimSpace(q.Search), Start: q.Start, End: q.End,
		Running: q.Running, Latest: q.Latest, PlanID: q.PlanID, RunID: q.RunID, AllInst: scope.All, InstanceIDs: scope.IDList(), Offset: q.Offset, Limit: q.Limit}
	if !scope.All && len(f.InstanceIDs) == 0 {
		return []ReportSummary{}, 0, nil
	}
	return s.repo.ListReports(f)
}

// Fact 是报告摘要里的一条“键 值”。
type Fact struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Block 是报告顶部的提示块（需处理的问题、需要关注、优化建议……）。
type Block struct {
	Kind  string   `json:"kind"` // err / warn / info
	Title string   `json:"title"`
	Items []string `json:"items"`
}

// ReportView 是详情页的数据：两种报告统一成“摘要 + 提示块 + 分区”。
type ReportView struct {
	Report   ReportSummary `json:"report"`
	Engine   string        `json:"engine"`
	Facts    []Fact        `json:"facts"`
	Blocks   []Block       `json:"blocks"`
	Sections []Section     `json:"sections"`
	Note     string        `json:"note"`
}

// GetReport 取一份报告的详情；实例不在可见范围内按“不存在”处理。
func (s *Service) GetReport(scope Scope, ref ReportRef) (*ReportView, error) {
	d, err := s.repo.GetReport(ref.Kind, ref.ID)
	if err != nil {
		return nil, err
	}
	if d == nil || !scope.Has(d.InstanceID) {
		return nil, errNotFound("巡检报告")
	}
	v := &ReportView{Report: d.ReportSummary, Engine: KindLabel(d.DBType), Facts: []Fact{}, Blocks: []Block{}, Sections: []Section{}}
	switch ref.Kind {
	case KindRedis:
		s.viewRedis(v, d)
	case KindMSSQL:
		s.viewMSSQL(v, d)
	default:
		s.viewMySQL(v, d)
	}
	return v, nil
}

func (s *Service) viewMySQL(v *ReportView, d *ReportDetail) {
	if d.Result == "" {
		if d.Status == StatusFailed || d.Status == StatusCancelled {
			v.Blocks = append(v.Blocks, Block{Kind: "err", Title: "巡检没有产出结果", Items: []string{nonEmpty(d.Error, "未知原因")}})
		}
		return
	}
	var result jmap
	if err := json.Unmarshal([]byte(d.Result), &result); err != nil {
		v.Blocks = append(v.Blocks, Block{Kind: "err", Title: "报告内容无法解析", Items: []string{err.Error()}})
		return
	}
	server, capacity := sub(result, "server"), sub(result, "capacity")
	eng := sv(result, "engine")
	version := nonEmpty(sv(server, "version"), "-")
	label := KindLabel(d.DBType)
	switch eng {
	case "polardb":
		version = fmt.Sprintf("PolarDB %s（MySQL %s）", sv(result, "polardb_version"), version)
	case "polardbx":
		version = "PolarDB-X " + sv(result, "polardbx_version")
	case "adb":
		version = "AnalyticDB for MySQL 3.0"
	case "selectdb":
		version = "SelectDB"
	default:
		version = label + " " + version
	}
	v.Facts = append(v.Facts, Fact{"版本", version}, Fact{"运行时长", uptimeText(i64(server, "uptime_seconds"))})
	if len(capacity) > 0 && !has(capacity, "error") {
		v.Facts = append(v.Facts, Fact{"数据库 / 表", fmt.Sprintf("%d 个库 · %d 张表", i64(capacity, "database_count"), i64(capacity, "table_count"))},
			Fact{"数据总大小", fmt.Sprintf("%s MB", fmtNum(int64(f64(capacity, "total_data_size_mb"))))})
	}
	rep := sub(result, "inspection_report")
	for _, b := range []struct{ kind, title, key string }{{"err", "需处理的问题", "issues"}, {"warn", "需要关注", "warnings"}, {"info", "优化建议", "suggestions"}} {
		if list := strs(rep, b.key); len(list) > 0 {
			v.Blocks = append(v.Blocks, Block{Kind: b.kind, Title: b.title, Items: list})
		}
	}
	if eng != "" && eng != "mysql" {
		v.Note = fmt.Sprintf("该报告由 %s 专用巡检生成，这里只展示通用分区（连接、内存、查询、事务等），集群 / 节点分区不在这里展示。", KindLabel(eng))
	}
	if len(sub(result, "inspection_report")) == 0 {
		v.Note = "报告里没有评分信息。"
	}
	v.Sections = BuildMySQLSections(result)
	v.Report.HealthScore = coalesceScore(v.Report.HealthScore, rep)
}

func coalesceScore(cur *int, rep jmap) *int {
	if cur != nil {
		return cur
	}
	if has(rep, "health_score") {
		n := int(i64(rep, "health_score"))
		return &n
	}
	return nil
}

func (s *Service) viewMSSQL(v *ReportView, d *ReportDetail) {
	var sum jmap
	if d.Summary != "" {
		_ = json.Unmarshal([]byte(d.Summary), &sum)
	}
	if sum == nil {
		sum = jmap{}
	}
	server := sub(sum, "server")
	if len(server) > 0 {
		ver := fmt.Sprintf("SQL Server %s · %s", sv(server, "version"), strings.TrimSpace(sv(server, "level")+" "+sv(server, "cu")))
		if e := sv(server, "edition"); e != "" {
			ver += " · " + e
		}
		v.Facts = append(v.Facts, Fact{"版本", ver}, Fact{"启动时间", sv(server, "start_time")})
		if has(sum, "compare_with") {
			v.Facts = append(v.Facts, Fact{"对比窗口", fmt.Sprintf("与 #%d（%s）的差值", i64(sum, "compare_with"), sv(sum, "compare_time"))})
		} else {
			v.Facts = append(v.Facts, Fact{"对比窗口", "自实例启动以来的累计值"})
		}
		v.Facts = append(v.Facts, Fact{"CPU / 内存", fmt.Sprintf("%d 核 · %s MB · max server memory %s MB", i64(server, "cpu_count"), fmtNum(i64(server, "memory_mb")), fmtNum(i64(server, "max_memory_mb")))},
			Fact{"MAXDOP / 阈值", fmt.Sprintf("%d / %d", i64(server, "maxdop"), i64(server, "cost_threshold"))})
		names := strs(server, "db_names")
		more := ""
		if n := int(i64(server, "db_count")); n > len(names) {
			more = "、…"
		}
		v.Facts = append(v.Facts, Fact{"用户库", fmt.Sprintf("%d 个：%s%s", i64(server, "db_count"), strings.Join(names, "、"), more)})
	}
	grouped := map[string]*Section{}
	adv := msCheckAdvice()
	var order []string
	for _, sec := range msSections {
		grouped[sec[0]] = &Section{Code: sec[0], Name: sec[1]}
		order = append(order, sec[0])
	}
	for _, it := range d.Items {
		sec := grouped[it.Section]
		if sec == nil {
			sec = &Section{Code: it.Section, Name: it.Section}
			grouped[it.Section] = sec
			order = append(order, it.Section)
		}
		item := Item{Key: it.Key, Name: it.Name, Value: it.Value, Threshold: it.Threshold, Level: it.Level, DB: it.DB, Advice: msAdviceFor(adv, it.Key, it.Level)}
		if it.Detail != "" {
			var t DetailTable
			if json.Unmarshal([]byte(it.Detail), &t) == nil && len(t.Columns) > 0 {
				item.Table = &t
			}
		}
		sec.Items = append(sec.Items, item)
	}
	for _, c := range order {
		if sec := grouped[c]; len(sec.Items) > 0 {
			v.Sections = append(v.Sections, *sec)
		}
	}
	v.Sections = finalize(v.Sections)
	var top []string
	for _, t := range jrows(sum, "top") {
		line := fmt.Sprintf("[%s] %s：%s", sv(t, "section"), sv(t, "name"), sv(t, "value"))
		if db := sv(t, "db_name"); db != "" {
			line = fmt.Sprintf("[%s] %s（%s）：%s", sv(t, "section"), sv(t, "name"), db, sv(t, "value"))
		}
		top = append(top, line)
	}
	if len(top) > 0 {
		v.Blocks = append(v.Blocks, Block{Kind: "err", Title: fmt.Sprintf("需处理的 Top %d", len(top)), Items: top})
	}
	if sk := strs(sum, "skipped"); len(sk) > 0 {
		v.Blocks = append(v.Blocks, Block{Kind: "warn", Title: "未执行的检查", Items: sk})
	}
	if d.Status == StatusFailed && d.Error != "" {
		v.Blocks = append(v.Blocks, Block{Kind: "err", Title: "巡检失败", Items: []string{d.Error}})
	}
}

func nonEmpty(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func uptimeText(sec int64) string {
	if sec <= 0 {
		return "-"
	}
	return fmt.Sprintf("%d 天 %d 小时", sec/86400, sec%86400/3600)
}

// ---- 立即巡检、取消、删除 ----

// CreatedReport 是立即巡检新建的一份报告。
type CreatedReport struct {
	Ref          string `json:"ref"`
	InstanceID   uint   `json:"instance_id"`
	InstanceName string `json:"instance_name"`
}

// RunNowResult 是立即巡检的结果：哪些已提交，哪些被跳过。
type RunNowResult struct {
	Created     []CreatedReport `json:"created"`
	Busy        []string        `json:"busy"`        // 已有进行中的巡检
	Unsupported []string        `json:"unsupported"` // 类型暂不支持巡检
	NotFound    []uint          `json:"not_found"`   // 不存在或不在可见范围
	Disabled    []string        `json:"disabled"`    // 实例已停用（实例管理里停用的实例平台不再连接）
}

const maxRunNow = 100

// RunNowOptions 是立即巡检的可选项。
type RunNowOptions struct {
	RedisBigKeys bool // 对 Redis 实例做大 key 抽样（默认关闭；抽样数见 redisBigKeyDefault，且有硬上限）
}

// RunNow 为每个实例建一份报告并在后台异步执行（同时执行的数量受限）。
func (s *Service) RunNow(actor Actor, scope Scope, ids []uint) (*RunNowResult, error) {
	return s.RunNowWith(actor, scope, ids, RunNowOptions{})
}

// RunNowWith 同 RunNow，带可选项。
func (s *Service) RunNowWith(actor Actor, scope Scope, ids []uint, o RunNowOptions) (*RunNowResult, error) {
	ids = uniqUint(ids)
	if len(ids) == 0 {
		return nil, badInput("请选择要巡检的实例")
	}
	if len(ids) > maxRunNow {
		return nil, badInput("一次最多巡检 %d 个实例", maxRunNow)
	}
	res := &RunNowResult{Created: []CreatedReport{}, Busy: []string{}, Unsupported: []string{}, NotFound: []uint{}, Disabled: []string{}}
	disabled := s.disabledIDs()
	for _, id := range ids {
		inst, err := s.repo.GetInstance(id)
		if err != nil || inst == nil || !scope.Has(id) {
			res.NotFound = append(res.NotFound, id)
			continue
		}
		if disabled[id] {
			res.Disabled = append(res.Disabled, inst.InstanceName)
			continue
		}
		kind, ok := s.kindOf(inst.DBType)
		if !ok {
			res.Unsupported = append(res.Unsupported, fmt.Sprintf("%s（%s）", inst.InstanceName, KindLabel(inst.DBType)))
			continue
		}
		rid, busy, err := s.repo.CreateReport(kind, id, TriggerManual, clip(actor.Username, 30), 0, s.now(), staleManual)
		if err != nil {
			return res, err
		}
		if busy {
			res.Busy = append(res.Busy, inst.InstanceName)
			continue
		}
		ref := ReportRef{kind, rid}
		res.Created = append(res.Created, CreatedReport{Ref: ref.String(), InstanceID: id, InstanceName: inst.InstanceName})
		opt := RunOptions{}
		if o.RedisBigKeys && kind == KindRedis {
			opt.RedisBigKeys = max(s.redisBigKeyN, redisBigKeyDefault)
		}
		s.spawnManual(*inst, ref, opt)
	}
	if len(res.Created) > 0 {
		names := make([]string, len(res.Created))
		for i, c := range res.Created {
			names[i] = c.InstanceName
		}
		s.audit(actor, "立即巡检", fmt.Sprintf("实例：%s；报告：%s", strings.Join(names, "、"), refsText(res.Created)))
	}
	return res, nil
}

func refsText(c []CreatedReport) string {
	r := make([]string, len(c))
	for i, x := range c {
		r[i] = x.Ref
	}
	return strings.Join(r, "、")
}

func (s *Service) spawnManual(inst model.Instance, ref ReportRef, opt RunOptions) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select { // 排队等并发名额；排队期间报告已是“进行中 0 / 总数”
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		case <-s.base.Done():
			s.finishInterrupted(ref)
			return
		}
		if st, err := s.repo.ReportStatus(ref.Kind, ref.ID); err == nil && st != StatusRunning { // 排队期间被取消
			return
		}
		s.execute(inst, ref, opt)
	}()
}

func (s *Service) finishInterrupted(ref ReportRef) {
	if _, err := s.repo.FinishReport(ref.Kind, ref.ID, Finish{Status: StatusFailed, Error: "服务退出，巡检被中断", At: s.now()}); err != nil {
		slog.Error("巡检报告收尾失败", "report", ref.String(), "err", err)
	}
}

// ParseRefs 解析一组报告标识。
func ParseRefs(in []string) ([]ReportRef, error) {
	if len(in) == 0 {
		return nil, badInput("请选择报告")
	}
	if len(in) > 200 {
		return nil, badInput("一次最多操作 200 份报告")
	}
	seen := map[ReportRef]bool{}
	var out []ReportRef
	for _, s := range in {
		r, err := ParseRef(s)
		if err != nil {
			return nil, badInput("%v", err)
		}
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out, nil
}

// visibleRefs 过滤出实例在可见范围内的报告。
func (s *Service) visibleRefs(scope Scope, refs []ReportRef) ([]ReportRef, error) {
	var out []ReportRef
	for _, r := range refs {
		b, err := s.repo.ReportBrief(r.Kind, r.ID)
		if err != nil {
			return nil, err
		}
		if b != nil && scope.Has(b.InstanceID) {
			out = append(out, r)
		}
	}
	return out, nil
}

// CancelResult 是取消的结果。
type CancelResult struct {
	Cancelled int `json:"cancelled"`
	Skipped   int `json:"skipped"` // 不存在或已结束
}

// Cancel 取消进行中的巡检：状态先置为 cancelled，正在跑的任务在当前检查项结束后停下，已采集的部分保留。
func (s *Service) Cancel(actor Actor, scope Scope, refs []ReportRef) (*CancelResult, error) {
	vis, err := s.visibleRefs(scope, refs)
	if err != nil {
		return nil, err
	}
	done, err := s.repo.CancelReports(vis, fmt.Sprintf("%s 于 %s 取消", actor.Username, s.now().Format("2006-01-02 15:04:05")), s.now())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	for _, r := range done {
		if c := s.cancels[r.String()]; c != nil {
			c(errUserCancel)
		}
	}
	s.mu.Unlock()
	if len(done) == 0 {
		return nil, errConflict("所选报告不存在或都已结束")
	}
	names := make([]string, len(done))
	for i, r := range done {
		names[i] = r.String()
	}
	s.audit(actor, "取消巡检", "报告："+strings.Join(names, "、"))
	return &CancelResult{Cancelled: len(done), Skipped: len(refs) - len(done)}, nil
}

// DeleteResult 是删除的结果。
type DeleteResult struct {
	Deleted int `json:"deleted"`
	Running int `json:"running"` // 进行中的未删除，需先取消
}

// Delete 删除报告；进行中的跳过（先取消再删）。
func (s *Service) Delete(actor Actor, scope Scope, refs []ReportRef) (*DeleteResult, error) {
	vis, err := s.visibleRefs(scope, refs)
	if err != nil {
		return nil, err
	}
	deleted, running, err := s.repo.DeleteReports(vis)
	if err != nil {
		return nil, err
	}
	if len(deleted) == 0 {
		if len(running) > 0 {
			return nil, errConflict("进行中的巡检不能删除，请先取消")
		}
		return nil, errNotFound("巡检报告")
	}
	names := make([]string, len(deleted))
	for i, r := range deleted {
		names[i] = r.String()
	}
	s.audit(actor, "删除报告", "报告："+strings.Join(names, "、"))
	return &DeleteResult{Deleted: len(deleted), Running: len(running)}, nil
}

// disabledSource 是仓库可选实现的能力：返回在实例管理里被停用的实例 ID（旁路表 dbt_instance_ext）。
// 没实现（单测里的假仓库）或旁路表没建时视为没有停用的实例。
type disabledSource interface {
	DisabledInstanceIDs() (map[uint]bool, error)
}

// disabledIDs 取已停用实例的集合；读失败只记日志，按“没有停用”处理，不能因此让巡检整体不可用。
func (s *Service) disabledIDs() map[uint]bool {
	if d, ok := s.repo.(disabledSource); ok {
		m, err := d.DisabledInstanceIDs()
		if err != nil {
			slog.Warn("读取已停用实例失败", "err", err)
			return map[uint]bool{}
		}
		return m
	}
	return map[uint]bool{}
}
