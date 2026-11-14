// @author: kevinkelly
// @license: Apache Licence
// @file: templates.go
// @time: 2026/10/28

package firewalladmin

import (
	"strings"

	"dbtool/internal/audit"
	"dbtool/internal/firewall"
)

// template 是一个规则模板：应用后创建为观察模式的规则，管理员确认没有误伤后再切到生效。
type template struct {
	Key, Category, Name, Description string
	Rule                             firewall.Rule
}

// 模板里的取值都按“开箱能用、宁可少拦”挑：条件里用的对象名（敏感表 / 列）是示例，应用后在规则编辑器里改成实际的名字。
// 场景是否已接入执行见 activeScenes（目前在线查询 / SQL 工单 / 数据导出都已接入）；没接入的场景，模板标注“尚未接入”。
var templates = []template{
	{Key: "dangerous-func", Category: "防滥用", Name: "禁止危险函数",
		Description: "SLEEP / BENCHMARK 制造长时间占用，LOAD_FILE 读服务器文件，GET_LOCK 占用命名锁。",
		Rule: firewall.Rule{Priority: 20, Scenes: []string{firewall.SceneQuery, firewall.SceneWorkflow, firewall.SceneExport}, Env: "all", Match: "any",
			Conditions: []firewall.Condition{{T: firewall.CondKeyword, V: []string{"SLEEP", "BENCHMARK", "LOAD_FILE", "GET_LOCK"}}},
			Action:     firewall.ActDeny, Message: "语句里包含不允许使用的函数。"}},
	{Key: "into-outfile", Category: "防滥用", Name: "禁止写出文件",
		Description: "INTO OUTFILE / DUMPFILE 可以把数据写到数据库服务器磁盘。",
		Rule: firewall.Rule{Priority: 21, Scenes: []string{firewall.SceneQuery, firewall.SceneWorkflow, firewall.SceneExport}, Env: "all", Match: "any",
			Conditions: []firewall.Condition{{T: firewall.CondRegex, V: `into\s+(outfile|dumpfile)`}},
			Action:     firewall.ActDeny, Message: "不允许把查询结果写出到服务器文件。"}},
	{Key: "sys-views", Category: "防探测", Name: "禁止读取权限与会话视图",
		Description: "processlist 能看到别人正在执行的 SQL（含明文条件值），user_privileges 暴露账号权限。应用后可在“用户”里设为“排除 DBA 组”。",
		Rule: firewall.Rule{Priority: 30, Scenes: []string{firewall.SceneQuery}, Env: "all", Match: "any",
			Conditions: []firewall.Condition{{T: firewall.CondObject, V: []string{"information_schema.processlist", "information_schema.user_privileges", "performance_schema.*"}}},
			Action:     firewall.ActDeny, Message: "该系统视图仅 DBA 可查询。"}},
	{Key: "sensitive-reason", Category: "合规", Name: "敏感表访问要求写原因",
		Description: "用户表、证件表的查询要求说明用途，原因进命中记录。示例表名为 sys_users、user_card，应用后改成实际的敏感表。",
		Rule: firewall.Rule{Priority: 40, Scenes: []string{firewall.SceneQuery}, Env: "prod", Match: "any",
			Conditions: []firewall.Condition{{T: firewall.CondObject, V: []string{"*.sys_users", "*.user_card"}}},
			Action:     firewall.ActReason, Message: "该表含敏感数据，请说明查询用途。"}},
	{Key: "mask-sensitive", Category: "防拖库", Name: "敏感列强制脱敏",
		Description: "命中指定列时，不论有没有在“数据脱敏”里配置过，一律整体隐藏。示例列名为 password、id_card，应用后改成实际的列名。",
		Rule: firewall.Rule{Priority: 45, Scenes: []string{firewall.SceneQuery}, Env: "prod", Match: "any",
			Conditions: []firewall.Condition{{T: firewall.CondObject, V: []string{"*.*.password", "*.*.id_card"}}},
			Action:     firewall.ActMask, Message: "敏感列已被隐藏。"}},
	{Key: "cartesian", Category: "防大查询", Name: "笛卡尔积",
		Description: "JOIN 缺少连接条件，结果行数是两表行数之积，几秒内能打满 CPU。",
		Rule: firewall.Rule{Priority: 60, Scenes: []string{firewall.SceneQuery, firewall.SceneWorkflow}, Env: "all", Match: "all",
			Conditions: []firewall.Condition{{T: firewall.CondFeature, V: firewall.FeatCartesian}},
			Action:     firewall.ActDeny, Message: "多表查询缺少连接条件。"}},
	{Key: "no-where-select", Category: "防大查询", Name: "无条件的全表读取要求写原因",
		Description: "SELECT 没有 WHERE：大表上等同全表扫描。先要求说明原因，观察一阵再决定要不要直接拦。",
		Rule: firewall.Rule{Priority: 70, Scenes: []string{firewall.SceneQuery}, Env: "prod", Match: "all",
			Conditions: []firewall.Condition{{T: firewall.CondStmt, V: []string{"SELECT"}}, {T: firewall.CondFeature, V: firewall.FeatNoWhere}},
			Action:     firewall.ActReason, Message: "没有 WHERE 条件的查询会扫描整张表，请说明用途。"}},
	{Key: "big-in", Category: "防大查询", Name: "IN 列表过长",
		Description: "一次塞上千个值的 IN 往往是应用在循环拼接，优化器和网络都吃不消。",
		Rule: firewall.Rule{Priority: 80, Scenes: []string{firewall.SceneQuery, firewall.SceneWorkflow}, Env: "all", Match: "all",
			Conditions: []firewall.Condition{{T: firewall.CondFeature, V: firewall.FeatBigIn}},
			Action:     firewall.ActWarn, Message: "IN 列表超过 1000 项，建议改用临时表 JOIN。"}},
	{Key: "leading-wildcard", Category: "防大查询", Name: "LIKE 以 % 开头",
		Description: "前导通配符用不上索引，大表上是一次全表扫描。",
		Rule: firewall.Rule{Priority: 85, Scenes: []string{firewall.SceneQuery}, Env: "prod", Match: "all",
			Conditions: []firewall.Condition{{T: firewall.CondFeature, V: firewall.FeatLeadingWildcard}},
			Action:     firewall.ActWarn, Message: "LIKE 以 % 开头不能使用索引，大表上会很慢。"}},
	{Key: "no-where-dml", Category: "防误操作", Name: "禁止不带 WHERE 的 UPDATE / DELETE",
		Description: "生产库的整表更新 / 删除直接拒绝。SQL 工单场景接入后生效。",
		Rule: firewall.Rule{Priority: 10, Scenes: []string{firewall.SceneWorkflow}, Env: "prod", Match: "all",
			Conditions: []firewall.Condition{{T: firewall.CondStmt, V: []string{"UPDATE", "DELETE"}}, {T: firewall.CondFeature, V: firewall.FeatNoWhere}},
			Action:     firewall.ActDeny, Message: "生产库不允许不带 WHERE 的 UPDATE / DELETE，请补充条件，或分批执行。"}},
	{Key: "drop-truncate", Category: "防误操作", Name: "生产库 DROP / TRUNCATE 需要 DBA 处理",
		Description: "不可逆的结构操作不允许走普通工单。SQL 工单场景接入后生效。",
		Rule: firewall.Rule{Priority: 15, Scenes: []string{firewall.SceneWorkflow}, Env: "prod", Match: "all",
			Conditions: []firewall.Condition{{T: firewall.CondStmt, V: []string{"DROP", "TRUNCATE"}}},
			Action:     firewall.ActDeny, Message: "DROP / TRUNCATE 请联系 DBA 处理。"}},
	{Key: "offhours-ddl", Category: "防误操作", Name: "非工作时间的生产 DDL",
		Description: "夜间 / 周末的结构变更没人盯着，先告警提醒。SQL 工单场景接入后生效。",
		Rule: firewall.Rule{Priority: 50, Scenes: []string{firewall.SceneWorkflow}, Env: "prod", Match: "all",
			Conditions: []firewall.Condition{{T: firewall.CondStmt, V: []string{"CREATE", "ALTER", "DROP", "TRUNCATE"}}, {T: firewall.CondTime, V: "offhours"}},
			Action:     firewall.ActWarn, Message: "非工作时间的结构变更，请确认有人值守。"}},
}

