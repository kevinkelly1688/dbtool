// @author: kevinkelly
// @license: Apache Licence
// @file: board_test.go
// @time: 2026/10/28

package perf

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"dbtool/internal/notify"
	"dbtool/internal/settings"
)

// boardIssue 直接往内存仓库放一张工单，用来构造排行榜的统计数据。
func (f *fx) boardIssue(team uint, mod func(i *Issue)) *Issue {
	f.repo.nextID++
	i := &Issue{ID: f.repo.nextID, InstanceID: 1, DBName: "order", Checksum: strings.Repeat("a", 31) + string(rune('a'+f.repo.nextID)), Source: "collector",
		Severity: SevP2, Status: StAccepted, OwnerSide: SideDev, Category: CatMissingIndex, TeamID: ptr(team), CreateTime: f.now.AddDate(0, 0, -30), UpdateTime: f.now}
	mod(i)
	f.repo.issues[i.ID] = i
	return i
}

func at(t time.Time) *time.Time { return &t }

func TestBoardTeamScores(t *testing.T) {
	f := newFx(t)
	past, future := f.now.Add(-3*time.Hour), f.now.Add(10*time.Hour)
	// 订单研发（10）：1 个超时的 P0 处理中、1 个待受理的 P2、2 个本周验证关闭的（一个按时一个超时）
	f.boardIssue(10, func(i *Issue) {
		i.Severity, i.ImpactScore, i.ResolveDeadline = SevP0, 1800, &past
		i.CreateTime = f.now.AddDate(0, 0, -1)
	})
	f.boardIssue(10, func(i *Issue) {
		i.Status, i.ImpactScore, i.ResolveDeadline = StAssigned, 1800, &future
		i.CreateTime = f.now.AddDate(0, 0, -2)
	})
	f.boardIssue(10, func(i *Issue) {
		i.Status, i.CloseReason, i.BaselineDailySec = StClosed, CloseVerified, 600
		i.CreateTime, i.FixedAt, i.ClosedAt = f.now.AddDate(0, 0, -10), at(f.now.AddDate(0, 0, -5)), at(f.now.AddDate(0, 0, -4))
	})
	f.boardIssue(10, func(i *Issue) {
		i.Status, i.CloseReason, i.SLABreached, i.BaselineDailySec = StClosed, CloseVerified, true, 400
		i.CreateTime, i.ClosedAt = f.now.AddDate(0, 0, -8), at(f.now.AddDate(0, 0, -2))
	})
	// 会员研发（11）：不参与排行
	f.repo.teams = append(f.repo.teams, TeamConfig{Team: Team{ID: 101, ResourceGroupID: 11, Enabled: false}})
	f.boardIssue(11, func(i *Issue) { i.ImpactScore, i.ResolveDeadline = 500, &past })
	// DBA 处理的问题，默认（研发处理）排行榜里不统计
	f.boardIssue(10, func(i *Issue) { i.OwnerSide, i.ImpactScore, i.ResolveDeadline = SideDBA, 7200, &past })
	// 没有团队的不统计
	f.repo.nextID++
	f.repo.issues[f.repo.nextID] = &Issue{ID: f.repo.nextID, Status: StAccepted, ImpactScore: 99, CreateTime: f.now}

	b, err := f.svc.Board(7, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Teams) != 1 {
		t.Fatalf("不参与排行的团队不应出现: %+v", b.Teams)
	}
	x := b.Teams[0]
	// 健康分 = 100 − 超时1×15 − 未受理1×5 − 浪费1小时×10 − (100−50)/5 = 60
	if x.Team != "订单研发" || x.Score != 60 || x.Open != 2 || x.Overdue != 1 || x.NotAccepted != 1 || x.P0P1Open != 1 || x.New != 2 || x.Closed != 2 || x.Rank != 1 {
		t.Fatalf("%+v", x)
	}
	if x.SLARate == nil || *x.SLARate != 50 || x.AvgFixDays == nil || *x.AvgFixDays != 5.5 || x.WasteSecByDay != 3600 {
		t.Fatalf("按时率/修复天数/浪费: %+v %v %v", x, *x.SLARate, x.AvgFixDays)
	}
	// 全局：不区分处理方
	o := b.Overall
	if o.Open != 5 || o.Overdue != 3 || o.Closed != 2 || o.DBAOpen != 1 || o.SavedSecDay != 1000 || o.WasteSec != 1800+1800+500+7200+99 {
		t.Fatalf("%+v", o)
	}
	// 趋势与问题类型
	if len(b.Trend) == 0 || len(b.ByCategory) == 0 || b.ByCategory[0].Category != CatMissingIndex || b.ByCategory[0].Label != "缺少索引" {
		t.Fatalf("%+v %+v", b.Trend, b.ByCategory)
	}
	var created, closed int
	for _, w := range b.Trend {
		created += w.Created
		closed += w.Closed
	}
	if closed != 2 || created < 4 {
		t.Fatalf("趋势: %+v", b.Trend)
	}
	// 只看 DBA 处理的 / 全部
	b, _ = f.svc.Board(7, "dba")
	if len(b.Teams) != 1 || b.Teams[0].Overdue != 1 || b.Teams[0].Score != 65 { // 7200s=2h：100-15-20
		t.Fatalf("DBA 排行: %+v", b.Teams)
	}
	b, _ = f.svc.Board(7, "all")
	if b.Teams[0].Open != 3 || b.OwnerSide != "all" {
		t.Fatalf("全部: %+v", b.Teams)
	}
	// 区间外的完成不计入
	b, _ = f.svc.Board(3, "dev")
	if b.Teams[0].Closed != 1 || b.Teams[0].New != 2 {
		t.Fatalf("近 3 天: %+v", b.Teams[0])
	}
	// 参数校验
	_, err = f.svc.Board(7, "x")
	wantErr(t, err, 400, "处理方")
	if b, _ = f.svc.Board(0, ""); b.Days != 7 {
		t.Fatal("默认 7 天")
	}
	if b, _ = f.svc.Board(9999, ""); b.Days != 7 {
		t.Fatal("超范围回退")
	}
}

