// @author: kevinkelly
// @license: Apache Licence
// @file: config.go
// @time: 2026/10/28

package perf

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// 采集配置 / 团队配置 / 路由规则 / SLA 策略 的增删改查与校验。
// 在页内维护（按钮 perfcfg.*）；每次变更写 dbt_audit_log。

// ---- 采集配置 ----

// CollectConfigInput 是新增/修改采集配置的输入。
type CollectConfigInput struct {
	ID           uint   `json:"id"`
	InstanceID   uint   `json:"instance_id"`
	Product      string `json:"product"`
	Collector    string `json:"collector"`
	CustomSQL    string `json:"custom_sql"`
	MinAvgMs     int    `json:"min_avg_ms"`
	MinTotalSec  int    `json:"min_total_sec"`
	ExcludeRegex string `json:"exclude_regex"`
	ExcludeDBs   string `json:"exclude_dbs"`
	TeamID       uint   `json:"team_id"` // 0 表示取实例关联的第一个资源组
	Enabled      bool   `json:"enabled"`
}

// 采集方式与实例类型的对应：平台慢日志（dbt_slow_logs）只覆盖原采集任务读取的类型；其余按各自读取的系统表限定。
var collectorDBTypes = map[string][]string{
	CollPlatformSlow: {"mysql", "polardb", "polardb-x", "selectdb", "doris"},
	CollPtDigest:     {"mysql"},
	CollPerfSchema:   {"mysql", "polardb"},
	CollSlowLogTable: {"mysql", "polardb"},
	CollDorisAudit:   {"selectdb", "doris"},
	CollPolarDBXSum:  {"polardb-x"},
	CollPolarDBXSlow: {"polardb-x"},
}

// buildCollectConfig 校验输入并构造配置；preview 为 true 时不检查“一个实例只能有一条配置”。
func (s *Service) buildCollectConfig(in CollectConfigInput, preview bool) (*CollectConfig, *model.Instance, error) {
	inst, err := s.instanceOf(in.InstanceID)
	if err != nil {
		return nil, nil, err
	}
	if labelOf(Products, in.Product) == in.Product {
		return nil, nil, bad("产品类型不合法")
	}
	if labelOf(Collectors, in.Collector) == in.Collector {
		return nil, nil, bad("采集方式不合法")
	}
	if allowed, ok := collectorDBTypes[in.Collector]; ok && !contains(allowed, inst.DBType) {
		return nil, nil, bad(fmt.Sprintf("采集方式“%s”不适用于 %s 类型的实例（适用：%s）", labelOf(Collectors, in.Collector), inst.DBType, strings.Join(allowed, "、")))
	}
	// 阈值为 0 等于“所有 SQL 都建单”，会瞬间产生海量工单，所以必须大于 0。
	if in.MinAvgMs <= 0 || in.MinAvgMs > 36_000_000 {
		return nil, nil, bad("平均耗时阈值必须大于 0（1 到 36000000 毫秒）")
	}
	if in.MinTotalSec <= 0 || in.MinTotalSec > 31_536_000 {
		return nil, nil, bad("窗口总耗时阈值必须大于 0（1 到 31536000 秒）")
	}
	in.ExcludeRegex = strings.TrimSpace(in.ExcludeRegex)
	if len([]rune(in.ExcludeRegex)) > 500 {
		return nil, nil, bad("排除规则过长（最多 500 个字符）")
	}
	if in.ExcludeRegex != "" {
		if _, err := regexp.Compile(in.ExcludeRegex); err != nil {
			return nil, nil, bad(fmt.Sprintf("排除规则不是合法的正则：%s", err.Error()))
		}
	}
	in.ExcludeDBs = strings.TrimSpace(in.ExcludeDBs)
	if len([]rune(in.ExcludeDBs)) > 500 {
		return nil, nil, bad("排除库过长（最多 500 个字符）")
	}
	custom := strings.TrimSpace(in.CustomSQL)
	if in.Collector == CollCustomSQL {
		if err := ValidateCustomSQL(custom); err != nil {
			return nil, nil, bad(err.Error())
		}
	} else if custom != "" {
		if err := ValidateCustomSQL(custom); err != nil { // 不是自定义方式也不允许存一段危险 SQL
			return nil, nil, bad(err.Error())
		}
	}
	var teamID *uint
	if in.TeamID != 0 {
		g, err := s.group(in.TeamID)
		if err != nil {
			return nil, nil, err
		}
		teamID = ptr(g.ID)
	}
	if !preview {
		if old, err := s.repo.GetCollectConfigByInstance(in.InstanceID); err != nil {
			return nil, nil, err
		} else if old != nil && old.ID != in.ID {
			return nil, nil, conflict("该实例已有采集配置，请直接编辑")
		}
	}
	cfg := &CollectConfig{ID: in.ID, InstanceID: in.InstanceID, Product: in.Product, Collector: in.Collector, CustomSQL: custom, MinAvgMs: in.MinAvgMs,
		MinTotalSec: in.MinTotalSec, ExcludeRegex: in.ExcludeRegex, ExcludeDBs: in.ExcludeDBs, TeamID: teamID, Enabled: in.Enabled}
	return cfg, inst, nil
}

