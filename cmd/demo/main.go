// @author: kevinkelly
// @license: Apache Licence
// @file: main.go
// @time: 2026/10/28

// 演示服务：用内存里的假数据运行整套页面，不连接任何真实数据库，仅用于本地预览界面。
//
//	go run ./cmd/demo        # 打开 http://127.0.0.1:8000 ，账号 admin / admin123
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"

	"dbtool/internal/btnperm"
	"dbtool/internal/config"
	"dbtool/internal/crypto"
	"dbtool/internal/duty"
	"dbtool/internal/engine"
	"dbtool/internal/model"
	"dbtool/internal/privapply/memrepo"
	"dbtool/internal/query"
	"dbtool/internal/server"
	"dbtool/internal/settings"
	"dbtool/internal/sqlflow"
	sqlmem "dbtool/internal/sqlflow/memrepo"
	"dbtool/internal/sysconfig"
)

// repo 在 memrepo（权限、资源组、审批流、权限申请的内存实现）之上，补充演示用的用户、实例连接、查询日志和脱敏配置。
type repo struct {
	*memrepo.Repo
	users map[string]*model.User
	logs  []*model.QueryLog
}

// 演示用脱敏配置：所有实例的 shop.users.phone 保留前 3 后 4 位（演示环境没有 goInception，走列名脱敏）。
func (r *repo) ActiveMaskingColumns(inst uint) ([]model.DataMaskingColumn, error) {
	return []model.DataMaskingColumn{{Active: true, InstanceID: inst, TableSchema: "shop", TableName_: "users", ColumnName: "phone",
		Kind: "keep", KeepPrefix: 3, KeepSuffix: 4}}, nil
}

