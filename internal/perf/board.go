// @author: kevinkelly
// @license: Apache Licence
// @file: board.go
// @time: 2026/10/28

package perf

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"dbtool/internal/notify"
)

// ---- 仓库聚合结果（SQL GROUP BY） ----

// BoardQuery 是排行榜统计的条件：统计区间 [Start, End)；Now 用来判断超时；OwnerSide 为空表示全部。
type BoardQuery struct {
	Start, End, Now time.Time
	OwnerSide       string
}

// TeamAgg 是某个团队的原始聚合。
type TeamAgg struct {
	TeamID       uint
	TeamName     string
	Open         int     // 未关闭
	NotAccepted  int     // 待分派 + 待受理
	Overdue      int     // SLA 计时中且已过解决截止
	P0P1Open     int     // 未关闭的 P0/P1
	New          int     // 区间内新增
	Closed       int     // 区间内“优化完成(已验证)”关闭
	ClosedOnTime int     // 其中从未超时的
	Waste        float64 // 未关闭问题的日均浪费 DB 时间(s) 合计
	FixDaysAvg   float64 // 区间内验证关闭的工单，建单到研发标记修复(无则关闭)的平均天数
	FixDaysN     int     // 参与平均的工单数
}

// OverallAgg 是全局聚合（不区分处理方，DBA 处理的未关闭数单独给出）。
type OverallAgg struct {
	Open    int
	Overdue int
	New     int
	Closed  int
	DBAOpen int
	Waste   float64 // 未关闭问题日均浪费 DB 时间(s)
	Saved   float64 // 区间内验证关闭的工单修复前日均耗时(s) 合计，即每天节省的 DB 时间
}

// WeekCount 是某一周（周一起）的新增与优化完成数。
type WeekCount struct {
	Week    string `json:"week"` // 周一的日期
	Created int    `json:"created"`
	Closed  int    `json:"closed"`
}

// CategoryAgg 是未关闭问题按类型的聚合。
type CategoryAgg struct {
	Category string  `json:"category"`
	Label    string  `json:"label"`
	Count    int     `json:"count"`
	Waste    float64 `json:"waste"` // 日均浪费 DB 时间(s)
}

// ---- 对外结构 ----

// TeamStat 是排行榜里的一行。健康分 = 100 − 超时×15 − 未受理×5 − 日均浪费 DB 小时×10 − (100−按时率)/5，限制在 0–100。
type TeamStat struct {
	Rank          int      `json:"rank"` // 1 表示最差
	TeamID        uint     `json:"team_id"`
	Team          string   `json:"team"`
	Score         int      `json:"score"`
	Open          int      `json:"open"`
	NotAccepted   int      `json:"not_accepted"`
	Overdue       int      `json:"overdue"`
	P0P1Open      int      `json:"p0p1_open"`
	New           int      `json:"new"`
	Closed        int      `json:"closed"`
	SLARate       *float64 `json:"sla_rate"`     // 按时率(%)，期间没有完成的为空
	AvgFixDays    *float64 `json:"avg_fix_days"` // 平均修复天数
	WasteSecByDay int      `json:"waste_sec_per_day"`
}

// BoardOverall 是排行榜顶部的指标。
type BoardOverall struct {
	Open        int `json:"open"`
	Overdue     int `json:"overdue"`
	New         int `json:"new"`
	Closed      int `json:"closed"`
	DBAOpen     int `json:"dba_open"`
	WasteSec    int `json:"waste_sec_per_day"`
	SavedSecDay int `json:"saved_sec_per_day"`
}

// BoardResult 是排行榜页面的数据。
type BoardResult struct {
	Days       int           `json:"days"`
	OwnerSide  string        `json:"owner_side"`
	Teams      []TeamStat    `json:"teams"`
	Overall    BoardOverall  `json:"overall"`
	Trend      []WeekCount   `json:"trend"`
	ByCategory []CategoryAgg `json:"by_category"`
}

