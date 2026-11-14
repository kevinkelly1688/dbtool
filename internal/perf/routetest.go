// @author: kevinkelly
// @license: Apache Licence
// @file: routetest.go
// @time: 2026/10/28

package perf

import (
	"fmt"
	"strings"
)

// 路由测试：输入 实例 / 库 / 表（或一条 SQL），返回会命中哪条路由规则、指派给哪个团队和负责人，以及每一步的依据。只读，不写任何数据。

// RouteTestInput 是路由测试的输入。
type RouteTestInput struct {
	InstanceID uint   `json:"instance_id"`
	DB         string `json:"db"`
	Table      string `json:"table"`      // 表名；可写多个，用逗号分隔
	SQL        string `json:"sql"`        // 可选：从 SQL 里提取涉及的表（与 table 合并）
	OwnerSide  string `json:"owner_side"` // dev（默认）/ dba：处理方为 DBA 时改派给 DBA 值班
}

// RouteTestResult 是路由测试的结果。
type RouteTestResult struct {
	Matched     bool     `json:"matched"` // 是否命中路由规则
	Rule        *Route   `json:"rule,omitempty"`
	RuleTbl     string   `json:"rule_tbl_regex,omitempty"`
	RuleInst    string   `json:"rule_instance,omitempty"`
	TeamID      uint     `json:"team_id"`
	Team        string   `json:"team"`
	TeamSrc     string   `json:"team_src"` // route / collect_config / instance_group / ""
	AssigneeID  uint     `json:"assignee_id"`
	Assignee    string   `json:"assignee"`
	AssigneeSrc string   `json:"assignee_src"` // route / db_owner / team_default / dba_rotation / ""
	DBAID       uint     `json:"dba_id"`
	DBA         string   `json:"dba"`
	Tables      []string `json:"tables"`
	Summary     string   `json:"summary"`
}

var srcLabel = map[string]string{
	"route": "命中的路由规则", "db_owner": "库负责人（数据库管理）", "team_default": "团队默认接口人", "dba_rotation": "DBA 值班轮询",
	"collect_config": "采集配置的默认团队", "instance_group": "实例所属资源组",
}

// TestRoute 模拟一次路由。
func (s *Service) TestRoute(in RouteTestInput) (*RouteTestResult, error) {
	inst, err := s.instanceOf(in.InstanceID)
	if err != nil {
		return nil, err
	}
	in.DB = strings.TrimSpace(in.DB)
	if len([]rune(in.DB)) > 128 {
		return nil, bad("库名过长")
	}
	if len(in.SQL) > 200000 {
		return nil, bad("SQL 过长")
	}
	side := in.OwnerSide
	switch side {
	case "":
		side = SideDev
	case SideDev, SideDBA:
	default:
		return nil, bad("处理方不合法")
	}
	var tables []string
	for _, t := range splitComma(strings.ReplaceAll(in.Table, "`", "")) {
		tables = append(tables, strings.ToLower(t))
	}
	if strings.TrimSpace(in.SQL) != "" {
		for _, t := range ExtractTables(in.SQL, 10) {
			if !contains(tables, t) {
				tables = append(tables, t)
			}
		}
	}
	if len(tables) > 20 {
		tables = tables[:20]
	}
	cfg := s.collectConfigFor(inst.ID, inst)
	r, tr, err := s.routeTrace(inst.ID, in.DB, side, cfg, tables)
	if err != nil {
		return nil, err
	}
	out := &RouteTestResult{Tables: nonNilStrs(tables), TeamID: r.teamID, AssigneeID: r.assigneeID, DBAID: r.dbaID, TeamSrc: tr.TeamSrc, AssigneeSrc: tr.AssigneeSrc}
	if tr.Rule != nil {
		out.Matched, out.Rule = true, tr.Rule
		if tr.Rule.InstanceID != nil {
			out.RuleInst = inst.InstanceName
		}
		if tbl := s.routeTblRegexes()[tr.Rule.ID]; tbl != nil {
			out.RuleTbl = strings.TrimSuffix(strings.TrimPrefix(tbl.String(), "^(?:"), ")$")
		}
	}
	if r.teamID != 0 {
		if g, err := s.group(r.teamID); err == nil {
			out.Team = g.Name
		}
	}
	ids := uniqueUints([]uint{r.assigneeID, r.dbaID})
	if len(ids) > 0 {
		if us, err := s.repo.GetUsers(ids); err == nil {
			out.Assignee, out.DBA = us[r.assigneeID].Name(), us[r.dbaID].Name()
		}
	}
	switch {
	case out.Matched:
		out.Summary = fmt.Sprintf("命中路由规则 #%d（优先级 %d，库名 %s）→ 团队 %s，负责人 %s", tr.Rule.ID, tr.Rule.Priority, tr.Rule.DBRegex, orDefault(out.Team, "无"), orDefault(out.Assignee, "待分派"))
	case r.teamID != 0:
		out.Summary = fmt.Sprintf("没有命中任何路由规则 → 团队取自「%s」：%s；负责人：%s", srcLabel[tr.TeamSrc], out.Team, orDefault(out.Assignee, "无，工单将停在“待分派”"))
	default:
		out.Summary = "没有命中任何路由规则，也找不到团队：工单将停在“待分派”"
	}
	return out, nil
}