// SaveCollectConfig 新增（ID 为 0）或修改采集配置。修改时保留采集进度（last_collect_time / last_status）。
func (s *Service) SaveCollectConfig(v Viewer, in CollectConfigInput) (*CollectConfig, error) {
	if !v.IsDBA() {
		return nil, forbid("需要 DBA(性能工单管理) 权限")
	}
	act := "新增"
	var old *CollectConfig
	if in.ID != 0 {
		var err error
		if old, err = s.repo.GetCollectConfig(in.ID); err != nil {
			return nil, err
		}
		if old == nil {
			return nil, notFound("采集配置不存在")
		}
		if old.InstanceID != in.InstanceID {
			return nil, bad("不能更换采集配置所属的实例，请删除后重建")
		}
		act = "修改"
	}
	cfg, inst, err := s.buildCollectConfig(in, false)
	if err != nil {
		return nil, err
	}
	if old != nil {
		cfg.LastCollectTime, cfg.LastStatus = old.LastCollectTime, old.LastStatus
	}
	if err := s.repo.SaveCollectConfig(cfg); err != nil {
		return nil, err
	}
	s.audit(whoOf(v.Actor), "性能工单配置-采集配置"+act, fmt.Sprintf("实例 %s，%s，启用=%v，阈值 %dms/%ds", inst.InstanceName, labelOf(Collectors, cfg.Collector), cfg.Enabled, cfg.MinAvgMs, cfg.MinTotalSec))
	return cfg, nil
}

// DeleteCollectConfig 删除采集配置（已建的工单不受影响）。
func (s *Service) DeleteCollectConfig(v Viewer, id uint) error {
	if !v.IsDBA() {
		return forbid("需要 DBA(性能工单管理) 权限")
	}
	cfg, err := s.repo.GetCollectConfig(id)
	if err != nil {
		return err
	}
	if cfg == nil {
		return notFound("采集配置不存在")
	}
	if err := s.repo.DeleteCollectConfig(id); err != nil {
		return err
	}
	name := fmt.Sprint(cfg.InstanceID)
	if inst, _ := s.repo.GetInstance(cfg.InstanceID); inst != nil {
		name = inst.InstanceName
	}
	s.audit(whoOf(v.Actor), "性能工单配置-采集配置删除", fmt.Sprintf("实例 %s", name))
	return nil
}

// InitResult 是批量生成采集配置的结果。
type InitResult struct {
	Created int `json:"created"`
	Skipped int `json:"skipped"` // 已有配置的实例数
}

