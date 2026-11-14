// @author: kevinkelly
// @license: Apache Licence
// @file: ready_test.go
// @time: 2026/10/28

package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"dbtool/internal/config"
	"dbtool/internal/crypto"
	"dbtool/internal/model"
)

type readyRepo struct {
	*fakeRepo
	mu      sync.Mutex
	pingErr error
	tblErr  error
	missing map[string]bool
	pings   int
	hang    bool
}

func (r *readyRepo) PingMetaDB(ctx context.Context) error {
	r.mu.Lock()
	r.pings++
	hang, err := r.hang, r.pingErr
	r.mu.Unlock()
	if hang {
		<-ctx.Done()
		return ctx.Err()
	}
	return err
}

func (r *readyRepo) ExistingTables(_ context.Context, names []string) (map[string]bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tblErr != nil {
		return nil, r.tblErr
	}
	out := map[string]bool{}
	for _, n := range names {
		if !r.missing[n] {
			out[n] = true
		}
	}
	return out, nil
}

func newReadyEnv(t *testing.T) (*env, *readyRepo) {
	t.Helper()
	hash, _ := crypto.MakeDjangoPassword("pw-123", "salty", 1000)
	repo := &readyRepo{fakeRepo: &fakeRepo{users: map[string]*model.User{
		"bob": {ID: 2, Username: "bob", Password: hash, IsActive: true},
	}}, missing: map[string]bool{}}
	h, err := New(&config.Config{SecretKey: "unit-test-secret-key-unit-test-secret-key"}, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	return &env{h: h, repo: repo.fakeRepo}, repo
}

// advance 让时钟前进，使 /readyz 的结果缓存过期。
func advance(t *testing.T) {
	t.Helper()
	base := time.Now()
	n := 0
	statusClock = func() time.Time { n++; return base.Add(time.Duration(n) * time.Hour) }
	t.Cleanup(func() { statusClock = time.Now })
}

func getReady(t *testing.T, e *env) (int, ReadyReport, string) {
	t.Helper()
	w := e.do(httptest.NewRequest("GET", "/readyz", nil)) // 不带任何登录凭据
	var rep ReadyReport
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatalf("响应不是 JSON: %v %s", err, w.Body.String())
	}
	return w.Code, rep, w.Body.String()
}

func TestReadyzOK(t *testing.T) {
	advance(t)
	e, _ := newReadyEnv(t)
	code, rep, _ := getReady(t, e)
	if code != 200 || rep.Status != "ok" || len(rep.Degraded) != 0 {
		t.Fatalf("全部就绪应 200/ok: %d %+v", code, rep)
	}
	names := map[string]bool{}
	for _, c := range rep.Checks {
		names[c.Name] = c.OK
	}
	if !names["metadb"] || !names["required_tables"] || !names["optional_tables"] {
		t.Errorf("检查项: %+v", rep.Checks)
	}
	// /healthz 保持不变
	if w := e.do(httptest.NewRequest("GET", "/healthz", nil)); w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"ok"`) {
		t.Errorf("healthz: %d %s", w.Code, w.Body.String())
	}
}

func TestReadyzOptionalTableMissingIsDegradedNotFailure(t *testing.T) {
	advance(t)
	e, r := newReadyEnv(t)
	r.missing["dbt_query_event"] = true
	r.missing["dbt_redis_rule"] = true
	code, rep, body := getReady(t, e)
	if code != 200 || rep.Status != "degraded" {
		t.Fatalf("可选表缺失应 200/degraded: %d %+v", code, rep)
	}
	if len(rep.Degraded) != 2 || rep.Degraded[0] != "dbt_query_event" || rep.Degraded[1] != "dbt_redis_rule" {
		t.Errorf("degraded 列表: %v", rep.Degraded)
	}
	if !strings.Contains(body, "dbt_query_event") {
		t.Error("响应里应列出缺失的可选表")
	}
}

func TestReadyzRequiredTableMissingFails(t *testing.T) {
	advance(t)
	e, r := newReadyEnv(t)
	r.missing["dbt_sql_users"] = true
	code, rep, _ := getReady(t, e)
	if code != 503 || rep.Status != "fail" {
		t.Fatalf("核心表缺失应 503/fail: %d %+v", code, rep)
	}
	found := false
	for _, c := range rep.Checks {
		if c.Name == "required_tables" && !c.OK && strings.Contains(c.Detail, "dbt_sql_users") {
			found = true
		}
	}
	if !found {
		t.Errorf("应指出缺少 dbt_sql_users: %+v", rep.Checks)
	}
}

func TestReadyzPingFailureDoesNotLeakDetails(t *testing.T) {
	advance(t)
	e, r := newReadyEnv(t)
	r.pingErr = errors.New("dial tcp 10.1.2.3:3306: connect: refused (user=dbadmin password=Sup3rSecret)")
	code, rep, body := getReady(t, e)
	if code != 503 || rep.Status != "fail" {
		t.Fatalf("元数据库不可达应 503: %d %+v", code, rep)
	}
	for _, leak := range []string{"10.1.2.3", "3306", "dbadmin", "Sup3rSecret", "password"} {
		if strings.Contains(body, leak) {
			t.Errorf("响应泄露了 %q: %s", leak, body)
		}
	}
	// 表清单查询出错同样不回显错误文本
	r.pingErr = nil
	r.tblErr = errors.New("Error 1045: Access denied for user 'dbadmin'@'10.1.2.3'")
	code, _, body = getReady(t, e)
	if code != 503 || strings.Contains(body, "dbadmin") || strings.Contains(body, "10.1.2.3") {
		t.Errorf("表清单失败: %d %s", code, body)
	}
}

func TestReadyzTimeout(t *testing.T) {
	advance(t)
	e, r := newReadyEnv(t)
	r.hang = true
	start := time.Now()
	code, rep, _ := getReady(t, e)
	if code != 503 || rep.Status != "fail" {
		t.Fatalf("Ping 挂起应 503: %d", code)
	}
	if d := time.Since(start); d < time.Second || d > 4*time.Second {
		t.Errorf("应在约 2 秒超时返回: %v", d)
	}
}

func TestReadyzResultCached(t *testing.T) {
	e, r := newReadyEnv(t) // 真实时钟：2 秒缓存内只探测一次
	for i := 0; i < 5; i++ {
		getReady(t, e)
	}
	r.mu.Lock()
	n := r.pings
	r.mu.Unlock()
	if n != 1 {
		t.Errorf("缓存期内只应探测 1 次: %d", n)
	}
}

// 仓库既没有探针实现也没有元数据库句柄（测试 / 演示）：不报失败。
func TestReadyzNoProbeAvailable(t *testing.T) {
	e := newEnv(t)
	code, rep, _ := getReady(t, e)
	if code != 200 || rep.Status != "ok" {
		t.Errorf("无探针来源: %d %+v", code, rep)
	}
}
