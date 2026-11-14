// @author: kevinkelly
// @license: Apache Licence
// @file: engine.go
// @time: 2026/10/28

package redis

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"dbtool/internal/engine"
)

func init() {
	engine.Register("redis", func(c engine.Conn) (engine.Engine, error) { return New(c), nil })
}

// ConfigFromConn 把实例连接信息转成客户端配置：
//   - 账号为空只用密码（default 用户）；账号非空走 ACL（AUTH 用户名 密码）；
//   - 实例的“数据库名”字段若是数字，连接后 SELECT 到该库；
//   - 勾选“SSL”走 TLS，“校验证书”决定是否校验服务端证书。
func ConfigFromConn(c engine.Conn) Config {
	cfg := Config{Host: c.Host, Port: c.Port, Username: c.User, Password: c.Password, TLS: c.SSL, VerifyTLS: c.VerifySSL}
	if n, err := strconv.Atoi(strings.TrimSpace(c.DBName)); err == nil && n > 0 {
		cfg.DB = n
	}
	return cfg
}

// Engine 实现 engine.Engine：每次调用按需建立一个短连接（Redis 连接很轻，且不长期占用 maxclients）。
type Engine struct {
	cfg Config
}

// New 创建引擎（不立即连接）。
func New(c engine.Conn) *Engine { return &Engine{cfg: ConfigFromConn(c)} }

func (e *Engine) Name() string { return "redis" }
func (e *Engine) Close() error { return nil }

func (e *Engine) dial(ctx context.Context) (*Client, error) { return Dial(ctx, e.cfg) }

func (e *Engine) TestConnection(ctx context.Context) error {
	c, err := e.dial(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Ping(ctx)
}

// ServerVersion 返回 redis_version（INFO server）。
func (e *Engine) ServerVersion(ctx context.Context) (string, error) {
	c, err := e.dial(ctx)
	if err != nil {
		return "", err
	}
	defer c.Close()
	info, err := c.Info(ctx, "server")
	if err != nil {
		return "", err
	}
	return info["redis_version"], nil
}

var reKeyspace = regexp.MustCompile(`^db(\d+)$`)

// AllDatabases 返回 db0…dbN：N 取 CONFIG GET databases，读不到时退回 INFO keyspace 里有数据的库。
func (e *Engine) AllDatabases(ctx context.Context) ([]string, error) {
	c, err := e.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if cfg, err := c.ConfigGet(ctx, "databases"); err == nil {
		if n, _ := strconv.Atoi(cfg["databases"]); n > 0 && n <= 1024 {
			out := make([]string, n)
			for i := range out {
				out[i] = "db" + strconv.Itoa(i)
			}
			return out, nil
		}
	}
	info, err := c.Info(ctx, "keyspace")
	if err != nil {
		return nil, err
	}
	var out []string
	for k := range info {
		if reKeyspace.MatchString(k) {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := strconv.Atoi(out[i][2:])
		b, _ := strconv.Atoi(out[j][2:])
		return a < b
	})
	return out, nil
}

// AllTables / AllColumns：Redis 没有表和列。
func (e *Engine) AllTables(context.Context, string) ([]string, error)          { return nil, nil }
func (e *Engine) AllColumns(context.Context, string, string) ([]string, error) { return nil, nil }

// Query 执行一条白名单内的只读命令（命令行文本按空白分词，支持双引号包裹含空格的参数）。
// 结果集：单列 result，数组的每个元素一行，标量一行。
func (e *Engine) Query(ctx context.Context, line string, opt engine.QueryOptions) (*engine.ResultSet, error) {
	args, err := SplitCommand(line)
	if err != nil {
		return nil, err
	}
	if err := Check(args); err != nil {
		return nil, err
	}
	cfg := e.cfg
	if n, err := strconv.Atoi(strings.TrimPrefix(opt.DBName, "db")); err == nil && n >= 0 {
		cfg.DB = n
	}
	start := time.Now()
	c, err := Dial(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	v, err := c.Do(ctx, args...)
	if err != nil {
		return nil, err
	}
	rs := &engine.ResultSet{ColumnList: []string{"result"}}
	flatten(v, &rs.Rows, 0)
	if opt.Limit > 0 && len(rs.Rows) > opt.Limit {
		rs.Rows = rs.Rows[:opt.Limit]
	}
	rs.CostTimeMs = time.Since(start).Milliseconds()
	return rs, nil
}

func flatten(v any, rows *[][]any, depth int) {
	switch x := v.(type) {
	case nil:
		*rows = append(*rows, []any{nil})
	case []any:
		if depth >= 1 { // 嵌套数组压成一行文本，保持单列
			*rows = append(*rows, []any{fmt.Sprint(x...)})
			return
		}
		for _, it := range x {
			flatten(it, rows, depth+1)
		}
	default:
		*rows = append(*rows, []any{x})
	}
}

// SplitCommand 把命令行拆成参数：按空白分词，双引号内的空白保留，反斜杠转义引号和反斜杠。
func SplitCommand(line string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inQuote, has := false, false
	rs := []rune(strings.TrimSpace(line))
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case inQuote && r == '\\' && i+1 < len(rs) && (rs[i+1] == '"' || rs[i+1] == '\\'):
			i++
			cur.WriteRune(rs[i])
		case r == '"':
			inQuote = !inQuote
			has = true
		case !inQuote && (r == ' ' || r == '\t' || r == '\n' || r == '\r'):
			if has {
				out = append(out, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteRune(r)
			has = true
		}
	}
	if inQuote {
		return nil, errors.New("命令里的引号没有闭合")
	}
	if has {
		out = append(out, cur.String())
	}
	if len(out) == 0 {
		return nil, errors.New("命令为空")
	}
	return out, nil
}