// InitCollectConfigs 为支持的实例类型批量生成默认采集配置（按类型选产品与采集方式）。生成的配置默认停用，DBA 确认后再启用。
func (s *Service) InitCollectConfigs(v Viewer) (*InitResult, error) {
	if !v.IsDBA() {
		return nil, forbid("需要 DBA(性能工单管理) 权限")
	}
	insts, err := s.repo.ListInstances(SupportedDBTypes)
	if err != nil {
		return nil, err
	}
	res := &InitResult{}
	for _, in := range insts {
		old, err := s.repo.GetCollectConfigByInstance(in.ID)
		if err != nil {
			return nil, err
		}
		if old != nil {
			res.Skipped++
			continue
		}
		c := defaultCollectConfig(in.ID, in.DBType)
		c.Enabled = false
		if err := s.repo.SaveCollectConfig(c); err != nil {
			return nil, err
		}
		res.Created++
	}
	s.audit(whoOf(v.Actor), "性能工单配置-批量生成采集配置", fmt.Sprintf("新建 %d 条（默认停用），已有配置 %d 个实例", res.Created, res.Skipped))
	return res, nil
}

// CollectRow 是采集配置列表的一行。
type CollectRow struct {
	CollectConfig
	Instance     string `json:"instance"`
	DBType       string `json:"db_type"`
	ProductLabel string `json:"product_label"`
	CollectLabel string `json:"collector_label"`
	Team         string `json:"team"`
	LastCollect  string `json:"last_collect_time_text"`
}

// CollectOverview 是采集配置页的数据。
type CollectOverview struct {
	Rows       []CollectRow    `json:"rows"`
	Instances  []InstanceBrief `json:"instances"`
	Teams      []GroupBrief    `json:"teams"`
	Products   []Option        `json:"products"`
	Collectors []Option        `json:"collectors"`
	Jobs       JobsStatus      `json:"jobs"`
	Window     WindowInfo      `json:"window"` // 采集窗口（页内可改）
}

func (s *Service) CollectOverview() (*CollectOverview, error) {
	cfgs, err := s.repo.ListCollectConfigs()
	if err != nil {
		return nil, err
	}
	insts, err := s.repo.ListInstances(SupportedDBTypes)
	if err != nil {
		return nil, err
	}
	groups, err := s.repo.ListGroups()
	if err != nil {
		return nil, err
	}
	byInst := map[uint]InstanceBrief{}
	for _, i := range insts {
		byInst[i.ID] = i
	}
	byGroup := map[uint]string{}
	for _, g := range groups {
		byGroup[g.ID] = g.Name
	}
	out := &CollectOverview{Rows: []CollectRow{}, Instances: insts, Teams: groups, Products: Products, Collectors: Collectors, Jobs: s.JobsStatus(), Window: s.windowInfo()}
	for _, c := range cfgs {
		in := byInst[c.InstanceID]
		if in.ID == 0 { // 实例已被删除或类型不在支持范围：仍然列出，便于清理
			in = InstanceBrief{ID: c.InstanceID, Name: fmt.Sprintf("#%d", c.InstanceID)}
		}
		r := CollectRow{CollectConfig: c, Instance: in.Name, DBType: in.DBType, ProductLabel: labelOf(Products, c.Product), CollectLabel: labelOf(Collectors, c.Collector),
			LastCollect: fmtTime(c.LastCollectTime)}
		if c.TeamID != nil {
			r.Team = byGroup[*c.TeamID]
		}
		out.Rows = append(out.Rows, r)
	}
	sort.SliceStable(out.Rows, func(a, b int) bool { return out.Rows[a].Instance < out.Rows[b].Instance })
	return out, nil
}

// ---- 团队配置 ----

// TeamInput 是新增/修改团队配置的输入。
type TeamInput struct {
	ID                uint   `json:"id"`
	ResourceGroupID   uint   `json:"resource_group_id"`
	DefaultAssigneeID uint   `json:"default_assignee_id"`
	LeaderIDs         []uint `json:"leader_ids"`
	DirectorIDs       []uint `json:"director_ids"`
	Enabled           bool   `json:"enabled"`
	Remark            string `json:"remark"`
}

func (s *Service) activeUsers(ids []uint, what string) ([]uint, error) {
	ids = uniqueUints(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > 50 {
		return nil, bad(fmt.Sprintf("%s最多 50 人", what))
	}
	us, err := s.repo.GetUsers(ids)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if u, ok := us[id]; !ok || !u.Active {
			return nil, bad(fmt.Sprintf("%s里有不存在或已停用的用户（#%d）", what, id))
		}
	}
	return ids, nil
}

