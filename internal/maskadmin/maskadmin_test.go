// @author: kevinkelly
// @license: Apache Licence
// @file: maskadmin_test.go
// @time: 2026/10/28

package maskadmin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/engine"
	"dbtool/internal/engine/dm"
	"dbtool/internal/engine/oracle"
	"dbtool/internal/masking"
	"dbtool/internal/model"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
	"dbtool/internal/settings"
)

// ---- 假依赖 ----

type fakeStore struct {
	ready    bool
	policies []model.MaskingPolicy
	audits   []model.AuditEntry
	failNext error
	nextID   uint
	cfg      map[string]string // 与 servertest.Repo.Config 是同一个 map：模拟“写库后立刻读得到”
	cfgDesc  map[string]string
}

func (f *fakeStore) SetConfig(item, value, description string, a *model.AuditEntry) error {
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return err
	}
	if f.cfgDesc == nil {
		f.cfgDesc = map[string]string{}
	}
	f.cfg[item] = value
	f.cfgDesc[item] = description
	if a != nil {
		f.audits = append(f.audits, *a)
	}
	return nil
}

func (f *fakeStore) PolicyReady() bool { return f.ready }
func (f *fakeStore) ListPolicies(id uint) ([]model.MaskingPolicy, error) {
	var out []model.MaskingPolicy
	for _, p := range f.policies {
		if p.InstanceID == id {
			out = append(out, p)
		}
	}
	return out, nil
}
func (f *fakeStore) Counts() (map[uint]int, error) {
	out := map[uint]int{}
	for _, p := range f.policies {
		if p.Active {
			out[p.InstanceID]++
		}
	}
	return out, nil
}
func (f *fakeStore) Commit(ops []storeOp, a *model.AuditEntry) error {
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return err
	}
	for _, op := range ops {
		switch op.kind {
		case opInsertPolicy:
			f.nextID++
			p := op.policy
			p.ID = 1000 + f.nextID
			f.policies = append(f.policies, p)
		case opUpdatePolicy:
			for i := range f.policies {
				if f.policies[i].ID == op.policy.ID {
					f.policies[i] = op.policy
				}
			}
		case opDeletePolicy:
			for i := range f.policies {
				if f.policies[i].ID == op.id {
					f.policies = append(f.policies[:i], f.policies[i+1:]...)
					break
				}
			}
		}
	}
	if a != nil {
		f.audits = append(f.audits, *a)
	}
	return nil
}

// catalogEngine 是返回固定字段目录的假引擎。
type catalogEngine struct {
	engine.Engine
	sql  string
	rows [][]any
	dbs  []string
	err  error
}

func (e *catalogEngine) Close() error { return nil }
func (e *catalogEngine) AllDatabases(context.Context) ([]string, error) {
	return e.dbs, e.err
}
func (e *catalogEngine) Query(_ context.Context, q string, _ engine.QueryOptions) (*engine.ResultSet, error) {
	e.sql = q
	if e.err != nil {
		return nil, e.err
	}
	return &engine.ResultSet{ColumnList: []string{"t", "c", "ty", "cm", "v"}, Rows: e.rows}, nil
}

var actor = audit.Actor{ID: 1, Username: "root", Display: "管理员", IsSuperuser: true}

type env struct {
	st  *fakeStore
	eng *catalogEngine
	svc *Service
	te  *servertest.Env
	// openFn 不为空时按它打开引擎（Oracle / 达梦的目录走引擎元数据 API，需要换成别的假引擎）
	openFn func(string, engine.Conn) (engine.Engine, error)
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{st: &fakeStore{ready: true}, eng: &catalogEngine{}}
	repo := &servertest.Repo{Instances: []model.Instance{
		{ID: 1, InstanceName: "prod-mysql", DBType: "mysql"},
		{ID: 2, InstanceName: "risk-pg", DBType: "pgsql"},
		{ID: 3, InstanceName: "cache-redis", DBType: "redis"},
		{ID: 4, InstanceName: "es-log", DBType: "elasticsearch"},
		{ID: 5, InstanceName: "legacy-cassandra", DBType: "cassandra"},
		{ID: 6, InstanceName: "app-mongo", DBType: "mongo"},
		{ID: 7, InstanceName: "erp-oracle", DBType: "oracle"},
		{ID: 8, InstanceName: "gov-dm", DBType: "dm"},
	}, Config: map[string]string{settings.KeyQueryMaskEnabled: "true"}}
	e.st.cfg = repo.Config
	open := func(dbType string, c engine.Conn) (engine.Engine, error) {
		if e.openFn != nil {
			return e.openFn(dbType, c)
		}
		return e.eng, nil
	}
	e.te = servertest.New(t, servertest.Options{Repo: repoWithUsers(t, repo), Hooks: []func(*server.Server, server.Routes){
		func(s *server.Server, rt server.Routes) {
			e.svc = NewService(s.Repo(), e.st, open)
			register(s, rt, e.svc)
		}}})
	e.svc.now = func() time.Time { return time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC) }
	return e
}

