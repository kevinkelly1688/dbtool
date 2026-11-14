// @author: kevinkelly
// @license: Apache Licence
// @file: fixture_test.go
// @time: 2026/10/28

package perf

import (
	"context"
	"sync"
	"testing"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/model"
	"dbtool/internal/notify"
	"dbtool/internal/settings"
)

// recNotifier 记录发出的通知。
type recNotifier struct {
	mu     sync.Mutex
	events []notify.Event
}

func (r *recNotifier) Notify(_ context.Context, ev notify.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recNotifier) last() notify.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return notify.Event{}
	}
	return r.events[len(r.events)-1]
}

func (r *recNotifier) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func (r *recNotifier) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = nil
}

// 固定的用户 ID。
const (
	uRoot     = 1
	uDev      = 2 // zhangsan，订单研发
	uDev2     = 3 // lisi，会员研发
	uTL       = 4 // 订单研发 TL
	uDirector = 5
	uDBA      = 6 // dba1，perf_manage
	uStranger = 7 // 没有任何资源组
)

// fx 是服务测试的夹具：内存仓库、可调的时钟、同步通知。
type fx struct {
	t    *testing.T
	repo *memRepo
	svc  *Service
	n    *recNotifier
	now  time.Time
}

func newFx(t *testing.T) *fx {
	t.Helper()
	r := newMemRepo()
	f := &fx{t: t, repo: r, n: &recNotifier{}, now: time.Date(2026, 10, 6, 10, 0, 0, 0, time.Local)} // 周二
	for _, u := range []UserBrief{
		{ID: uRoot, Username: "root", Display: "管理员", Active: true},
		{ID: uDev, Username: "zhangsan", Display: "张三", Active: true},
		{ID: uDev2, Username: "lisi", Display: "李四", Active: true},
		{ID: uTL, Username: "tl", Display: "王TL", Active: true},
		{ID: uDirector, Username: "boss", Display: "总监", Active: true},
		{ID: uDBA, Username: "dba1", Display: "DBA小刘", Active: true},
		{ID: uStranger, Username: "bob", Display: "鲍勃", Active: true},
		{ID: 8, Username: "gone", Display: "已离职", Active: false},
	} {
		r.users[u.ID] = u
	}
	r.groups = []GroupBrief{{ID: 10, Name: "订单研发", Webhooks: []string{"钉钉"}}, {ID: 11, Name: "会员研发"}}
	r.ugroups[uDev], r.ugroups[uTL], r.ugroups[uDev2] = []uint{10}, []uint{10}, []uint{11}
	r.insts[1] = &model.Instance{ID: 1, InstanceName: "order-db", DBType: "mysql", Host: "10.0.0.1", Port: 3306}
	r.insts[2] = &model.Instance{ID: 2, InstanceName: "report-doris", DBType: "doris", Host: "10.0.0.2", Port: 9030}
	r.insts[3] = &model.Instance{ID: 3, InstanceName: "pg-1", DBType: "pgsql", Host: "10.0.0.3", Port: 5432}
	r.igroups[1] = []uint{10}
	r.igroups[2] = []uint{11}
	r.teams = []TeamConfig{{Team: Team{ID: 100, ResourceGroupID: 10, DefaultAssigneeID: ptr(uint(uDev)), Enabled: true},
		LeaderIDs: []uint{uTL}, DirectorIDs: []uint{uDirector}}}
	r.owners["1:order"] = "zhangsan"
	r.cfg[settings.KeyPerfDBAUsers] = "dba1"
	r.cfg[settings.KeyPerfQuietHours] = "0-0"
	r.cfg[settings.KeyPerfWeekendSilent] = "false"
	r.collect = []CollectConfig{{ID: 1, InstanceID: 1, Product: ProdMySQL, Collector: CollPlatformSlow, MinAvgMs: 1000, MinTotalSec: 10,
		ExcludeRegex: defaultExcludeRegex, ExcludeDBs: defaultExcludeDBs, Enabled: true}}
	f.svc = NewService(r, Options{Notifier: f.n, BaseURL: "http://dba.example.com", Collectors: map[string]Collector{}})
	f.svc.SetClock(func() time.Time { return f.now })
	f.svc.SetAsync(func(fn func()) { fn() })
	return f
}