// SaveTeam 新增（ID 为 0）或修改团队配置。团队就是资源组，这里只补充性能工单需要的信息。
func (s *Service) SaveTeam(v Viewer, in TeamInput) (*TeamConfig, error) {
	if !v.IsDBA() {
		return nil, forbid("需要 DBA(性能工单管理) 权限")
	}
	var cur *TeamConfig
	if in.ID != 0 {
		var err error
		if cur, err = s.repo.GetTeam(in.ID); err != nil {
			return nil, err
		}
		if cur == nil {
			return nil, notFound("团队配置不存在")
		}
	}
	g, err := s.group(in.ResourceGroupID)
	if err != nil {
		return nil, err
	}
	in.Remark = strings.TrimSpace(in.Remark)
	if len([]rune(in.Remark)) > 200 {
		return nil, bad("备注过长（最多 200 个字符）")
	}
	t := &TeamConfig{Team: Team{ID: in.ID, ResourceGroupID: g.ID, Enabled: in.Enabled, Remark: in.Remark}}
	if in.DefaultAssigneeID != 0 {
		if _, err := s.activeUsers([]uint{in.DefaultAssigneeID}, "默认接口人"); err != nil {
			return nil, err
		}
		t.DefaultAssigneeID = ptr(in.DefaultAssigneeID)
	}
	if t.LeaderIDs, err = s.activeUsers(in.LeaderIDs, "团队 TL"); err != nil {
		return nil, err
	}
	if t.DirectorIDs, err = s.activeUsers(in.DirectorIDs, "总监"); err != nil {
		return nil, err
	}
	old, err := s.repo.GetTeamByGroup(g.ID)
	if err != nil {
		return nil, err
	}
	if old != nil && old.ID != in.ID {
		return nil, conflict("该资源组已有团队配置，请直接编辑")
	}
	act := "新增"
	if cur != nil {
		if cur.ResourceGroupID != g.ID {
			return nil, bad("不能更换团队配置所属的资源组，请删除后重建")
		}
		act = "修改"
	}
	if err := s.repo.SaveTeam(t); err != nil {
		return nil, err
	}
	s.audit(whoOf(v.Actor), "性能工单配置-团队配置"+act, fmt.Sprintf("团队 %s，TL %d 人，总监 %d 人，参与排行=%v", g.Name, len(t.LeaderIDs), len(t.DirectorIDs), t.Enabled))
	return t, nil
}

func (s *Service) DeleteTeam(v Viewer, id uint) error {
	if !v.IsDBA() {
		return forbid("需要 DBA(性能工单管理) 权限")
	}
	t, err := s.repo.GetTeam(id)
	if err != nil {
		return err
	}
	if t == nil {
		return notFound("团队配置不存在")
	}
	if err := s.repo.DeleteTeam(id); err != nil {
		return err
	}
	name := fmt.Sprint(t.ResourceGroupID)
	if g, err := s.group(t.ResourceGroupID); err == nil {
		name = g.Name
	}
	s.audit(whoOf(v.Actor), "性能工单配置-团队配置删除", fmt.Sprintf("团队 %s", name))
	return nil
}

// TeamRow 是团队配置列表的一行。
type TeamRow struct {
	ID              uint     `json:"id"`
	ResourceGroupID uint     `json:"resource_group_id"`
	Team            string   `json:"team"`
	DefaultAssignee string   `json:"default_assignee"`
	DefaultID       uint     `json:"default_assignee_id"`
	Leaders         []string `json:"leaders"`
	LeaderIDs       []uint   `json:"leader_ids"`
	Directors       []string `json:"directors"`
	DirectorIDs     []uint   `json:"director_ids"`
	Webhooks        []string `json:"webhooks"` // 资源组上配置的群机器人
	Enabled         bool     `json:"enabled"`
	Remark          string   `json:"remark"`
}