// repoWithUsers 给仓库补上 root / bob（servertest 只在没传仓库时才建默认用户）。
func repoWithUsers(t *testing.T, r *servertest.Repo) *servertest.Repo {
	t.Helper()
	def := servertest.New(t, servertest.Options{})
	r.Users, r.Perms = def.Repo.Users, def.Repo.Perms
	return r
}

func keep(n, m int) *RuleSpec { return &RuleSpec{Kind: masking.KindKeep, Prefix: n, Suffix: m} }

func set(db, tb, f string, r *RuleSpec) Change {
	return Change{Op: "set", ScopeDB: db, ScopeTable: tb, Field: f, Rule: r}
}

// ---- 保存 ----

func TestSaveAddsPoliciesAndWritesAudit(t *testing.T) {
	e := newEnv(t)
	res, err := e.svc.Save(actor, SaveRequest{InstanceID: 1, Changes: []Change{
		set("shop", "users", "phone", keep(3, 4)),
		set("shop", "orders", "receiver_phone", keep(3, 4)),
		set("*", "*", "email", keep(2, 0)),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 3 || res.Updated != 0 || len(e.st.policies) != 3 {
		t.Fatalf("res=%+v policies=%d", res, len(e.st.policies))
	}
	p := e.st.policies[0]
	if p.InstanceID != 1 || p.RuleKind != "keep" || p.KeepPrefix != 3 || p.KeepSuffix != 4 || !p.Active || p.UpdatedBy != "root" {
		t.Errorf("策略 = %+v", p)
	}
	if len(e.st.audits) != 1 || !strings.Contains(e.st.audits[0].ExtraInfo, "新增 3") || !strings.Contains(e.st.audits[0].ExtraInfo, "prod-mysql") || e.st.audits[0].Action != "数据脱敏配置变更" {
		t.Errorf("审计 = %+v", e.st.audits)
	}
	// 同一个字段再设置：覆盖，不新增
	res, err = e.svc.Save(actor, SaveRequest{InstanceID: 1, Changes: []Change{set("SHOP", "Users", "PHONE", &RuleSpec{Kind: masking.KindWhole})}})
	if err != nil || res.Updated != 1 || res.Added != 0 || len(e.st.policies) != 3 {
		t.Fatalf("覆盖: %+v %v policies=%d", res, err, len(e.st.policies))
	}
	if e.st.policies[0].RuleKind != "whole" {
		t.Errorf("规则应被覆盖: %+v", e.st.policies[0])
	}
}

func TestSaveEnableDisable(t *testing.T) {
	e := newEnv(t)
	e.st.policies = []model.MaskingPolicy{{ID: 1, InstanceID: 1, ScopeDB: "shop", ScopeTable: "users", FieldPath: "phone", RuleKind: "whole", Active: true}}
	res, err := e.svc.Save(actor, SaveRequest{InstanceID: 1, Changes: []Change{
		{Op: "disable", ScopeDB: "shop", ScopeTable: "users", Field: "phone"},
	}})
	if err != nil || res.Toggled != 1 || e.st.policies[0].Active {
		t.Fatalf("%+v %v policies=%+v", res, err, e.st.policies)
	}
	if _, err := e.svc.Save(actor, SaveRequest{InstanceID: 1, Changes: []Change{{Op: "enable", ScopeDB: "shop", ScopeTable: "users", Field: "phone"}}}); err != nil || !e.st.policies[0].Active {
		t.Errorf("启用失败: %v %+v", err, e.st.policies)
	}
}

func TestSaveValidation(t *testing.T) {
	e := newEnv(t)
	long := strings.Repeat("x", 300)
	bad := map[string]SaveRequest{
		"没有变更":   {InstanceID: 1},
		"不支持的实例": {InstanceID: 3, Changes: []Change{set("a", "b", "c", keep(1, 1))}},
		"ES 不支持": {InstanceID: 4, Changes: []Change{set("a", "b", "c", keep(1, 1))}},
		"Cassandra 不支持（没有在线查询入口）": {InstanceID: 5, Changes: []Change{set("a", "b", "c", keep(1, 1))}},
		"不存在的实例":  {InstanceID: 99, Changes: []Change{set("a", "b", "c", keep(1, 1))}},
		"缺规则":     {InstanceID: 1, Changes: []Change{{Op: "set", ScopeDB: "a", ScopeTable: "b", Field: "c"}}},
		"规则不合法":   {InstanceID: 1, Changes: []Change{set("a", "b", "c", &RuleSpec{Kind: "keep", Prefix: 99})}},
		"正则不合法":   {InstanceID: 1, Changes: []Change{set("a", "b", "c", &RuleSpec{Kind: "regex", Regex: "(", Hide: 1})}},
		"库表通配不成对": {InstanceID: 1, Changes: []Change{set("*", "b", "c", keep(1, 1))}},
		"库名为空":    {InstanceID: 1, Changes: []Change{set("", "b", "c", keep(1, 1))}},
		"字段名过长":   {InstanceID: 1, Changes: []Change{set("a", "b", long, keep(1, 1))}},
		"未知操作":    {InstanceID: 1, Changes: []Change{{Op: "drop", ScopeDB: "a", ScopeTable: "b", Field: "c"}}},
	}
	for name, req := range bad {
		if _, err := e.svc.Save(actor, req); err == nil {
			t.Errorf("%s 应被拒绝", name)
		}
	}
	if len(e.st.policies) != 0 || len(e.st.audits) != 0 {
		t.Errorf("被拒绝的请求不能写数据: %+v %+v", e.st.policies, e.st.audits)
	}
	// 一次变更过多
	var many []Change
	for i := 0; i < maxChanges+1; i++ {
		many = append(many, set("a", "b", fmt.Sprintf("c%d", i), keep(1, 1)))
	}
	if _, err := e.svc.Save(actor, SaveRequest{InstanceID: 1, Changes: many}); err == nil {
		t.Error("超过上限应被拒绝")
	}
	// 策略表没建：给出明确提示，不写任何数据
	e.st.ready = false
	_, err := e.svc.Save(actor, SaveRequest{InstanceID: 1, Changes: []Change{set("a", "b", "c", keep(1, 1))}})
	var me *Error
	if !errors.As(err, &me) || me.Code != "not_ready" || !strings.Contains(me.Message, "230_masking_policy.sql") {
		t.Errorf("策略表没建: %v", err)
	}
}

func TestSaveLastChangeWinsAndStoreFailure(t *testing.T) {
	e := newEnv(t)
	res, err := e.svc.Save(actor, SaveRequest{InstanceID: 1, Changes: []Change{
		set("a", "b", "c", keep(1, 1)),
		set("A", "B", "C", keep(2, 2)),
	}})
	if err != nil || res.Added != 1 || len(e.st.policies) != 1 || e.st.policies[0].KeepPrefix != 2 {
		t.Fatalf("同一字段多次出现应以最后一次为准: %+v %v %+v", res, err, e.st.policies)
	}
	e.st.failNext = errors.New("db down")
	if _, err := e.svc.Save(actor, SaveRequest{InstanceID: 1, Changes: []Change{set("a", "b", "d", keep(1, 1))}}); err == nil {
		t.Error("存储失败应返回错误")
	}
}

// ---- 读取 ----

func TestConfigListsPolicies(t *testing.T) {
	e := newEnv(t)
	e.st.policies = []model.MaskingPolicy{{ID: 1, InstanceID: 1, ScopeDB: "shop", ScopeTable: "users", FieldPath: "phone", RuleKind: "keep", KeepPrefix: 3, KeepSuffix: 4, Active: true, UpdatedBy: "root"}}
	v, err := e.svc.Config(1)
	if err != nil {
		t.Fatal(err)
	}
	if !v.PolicyOK || !v.MaskingOn || !v.ExportOn || v.QueryCheck {
		t.Errorf("开关: %+v", v)
	}
	if len(v.Items) != 1 || v.Items[0].Source != "policy" || v.Items[0].RuleText != "保留前 3 位 + *** + 后 4 位" {
		t.Fatalf("应只有 1 项新策略: %+v", v.Items)
	}
	// 导出开关
	e.te.Repo.Config[settings.KeyExportMaskEnabled] = "false"
	if v, _ = e.svc.Config(1); v.ExportOn {
		t.Error("export_data_masking=false 时导出不脱敏")
	}
	e.te.Repo.Config[settings.KeyQueryMaskEnabled] = "false"
	e.te.Repo.Config[settings.KeyExportMaskEnabled] = "true"
	if v, _ = e.svc.Config(1); v.MaskingOn || v.ExportOn {
		t.Errorf("总开关关闭时导出也不脱敏: %+v", v)
	}
}

func TestInstancesListsSupportedFirstWithReasons(t *testing.T) {
	e := newEnv(t)
	e.st.policies = []model.MaskingPolicy{{InstanceID: 1, Active: true}, {InstanceID: 1, Active: true}}
	list, err := e.svc.Instances()
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]InstanceItem{}
	for _, it := range list {
		by[it.Name] = it
	}
	for _, n := range []string{"prod-mysql", "risk-pg", "app-mongo"} {
		if !by[n].Supported || by[n].Engine == nil {
			t.Errorf("%s 应支持脱敏: %+v", n, by[n])
		}
	}
	for _, n := range []string{"cache-redis", "es-log", "legacy-cassandra"} {
		if by[n].Supported || by[n].Why == "" {
			t.Errorf("%s 不应支持且要给出原因: %+v", n, by[n])
		}
	}
	if by["prod-mysql"].Count != 2 {
		t.Errorf("已配置计数: %+v", by["prod-mysql"])
	}
	if !list[0].Supported || list[len(list)-1].Supported {
		t.Errorf("支持的实例应排在前面: %+v", list)
	}
	if by["cache-redis"].Why != "键值结构，没有表 / 字段" || !strings.Contains(by["es-log"].Why, "索引文档") {
		t.Errorf("原因文案: %q / %q", by["cache-redis"].Why, by["es-log"].Why)
	}
	if by["risk-pg"].Engine.Locate != "exact" || by["app-mongo"].Engine.Locate != "path" || by["prod-mysql"].Engine.Hier != "库 › 表 › 字段" {
		t.Errorf("引擎说明: %+v %+v", by["risk-pg"].Engine, by["app-mongo"].Engine)
	}
}

func TestCatalogGroupsColumnsByTable(t *testing.T) {
	e := newEnv(t)
	e.eng.rows = [][]any{
		{"users", "id", "bigint", "", int64(0)},
		{"users", "phone", "varchar(20)", "手机号", int64(0)},
		{"v_users", "phone", "varchar(20)", "", int64(1)},
		{"orders", "id", "bigint", "", []byte("0")},
	}
	cat, err := e.svc.Catalog(context.Background(), 1, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Tables) != 3 || cat.Tables[0].Name != "users" || len(cat.Tables[0].Columns) != 2 || cat.Tables[0].Columns[1].Comment != "手机号" {
		t.Fatalf("目录: %+v", cat)
	}
	if !cat.Tables[1].View || cat.Tables[2].View {
		t.Errorf("视图标记: %+v", cat.Tables)
	}
	if !strings.Contains(e.eng.sql, "information_schema.COLUMNS") || !strings.Contains(e.eng.sql, "'shop'") {
		t.Errorf("MySQL 应读 information_schema 并带库名: %s", e.eng.sql)
	}
	if _, err := e.svc.Catalog(context.Background(), 1, ""); err == nil {
		t.Error("库名为空应报错")
	}
	// 库名里的引号被转义，不会拼进语句
	_, _ = e.svc.Catalog(context.Background(), 1, "a'b\\")
	if !strings.Contains(e.eng.sql, `'a''b\\'`) {
		t.Errorf("库名应转义: %s", e.eng.sql)
	}
	// PostgreSQL：读 pg_catalog，表名带架构
	e.eng.rows = [][]any{{"public.account", "phone", "character varying(20)", "", "0"}}
	cat, err = e.svc.Catalog(context.Background(), 2, "riskdb")
	if err != nil || cat.Tables[0].Name != "public.account" || !strings.Contains(e.eng.sql, "pg_catalog.pg_attribute") {
		t.Errorf("PG 目录: %+v %v sql=%s", cat, err, e.eng.sql)
	}
	// 引擎出错
	e.eng.err = errors.New("connection refused")
	if _, err := e.svc.Catalog(context.Background(), 1, "shop"); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("引擎错误应带原因: %v", err)
	}
}

func TestEngineInfoCoverage(t *testing.T) {
	for _, ty := range []string{"mysql", "polardb", "adb", "polardb-x", "polardbx", "selectdb", "doris", "mssql", "pgsql", "clickhouse", "mongo", "oracle", "dm"} {
		if info := engineInfo(ty); info == nil || info.Locate == "" || info.How == "" {
			t.Errorf("%s 缺少引擎说明", ty)
		}
		if !supported(ty) {
			t.Errorf("%s 应在脱敏范围内（有在线查询入口）", ty)
		}
	}
	// 定位方式要与查询侧一致：PolarDB-X 与 SelectDB / Doris 一样按列名保守匹配，MySQL / PolarDB / ADB 用语法树精确定位。
	for ty, want := range map[string]string{"polardb-x": "name", "polardbx": "name", "selectdb": "name", "doris": "name", "oracle": "name", "dm": "name", "mysql": "exact", "polardb": "exact", "adb": "exact"} {
		if got := engineInfo(ty).Locate; got != want {
			t.Errorf("%s 的定位方式 = %q，应为 %q", ty, got, want)
		}
	}
	for _, ty := range []string{"redis", "memcached", "elasticsearch", "opensearch", "cassandra", "tdengine"} {
		if supported(ty) {
			t.Errorf("%s 不应在脱敏范围内", ty)
		}
		if unsupportedReason(ty) == "" {
			t.Errorf("%s 应有不支持的原因", ty)
		}
	}
}

// ---- HTTP ----

// 总开关关着时，页面上的“开启总开关”把 data_masking 置为 true 并写审计；已经开着再点不写库、不重复记审计。
func TestEnableMasking(t *testing.T) {
	for _, before := range []map[string]string{{}, {settings.KeyQueryMaskEnabled: "false"}, {settings.KeyQueryMaskEnabled: "False"}} {
		e := newEnv(t)
		for k := range e.st.cfg {
			delete(e.st.cfg, k)
		}
		for k, v := range before {
			e.st.cfg[k] = v
		}
		v, _ := e.svc.Config(1)
		if v.MaskingOn {
			t.Fatalf("前提：总开关应是关闭的 %v", before)
		}
		res, err := e.svc.EnableMasking(actor)
		if err != nil || !res.MaskingOn || !res.Changed {
			t.Fatalf("开启 %v: %+v %v", before, res, err)
		}
		if e.st.cfg[settings.KeyQueryMaskEnabled] != "true" || e.st.cfgDesc[settings.KeyQueryMaskEnabled] == "" {
			t.Errorf("应写入 data_masking=true 和说明: %v %v", e.st.cfg, e.st.cfgDesc)
		}
		if v, _ := e.svc.Config(1); !v.MaskingOn || !v.ExportOn {
			t.Errorf("开启后配置视图应显示已开启且导出同步脱敏: %+v", v)
		}
		if len(e.st.audits) != 1 || e.st.audits[0].Action != "数据脱敏总开关" || e.st.audits[0].UserName != "root" ||
			!strings.Contains(e.st.audits[0].ExtraInfo, settings.KeyQueryMaskEnabled) {
			t.Errorf("审计 = %+v", e.st.audits)
		}
		// 再点一次：幂等
		res, err = e.svc.EnableMasking(actor)
		if err != nil || !res.MaskingOn || res.Changed || len(e.st.audits) != 1 {
			t.Errorf("已开启时应是空操作: %+v %v 审计=%d", res, err, len(e.st.audits))
		}
	}
}

func TestEnableMaskingAuditMentionsExportSwitchAndPriorValue(t *testing.T) {
	e := newEnv(t)
	e.st.cfg[settings.KeyQueryMaskEnabled] = "false"
	e.st.cfg[settings.KeyExportMaskEnabled] = "false"
	if _, err := e.svc.EnableMasking(actor); err != nil {
		t.Fatal(err)
	}
	info := e.st.audits[0].ExtraInfo
	if !strings.Contains(info, "原值 false") || !strings.Contains(info, "导出仍是原值") {
		t.Errorf("审计应写明原值和导出开关状态: %q", info)
	}
}

func TestEnableMaskingStoreFailureLeavesSwitchOff(t *testing.T) {
	e := newEnv(t)
	delete(e.st.cfg, settings.KeyQueryMaskEnabled)
	e.st.failNext = errors.New("db down")
	if _, err := e.svc.EnableMasking(actor); err == nil {
		t.Fatal("写库失败应返回错误")
	}
	if v, _ := e.svc.Config(1); v.MaskingOn || len(e.st.audits) != 0 {
		t.Errorf("失败后开关应保持关闭且没有审计: %+v 审计=%d", v, len(e.st.audits))
	}
}

func TestHTTPEnableMaskingSuperuserOnly(t *testing.T) {
	e := newEnv(t)
	delete(e.st.cfg, settings.KeyQueryMaskEnabled)
	if w := e.te.Anonymous("POST", "/api/v1/settings/masking/enable"); w.Code != http.StatusUnauthorized {
		t.Errorf("未登录 = %d", w.Code)
	}
	if w := e.te.Login("bob").Do("POST", "/api/v1/settings/masking/enable", map[string]any{}); w.Code != http.StatusForbidden {
		t.Errorf("普通用户 = %d", w.Code)
	}
	if _, on := e.st.cfg[settings.KeyQueryMaskEnabled]; on || len(e.st.audits) != 0 {
		t.Fatal("被拒绝的请求不应写任何东西")
	}
	w := e.te.Login("root").Do("POST", "/api/v1/settings/masking/enable", map[string]any{})
	if w.Code != http.StatusOK {
		t.Fatalf("超级用户 = %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Data ToggleResult `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if !body.Data.MaskingOn || !body.Data.Changed || e.st.cfg[settings.KeyQueryMaskEnabled] != "true" {
		t.Errorf("结果 = %+v cfg=%v", body.Data, e.st.cfg)
	}
}

func TestHTTPSuperuserOnlyAndEndToEnd(t *testing.T) {
	e := newEnv(t)
	routes := []struct{ method, path string }{
		{"GET", "/api/v1/settings/masking/instances"}, {"GET", "/api/v1/settings/masking/databases?instance_id=1"},
		{"GET", "/api/v1/settings/masking/catalog?instance_id=1&db=shop"}, {"GET", "/api/v1/settings/masking/config?instance_id=1"},
		{"POST", "/api/v1/settings/masking/save"},
	}
	bob := e.te.Login("bob")
	for _, r := range routes {
		if w := e.te.Anonymous(r.method, r.path); w.Code != http.StatusUnauthorized {
			t.Errorf("未登录 %s %s = %d", r.method, r.path, w.Code)
		}
		if w := bob.Do(r.method, r.path, map[string]any{}); w.Code != http.StatusForbidden {
			t.Errorf("普通用户 %s %s = %d", r.method, r.path, w.Code)
		}
	}
	if code, _ := bob.Page("/settings/masking"); code != http.StatusForbidden {
		t.Errorf("普通用户访问页面 = %d", code)
	}
	root := e.te.Login("root")
	if code, body := root.Page("/settings/masking"); code != http.StatusOK || !strings.Contains(body, "mk-tree") {
		t.Errorf("超级用户页面 = %d", code)
	}
	var list []InstanceItem
	root.GetJSON("/api/v1/settings/masking/instances", &list)
	if len(list) != 8 {
		t.Errorf("实例数 = %d", len(list))
	}
	w := root.Do("POST", "/api/v1/settings/masking/save", SaveRequest{InstanceID: 1, Changes: []Change{set("shop", "users", "phone", keep(3, 4))}})
	if w.Code != http.StatusOK {
		t.Fatalf("保存 = %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Data SaveResult `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Data.Added != 1 || len(e.st.policies) != 1 {
		t.Errorf("保存结果: %+v policies=%d", body.Data, len(e.st.policies))
	}
	// 非法请求：业务错误 400，且不写数据
	w = root.Do("POST", "/api/v1/settings/masking/save", SaveRequest{InstanceID: 3, Changes: []Change{set("a", "b", "c", keep(1, 1))}})
	if w.Code != http.StatusBadRequest {
		t.Errorf("不支持的实例 = %d %s", w.Code, w.Body.String())
	}
	if w := root.Do("GET", "/api/v1/settings/masking/config?instance_id=0", nil); w.Code != http.StatusBadRequest {
		t.Errorf("缺实例 = %d", w.Code)
	}
}

// oraEngine / dmEngine 是 Oracle / 达梦的假引擎：字段目录走引擎的元数据 API（ColumnRows + TableInfos），不拼 SQL。
type oraEngine struct {
	engine.Engine
	rows  []oracle.ColumnRow
	infos []oracle.TableInfo
	max   int
	err   error
}

func (e *oraEngine) Close() error { return nil }
func (e *oraEngine) ColumnRows(_ context.Context, _ string, max int) ([]oracle.ColumnRow, error) {
	e.max = max
	return e.rows, e.err
}
func (e *oraEngine) TableInfos(context.Context, string) ([]oracle.TableInfo, error) {
	return e.infos, nil
}

type dmEngine struct {
	engine.Engine
	rows  []dm.ColumnRow
	infos []dm.TableInfo
	err   error
}

func (e *dmEngine) Close() error { return nil }
func (e *dmEngine) ColumnRows(context.Context, string, int) ([]dm.ColumnRow, error) {
	return e.rows, e.err
}
func (e *dmEngine) TableInfos(context.Context, string) ([]dm.TableInfo, error) { return e.infos, nil }

func TestOracleAndDMCatalog(t *testing.T) {
	oe := &oraEngine{
		rows: []oracle.ColumnRow{
			{Table: "EMPLOYEES", Column: "ID", Type: "NUMBER(10)"}, {Table: "EMPLOYEES", Column: "PHONE", Type: "VARCHAR2(20 CHAR)", Comment: "手机号"},
			{Table: "EMP_V", Column: "PHONE", Type: "VARCHAR2(20 CHAR)"},
		},
		infos: []oracle.TableInfo{{Name: "EMPLOYEES", Kind: "table"}, {Name: "EMP_V", Kind: "view", IsView: true}},
	}
	de := &dmEngine{
		rows:  []dm.ColumnRow{{Table: "T1", Column: "C1", Type: "INT"}, {Table: "T1", Column: "C2", Type: "VARCHAR(10)", Comment: "备注"}},
		infos: []dm.TableInfo{{Name: "T1", Kind: "table"}},
	}
	e := newEnv(t)
	e.openFn = func(dbType string, _ engine.Conn) (engine.Engine, error) {
		if dbType == "dm" {
			return de, nil
		}
		return oe, nil
	}
	cat, err := e.svc.Catalog(context.Background(), 7, "HR")
	if err != nil || len(cat.Tables) != 2 || cat.Tables[0].Name != "EMPLOYEES" || len(cat.Tables[0].Columns) != 2 || cat.Tables[0].Columns[1].Comment != "手机号" {
		t.Fatalf("Oracle 目录: %+v %v", cat, err)
	}
	if cat.Tables[0].View || !cat.Tables[1].View {
		t.Errorf("视图标记: %+v", cat.Tables)
	}
	if oe.max != maxCatalogColumns+1 {
		t.Errorf("多取一行用来判断是否截断: %d", oe.max)
	}
	cat, err = e.svc.Catalog(context.Background(), 8, "APP")
	if err != nil || len(cat.Tables) != 1 || cat.Tables[0].Columns[1].Type != "VARCHAR(10)" {
		t.Fatalf("达梦目录: %+v %v", cat, err)
	}
	// 超出上限：截断并给出说明
	for i := 0; i < maxCatalogColumns+1; i++ {
		oe.rows = append(oe.rows, oracle.ColumnRow{Table: "BIG", Column: fmt.Sprintf("C%d", i), Type: "INT"})
	}
	cat, err = e.svc.Catalog(context.Background(), 7, "HR")
	if err != nil || !cat.Truncated || cat.Note == "" {
		t.Errorf("应标记已截断: %+v %v", cat, err)
	}
	// 引擎出错
	oe.err = errors.New("ORA-12541: no listener")
	if _, err := e.svc.Catalog(context.Background(), 7, "HR"); err == nil || !strings.Contains(err.Error(), "no listener") {
		t.Errorf("引擎错误应带原因: %v", err)
	}
	for _, ty := range []string{"oracle", "dm"} {
		info := engineInfo(ty)
		if info == nil || info.Locate != "name" || info.Path != "库.表.列" {
			t.Errorf("%s 引擎说明: %+v", ty, info)
		}
	}
}
