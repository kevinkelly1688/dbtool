// @author: kevinkelly
// @license: Apache Licence
// @file: ext_test.go
// @time: 2026/10/28

package audit

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"

	"dbtool/internal/btnperm"
	"dbtool/internal/model"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		action, extra string
		result, cat   string
		code          string
		risky         bool
	}{
		{"登入", "10.0.0.1（本地）", ResultOK, CatLogin, "auth.login", false},
		{"登入失败", "", ResultFailed, CatLogin, "auth.login", false},
		{"登入失败", "账号已被临时锁定", ResultDenied, CatLogin, "auth.login", false},
		{"账号锁定", "连续失败 5 次", ResultDenied, CatLogin, "", false},
		{"2FA-绑定", "totp", ResultOK, CatLogin, "", false},
		{"2FA-验证失败过多", "totp", ResultDenied, CatLogin, "", false},
		{"离线下载", "工单 1", ResultOK, CatQuery, "export.download", true},
		{"导出审计日志", "导出 3 行", ResultOK, CatSys, "audit.export", true},
		{"修改审批流", "x", ResultOK, CatSys, "flow.edit", true},
		// S7a-B3：系统管理里启停用户、彻底删除资源组、调整资源组关联用户 / 实例也要归到对应按钮码
		{"修改查询权限", "用户:a", ResultOK, CatQuery, "qpriv.manage", true},
		{"回收查询权限", "用户:a", ResultOK, CatQuery, "qpriv.manage", true},
		{"启用用户", "用户：a", ResultOK, CatSys, "user.disable", true},
		{"停用用户", "用户：a", ResultOK, CatSys, "user.disable", true},
		{"彻底删除资源组", "资源组：g", ResultOK, CatSys, "rg.delete", true},
		{"调整资源组关联用户", "资源组：g", ResultOK, CatSys, "rg.relations", false},
		{"调整资源组关联实例", "资源组：g", ResultOK, CatSys, "rg.relations", false},
		{"创建数据库账号", "实例：a", ResultOK, CatInst, "account.create", true},
		{"修改数据库账号密码", "实例：a，结果：失败：Access denied", ResultFailed, CatInst, "account.pwd", true},
		{"终止会话", "实例：a", ResultOK, CatInst, "session.kill", true},
		{"登录方式-测试", "LDAP：false", ResultFailed, CatSys, "authp.test", false},
		{"登录方式-测试", "LDAP：true", ResultOK, CatSys, "authp.test", false},
		{"登录方式-停用", "x", ResultOK, CatSys, "authp.toggle", true},
		{"性能巡检·立即巡检", "实例：a", ResultOK, CatInst, "inspect.run", false},
		{"性能巡检·删除报告", "报告：a", ResultOK, CatInst, "inspect.delete", true},
		{"性能巡检·某新动作", "", ResultOK, CatInst, "", false},
		{"SQL优化-SQLAdvisor", "", ResultOK, CatOpt, "optimize.explain", false}, // 已下线的工具：历史审计记录归到“执行计划”
		{"SQL优化-执行计划", "", ResultOK, CatOpt, "optimize.explain", false},
		{"SQL分析", "", ResultOK, CatSQLFlow, "sqlanalyze.run", false},
		{"分区维护-定时执行", "", ResultOK, CatQuery, "partman.run", true},
		{"性能工单配置-SLA策略修改", "", ResultOK, CatPerf, "perfcfg.sla", true},
		{"性能工单配置-路由规则新增", "", ResultOK, CatPerf, "perfcfg.team", false},
		{"性能工单-受理", "工单#1", ResultOK, CatPerf, "", false},
		{"性能工单-验证通过", "工单#1", ResultOK, CatPerf, "perf.verify", false},
		{"性能工单-验证不通过", "工单#1", ResultOK, CatPerf, "perf.verify", false},
		{"性能工单-同意豁免", "工单#1", ResultOK, CatPerf, "perf.approve", false},
		{"性能工单-同意延期", "工单#1", ResultOK, CatPerf, "perf.approve", false},
		{"性能工单-驳回延期", "工单#1", ResultOK, CatPerf, "perf.approve", false},
		{"性能工单-误报关闭", "工单#1", ResultOK, CatPerf, "perf.close_dup", true},
		{"数据清理", "一轮清理结束", ResultOK, CatSys, "", false},
		{"数据清理预览", "查看数据清理的 dry-run 统计", ResultOK, CatSys, "", false},
		{"修改系统配置项", "修改系统配置项 max_export_rows：10000 → 500000", ResultOK, CatSys, "", false},
		{"密文重加密", "dbt-admin reencrypt：改写 5 个加密值", ResultOK, CatSys, "", false},
		{"SQL上线-执行状态恢复", "工单#1", ResultOK, CatSQLFlow, "", false},
		{"SQL上线-标记执行异常", "工单#1", ResultOK, CatSQLFlow, "sqlworkflow.view_own", false},
		{"数据归档-暂存文件遗留", "暂存目录有 2 个遗留 .tmp 文件", ResultOK, CatQuery, "archive.view", false},
		{"数据归档-运行中断", "运行 #1 失联", ResultOK, CatQuery, "archive.view", false},
		{"数据归档-运行结束", "运行 #1 结束", ResultOK, CatQuery, "archive.view", false},
		{"数据归档-驳回", "驳回任务", ResultOK, CatQuery, "archive.view", false},
		{"数据归档-审批通过", "审批通过任务", ResultOK, CatQuery, "archive.view", false},
		{"数据归档-提交申请", "新建任务", ResultOK, CatQuery, "archive.apply", false},
		{"数据归档-修改任务", "调整参数", ResultOK, CatQuery, "archive.apply", false},
		{"数据归档-试运行", "x", ResultOK, CatQuery, "archive.dryrun", false},
		{"数据归档-立即执行", "x", ResultOK, CatQuery, "archive.run", true},
		{"数据归档-终止运行", "x", ResultOK, CatQuery, "archive.stop", true},
		{"数据归档-恢复数据", "x", ResultOK, CatQuery, "archive.restore", true},
		{"数据归档-存储-测试", "x", ResultOK, CatQuery, "archive.store_edit", true},
		{"数据归档-策略修改", "x", ResultOK, CatQuery, "archive.policy_edit", true},
		{"直接授予查询权限", "用户:a", ResultOK, CatQuery, "qpriv.manage", true},
		{"通知渠道-新增", "渠道「x」", ResultOK, CatSys, "notify.edit", false},
		{"通知渠道-测试发送", "渠道「x」（钉钉）；结果：失败", ResultFailed, CatSys, "notify.test", false},
		{"高危按钮操作", "终止会话（session.kill）· POST /api/v1/x", ResultOK, CatInst, "session.kill", true},
		{"高危按钮操作", "没有权限码", ResultOK, CatSys, "", true},
		// B0：退出登录 / 水印篡改 / 无权限拒绝 / 测试连接 / 被拒绝的高危操作 / 取消分析任务
		{"退出登录", "IP：10.0.0.1", ResultOK, CatLogin, "", false},
		{"水印篡改", "IP：10.0.0.1；类型：removed（水印层被删除）", ResultOK, CatSys, "", false},
		{ActionNoPerm, "终止会话（session.kill）· POST /api/v1/instances/1/kill", ResultDenied, CatInst, "session.kill", true},
		{ActionNoPerm, "编辑审批流（flow.edit）· PUT /api/v1/x；可选按钮：flow.delete", ResultDenied, CatSys, "flow.edit", true},
		{ActionNoPerm, "没有权限码", ResultDenied, CatSys, "", false},
		{"测试连接", "实例：a，地址：h:3306，类型：mysql，结果：成功（8.0，3 ms）", ResultOK, CatInst, "instance.test", false},
		{"测试连接", "实例：a，地址：h:3306，类型：mysql，结果：失败：timeout", ResultFailed, CatInst, "instance.test", false},
		{"删除实例（拒绝）", "实例：a；原因：仍被引用", ResultDenied, CatInst, "instance.delete", true},
		{"终止会话（拒绝）", "实例：a；原因：x", ResultDenied, CatInst, "session.kill", true},
		{"修改实例参数（拒绝）", "实例：a；原因：x", ResultDenied, CatInst, "param.edit", true},
		{"创建数据库账号（拒绝）", "实例：a；原因：x", ResultDenied, CatInst, "account.create", true},
		{"清理Binlog（拒绝）", "实例：a；原因：x", ResultDenied, CatInst, "instance.purge_binlog", true},
		{"SQL分析-取消", "任务:abc 提交人:dev 标题:t", ResultOK, CatSQLFlow, "sqlanalyze.run", false},
		{"SQL上线-提交（拒绝）", "工单id：1", ResultDenied, CatSQLFlow, "sqlworkflow.submit", false},
		{"Redis慢查询-重置慢日志", "实例：r（#4）；结果：失败（重置前的保底采集失败，已取消重置：x）", ResultFailed, CatSys, "redis.slowlog_reset", true},
		{"登录方式-测试", "公司 SSO（测试登录回调）：false", ResultFailed, CatSys, "authp.test", false},
		{"登录方式-测试", "公司 SSO（测试登录回调）：true", ResultOK, CatSys, "authp.test", false},
		// SQL 防火墙（internal/firewalladmin）：归到系统管理；ExtraInfo 以高危标记开头的才标高危
		{"SQL 防火墙规则变更", "新建规则「x」：场景 在线查询", ResultOK, CatSys, "", false},
		{"SQL 防火墙规则变更", FirewallRiskyMark + "规则「x」由观察切到生效", ResultOK, CatSys, "", true},
		{"SQL 防火墙例外", "为规则「x」添加例外：用户 a", ResultOK, CatSys, "", false},
		{"SQL 防火墙模式", FirewallRiskyMark + "全局模式：拦截 → 观察", ResultOK, CatSys, "", true},
		{"SQL 防火墙总开关", FirewallRiskyMark + "关闭 SQL 防火墙总开关", ResultOK, CatSys, "", true},
		{"从没见过的动作", "", ResultOK, CatOther, "", false},
	}
	for _, c := range cases {
		got := Classify(c.action, c.extra)
		if got.Result != c.result || got.Category != c.cat || got.Code != c.code || got.Risky != c.risky {
			t.Errorf("Classify(%q, %q) = %+v，期望 结果=%s 类别=%s 码=%q 高危=%v", c.action, c.extra, got, c.result, c.cat, c.code, c.risky)
		}
		if got.Module == "" {
			t.Errorf("Classify(%q) 模块为空", c.action)
		}
		if c.code != "" && c.code != loginCode && !btnperm.Valid(c.code) {
			t.Errorf("规则里的权限码 %q 不在按钮登记表里", c.code)
		}
	}
	if m := Classify("离线下载", "").Module; m != "数据导出" {
		t.Errorf("模块应取权限码所在页面名: %q", m)
	}
	if m := Classify("登入", "").Module; m != "登录认证" {
		t.Errorf("登录模块: %q", m)
	}
}

