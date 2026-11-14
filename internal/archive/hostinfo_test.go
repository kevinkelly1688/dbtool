// @author: kevinkelly
// @license: Apache Licence
// @file: hostinfo_test.go
// @time: 2026/10/28

package archive

import (
	"context"
	"errors"
	"testing"

	"dbtool/internal/model"
)

func TestSlaveByHostMatchesRegisteredSlaves(t *testing.T) {
	e := newEnv(t)
	src := e.svc.meta.(*fakeSource)
	src.list = append(src.list,
		model.Instance{ID: 20, InstanceName: "s-ip", Type: "slave", DBType: "mysql", IsEnable: true, Host: "10.0.0.2"},
		model.Instance{ID: 21, InstanceName: "s-name", Type: "slave", DBType: "mariadb", IsEnable: true, Host: "db-s2.corp"},
		model.Instance{ID: 22, InstanceName: "s-off", Type: "slave", DBType: "mysql", IsEnable: false, Host: "10.0.0.2"},
		model.Instance{ID: 23, InstanceName: "s-mssql", Type: "slave", DBType: "mssql", IsEnable: true, Host: "10.0.0.2"},
		model.Instance{ID: 24, InstanceName: "m", Type: "master", DBType: "mysql", IsEnable: true, Host: "10.0.0.2"})
	old := lookupHost
	defer func() { lookupHost = old }()
	lookupHost = func(ctx context.Context, h string) ([]string, error) {
		if h == "db-s2.corp" {
			return []string{"10.0.0.3"}, nil
		}
		return nil, errors.New("nxdomain")
	}
	ctx := context.Background()
	if c, err := e.svc.slaveByHost(ctx, "10.0.0.2"); err != nil || len(c) != 1 {
		t.Fatalf("只匹配启用的 MySQL/MariaDB 从库：%d %v", len(c), err)
	}
	if c, _ := e.svc.slaveByHost(ctx, "10.0.0.3"); len(c) != 1 {
		t.Fatalf("登记的是主机名时解析后匹配：%d", len(c))
	}
	if c, _ := e.svc.slaveByHost(ctx, "10.9.9.9"); len(c) != 0 {
		t.Fatalf("认不出的主机返回空：%d", len(c))
	}
}

func TestDiskFreeFromResult(t *testing.T) {
	cases := []struct {
		in   string
		pct  float64
		ok   bool
		name string
	}{
		{`{"capacity":{"disk_free_pct":42.5}}`, 42.5, true, "百分比字段"},
		{`{"capacity":{"disk_free_bytes":25,"disk_total_bytes":100}}`, 25, true, "字节换算"},
		{`{"capacity":{"database_count":3,"table_count":9,"total_data_size_mb":10.5}}`, 0, false, "目前的 MySQL 巡检没有磁盘指标"},
		{`{"capacity":{"disk_free_pct":142}}`, 0, false, "越界"},
		{`{"capacity":{"disk_free_bytes":5,"disk_total_bytes":0}}`, 0, false, "总量为 0"},
		{`not json`, 0, false, "非 JSON"},
		{``, 0, false, "空"},
	}
	for _, c := range cases {
		if p, ok := diskFreeFromResult(c.in); ok != c.ok || (ok && p != c.pct) {
			t.Errorf("%s：%v %v", c.name, p, ok)
		}
	}
}
