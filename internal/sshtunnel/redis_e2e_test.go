// @author: kevinkelly
// @license: Apache Licence
// @file: redis_e2e_test.go
// @time: 2026/10/28

package sshtunnel

import (
	"context"
	"strings"
	"testing"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/engine/redis"
	"dbtool/internal/engine/redis/redistest"
	"dbtool/internal/model"
)

// Redis 引擎（自写 RESP 客户端）只读 engine.Conn 的 Host/Port，所以经转发器自然生效。
func TestRedisClientThroughTunnel(t *testing.T) {
	srv, err := redistest.New(func(a []string) any {
		switch strings.ToUpper(a[0]) {
		case "PING":
			return redistest.Simple("PONG")
		case "SLOWLOG":
			return int64(0)
		}
		return redistest.Err("ERR unknown")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ssrv := startSSH(t, "pw", nil)
	sh, sp := ssrv.hostPort(t)
	m := newMgr(t, Options{})
	r := &Resolver{Mgr: m}
	tid := uint(1)
	inst := &model.Instance{ID: 1, DBType: "redis", Host: srv.Host(), Port: srv.Port(), TunnelID: &tid,
		Tunnel: &model.Tunnel{ID: 1, Host: sh, Port: sp, User: ptr("tunnel"), Password: ptr("pw")}}
	conn := r.Apply(inst, engine.Conn{Host: srv.Host(), Port: srv.Port()})
	if conn.Port == srv.Port() {
		t.Fatalf("应改写端口: %+v", conn)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cl, err := redis.Dial(ctx, redis.ConfigFromConn(conn))
	if err != nil {
		t.Fatalf("Redis 客户端经隧道连接: %v", err)
	}
	defer cl.Close()
	if err := cl.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := cl.SlowlogLen(ctx); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}

	// 隧道失败时 Redis 客户端的错误里能看出是隧道
	bad := &model.Instance{ID: 2, DBType: "redis", Host: srv.Host(), Port: srv.Port(), TunnelID: &tid,
		Tunnel: &model.Tunnel{ID: 1, Host: sh, Port: sp, User: ptr("tunnel"), Password: ptr("wrong")}}
	conn = r.Apply(bad, engine.Conn{Host: srv.Host(), Port: srv.Port()})
	cl2, err := redis.Dial(ctx, redis.ConfigFromConn(conn))
	if err == nil {
		defer cl2.Close()
		err = cl2.Ping(ctx)
	}
	if err == nil || !strings.Contains(err.Error(), "SSH 隧道") {
		t.Fatalf("隧道失败的错误应带 SSH 隧道说明: %v", err)
	}
}