func (f *fx) dev() Viewer {
	return Viewer{Actor: audit.Actor{ID: uDev, Username: "zhangsan", Display: "张三"}, Groups: map[uint]bool{10: true}}
}

func (f *fx) dev2() Viewer {
	return Viewer{Actor: audit.Actor{ID: uDev2, Username: "lisi", Display: "李四"}, Groups: map[uint]bool{11: true}}
}

func (f *fx) stranger() Viewer {
	return Viewer{Actor: audit.Actor{ID: uStranger, Username: "bob", Display: "鲍勃"}, Groups: map[uint]bool{}}
}

func (f *fx) dba() Viewer {
	return Viewer{Actor: audit.Actor{ID: uDBA, Username: "dba1", Display: "DBA小刘"}, Manage: true, Groups: map[uint]bool{}}
}

func (f *fx) root() Viewer {
	return Viewer{Actor: audit.Actor{ID: uRoot, Username: "root", Display: "管理员", IsSuperuser: true}}
}

func (f *fx) cfg() *CollectConfig {
	c, _ := f.repo.GetCollectConfig(1)
	return c
}

func (f *fx) inst() *model.Instance { i, _ := f.repo.GetInstance(1); return i }

// digest 造一条采集结果：库 order，默认窗口内的 cnt 次执行共 total 秒。
func (f *fx) digest(sql string, cnt int64, total float64) Digest {
	now := f.now
	return Digest{DBName: "order", SampleSQL: sql, ExecCount: cnt, TotalSec: total, MaxMs: 5000, RowsExamined: float64(cnt) * 100000, RowsSent: float64(cnt) * 10,
		FirstSeen: &now, LastSeen: &now, SampleReal: true}
}

// ingest 入库一批采集结果并返回统计。
func (f *fx) ingest(ds ...Digest) IngestResult {
	f.t.Helper()
	res, err := f.svc.Ingest(context.Background(), f.cfg(), f.inst(), ds, f.now)
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

const sqlOrderByUser = "select * from t_order where user_id = 1"

// newIssue 建一张工单（日均浪费 4000 秒 → P0，库 order 的负责人是张三，团队订单研发，跟进 DBA 是 dba1）。
func (f *fx) newIssue() uint {
	f.t.Helper()
	f.ingest(f.digest(sqlOrderByUser, 1000, 4000))
	i, _ := f.repo.FindIssue(1, "order", Checksum(Fingerprint(sqlOrderByUser)))
	if i == nil {
		f.t.Fatal("没有建出工单")
	}
	return i.ID
}

func (f *fx) issue(id uint) *Issue {
	f.t.Helper()
	i, err := f.repo.GetIssue(id)
	if err != nil || i == nil {
		f.t.Fatalf("取工单 %d 失败: %v", id, err)
	}
	return i
}

func (f *fx) act(v Viewer, id uint, in ActionInput) (*ActionResult, error) {
	return f.svc.Act(v, id, in)
}

func (f *fx) mustAct(v Viewer, id uint, in ActionInput) {
	f.t.Helper()
	if _, err := f.svc.Act(v, id, in); err != nil {
		f.t.Fatalf("%s 失败: %v", in.Action, err)
	}
}

// accept 让张三受理（承诺明天）。
func (f *fx) accept(id uint) {
	f.t.Helper()
	f.mustAct(f.dev(), id, ActionInput{Action: ActAccept, PlanFinishDate: f.now.AddDate(0, 0, 1).Format(dateLayout)})
}

func hasUser(ev notify.Event, name string) bool { return contains(ev.ExtraUsers, name) }