func (t template) pending() bool {
	for _, s := range t.Rule.Scenes {
		if activeScenes[s] {
			return false
		}
	}
	return true
}

func ruleInputOf(r firewall.Rule) RuleInput {
	on := r.Enabled
	return RuleInput{
		Name: r.Name, Description: r.Description, Priority: r.Priority, Enabled: &on, Mode: r.Mode, Scenes: nz(r.Scenes), Env: r.Env,
		InstanceIDs: nz(r.InstanceIDs), GroupMode: r.GroupMode, GroupIDs: nz(r.GroupIDs), Match: r.Match, Conditions: r.Conditions,
		Action: r.Action, ActionArg: r.ActionArg, Message: r.Message, Notify: r.Notify,
	}
}

// ruleOfTemplate 把模板转成待保存的规则：观察模式、启用、名称与说明取模板的。
func (t template) rule() firewall.Rule {
	r := t.Rule
	r.Name, r.Description = t.Name, t.Description
	r.Enabled, r.Mode = true, firewall.ModeObserve
	if r.GroupMode == "" {
		r.GroupMode = "all"
	}
	return r
}

// Templates 返回规则模板，并标出哪些已经有同名规则。
func (s *Service) Templates() ([]TemplateView, error) {
	rows, err := s.st.ListRules()
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, m := range rows {
		have[strings.ToLower(strings.TrimSpace(m.Name))] = true
	}
	out := make([]TemplateView, 0, len(templates))
	for _, t := range templates {
		out = append(out, TemplateView{Key: t.Key, Category: t.Category, Name: t.Name, Description: t.Description,
			Rule: ruleInputOf(t.rule()), Pending: t.pending(), Applied: have[strings.ToLower(t.Name)]})
	}
	return out, nil
}

// ApplyTemplate 用模板创建一条观察模式的规则（不会拦截任何语句）。已有同名规则时返回冲突。
func (s *Service) ApplyTemplate(actor audit.Actor, key string) (*RuleView, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	for _, t := range templates {
		if t.Key != key {
			continue
		}
		r, err := s.toRule(ruleInputOf(t.rule()))
		if err != nil {
			return nil, err
		}
		rows, err := s.st.ListRules()
		if err != nil {
			return nil, err
		}
		if len(rows) >= firewall.MaxRules {
			return nil, badReq("规则数量已达上限，请先清理不再使用的规则")
		}
		return s.insert(actor, r, "从模板添加规则") // i18n-ignore 审计文本，存库后不翻译
	}
	return nil, notFound("模板不存在")
}
