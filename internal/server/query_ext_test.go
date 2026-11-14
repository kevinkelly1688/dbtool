// @author: kevinkelly
// @license: Apache Licence
// @file: query_ext_test.go
// @time: 2026/10/28

package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQueryExtAPI(t *testing.T) {
	e := newEnv(t)
	sc, csrf, _ := e.login(t, "root", "pw-123")
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(sc)
		return e.do(req)
	}

	// 未登录
	if w := e.do(httptest.NewRequest("GET", "/api/v1/query/instances/1/databases/shop/meta", nil)); w.Code != 401 {
		t.Errorf("未登录应 401: %d", w.Code)
	}

	// 库表树元数据：假引擎的 SHOW TABLE STATUS 解析不出来时退回只列表名；管理员不上锁
	w := get("/api/v1/query/instances/1/databases/shop/meta")
	var out struct {
		Data struct {
			DB     string `json:"db"`
			Tables []struct {
				Name   string `json:"name"`
				Locked bool   `json:"locked"`
				Rows   int64  `json:"rows"`
			} `json:"tables"`
			Policy struct {
				Mode string `json:"mode"`
			} `json:"policy"`
		} `json:"data"`
	}
	if w.Code != 200 {
		t.Fatalf("meta=%d %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Data.DB != "shop" || len(out.Data.Tables) != 2 || out.Data.Tables[0].Locked || out.Data.Tables[0].Rows != -1 || out.Data.Policy.Mode != "admin" {
		t.Errorf("meta 内容: %s", w.Body.String())
	}

	// mysql 系统库一律拒绝
	if w = get("/api/v1/query/instances/1/databases/mysql/meta"); w.Code != 403 {
		t.Errorf("mysql 库应 403: %d", w.Code)
	}
	// 搜索：空关键字返回空数组
	if w = get("/api/v1/query/instances/1/databases/shop/search?q="); w.Code != 200 || !strings.Contains(w.Body.String(), `"data":[]`) {
		t.Errorf("空搜索: %d %s", w.Code, w.Body.String())
	}
	// 历史的状态筛选：非法值 400；仓库没有旁路表时合法值返回空页
	if w = get("/api/v1/query/history?status=bogus"); w.Code != 400 {
		t.Errorf("非法状态应 400: %d", w.Code)
	}
	if w = get("/api/v1/query/history?status=blocked"); w.Code != 200 || !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Errorf("无旁路表时的状态筛选: %d %s", w.Code, w.Body.String())
	}

	// 估算接口需要 CSRF；引擎给不出 rows 时返回 -1（未知）
	post := func(token string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]any{"instance_id": 1, "db": "shop", "sql": "select id from orders limit 10"})
		req := httptest.NewRequest("POST", "/api/v1/query/estimate", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", token)
		req.AddCookie(sc)
		return e.do(req)
	}
	if w = post("forged"); w.Code != 403 {
		t.Errorf("缺少有效 CSRF 应 403: %d", w.Code)
	}
	if w = post(csrf); w.Code != 200 || !strings.Contains(w.Body.String(), `"rows":-1`) {
		t.Errorf("estimate: %d %s", w.Code, w.Body.String())
	}
}
