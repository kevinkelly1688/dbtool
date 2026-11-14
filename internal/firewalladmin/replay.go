// @author: kevinkelly
// @license: Apache Licence
// @file: replay.go
// @time: 2026/10/28

package firewalladmin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"dbtool/internal/firewall"
	"dbtool/internal/review"
	"dbtool/internal/sqlx"
)

// replayItem 是回放里的一条语句（来自在线查询日志或工单）。
type replayItem struct {
	scene      string
	user       string
	instID     uint
	instName   string
	dbType     string
	env        string
	db         string
	sql        string
	at         time.Time
	affectRows int64
}

func stmtKey(s string) string { return strings.TrimRight(strings.TrimSpace(s), "; \t\r\n") }

// Replay 用最近的真实语句回放一条规则（已保存的或草稿），统计会命中多少、涉及谁。不写命中表、不发通知。
// 数据来源：规则的场景含“在线查询”时取在线查询日志（dbt_query_log）；含“SQL 工单 / 数据导出”时取最近提交的工单语句。
func (s *Service) Replay(ctx context.Context, rq ReplayRequest) (*ReplayResult, error) {
	var rule firewall.Rule
	switch {
	case rq.Rule != nil:
		r, err := s.toRule(*rq.Rule)
		if err != nil {
			return nil, err
		}
		rule = r
		rule.ID = rq.RuleID
	case rq.RuleID != 0:
		m, err := s.st.GetRule(rq.RuleID)
		if err != nil {
			return nil, err
		}
		if m == nil {
			return nil, notFound("规则不存在")
		}
		r := firewall.RuleFromModel(*m)
		n, err := firewall.Normalize(r)
		if err != nil {
			return nil, badReq(fmt.Sprintf("这条规则当前无效，无法回放：%s", err.Error()))
		}
		n.ID = r.ID
		rule = n
	default:
		return nil, badReq("请指定要回放的规则")
	}
	rule.Enabled = true
	days := rq.Days
	if days <= 0 {
		days = 7
	}
	days = min(days, replayMaxDays)
	res := &ReplayResult{Days: days, Samples: []ReplaySample{}, Notes: []string{}, ByScene: map[string]int{}}
	wantQuery := contains(rule.Scenes, firewall.SceneQuery)
	wantFlow := contains(rule.Scenes, firewall.SceneWorkflow)
	wantExport := contains(rule.Scenes, firewall.SceneExport)
	res.Applicable = wantQuery || wantFlow || wantExport
	if !res.Applicable {
		res.Notes = append(res.Notes, "这条规则没有选择任何场景，没有可回放的数据。")
		return res, nil
	}
	sim, err := firewall.NewSimulator([]firewall.Rule{rule}, nil, false)
	if err != nil {
		return nil, badReq(err.Error())
	}
	since := s.now().AddDate(0, 0, -days)

	insts, err := s.instances()
	if err != nil {
		return nil, err
	}
	byName := map[string]InstanceItem{}
	byID := map[uint]InstanceItem{}
	for _, i := range insts {
		byName[i.Name] = i
		byID[i.ID] = i
	}

	var items []replayItem
	queryLogs := 0
	if wantQuery {
		logs, err := s.st.QueryLogs(since, replayMaxRows)
		if err != nil {
			return nil, err
		}
		if len(logs) >= replayMaxRows {
			res.Truncated = true
		}
		queryLogs = len(logs)
		for _, l := range logs {
			in := byName[l.InstanceName]
			items = append(items, replayItem{scene: firewall.SceneQuery, user: l.Username, instID: in.ID, instName: l.InstanceName, dbType: in.DBType,
				env: in.Env, db: l.DBName, sql: l.SQLLog, at: l.CreateTime, affectRows: -1})
		}
	}
	if wantFlow || wantExport {
		wfs, err := s.st.Workflows(since, replayMaxWorkflows)
		if err != nil {
			return nil, err
		}
		if len(wfs) >= replayMaxWorkflows {
			res.Truncated = true
		}
		for _, w := range wfs {
			scene := firewall.SceneWorkflow
			if w.Export {
				scene = firewall.SceneExport
			}
			if (scene == firewall.SceneWorkflow && !wantFlow) || (scene == firewall.SceneExport && !wantExport) {
				continue
			}
			affect := map[string]int64{}
			if rows, err := review.ParseRows(w.Review); err == nil {
				for _, r := range rows {
					if r.AffectedRows > 0 { // 0 视为未知（原生降级检查没有影响行数）
						affect[stmtKey(r.SQL)] = int64(r.AffectedRows)
					}
				}
			}
			in := byID[w.InstanceID]
			for _, st := range sqlx.Split(w.SQL) {
				a := int64(-1)
				if v, ok := affect[stmtKey(st)]; ok {
					a = v
				}
				items = append(items, replayItem{scene: scene, user: w.Engineer, instID: in.ID, instName: in.Name, dbType: in.DBType,
					env: in.Env, db: w.DBName, sql: st, at: w.At, affectRows: a})
			}
		}
	}

	seen := map[string]bool{}
	var names []string
	for _, it := range items {
		if !seen[it.user] {
			seen[it.user] = true
			names = append(names, it.user)
		}
	}
	groupsOf, err := s.st.UserGroups(names)
	if err != nil {
		return nil, err
	}
	var dba uint
	if gs, err := s.st.Groups(); err == nil {
		for _, g := range gs {
			if strings.EqualFold(strings.TrimSpace(g.Name), "DBA") {
				dba, res.DBAKnown = g.ID, true
			}
		}
	}

	deadline := time.Now().Add(replayBudget)
	hitUsers := map[string]bool{}
	for i, it := range items {
		if i%200 == 0 && (ctx.Err() != nil || time.Now().After(deadline)) {
			res.Truncated = true
			break
		}
		if i >= replayMaxRows {
			res.Truncated = true
			break
		}
		res.Scanned++
		res.ByScene[it.scene]++
		sql := it.sql
		d := sim.Run(firewall.Request{
			Scene: it.scene, User: it.user, GroupIDs: nz(groupsOf[it.user]), InstanceID: it.instID, InstanceName: it.instName,
			DBType: it.dbType, Env: it.env, DB: it.db, SQL: sql, Caps: capsOf(it.scene), AffectRows: it.affectRows, Now: it.at,
			Tables: func() ([]firewall.Object, error) { return benchTables(sql), nil },
		})
		if len(d.Hits) == 0 {
			for _, sk := range d.Skipped {
				if sk.Unknown {
					res.Unevaluated++
					break
				}
			}
			continue
		}
		res.Hits++
		hitUsers[it.user] = true
		if dba != 0 && contains2(groupsOf[it.user], dba) {
			res.DBAHits++
		}
		if len(res.Samples) < replaySamples {
			h := d.Hits[0]
			res.Samples = append(res.Samples, ReplaySample{
				Scene: it.scene, SQL: truncate(strings.Join(strings.Fields(sql), " "), 160), User: it.user, Instance: it.instName, DB: it.db,
				At: timeText(it.at), Why: nz(h.Matched), Would: rule.Action,
			})
		}
	}
	res.Users = len(hitUsers)
	if res.Scanned > 0 {
		res.Rate = float64(res.Hits) / float64(res.Scanned)
	}
	switch {
	case res.Scanned == 0:
		res.Advice = fmt.Sprintf("最近 %d 天没有可回放的语句，没有数据可评估。", days)
	case res.Hits == 0:
		res.Advice = "样本里没有语句命中这条规则，不会影响任何人。"
	case res.Rate > replayAdviceAt:
		res.Advice = fmt.Sprintf("命中 %.1f%%（超过 1%%）：建议先用观察模式跑一周，看过命中记录再决定是否生效。", res.Rate*100)
	default:
		res.Advice = "命中很少，涉及的用户见下方样例；仍建议先观察一段时间再生效。"
	}
	if queryLogs > 0 {
		res.Notes = append(res.Notes, "在线查询日志里的语句是系统追加 LIMIT 之后的形态，“没有 LIMIT”类条件对在线查询偏乐观。")
	}
	if wantFlow || wantExport {
		res.Notes = append(res.Notes, "工单语句按分号切分后逐条评估；预估影响行数取自提交时 goInception 检查结果里的值，没有该值（原生降级检查）的语句视为未知。")
	}
	if res.Unevaluated > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("有 %d 条语句因预估扫描 / 影响行数不可用，规则里的这类条件没能评估（回放不做 EXPLAIN）。", res.Unevaluated))
	}
	if res.Truncated {
		res.Notes = append(res.Notes, fmt.Sprintf("样本已达预算上限（最多 %d 条 / %d 秒），只统计了最近的 %d 条。", replayMaxRows, int(replayBudget.Seconds()), res.Scanned))
	}
	return res, nil
}

func contains2(list []uint, v uint) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
