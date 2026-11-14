// @author: kevinkelly
// @license: Apache Licence
// @file: client.go
// @time: 2026/10/28

// Package goinception 是 goInception 的 Go 客户端，目前只用到“打印语法树”能力，
// 用来可靠地解析一条查询语句涉及的库表。审核、执行、脱敏列映射在后续里程碑补充。
package goinception

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	drv "github.com/go-sql-driver/mysql"
)

// Config 是 goInception 服务本身的连接信息，对应 dbt_setting 的 go_inception_*。
type Config struct {
	Host     string
	Port     int
	User     string
	Password string
}

// Target 是被解析语句所属的业务实例（隧道转换后的最终地址）。goInception 需要连上它读取表结构。
type Target struct {
	Host     string
	Port     int
	User     string
	Password string
}

type Client struct{ cfg Config }

func New(cfg Config) *Client {
	if cfg.Port == 0 {
		cfg.Port = 4000
	}
	return &Client{cfg: cfg}
}

// headerSafe 校验写进 goInception 注释头的取值。header 形如 `/*--user='x';--password='y';...*/`，
// 取值里出现引号、反斜杠、注释结束符或换行会破坏头部结构，直接拒绝，而不是尝试转义。
func headerSafe(name, v string) error {
	if strings.ContainsAny(v, "'\\\r\n") || strings.Contains(v, "*/") {
		return fmt.Errorf("实例%s包含 goInception 不支持的字符（引号、反斜杠、换行或 */）", name)
	}
	return nil
}

// BuildPrintScript 生成“打印语法树”的脚本。stmt 必须是已切分好的单条语句。
func BuildPrintScript(t Target, db, stmt string) (string, error) {
	for name, v := range map[string]string{"账号": t.User, "密码": t.Password, "地址": t.Host} {
		if err := headerSafe(name, v); err != nil {
			return "", err
		}
	}
	stmt = strings.TrimRight(strings.TrimSpace(stmt), "; \t\r\n")
	var b strings.Builder
	fmt.Fprintf(&b, "/*--user='%s';--password='%s';--host='%s';--port=%d;--enable-query-print;*/\n", t.User, t.Password, t.Host, t.Port)
	b.WriteString("inception_magic_start;\n")
	if db != "" {
		b.WriteString("use `" + strings.ReplaceAll(db, "`", "``") + "`;\n")
	}
	b.WriteString(stmt + ";\n")
	b.WriteString("inception_magic_commit;")
	return b.String(), nil
}

func (c *Client) open() (*sql.DB, error) {
	cfg := drv.NewConfig()
	cfg.User, cfg.Passwd = c.cfg.User, c.cfg.Password
	cfg.Net, cfg.Addr = "tcp", fmt.Sprintf("%s:%d", c.cfg.Host, c.cfg.Port)
	cfg.Timeout = 10 * time.Second
	cfg.MultiStatements = true // 允许多语句
	cfg.Params = map[string]string{"charset": "utf8mb4"}
	return sql.Open("mysql", cfg.FormatDSN())
}

// QueryTree 让 goInception 解析语句，返回语法树 JSON。
func (c *Client) QueryTree(ctx context.Context, t Target, db, stmt string) (any, error) {
	script, err := BuildPrintScript(t, db, stmt)
	if err != nil {
		return nil, err
	}
	conn, err := c.open()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	rows, err := conn.QueryContext(ctx, script)
	if err != nil {
		return nil, fmt.Errorf("goInception 请求失败: %w", err)
	}
	defer rows.Close()

	var tree string
	for {
		cols, err := rows.Columns()
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			vals := make([]sql.NullString, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return nil, err
			}
			row := map[string]string{}
			for i, c := range cols {
				row[strings.ToLower(c)] = vals[i].String
			}
			if got, err := interpretRow(row); err != nil {
				return nil, err
			} else if got != "" {
				tree = got
			}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if !rows.NextResultSet() {
			break
		}
	}
	if tree == "" {
		return nil, errors.New("goInception 未返回语法树")
	}
	var v any
	if err := json.Unmarshal([]byte(tree), &v); err != nil {
		return nil, fmt.Errorf("解析 goInception 语法树失败: %w", err)
	}
	return v, nil
}

// interpretRow 处理结果里的一行：出错则返回错误，带语法树则返回语法树文本。
// 兼容审核结果格式（有 error_level / error_message 列）被误返回的场景。
func interpretRow(row map[string]string) (string, error) {
	if msg, ok := row["error_message"]; ok && row["error_level"] != "" && row["error_level"] != "0" {
		return "", fmt.Errorf("goInception 错误: %s", msg)
	}
	if msg := row["errmsg"]; msg != "" {
		return "", fmt.Errorf("goInception 错误: %s", msg)
	}
	if lvl := row["errlevel"]; lvl != "" && lvl != "0" {
		return "", fmt.Errorf("goInception 错误: errlevel=%s", lvl)
	}
	return row["query_tree"], nil
}
