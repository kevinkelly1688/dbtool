// @author: kevinkelly
// @license: Apache Licence
// @file: integration_test.go
// @time: 2026/10/28

package mongo

import (
	"context"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"dbtool/internal/engine"
)

// 集成测试：默认跳过。设置 MONGO_IT_URI=mongodb://user:pass@host:27017 后才会连真实 MongoDB，
// 只做只读检查（ping / 版本 / 库列表 / 对第一个库做一次 getCollectionNames）。
func TestIntegration(t *testing.T) {
	raw := os.Getenv("MONGO_IT_URI")
	if raw == "" {
		t.Skip("未设置 MONGO_IT_URI，跳过真实 MongoDB 集成测试")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	c := engine.Conn{Host: u.Hostname(), Port: port, User: u.User.Username()}
	c.Password, _ = u.User.Password()
	e, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e.TestConnection(ctx); err != nil {
		t.Fatal(err)
	}
	v, err := e.ServerVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("version=%s", v)
	dbs, err := e.AllDatabases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("databases=%v", dbs)
	if len(dbs) == 0 {
		return
	}
	ck, err := CheckUserQuery(`db.getCollectionNames()`)
	if err != nil {
		t.Fatal(err)
	}
	sh, err := e.RunUser(ctx, dbs[0], ck, QueryLimits{MaxRows: 50, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("collections=%d", len(sh.Rows))
}