// RouteRow 是路由规则列表的一行。
type RouteRow struct {
	Route
	Instance string `json:"instance"` // 空表示全部实例
	Team     string `json:"team"`
	Assignee string `json:"assignee"`
	TblRegex string `json:"tbl_regex"` // 表名正则（空表示不限制）
}

// TeamOverview 是团队配置页的数据。
type TeamOverview struct {
	Teams     []TeamRow       `json:"teams"`
	Routes    []RouteRow      `json:"routes"`
	Groups    []GroupBrief    `json:"groups"`
	Users     []UserBrief     `json:"users"`
	Instances []InstanceBrief `json:"instances"`
	DBAUsers  string          `json:"dba_users"`       // 系统配置 perf.dba_users
	TblRegex  bool            `json:"tbl_regex_ready"` // 路由规则的表名正则是否可用（dbt_perf_route_ext 已创建）
}

func (s *Service) TeamOverview() (*TeamOverview, error) {
	teams, err := s.repo.ListTeams()
	if err != nil {
		return nil, err
	}
	routes, err := s.repo.ListRoutes()
	if err != nil {
		return nil, err
	}
	groups, err := s.repo.ListGroups()
	if err != nil {
		return nil, err
	}
	users, err := s.repo.ListActiveUsers()
	if err != nil {
		return nil, err
	}
	insts, err := s.repo.ListInstances(SupportedDBTypes)
	if err != nil {
		return nil, err
	}
	gname := map[uint]GroupBrief{}
	for _, g := range groups {
		gname[g.ID] = g
	}
	uname := map[uint]string{}
	for _, u := range users {
		uname[u.ID] = u.Name()
	}
	names := func(ids []uint) []string {
		out := []string{}
		for _, id := range ids {
			out = append(out, orDefault(uname[id], fmt.Sprintf("#%d", id)))
		}
		return out
	}
	iname := map[uint]string{}
	for _, i := range insts {
		iname[i.ID] = i.Name
	}
	out := &TeamOverview{Teams: []TeamRow{}, Routes: []RouteRow{}, Groups: groups, Users: users, Instances: insts, DBAUsers: s.cfgStr(settings.KeyPerfDBAUsers, ""),
		TblRegex: s.extReady(tblRouteExt)}
	var tblRe map[uint]string
	if out.TblRegex {
		if m, err := s.ext().ListRouteExt(); err == nil {
			tblRe = m
		}
	}
	for _, t := range teams {
		g := gname[t.ResourceGroupID]
		out.Teams = append(out.Teams, TeamRow{ID: t.ID, ResourceGroupID: t.ResourceGroupID, Team: orDefault(g.Name, fmt.Sprintf("#%d", t.ResourceGroupID)),
			DefaultAssignee: uname[derefUint(t.DefaultAssigneeID)], DefaultID: derefUint(t.DefaultAssigneeID), Leaders: names(t.LeaderIDs), LeaderIDs: nonNilIDs(t.LeaderIDs),
			Directors: names(t.DirectorIDs), DirectorIDs: nonNilIDs(t.DirectorIDs), Webhooks: nonNilStrs(g.Webhooks), Enabled: t.Enabled, Remark: t.Remark})
	}
	for _, r := range routes {
		row := RouteRow{Route: r, Team: gname[r.TeamID].Name, Assignee: uname[derefUint(r.AssigneeID)], TblRegex: tblRe[r.ID]}
		if r.InstanceID != nil {
			row.Instance = orDefault(iname[*r.InstanceID], fmt.Sprintf("#%d", *r.InstanceID))
		}
		out.Routes = append(out.Routes, row)
	}
	return out, nil
}

func nonNilIDs(l []uint) []uint {
	if l == nil {
		return []uint{}
	}
	return l
}