// 规则表里出现的每个权限码都必须是按钮登记表里登记过的（防止登记表改名后这里悄悄失效）。
func TestRuleCodesAreRegistered(t *testing.T) {
	check := func(m map[string]rule) {
		for a, ru := range m {
			if ru.code != "" && ru.code != loginCode && !btnperm.Valid(ru.code) {
				t.Errorf("动作 %q 的权限码 %q 未登记", a, ru.code)
			}
		}
	}
	check(exactRules)
	check(inspectRules)
	for _, a := range []string{"SQL优化-x", "分区维护-x", "性能工单配置-采集配置新增"} {
		if c := Classify(a, "").Code; c != "" && !btnperm.Valid(c) {
			t.Errorf("%s -> %s 未登记", a, c)
		}
	}
}

func TestIPFromEntry(t *testing.T) {
	cases := []struct{ action, extra, want string }{
		{"登入", "10.1.2.3（本地）", "10.1.2.3"},
		{"登入", "10.1.2.3（API）", "10.1.2.3"},
		{"登入", "2001:db8::1（本地）", "2001:db8::1"},
		{"登入", "（本地）", ""},
		{"注册用户", "192.168.0.9", "192.168.0.9"},
		{"登入失败", "10.1.2.3", ""},
		{"修改用户", "10.1.2.3（x）", ""},
	}
	for _, c := range cases {
		if got := ipFromEntry(c.action, c.extra); got != c.want {
			t.Errorf("ipFromEntry(%q,%q)=%q, want %q", c.action, c.extra, got, c.want)
		}
	}
}