// Board 计算团队排行榜。days 取 1–365；ownerSide 为 dev/dba，空或 all 表示全部。
func (s *Service) Board(days int, ownerSide string) (*BoardResult, error) {
	if days <= 0 || days > 365 {
		days = 7
	}
	switch ownerSide {
	case "", "all":
		ownerSide = ""
	case SideDev, SideDBA:
	default:
		return nil, bad("处理方不合法")
	}
	now := s.now()
	q := BoardQuery{Start: now.AddDate(0, 0, -days), End: now, Now: now, OwnerSide: ownerSide}
	teams, err := s.teamStats(q)
	if err != nil {
		return nil, err
	}
	ov, err := s.repo.OverallAggregate(q)
	if err != nil {
		return nil, err
	}
	trend, err := s.repo.WeeklyTrend(now.AddDate(0, 0, -12*7))
	if err != nil {
		return nil, err
	}
	cats, err := s.repo.CategoryWaste()
	if err != nil {
		return nil, err
	}
	for i := range cats {
		cats[i].Label = labelOf(Categories, cats[i].Category)
	}
	return &BoardResult{Days: days, OwnerSide: orDefault(ownerSide, "all"), Teams: teams, Trend: nonNilWeeks(trend), ByCategory: nonNilCats(cats),
		Overall: BoardOverall{Open: ov.Open, Overdue: ov.Overdue, New: ov.New, Closed: ov.Closed, DBAOpen: ov.DBAOpen,
			WasteSec: int(math.Round(ov.Waste)), SavedSecDay: int(math.Round(ov.Saved))}}, nil
}

func nonNilWeeks(w []WeekCount) []WeekCount {
	if w == nil {
		return []WeekCount{}
	}
	return w
}

func nonNilCats(c []CategoryAgg) []CategoryAgg {
	if c == nil {
		return []CategoryAgg{}
	}
	return c
}

// teamStats 返回按健康分从低到高排序的团队统计；不参与排行（dbt_perf_team.enabled=0）的团队不出现。
func (s *Service) teamStats(q BoardQuery) ([]TeamStat, error) {
	aggs, err := s.repo.TeamAggregates(q)
	if err != nil {
		return nil, err
	}
	cfgs, err := s.repo.ListTeams()
	if err != nil {
		return nil, err
	}
	disabled := map[uint]bool{}
	for _, t := range cfgs {
		if !t.Enabled {
			disabled[t.ResourceGroupID] = true
		}
	}
	out := make([]TeamStat, 0, len(aggs))
	for _, a := range aggs {
		if disabled[a.TeamID] {
			continue
		}
		st := TeamStat{TeamID: a.TeamID, Team: a.TeamName, Open: a.Open, NotAccepted: a.NotAccepted, Overdue: a.Overdue, P0P1Open: a.P0P1Open,
			New: a.New, Closed: a.Closed, WasteSecByDay: int(math.Round(a.Waste))}
		penalty := 0.0
		if a.Closed > 0 {
			r := math.Round(float64(a.ClosedOnTime)*1000/float64(a.Closed)) / 10
			st.SLARate = &r
			penalty = (100 - r) / 5
		}
		if a.FixDaysN > 0 {
			d := math.Round(a.FixDaysAvg*10) / 10
			st.AvgFixDays = &d
		}
		score := 100 - float64(a.Overdue)*15 - float64(a.NotAccepted)*5 - a.Waste/3600*10 - penalty
		st.Score = int(math.Max(0, math.Min(100, math.Round(score))))
		out = append(out, st)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score < out[j].Score
		}
		if out[i].Overdue != out[j].Overdue {
			return out[i].Overdue > out[j].Overdue
		}
		return out[i].WasteSecByDay > out[j].WasteSecByDay
	})
	for i := range out {
		out[i].Rank = i + 1
	}
	return out, nil
}

func fmtRate(r *float64) string {
	if r == nil {
		return "-"
	}
	return fmt.Sprintf("%g%%", *r)
}