func nonNilStrs(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

// ---- 路由规则 ----

// RouteInput 是新增/修改路由规则的输入。
type RouteInput struct {
	ID         uint   `json:"id"`
	InstanceID uint   `json:"instance_id"` // 0 表示全部实例
	DBRegex    string `json:"db_regex"`
	TeamID     uint   `json:"team_id"`
	AssigneeID uint   `json:"assignee_id"` // 0 表示不指定负责人
	Priority   int    `json:"priority"`
	TblRegex   string `json:"tbl_regex"` // 表名正则（整体匹配；空 / .* 表示不限制）。需要 dbt_perf_route_ext 表
}

func (s *Service) SaveRoute(v Viewer, in RouteInput) (*Route, error) {
	if !v.IsDBA() {
		return nil, forbid("需要 DBA(性能工单管理) 权限")
	}
	in.DBRegex = strings.TrimSpace(in.DBRegex)
	if in.DBRegex == "" {
		in.DBRegex = ".*"
	}
	if len([]rune(in.DBRegex)) > 200 {
		return nil, bad("库名正则过长（最多 200 个字符）")
	}
	if _, err := regexp.Compile("^(?:" + in.DBRegex + ")$"); err != nil {
		return nil, bad(fmt.Sprintf("库名正则不合法：%s", err.Error()))
	}
	if in.Priority < 0 || in.Priority > 100000 {
		return nil, bad("优先级需在 0 到 100000 之间")
	}
	tblRegex, err := normTblRegex(in.TblRegex)
	if err != nil {
		return nil, err
	}
	if tblRegex != "" && !s.extReady(tblRouteExt) {
		return nil, extMissing(tblRouteExt)
	}
	g, err := s.group(in.TeamID)
	if err != nil {
		return nil, err
	}
	r := &Route{ID: in.ID, DBRegex: in.DBRegex, TeamID: g.ID, Priority: in.Priority}
	instName := "全部"
	if in.InstanceID != 0 {
		inst, err := s.instanceOf(in.InstanceID)
		if err != nil {
			return nil, err
		}
		r.InstanceID, instName = ptr(inst.ID), inst.InstanceName
	}
	if in.AssigneeID != 0 {
		if _, err := s.activeUsers([]uint{in.AssigneeID}, "负责人"); err != nil {
			return nil, err
		}
		r.AssigneeID = ptr(in.AssigneeID)
	}
	act := "新增"
	if in.ID != 0 {
		if cur, err := s.repo.GetRoute(in.ID); err != nil {
			return nil, err
		} else if cur == nil {
			return nil, notFound("路由规则不存在")
		}
		act = "修改"
	}
	if err := s.repo.SaveRoute(r); err != nil {
		return nil, err
	}
	if s.extReady(tblRouteExt) { // 空表示删除旧的表名正则（规则改回只看库名）
		if err := s.ext().SaveRouteExt(r.ID, tblRegex); err != nil {
			return nil, err
		}
	}
	tblNote := ""
	if tblRegex != "" {
		tblNote = fmt.Sprintf("，表名 %s", tblRegex)
	}
	s.audit(whoOf(v.Actor), "性能工单配置-路由规则"+act, fmt.Sprintf("优先级 %d，实例 %s，库名 %s%s → 团队 %s", r.Priority, instName, r.DBRegex, tblNote, g.Name))
	return r, nil
}

func (s *Service) DeleteRoute(v Viewer, id uint) error {
	if !v.IsDBA() {
		return forbid("需要 DBA(性能工单管理) 权限")
	}
	r, err := s.repo.GetRoute(id)
	if err != nil {
		return err
	}
	if r == nil {
		return notFound("路由规则不存在")
	}
	if err := s.repo.DeleteRoute(id); err != nil {
		return err
	}
	if s.extReady(tblRouteExt) {
		_ = s.ext().SaveRouteExt(id, "") // 旁路表里的表名正则一并清掉
	}
	s.audit(whoOf(v.Actor), "性能工单配置-路由规则删除", fmt.Sprintf("规则 #%d（库名 %s）", id, r.DBRegex))
	return nil
}

// ---- SLA 策略 ----

// SLAOverview 是 SLA 策略页的数据：四个级别的策略，以及只读展示的级别评估规则。
type SLAOverview struct {
	Policies []SLAPolicy             `json:"policies"`
	Rules    map[string]SeverityRule `json:"rules"`
	Custom   bool                    `json:"rules_custom"` // 是否有 perf.severity_rules 覆盖

	Escalation      EscMatrix `json:"escalation"`       // 各级升级要通知的对象
	EscalationReady bool      `json:"escalation_ready"` // dbt_perf_setting 是否已创建（没创建时矩阵只读、用默认值）
}

func (s *Service) SLAOverview() (*SLAOverview, error) {
	list, err := s.repo.ListSLA()
	if err != nil {
		return nil, err
	}
	have := map[string]SLAPolicy{}
	for _, p := range list {
		have[p.Severity] = p
	}
	out := &SLAOverview{Rules: s.SeverityRules()}
	for _, d := range defaultSLA {
		if p, ok := have[d.Severity]; ok {
			out.Policies = append(out.Policies, p)
		} else {
			out.Policies = append(out.Policies, d)
		}
	}
	_, out.Custom = s.repo.ConfigValue(settings.KeyPerfSeverityRules)
	out.Escalation, out.EscalationReady = s.escalationMatrix(), s.extReady(tblSetting)
	return out, nil
}

const maxSLAHours = 24 * 365

// SaveSLA 一次保存四个级别的策略（同一事务）。新策略只对之后建立或重算时限的工单生效，已有工单的截止时间不变。
func (s *Service) SaveSLA(v Viewer, ps []SLAPolicy) error {
	if !v.IsDBA() {
		return forbid("需要 DBA(性能工单管理) 权限")
	}
	if len(ps) != len(Severities) {
		return bad("需要同时提交 P0 到 P3 四个级别的策略")
	}
	seen := map[string]bool{}
	out := make([]SLAPolicy, 0, len(ps))
	for _, p := range ps {
		if _, ok := severityRank[p.Severity]; !ok || seen[p.Severity] {
			return bad(fmt.Sprintf("级别不合法或重复：%s", p.Severity))
		}
		seen[p.Severity] = true
		name := severityLabel[p.Severity]
		switch {
		case p.AcceptHours < 1 || p.AcceptHours > maxSLAHours:
			return bad(fmt.Sprintf("%s：受理时限需在 1 到 8760 小时之间", name))
		case p.ResolveHours < 1 || p.ResolveHours > maxSLAHours:
			return bad(fmt.Sprintf("%s：解决时限需在 1 到 8760 小时之间", name))
		case p.AcceptHours > p.ResolveHours:
			return bad(fmt.Sprintf("%s：受理时限不能大于解决时限", name))
		case p.L3AfterHours < 0 || p.L3AfterHours > maxSLAHours:
			return bad(fmt.Sprintf("%s：超时后升级总监的时间需在 0 到 8760 小时之间", name))
		case p.RemindIntervalHours < 1 || p.RemindIntervalHours > 24*30:
			return bad(fmt.Sprintf("%s：重复催办间隔需在 1 到 720 小时之间", name))
		case p.MaxExtendTimes < 0 || p.MaxExtendTimes > 20:
			return bad(fmt.Sprintf("%s：最多延期次数需在 0 到 20 之间", name))
		}
		p.ID = 0
		out = append(out, p)
	}
	sort.Slice(out, func(a, b int) bool { return severityRank[out[a].Severity] < severityRank[out[b].Severity] })
	old := s.policies()
	if err := s.repo.SaveSLA(out); err != nil {
		return err
	}
	var diff []string
	for _, p := range out {
		o := old[p.Severity]
		if o.AcceptHours != p.AcceptHours || o.ResolveHours != p.ResolveHours || o.L3AfterHours != p.L3AfterHours || o.RemindIntervalHours != p.RemindIntervalHours || o.MaxExtendTimes != p.MaxExtendTimes {
			diff = append(diff, fmt.Sprintf("%s 受理%dh/解决%dh/升级总监%dh/催办%dh/延期%d次", p.Severity, p.AcceptHours, p.ResolveHours, p.L3AfterHours, p.RemindIntervalHours, p.MaxExtendTimes))
		}
	}
	if len(diff) == 0 {
		diff = []string{"无变化"}
	}
	s.audit(whoOf(v.Actor), "性能工单配置-SLA策略修改", strings.Join(diff, "；"))
	return nil
}
