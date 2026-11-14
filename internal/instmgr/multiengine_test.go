// @author: kevinkelly
// @license: Apache Licence
// @file: multiengine_test.go
// @time: 2026/10/28

package instmgr

import (
	"context"
	"strings"
	"testing"

	"dbtool/internal/engine"
	"dbtool/internal/engine/clickhouse"
	_ "dbtool/internal/engine/dm"     // 注册达梦引擎
	_ "dbtool/internal/engine/oracle" // 注册 Oracle 引擎
)

// PostgreSQL / MongoDB / ClickHouse：只接入在线查询。实例管理里这些类型能新建 / 测试连接，
// 但会话、账号、参数、数据库管理、Binlog 等功能一律“暂不支持”（supportsXxx 是显式白名单，不含这些类型）。

var newTypes = []string{"pgsql", "mongo", "clickhouse", "oracle", "dm"}

func TestMultiEngineTypesAreNotMySQLLike(t *testing.T) {
	for _, ty := range newTypes {
		if mysqlLike[ty] || supportsSession(ty) || supportsTrx(ty) || supportsAccount(ty) || supportsDatabase(ty) || supportsParam(ty) || supportsBinlogPurge(ty) || supportsCharset(ty) {
			t.Errorf("%s 不应被当成 MySQL 系", ty)
		}
		if !validDBType(ty) || !queryOnly(ty) {
			t.Errorf("%s 应是可选的类型", ty)
		}
		codes, label := featuresOf(ty)
		if len(codes) != 1 || codes[0] != "query" || label != "在线查询" {
			t.Errorf("%s 的支持范围: %v %q", ty, codes, label)
		}
	}
	for _, ty := range []string{"mysql", "mssql", "redis", "doris", "polardb", "cassandra"} {
		if codes, label := featuresOf(ty); codes != nil || label != "" {
			t.Errorf("%s 不应有支持范围标记: %v %q", ty, codes, label)
		}
	}
	if dbTypeLabel("pgsql") != "PgSQL" || dbTypeLabel("mongo") != "Mongo" || dbTypeLabel("clickhouse") != "ClickHouse" || dbTypeLabel("oracle") != "Oracle" || dbTypeLabel("dm") != "达梦 DM" {
		t.Error("类型标签")
	}
	// 用户名：Mongo 可空，PG / ClickHouse 必填（沿用 noAuthTypes）
	if !noAuthTypes["mongo"] || noAuthTypes["pgsql"] || noAuthTypes["clickhouse"] {
		t.Errorf("noAuthTypes: %v", noAuthTypes)
	}
}

