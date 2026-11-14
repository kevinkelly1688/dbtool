// @author: kevinkelly
// @license: Apache Licence
// @file: redis_test.go
// @time: 2026/10/28

package instmgr

import (
	"context"
	"strings"
	"testing"
	"time"

	_ "dbtool/internal/engine/redis" // 注册 redis 引擎（生产环境在 cmd/server/main.go 里注册）
	"dbtool/internal/engine/redis/redistest"
)

// Redis 实例的连通性测试 / 健康探测走 internal/engine 里注册的 redis 引擎（Probe），不走 MySQL 协议。
func TestProbeRedis(t *testing.T) {
	srv, err := redistest.New(func(a []string) any {
		switch strings.ToUpper(a[0]) {
		case "AUTH":
			if len(a) == 2 && a[1] == "s3cret" {
				return redistest.Simple("OK")
			}
			return redistest.Err("WRONGPASS invalid username-password pair or user is disabled.")
		case "PING":
			return redistest.Simple("PONG")
		case "INFO":
			return "# Server\r\nredis_version:7.2.4\r\n"
		}
		return redistest.Err("ERR unknown command")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ver, err := NewDialer().Probe(ctx, ConnInfo{DBType: "redis", Host: srv.Host(), Port: srv.Port(), Password: "s3cret"})
	if err != nil || ver != "7.2.4" {
		t.Fatalf("ver=%q err=%v", ver, err)
	}
	// 密码错误：报错，且错误里不带密码
	_, err = NewDialer().Probe(ctx, ConnInfo{DBType: "redis", Host: srv.Host(), Port: srv.Port(), Password: "bad-password"})
	if err == nil || strings.Contains(err.Error(), "bad-password") {
		t.Fatalf("err=%v", err)
	}
	// 实例管理只对 mysql 系走数据库连接，redis 走 Probe
	if mysqlLike["redis"] || supportsAccount("redis") || supportsSession("redis") || supportsBinlogPurge("redis") {
		t.Error("redis 不应被当成 MySQL 系")
	}
	if !noAuthTypes["redis"] || !validDBType("redis") || dbTypeLabel("redis") != "Redis" {
		t.Error("redis 应是可选的、可以不填账号密码的类型")
	}
}