// WeeklyReport 生成并（send 为 true 时）发送周报：先发全局渠道一份全公司汇总，再发各团队渠道本团队情况并点名超时工单。
// 返回汇总文本。注意：notify 包按资源组发送时“全局渠道”也会收到各团队的消息。
func (s *Service) WeeklyReport(send bool) (string, error) {
	now := s.now()
	start := now.AddDate(0, 0, -7)
	q := BoardQuery{Start: start, End: now, Now: now, OwnerSide: SideDev}
	stats, err := s.teamStats(q)
	if err != nil {
		return "", err
	}
	ov, err := s.repo.OverallAggregate(BoardQuery{Start: start, End: now, Now: now})
	if err != nil {
		return "", err
	}
	title := fmt.Sprintf("【SQL性能周报】%s-%s", start.Format("01.02"), now.Format("01.02"))
	base := strings.TrimRight(s.BaseURL, "/")
	lines := []string{
		fmt.Sprintf("全局：未关闭 %d（超时 %d），本周新增 %d、优化完成 %d", ov.Open, ov.Overdue, ov.New, ov.Closed),
		fmt.Sprintf("未关闭问题每天浪费DB时间 %.1f 小时；本周优化节省 %.1f 小时/天", ov.Waste/3600, ov.Saved/3600),
		"",
		"团队健康度（由低到高）：",
	}
	for idx, x := range stats {
		if idx >= 15 {
			break
		}
		lines = append(lines, fmt.Sprintf("%d. %s  健康分%d｜未关闭%d 超时%d 未受理%d｜本周完成%d 按时率%s｜日均浪费%d分钟",
			x.Rank, x.Team, x.Score, x.Open, x.Overdue, x.NotAccepted, x.Closed, fmtRate(x.SLARate), x.WasteSecByDay/60))
	}
	lines = append(lines, "\n看板："+base+"/perf/board")
	text := strings.Join(lines, "\n")
	if !send {
		return text, nil
	}
	s.send(func() *notify.Event {
		return &notify.Event{Kind: KindPerfIssue, Name: notify.EventPending, GroupID: 0, Title: title, Content: text}
	})
	for _, x := range stats {
		x := x
		rows, _, err := s.repo.ListIssues(IssueQuery{View: ViewOpen, OwnerSide: SideDev, TeamID: x.TeamID, Sort: "resolve_deadline", Limit: 8, Now: now})
		if err != nil {
			return text, err
		}
		tl := []string{
			fmt.Sprintf("%s：健康分 %d（全公司倒数第 %d/%d）", x.Team, x.Score, x.Rank, len(stats)),
			fmt.Sprintf("未关闭 %d，超时 %d，未受理 %d，本周完成 %d，按时率 %s", x.Open, x.Overdue, x.NotAccepted, x.Closed, fmtRate(x.SLARate)),
			fmt.Sprintf("未关闭问题每天浪费DB时间 %d 分钟", x.WasteSecByDay/60),
		}
		var at []uint
		var todo []string
		for idx := range rows {
			r := &rows[idx]
			if !isSLARunning(r.Status) {
				continue
			}
			flag := ""
			if r.ResolveDeadline != nil {
				flag = "截止" + r.ResolveDeadline.Format("01-02")
				if r.ResolveDeadline.Before(now) {
					flag = "[已超时]"
				}
			}
			todo = append(todo, fmt.Sprintf("#%d %s %s %s 日均%.0fs %s", r.ID, r.Severity, flag, orDefault(r.AssigneeName, "待分派"), r.ImpactScore, s.issueURL(r.ID)))
			at = append(at, derefOr(r.AssigneeID))
		}
		if len(todo) > 0 {
			tl = append(tl, "待处理：")
			tl = append(tl, todo...)
		}
		content := strings.Join(tl, "\n")
		s.send(func() *notify.Event {
			return &notify.Event{Kind: KindPerfIssue, Name: notify.EventPending, GroupID: x.TeamID, Title: title, Content: content, ExtraUsers: s.usernames(at...)}
		})
	}
	return text, nil
}
