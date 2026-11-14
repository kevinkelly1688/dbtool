// @author: kevinkelly
// @license: Apache Licence
// @file: fake_test.go
// @time: 2026/10/28

package clickhouse

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"dbtool/internal/engine"
)

// fakeCH 是 ClickHouse HTTP 接口的假服务：记录每个请求，按语句内容路由到预置的响应。
type fakeCH struct {
	t    *testing.T
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []recorded
	// routes 按顺序匹配：第一个 match 返回 true 的处理请求；都不匹配返回默认的一行结果。
	routes []route
}

type recorded struct {
	Method string
	Query  url.Values
	Header http.Header
	Body   string
}

type route struct {
	match   func(body string) bool
	handler func(w http.ResponseWriter, r *http.Request)
}

func contains(sub string) func(string) bool {
	return func(b string) bool { return strings.Contains(b, sub) }
}

func newFake(t *testing.T) *fakeCH {
	f := &fakeCH{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func newFakeTLS(t *testing.T) *fakeCH {
	f := &fakeCH{t: t}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCH) serve(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	body := string(b)
	f.mu.Lock()
	f.reqs = append(f.reqs, recorded{Method: r.Method, Query: r.URL.Query(), Header: r.Header.Clone(), Body: body})
	routes := f.routes
	f.mu.Unlock()
	for _, rt := range routes {
		if rt.match(body) {
			rt.handler(w, r)
			return
		}
	}
	if strings.Contains(body, "FROM system.tables WHERE (database, name) IN") {
		writeJC(w, [][2]string{{"database", "String"}, {"name", "String"}, {"engine", "String"}}, nil)
		return
	}
	writeJC(w, [][2]string{{"x", "UInt8"}}, [][]any{{1}})
}

func (f *fakeCH) on(match func(string) bool, h func(w http.ResponseWriter, r *http.Request)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes = append(f.routes, route{match, h})
}

// onJC 让包含 sub 的语句返回给定的结果集。
func (f *fakeCH) onJC(sub string, cols [][2]string, rows [][]any) {
	f.on(contains(sub), func(w http.ResponseWriter, r *http.Request) { writeJC(w, cols, rows) })
}

func (f *fakeCH) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.reqs...)
}

func (f *fakeCH) conn() engine.Conn {
	host, portS, _ := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(f.srv.URL, "http://"), "https://"))
	port, _ := strconv.Atoi(portS)
	return engine.Conn{Host: host, Port: port, User: "reader", Password: "s3cr3t-Pa55", DBName: "app"}
}

func (f *fakeCH) engine() *Engine {
	f.t.Helper()
	c := f.conn()
	c.SSL = strings.HasPrefix(f.srv.URL, "https://")
	e, err := NewEngine(c)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { e.Close() })
	return e
}

// writeJC 按 JSONCompact 的格式输出。
func writeJC(w http.ResponseWriter, cols [][2]string, rows [][]any) {
	var sb strings.Builder
	sb.WriteString(`{"meta":[`)
	for i, c := range cols {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"name":` + jsonStr(c[0]) + `,"type":` + jsonStr(c[1]) + `}`)
	}
	sb.WriteString(`],"data":[`)
	for i, r := range rows {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(rowJSON(r))
	}
	sb.WriteString(`],"rows":` + strconv.Itoa(len(rows)) + `,"statistics":{"elapsed":0.001,"rows_read":` + strconv.Itoa(len(rows)*10) + `,"bytes_read":4096}}`)
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = io.WriteString(w, sb.String())
}

func jsonStr(s string) string { b, _ := json.Marshal(s); return string(b) }

// rowJSON：json.RawMessage 原样输出，其余走 json.Marshal。
func rowJSON(r []any) string {
	parts := make([]string, len(r))
	for i, v := range r {
		if raw, ok := v.(json.RawMessage); ok {
			parts[i] = string(raw)
			continue
		}
		b, _ := json.Marshal(v)
		parts[i] = string(b)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

// chError 返回带错误码头的 500 响应，格式与真实服务端一致。
func chError(code int, msg string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-ClickHouse-Exception-Code", strconv.Itoa(code))
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "Code: "+strconv.Itoa(code)+". DB::Exception: "+msg+" (version 24.3.2.23 (official build))\n")
	}
}

// lastBodyHas 判断最近一个请求的语句是否包含 sub。
func (f *fakeCH) lastBodyHas(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs) > 0 && strings.Contains(f.reqs[len(f.reqs)-1].Body, sub)
}
