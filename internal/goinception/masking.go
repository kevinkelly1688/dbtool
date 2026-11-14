// @author: kevinkelly
// @license: Apache Licence
// @file: masking.go
// @time: 2026/10/28

package goinception

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"dbtool/internal/masking"
)

// BuildMaskingScript 生成获取“结果列来源”的脚本（goInception 的 --masking=1，
// 见 https://github.com/hanchuanchuan/goInception/pull/355）。
// 注释头里的取值不加引号，所以这里对取值的限制比打印语法树更严（不允许空白和分号）。
func BuildMaskingScript(t Target, db, stmt string) (string, error) {
	for name, v := range map[string]string{"账号": t.User, "密码": t.Password, "地址": t.Host} {
		if err := headerSafe(name, v); err != nil {
			return "", err
		}
		if strings.ContainsAny(v, "; \t") {
			return "", fmt.Errorf("实例%s包含空白或分号，无法用于 goInception 脱敏解析", name)
		}
	}
	stmt = strings.TrimRight(strings.TrimSpace(stmt), "; \t\r\n")
	var b strings.Builder
	fmt.Fprintf(&b, "/*--user=%s;--password=%s;--host=%s;--port=%d;--masking=1;*/\n", t.User, t.Password, t.Host, t.Port)
	b.WriteString("inception_magic_start;\n")
	if db != "" {
		b.WriteString("use `" + strings.ReplaceAll(db, "`", "``") + "`;\n")
	}
	b.WriteString(stmt + ";\n")
	b.WriteString("inception_magic_commit;")
	return b.String(), nil
}

// ParseSelectList 解析 goInception 在 query_tree 列里返回的结果列清单。
func ParseSelectList(raw string) ([]masking.SelectColumn, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var items []map[string]any
	if err := dec.Decode(&items); err != nil {
		return nil, fmt.Errorf("解析 goInception 脱敏信息失败: %w", err)
	}
	out := make([]masking.SelectColumn, 0, len(items))
	for _, m := range items {
		sc := masking.SelectColumn{}
		if n, ok := m["index"].(json.Number); ok {
			i, err := n.Int64()
			if err != nil {
				return nil, fmt.Errorf("goInception 脱敏信息的 index 非整数: %v", n)
			}
			sc.Index = int(i)
		} else {
			return nil, errors.New("goInception 脱敏信息缺少 index")
		}
		sc.Field, _ = m["field"].(string)
		sc.Type, _ = m["type"].(string)
		sc.Table, _ = m["table"].(string)
		sc.Schema, _ = m["schema"].(string)
		sc.Alias, _ = m["alias"].(string)
		out = append(out, sc)
	}
	return out, nil
}

// SelectList 让 goInception 给出语句每个结果列的来源（库、表、字段）。
// 出错或返回内容不符合预期时返回错误，调用方应按脱敏失败处理，不能当作“没有要脱敏的列”。
func (c *Client) SelectList(ctx context.Context, t Target, db, stmt string) ([]masking.SelectColumn, error) {
	script, err := BuildMaskingScript(t, db, stmt)
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
			got, err := interpretRow(row)
			if err != nil {
				return nil, err
			}
			if got != "" && tree == "" {
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
		return nil, errors.New("goInception 未返回语法信息")
	}
	return ParseSelectList(tree)
}