func TestBoardRankingOrder(t *testing.T) {
	f := newFx(t)
	past := f.now.Add(-time.Hour)
	f.repo.groups = append(f.repo.groups, GroupBrief{ID: 12, Name: "财务研发"})
	// 订单：2 个超时 → 70；会员：1 个超时 → 85；财务：没有问题关闭，只有 1 个正常的 → 100
	for k := 0; k < 2; k++ {
		f.boardIssue(10, func(i *Issue) { i.ResolveDeadline = &past })
	}
	f.boardIssue(11, func(i *Issue) { i.ResolveDeadline = &past })
	f.boardIssue(12, func(i *Issue) { i.ResolveDeadline = at(f.now.Add(time.Hour)) })
	b, _ := f.svc.Board(7, "dev")
	var got []string
	for _, x := range b.Teams {
		got = append(got, x.Team+":"+strconv.Itoa(x.Score)+":"+strconv.Itoa(x.Rank))
	}
	if strings.Join(got, ",") != "订单研发:70:1,会员研发:85:2,财务研发:100:3" {
		t.Fatalf("排序（健康分从低到高）: %v", got)
	}
	// 健康分相同按超时数再按浪费
	f.boardIssue(11, func(i *Issue) { i.Status = StAssigned })
}

func TestWeeklyReport(t *testing.T) {
	f := newFx(t)
	past := f.now.Add(-26 * time.Hour)
	f.boardIssue(10, func(i *Issue) {
		i.Severity, i.ImpactScore, i.ResolveDeadline, i.AssigneeID = SevP1, 1200, &past, ptr(uint(uDev))
		i.Fingerprint = "select * from t"
	})
	f.boardIssue(10, func(i *Issue) {
		i.Status, i.CloseReason, i.ClosedAt, i.CreateTime = StClosed, CloseVerified, at(f.now.AddDate(0, 0, -1)), f.now.AddDate(0, 0, -3)
		i.BaselineDailySec = 3600
	})
	text, err := f.svc.WeeklyReport(false)
	if err != nil {
		t.Fatal(err)
	}
	if f.n.count() != 0 {
		t.Fatal("dry-run 不发送")
	}
	for _, want := range []string{"全局：未关闭 1（超时 1）", "优化完成 1", "1. 订单研发  健康分", "按时率100%", "本周优化节省 1.0 小时/天", "/perf/board"} {
		if !strings.Contains(text, want) {
			t.Errorf("周报缺少 %q:\n%s", want, text)
		}
	}
	if _, err = f.svc.WeeklyReport(true); err != nil {
		t.Fatal(err)
	}
	if f.n.count() != 2 {
		t.Fatalf("应发两条（全局一条、团队一条）: %d", f.n.count())
	}
	mgmt, team := f.n.events[0], f.n.events[1]
	if mgmt.GroupID != 0 || !strings.HasPrefix(mgmt.Title, "【SQL性能周报】") || mgmt.Name != notify.EventPending {
		t.Fatalf("全局: %+v", mgmt)
	}
	if team.GroupID != 10 || !strings.Contains(team.Content, "[已超时]") || !hasUser(team, "zhangsan") || !strings.Contains(team.Content, "全公司倒数第 1/1") {
		t.Fatalf("团队: %+v", team)
	}
	// 静默模式不发
	f.n.reset()
	f.repo.cfg[settings.KeyPerfSilentMode] = "true"
	f.svc.WeeklyReport(true)
	if f.n.count() != 0 {
		t.Fatal("静默")
	}
}