func TestBindIPIsPerGoroutine(t *testing.T) {
	if boundIP() != "" {
		t.Fatal("未绑定时应为空")
	}
	release := BindIP("1.2.3.4")
	if boundIP() != "1.2.3.4" {
		t.Fatal("绑定后应能读到")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if boundIP() != "" {
			t.Error("其他 goroutine 不应读到")
		}
	}()
	wg.Wait()
	release()
	if boundIP() != "" {
		t.Fatal("释放后应为空")
	}
	BindIP("")() // 空 IP 不绑定
}

// ---- GORM 回调 ----

type rec struct {
	sql  string
	args []driver.Value
}

type recDriver struct {
	mu      sync.Mutex
	logs    []rec
	failExt error
	nextID  int64
}

func (d *recDriver) Open(string) (driver.Conn, error) { return &recConn{d}, nil }

type recConn struct{ d *recDriver }

func (c *recConn) Prepare(q string) (driver.Stmt, error) { return &recStmt{c.d, q}, nil }
func (c *recConn) Close() error                          { return nil }
func (c *recConn) Begin() (driver.Tx, error)             { return recTx{}, nil }

type recTx struct{}

func (recTx) Commit() error   { return nil }
func (recTx) Rollback() error { return nil }

type recStmt struct {
	d *recDriver
	q string
}