func TestMultiEngineFormOptionsAndList(t *testing.T) {
	e := newEnv()
	e.repo.addInstance(6, "pg-01", "pgsql").Port = 5432
	e.repo.addInstance(7, "mongo-01", "mongo").Port = 27017
	e.repo.addInstance(8, "ch-01", "clickhouse").Port = 8123
	e.repo.addInstance(9, "ora-01", "oracle").Port = 1521
	e.repo.addInstance(10, "dm-01", "dm").Port = 5236
	o, err := e.svc.FormOptions()
	if err != nil {
		t.Fatal(err)
	}
	if o.DefaultPorts["pgsql"] != 5432 || o.DefaultPorts["mongo"] != 27017 || o.DefaultPorts["clickhouse"] != 8123 || o.DefaultPorts["oracle"] != 1521 || o.DefaultPorts["dm"] != 5236 || len(o.DefaultPorts) != 5 {
		t.Errorf("默认端口: %v", o.DefaultPorts)
	}
	list, err := e.svc.ListInstances(root, ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range list.Items {
		isNew := it.DBType == "pgsql" || it.DBType == "mongo" || it.DBType == "clickhouse" || it.DBType == "oracle" || it.DBType == "dm"
		if isNew != (len(it.Features) == 1 && it.FeaturesLabel == "在线查询") {
			t.Errorf("%s(%s): features=%v label=%q", it.Name, it.DBType, it.Features, it.FeaturesLabel)
		}
		if !isNew && (it.Features != nil || it.FeaturesLabel != "") {
			t.Errorf("%s 不应有支持范围标记", it.Name)
		}
	}
}

func TestMultiEngineUnsupportedFeaturesSayWhy(t *testing.T) {
	e := newEnv()
	e.repo.addInstance(6, "pg-01", "pgsql")
	e.repo.addInstance(7, "mongo-01", "mongo")
	e.repo.addInstance(8, "ch-01", "clickhouse")
	e.repo.addInstance(9, "ora-01", "oracle")
	e.repo.addInstance(10, "dm-01", "dm")
	ctx := context.Background()
	for id, label := range map[uint]string{6: "PgSQL", 7: "Mongo", 8: "ClickHouse", 9: "Oracle", 10: "达梦 DM"} {
		_, err := e.svc.Processlist(ctx, root, id, ProcFilter{})
		wantErr(t, err, 400, "暂不支持 "+label+" 类型实例")
		_, err = e.svc.ListAccounts(ctx, root, id, false, "")
		wantErr(t, err, 400, "暂不支持 "+label+" 类型实例")
		_, err = e.svc.ListDatabases(ctx, root, id, false, "")
		wantErr(t, err, 400, "暂不支持 "+label+" 类型实例")
		_, err = e.svc.ListParams(ctx, root, id, "", "")
		wantErr(t, err, 400, "暂不支持 "+label+" 类型实例")
		_, err = e.svc.ListBinlogs(ctx, root, id)
		wantErr(t, err, 400, "暂不支持 "+label+" 类型实例")
	}
	if len(e.dialer.dialed) != 0 {
		t.Errorf("不支持的功能不应连接目标库: %v", e.dialer.dialed)
	}
}

// 测试连接对新类型走 internal/engine 里注册的引擎（Probe）：这里用真实的 ClickHouse 引擎连假 HTTP 服务。
func TestProbeUsesRegisteredEngines(t *testing.T) {
	var _ = clickhouse.DBType // 保证引擎包被链接、已注册
	for _, ty := range newTypes {
		eng, err := engine.New(ty, engine.Conn{})
		if err == nil {
			_ = eng.Close()
		}
		if err != nil && strings.Contains(err.Error(), "暂不支持的数据库类型") {
			t.Errorf("%s 引擎没有注册: %v", ty, err)
		}
	}
	// 缺少 host 时由引擎报错（不是“暂不支持”）
	_, err := NewDialer().Probe(context.Background(), ConnInfo{DBType: "pgsql"})
	if err == nil || strings.Contains(err.Error(), "暂不支持") {
		t.Errorf("pgsql 缺 host: %v", err)
	}
	// 经 SSH 隧道的非 MySQL 实例暂不能测试连接（与 SQL Server 一致）
	_, err = NewDialer().Probe(context.Background(), ConnInfo{DBType: "pgsql", Host: "h", Port: 5432, Tunnel: &TunnelInfo{}})
	if err == nil || !strings.Contains(err.Error(), "暂不支持") {
		t.Errorf("隧道: %v", err)
	}
}

// Oracle 的连接要有 service_name 或 SID（实例表单里填，测试连接和查询都会传给引擎）；达梦不需要，也不支持 SSL。
func TestProbeOracleAndDMConnInfo(t *testing.T) {
	_, err := NewDialer().Probe(context.Background(), ConnInfo{DBType: "oracle", Host: "h", Port: 1521, User: "u"})
	if err == nil || !strings.Contains(err.Error(), "service_name 或 SID") {
		t.Errorf("Oracle 缺 service_name / SID: %v", err)
	}
	_, err = NewDialer().Probe(context.Background(), ConnInfo{DBType: "dm", Host: "h", Port: 5236, User: "u", SSL: true})
	if err == nil || !strings.Contains(err.Error(), "SSL") {
		t.Errorf("达梦不支持 SSL: %v", err)
	}
	// 表单里填的 service_name / SID 会带进测试连接的 ConnInfo
	e := newEnv()
	e.repo.addInstance(9, "ora-01", "oracle").Port = 1521
	_, err = e.svc.TestForm(context.Background(), root, 0, InstanceInput{Name: "x", Role: "master", DBType: "oracle", Host: "h", Port: 1521, User: "u", Password: "p", ServiceName: " ORCLPDB1 ", SID: " ORCL "})
	_ = err
	if len(e.dialer.probed) != 1 || e.dialer.probed[0].ServiceName != "ORCLPDB1" || e.dialer.probed[0].SID != "ORCL" {
		t.Errorf("测试连接应带上 service_name / SID: %+v", e.dialer.probed)
	}
}