func (r *repo) GetInstance(id uint) (*model.Instance, error) {
	for i := range r.Instances {
		if r.Instances[i].ID == id {
			return &r.Instances[i], nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}
func (r *repo) EngineConn(*model.Instance) engine.Conn { return engine.Conn{} }
func (r *repo) InsertQueryLog(l *model.QueryLog) error {
	l.ID = uint(len(r.logs) + 1)
	r.logs = append(r.logs, l)
	return nil
}

// 执行历史（内存版，语义同 store.ListQueryLogs）：用户精确匹配、关键词都包含且不区分大小写、新的在前。
func (r *repo) ListQueryLogs(f query.QueryLogFilter) ([]model.QueryLog, int64, error) {
	var all []model.QueryLog
	for i := len(r.logs) - 1; i >= 0; i-- {
		l := *r.logs[i]
		if f.Username != "" && l.Username != f.Username || f.Instance != "" && l.InstanceName != f.Instance {
			continue
		}
		if f.Start != nil && l.CreateTime.Before(*f.Start) || f.End != nil && !l.CreateTime.Before(*f.End) {
			continue
		}
		hit := true
		for _, k := range f.Keywords {
			hit = hit && strings.Contains(strings.ToLower(l.SQLLog), strings.ToLower(k))
		}
		if hit {
			all = append(all, l)
		}
	}
	total := int64(len(all))
	if f.Offset > len(all) {
		f.Offset = len(all)
	}
	all = all[f.Offset:]
	if f.Limit > 0 && f.Limit < len(all) {
		all = all[:f.Limit]
	}
	return all, total, nil
}

func (r *repo) QueryLogUsers() ([]model.QueryLogUser, error) {
	idx := map[string]int{}
	var out []model.QueryLogUser
	for _, l := range r.logs {
		i, ok := idx[l.Username]
		if !ok {
			i = len(out)
			idx[l.Username] = i
			out = append(out, model.QueryLogUser{Username: l.Username, UserDisplay: l.UserDisplay})
		}
		out[i].Count++
	}
	return out, nil
}

func (r *repo) GetUserByName(n string) (*model.User, error) {
	if u, ok := r.users[n]; ok {
		return u, nil
	}
	return nil, gorm.ErrRecordNotFound
}

type table struct {
	cols []string
	rows [][]any
}

var data = map[string]map[string]table{
	"shop": {
		"orders": {[]string{"id", "user_id", "amount", "status", "created_at"}, [][]any{
			{1001, 201, "299.00", "paid", "2026-10-01 09:12:00"}, {1002, 202, "59.90", "paid", "2026-10-01 09:40:00"},
			{1003, 201, "1280.00", "refunded", "2026-10-02 14:03:00"}, {1004, 205, "18.00", "paid", "2026-10-03 20:18:00"},
			{1005, 207, "399.00", "pending", "2026-10-04 08:55:00"}, {1006, nil, "88.00", "paid", "2026-10-04 09:30:00"}}},
		"users": {[]string{"id", "name", "phone", "city"}, [][]any{
			{201, "王小明", "13812345678", "杭州"}, {202, "李华", "13987654321", "上海"}, {205, "陈静", "15011112222", "成都"}}},
		"refunds": {[]string{"id", "order_id", "reason", "amount"}, [][]any{{1, 1003, "质量问题", "1280.00"}}},
	},
	"report": {"daily_sales": {[]string{"dt", "gmv", "orders"}, [][]any{{"2026-10-01", "88210.50", 412}, {"2026-10-02", "90311.00", 430}}}},
}

type demoEngine struct{ db string }

func (e *demoEngine) Name() string                                  { return "mysql" }
func (e *demoEngine) TestConnection(context.Context) error          { return nil }
func (e *demoEngine) ServerVersion(context.Context) (string, error) { return "8.0.36-demo", nil }
func (e *demoEngine) Close() error                                  { return nil }
func (e *demoEngine) AllDatabases(context.Context) ([]string, error) {
	return []string{"report", "shop"}, nil
}
func (e *demoEngine) AllTables(_ context.Context, db string) ([]string, error) {
	var out []string
	for t := range data[db] {
		out = append(out, t)
	}
	// 保持稳定顺序
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

// ReadOnly / ExecEach 让演示环境可以体验 SQL 工单的执行：每条语句假装执行 0.4 秒；语句里含 fail 的会报错（演示“失败即停”）。
func (e *demoEngine) ReadOnly(context.Context) (bool, error) { return false, nil }
func (e *demoEngine) ExecEach(ctx context.Context, _ string, stmts []string) ([]engine.ExecStmtResult, error) {
	out := make([]engine.ExecStmtResult, len(stmts))
	failed := false
	for i, st := range stmts {
		out[i].SQL = st
		if failed {
			out[i].Skipped = true
			continue
		}
		select {
		case <-time.After(400 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if strings.Contains(strings.ToLower(st), "fail") {
			out[i].Err = "Error 1146: Table doesn't exist（演示：语句里含 fail）"
			failed = true
			continue
		}
		out[i].Affected, out[i].ElapsedMs = 1, 400
	}
	return out, nil
}

func (e *demoEngine) AllColumns(_ context.Context, db, tb string) ([]string, error) {
	return data[db][tb].cols, nil
}

var fromRe = regexp.MustCompile("(?i)\\bfrom\\s+`?(\\w+)`?")

func (e *demoEngine) Query(ctx context.Context, sql string, opt engine.QueryOptions) (*engine.ResultSet, error) {
	time.Sleep(120 * time.Millisecond)
	low := strings.ToLower(strings.TrimSpace(sql))
	if strings.HasPrefix(low, "show databases") {
		return &engine.ResultSet{ColumnList: []string{"Database"}, Rows: [][]any{{"report"}, {"shop"}}}, nil
	}
	if strings.Contains(low, "sleep(") { // 方便演示取消/超时
		select {
		case <-time.After(30 * time.Second):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	m := fromRe.FindStringSubmatch(sql)
	if m == nil {
		return &engine.ResultSet{ColumnList: []string{"result"}, Rows: [][]any{{1}}}, nil
	}
	t, ok := data[opt.DBName][m[1]]
	if !ok {
		return nil, fmt.Errorf("Error 1146: Table '%s.%s' doesn't exist", opt.DBName, m[1])
	}
	if strings.HasPrefix(low, "explain") {
		return &engine.ResultSet{
			ColumnList: []string{"id", "select_type", "table", "type", "key", "rows", "Extra"},
			Rows:       [][]any{{1, "SIMPLE", m[1], "ALL", nil, len(t.rows), "Using where"}},
		}, nil
	}
	rows := t.rows
	if opt.Limit > 0 && len(rows) > opt.Limit {
		rows = rows[:opt.Limit]
	}
	return &engine.ResultSet{ColumnList: t.cols, Rows: rows}, nil
}

func newDemoRepo() *repo {
	hash := func(pw string) string {
		h, err := crypto.MakeDjangoPassword(pw, "demosalt", 1000)
		if err != nil {
			panic(err)
		}
		return h
	}
	m := memrepo.New()
	canRead := model.InstanceTag{TagCode: "can_read", TagName: "支持查询", Active: true}
	canWrite := model.InstanceTag{TagCode: "can_write", TagName: "支持上线", Active: true}
	m.Instances = []model.Instance{
		{ID: 1, InstanceName: "prod-mysql-01", DBType: "mysql", Type: "master", Tags: []model.InstanceTag{{TagCode: "prod", TagName: "生产", Active: true}, canRead, canWrite}},
		{ID: 2, InstanceName: "prod-mysql-02", DBType: "mysql", Type: "slave", Tags: []model.InstanceTag{{TagCode: "prod", TagName: "生产", Active: true}, canRead}},
		{ID: 3, InstanceName: "test-mysql-01", DBType: "mysql", Type: "master", Tags: []model.InstanceTag{{TagCode: "test", TagName: "测试", Active: true}, canRead, canWrite}},
	}
	// 资源组 1 关联实例 1、3；dev、lead、dba 都在资源组 1。审批流：组长(10) -> DBA(20)。
	m.RGs = []model.ResourceGroup{{GroupID: 1, GroupName: "演示研发组"}}
	m.InstRGs = map[uint][]uint{1: {1}, 3: {1}}
	m.UserRGs = map[uint][]uint{2: {1}, 3: {1}, 4: {1}}
	m.Groups = map[uint]*model.AuthGroup{10: {ID: 10, Name: "组长"}, 20: {ID: 20, Name: "DBA"}}
	m.Members = map[uint]map[uint]bool{3: {10: true}, 4: {20: true}}
	m.Users = map[uint]model.User{2: {ID: 2, Username: "dev", Display: "演示开发者"}, 3: {ID: 3, Username: "lead", Display: "演示组长"}, 4: {ID: 4, Username: "dba", Display: "演示DBA"}}
	// 演示账号的按钮权限：dev = 研发（RD 预设），lead = 组长（TL 预设 + 修改 / 回收查询权限），dba = DBA 预设。
	presetOf := func(name string, extra ...string) map[string]bool {
		out := map[string]bool{}
		for _, c := range append(btnperm.Preset(name), extra...) {
			out[c] = true
		}
		return out
	}
	m.Perms = map[uint]map[string]bool{
		2: presetOf("RD"),
		3: presetOf("TL", "qpriv.manage"),
		4: presetOf("DBA"),
	}
	m.Settings[[2]int{1, 1}] = &model.WorkflowAuditSetting{GroupID: 1, GroupName: "演示研发组", WorkflowType: 1, AuditAuthGroups: "10,20"}
	m.Settings[[2]int{1, 2}] = &model.WorkflowAuditSetting{GroupID: 1, GroupName: "演示研发组", WorkflowType: 2, AuditAuthGroups: "10,20"}
	m.Cfg = map[string]string{settings.KeyQueryForbidSelectStar: "true", settings.KeyQueryMaskEnabled: "true"}
	// dev 初始只有 shop.orders 表（最多 3 行）和 report 库（最多 10 行）的查询权限；申请 users 表可以体验完整流程。
	future := time.Now().AddDate(0, 1, 0)
	for _, inst := range []uint{1, 3} {
		m.Privs = append(m.Privs,
			&model.QueryPrivilege{PrivilegeID: uint(len(m.Privs) + 1), UserName: "dev", UserDisplay: "演示开发者", InstanceID: inst, PrivType: 2, DBName: "shop", TbName: "orders", LimitNum: 3, ValidDate: future},
			&model.QueryPrivilege{PrivilegeID: uint(len(m.Privs) + 2), UserName: "dev", UserDisplay: "演示开发者", InstanceID: inst, PrivType: 1, DBName: "report", LimitNum: 10, ValidDate: future})
	}
	// 演示用的执行历史：dev 与 admin 各有几条，管理员可以按用户过滤。
	var seedLogs []*model.QueryLog
	now := time.Now()
	for i, e := range []struct{ user, display, inst, db, sql string }{
		{"dev", "演示开发者", "prod-mysql-01", "shop", "SELECT id, user_id, amount FROM orders WHERE status = 'paid' LIMIT 100"},
		{"dev", "演示开发者", "prod-mysql-01", "shop", "SELECT id, name, city FROM users WHERE city = 'Hangzhou' LIMIT 100"},
		{"admin", "演示管理员", "prod-mysql-01", "shop", "SELECT city, COUNT(*) AS n FROM users GROUP BY city ORDER BY n DESC LIMIT 20"},
		{"dev", "演示开发者", "prod-mysql-01", "report", "SELECT dt, SUM(amount) FROM daily_sales WHERE dt >= '2026-10-01' GROUP BY dt LIMIT 31"},
		{"admin", "演示管理员", "prod-mysql-01", "shop", "EXPLAIN SELECT * FROM orders WHERE user_id = 7"},
		{"dev", "演示开发者", "prod-mysql-01", "shop", "SELECT o.id, o.amount, u.name FROM orders o JOIN users u ON u.id = o.user_id WHERE o.amount > 100 ORDER BY o.created_at DESC LIMIT 50"},
	} {
		seedLogs = append(seedLogs, &model.QueryLog{ID: uint(i + 1), Username: e.user, UserDisplay: e.display, InstanceName: e.inst, DBName: e.db, SQLLog: e.sql,
			EffectRow: int64(3 + i), CostTime: "0.0" + string(rune('1'+i)) + "2", CreateTime: now.Add(-time.Duration(len(seedLogs)+6-i) * 37 * time.Minute)})
	}
	return &repo{Repo: m, users: map[string]*model.User{
		"admin": {ID: 1, Username: "admin", Display: "演示管理员", Password: hash("admin123"), IsActive: true, IsSuperuser: true},
		"dev":   {ID: 2, Username: "dev", Display: "演示开发者", Password: hash("dev123"), IsActive: true},
		"lead":  {ID: 3, Username: "lead", Display: "演示组长", Password: hash("lead123"), IsActive: true},
		"dba":   {ID: 4, Username: "dba", Display: "演示DBA", Password: hash("dba123"), IsActive: true},
	}, logs: seedLogs}
}

// newDemoDuty 用内存存储和固定名单搭一个值班表服务，并按“三班倒”预置三个班次。
func newDemoDuty() *duty.Service {
	dir := duty.StaticDirectory{
		{Username: "admin", Display: "演示管理员", Email: "admin@demo.example", Active: true},
		{Username: "dev", Display: "演示开发者", Email: "dev@demo.example", Active: true},
		{Username: "lead", Display: "演示组长", Email: "lead@demo.example", Active: true},
		{Username: "dba", Display: "演示DBA", Email: "dba@demo.example", Active: true},
		{Username: "zhangsan", Display: "张三", Email: "zhangsan@demo.example", Active: true},
		{Username: "lisi", Display: "李四", Email: "lisi@demo.example", Active: true},
		{Username: "zhaowu", Display: "赵五", Email: "zhaowu@demo.example", Active: true},
	}
	svc := duty.New(duty.NewMemoryStore(), dir)
	all := []int{0, 1, 2, 3, 4, 5, 6}
	for _, in := range []duty.ShiftInput{
		{Name: "白班", Username: "zhangsan", Start: "08:00", End: "19:00", Weekdays: all, Contact: "分机 8001"},
		{Name: "晚班", Username: "lisi", Start: "19:00", End: "24:00", Weekdays: all, Contact: "分机 8002"},
		{Name: "夜班", Username: "zhaowu", Start: "00:00", End: "08:00", Weekdays: all},
	} {
		if _, err := svc.CreateShift("system", in); err != nil {
			slog.Error("演示值班表初始化失败", "err", err)
		}
	}
	return svc
}

func main() {
	listen := os.Getenv("LISTEN")
	if listen == "" {
		listen = "127.0.0.1:8000"
	}
	cfg := &config.Config{Debug: true, Listen: listen, PublicURL: "http://" + listen, SecretKey: "demo-secret-key-demo-secret-key-demo"}
	r := newDemoRepo()
	open := func(string, engine.Conn) (engine.Engine, error) { return &demoEngine{}, nil }
	sqlSvc := sqlflow.NewService(sqlmem.New(r.Repo), sqlflow.Options{Open: open})
	sqlSvc.BaseURL = cfg.PublicURL
	go sqlSvc.StartScheduler(context.Background(), 5*time.Second)
	// 系统配置（设置中心）：演示环境没有元数据库，用内存里的设置服务（重启即恢复）。
	demoSettings := settings.NewMemory(map[string]string{
		settings.KeyQueryMaxExecSeconds: "30",
		settings.KeyFirewallMode:        "enforce",
		settings.KeyGoInceptionHost:     "gi.demo.example.com",
		settings.KeyGoInceptionPassword: "demo-password",
	})
	server.AddRoutes(func(s *server.Server, rt server.Routes) {
		sysconfig.Mount(s, rt, demoSettings, sysconfig.Security{PublicSecretKey: true})
	})
	// 值班表：演示环境用内存里的班次（三班倒：张三 08:00–19:00、李四 19:00–24:00、赵五 00:00–08:00），顶栏时钟左边显示当前值班人。
	dutySvc := newDemoDuty()
	server.AddRoutes(func(s *server.Server, rt server.Routes) { duty.Mount(s, rt, dutySvc, nil) })
	h, err := server.New(cfg, r, server.Options{Opener: open, SQLFlow: sqlSvc})
	if err != nil {
		slog.Error("初始化失败", "err", err)
		os.Exit(1)
	}
	slog.Info("演示服务已启动（假数据，不连接真实数据库）", "url", "http://"+listen,
		"账号", "admin/admin123（管理员）、dev/dev123（申请人）、lead/lead123（一级审批：组长）、dba/dba123（二级审批：DBA，也有执行权限）",
		"SQL工单", "/sqlworkflow：dev 提交 → lead 通过 → dba 通过 → dev 执行（语句里含 fail 可演示失败即停）")
	if err := h.Run(listen); err != nil {
		slog.Error("退出", "err", err)
		os.Exit(1)
	}
}