func (s *recStmt) Close() error  { return nil }
func (s *recStmt) NumInput() int { return -1 }
func (s *recStmt) Exec(args []driver.Value) (driver.Result, error) {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	s.d.logs = append(s.d.logs, rec{strings.Join(strings.Fields(s.q), " "), args})
	if strings.Contains(s.q, ExtTable) && s.d.failExt != nil {
		return nil, s.d.failExt
	}
	s.d.nextID++
	return recResult{s.d.nextID}, nil
}
func (s *recStmt) Query(args []driver.Value) (driver.Rows, error) { return emptyRows{}, nil }

type recResult struct{ id int64 }

func (r recResult) LastInsertId() (int64, error) { return r.id, nil }
func (recResult) RowsAffected() (int64, error)   { return 1, nil }

type emptyRows struct{}

func (emptyRows) Columns() []string              { return []string{"c"} }
func (emptyRows) Close() error                   { return nil }
func (emptyRows) Next(dest []driver.Value) error { return io.EOF }

func (d *recDriver) ext() []rec {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []rec
	for _, l := range d.logs {
		if strings.Contains(l.sql, ExtTable) {
			out = append(out, l)
		}
	}
	return out
}

var driverSeq atomic.Int32

func newHookDB(t *testing.T) (*gorm.DB, *recDriver) {
	t.Helper()
	extDisabledUntil.Store(0)
	d := &recDriver{}
	name := "audit-ext-" + t.Name() + "-" + strconv.Itoa(int(driverSeq.Add(1)))
	sql.Register(name, d)
	sqlDB, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}), &gorm.Config{
		Logger: logger.Discard, NamingStrategy: schema.NamingStrategy{SingularTable: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	RegisterExtHook(db)
	return db, d
}

func TestHookWritesExtForEveryCaller(t *testing.T) {
	db, d := newHookDB(t)
	release := BindIP("203.0.113.9")
	defer release()

	// 直接 Create（多数模块的写法）
	e := model.AuditEntry{UserID: 1, UserName: "root", Action: "终止会话", ExtraInfo: "实例：a", ActionTime: time.Now()}
	if err := db.Create(&e).Error; err != nil {
		t.Fatal(err)
	}
	// 事务里 Create（sysadmin / btnperm 的写法）
	err := db.Transaction(func(tx *gorm.DB) error {
		return tx.Create(&model.AuditEntry{UserID: 1, UserName: "root", Action: "登入", ExtraInfo: "10.9.9.9（本地）", ActionTime: time.Now()}).Error
	})
	if err != nil {
		t.Fatal(err)
	}
	// 与 dbt_audit_log 无关的表不触发
	_ = db.Create(&model.NotifyChannel{Name: "x"}).Error

	got := d.ext()
	if len(got) != 2 {
		t.Fatalf("应写 2 条扩展信息: %+v", got)
	}
	a := got[0].args
	if a[0] != int64(1) || a[1] != ResultOK || a[2] != "203.0.113.9" || a[3] != CatInst || a[5] != "session.kill" || a[6] != true {
		t.Errorf("第一条: %+v", a)
	}
	if b := got[1].args; b[2] != "10.9.9.9" || b[3] != CatLogin || b[5] != "auth.login" {
		t.Errorf("登录审计应取额外信息里的 IP: %+v", b)
	}
	if !strings.Contains(got[0].sql, "ON DUPLICATE KEY UPDATE") {
		t.Errorf("应幂等: %s", got[0].sql)
	}
}

func TestHookBatchAndFailureNeverBreaksAudit(t *testing.T) {
	db, d := newHookDB(t)
	batch := []model.AuditEntry{{UserName: "a", Action: "登入失败", ActionTime: time.Now()}, {UserName: "b", Action: "新建用户", ActionTime: time.Now()}}
	if err := db.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	if n := len(d.ext()); n != 2 {
		t.Errorf("批量写入应各写一条扩展: %d", n)
	}

	// 扩展表不存在：审计本身照常成功，之后 1 分钟内不再尝试
	db, d = newHookDB(t)
	d.failExt = errors.New("Error 1146: Table 'dbt_audit_ext' doesn't exist")
	if err := db.Create(&model.AuditEntry{UserName: "a", Action: "x", ActionTime: time.Now()}).Error; err != nil {
		t.Fatalf("扩展表缺失不应影响审计写入: %v", err)
	}
	if err := db.Create(&model.AuditEntry{UserName: "a", Action: "y", ActionTime: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	if n := len(d.ext()); n != 1 {
		t.Errorf("失败后应暂停重试: %d", n)
	}
	extDisabledUntil.Store(0)
}
