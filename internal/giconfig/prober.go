// @author: kevinkelly
// @license: Apache Licence
// @file: prober.go
// @time: 2026/10/28

package giconfig

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	drv "github.com/go-sql-driver/mysql"
)

// sqlProber 用 MySQL 协议连接 goInception（默认 4000 端口）和备份库，只执行只读命令。
type sqlProber struct{}

func (sqlProber) open(c ConnInfo, timeout time.Duration) (*sql.DB, error) {
	if strings.TrimSpace(c.Host) == "" {
		return nil, fmt.Errorf("没有配置地址")
	}
	port := c.Port
	if port == 0 {
		port = 4000
	}
	cfg := drv.NewConfig()
	cfg.User, cfg.Passwd = c.User, c.Password
	cfg.Net, cfg.Addr = "tcp", net.JoinHostPort(c.Host, strconv.Itoa(port))
	cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout = timeout, timeout, timeout
	// 字符集必须用驱动选项设置：放进 Params 会被当成会话变量发 SET charset=…，MySQL 8.0 报 1193。
	if err := cfg.Apply(drv.Charset("utf8mb4", "")); err != nil {
		return nil, err
	}
	conn, err := drv.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(conn)
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(timeout * 2)
	return db, nil
}

// queryRows 执行查询，把每行读成“小写列名 → 文本”。
func queryRows(ctx context.Context, db *sql.DB, q string) ([]map[string]string, []string, error) {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}
	for i := range cols {
		cols[i] = strings.ToLower(cols[i])
	}
	var out []map[string]string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		row := map[string]string{}
		for i, c := range cols {
			row[c] = vals[i].String
		}
		out = append(out, row)
		if len(out) >= 2000 {
			break
		}
	}
	return out, cols, rows.Err()
}

func (p sqlProber) Ping(ctx context.Context, c ConnInfo) (PingResult, error) {
	db, err := p.open(c, 5*time.Second)
	if err != nil {
		return PingResult{}, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	start := time.Now()
	if err := db.PingContext(ctx); err != nil {
		return PingResult{}, err
	}
	res := PingResult{LatencyMS: time.Since(start).Milliseconds()}
	// 用 `inception get variables` 确认对方真的是 goInception。
	rows, cols, err := queryRows(ctx, db, "inception get variables")
	if err != nil {
		res.Note = fmt.Sprintf("已连接，但 `inception get variables` 执行失败：%s", err.Error())
		return res, nil
	}
	res.Variables = len(rows)
	nameCol, valCol := "variable_name", "value"
	if _, ok := rows0(rows)[nameCol]; !ok && len(cols) >= 2 {
		nameCol, valCol = cols[0], cols[1]
	}
	for _, r := range rows {
		switch strings.ToLower(r[nameCol]) {
		case "version", "inception_version", "goinception_version":
			res.Version = r[valCol]
		}
	}
	return res, nil
}

func rows0(rows []map[string]string) map[string]string {
	if len(rows) == 0 {
		return map[string]string{}
	}
	return rows[0]
}

// 判断“进行中的任务”用的语句：官方文档里是 inception get processlist，保险起见再试 show。
var processlistStmts = []string{"inception get processlist", "inception show processlist"}

func (p sqlProber) Tasks(ctx context.Context, c ConnInfo) (Tasks, error) {
	db, err := p.open(c, 5*time.Second)
	if err != nil {
		return Tasks{}, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var lastErr error
	for _, q := range processlistStmts {
		rows, cols, err := queryRows(ctx, db, q)
		if err != nil {
			lastErr = err
			continue
		}
		hasCmd := false
		for _, c := range cols {
			hasCmd = hasCmd || c == "command"
		}
		t := Tasks{Known: true, Items: []Task{}}
		for _, r := range rows {
			if strings.Contains(strings.ToLower(r["info"]), "processlist") {
				continue // 自己这条查询
			}
			t.Connections++
			if hasCmd && strings.EqualFold(r["command"], "sleep") {
				continue
			}
			t.Running++
			if len(t.Items) < 20 {
				info := r["info"]
				if rs := []rune(info); len(rs) > 200 {
					info = string(rs[:200]) + "…"
				}
				t.Items = append(t.Items, Task{ID: r["id"], User: r["user"], Host: r["host"], DB: r["db"], Command: r["command"], State: r["state"], Time: r["time"], Info: info})
			}
		}
		return t, nil
	}
	return Tasks{Known: false, Note: fmt.Sprintf("无法读取 goInception 的会话列表：%s", lastErr.Error())}, nil
}

func (p sqlProber) PingBackup(ctx context.Context, c ConnInfo) (BackupResult, error) {
	db, err := p.open(c, 5*time.Second)
	if err != nil {
		return BackupResult{}, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	start := time.Now()
	var version string
	if err := db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		return BackupResult{}, err
	}
	res := BackupResult{LatencyMS: time.Since(start).Milliseconds(), Version: version}
	rows, _, err := queryRows(ctx, db, "SHOW GRANTS FOR CURRENT_USER()")
	if err != nil {
		res.Note = "无法读取该账号的权限，不能确认有没有写权限"
		return res, nil
	}
	var all strings.Builder
	for _, r := range rows {
		for _, v := range r {
			all.WriteString(strings.ToUpper(v) + "\n")
		}
	}
	g := all.String()
	ok := strings.Contains(g, "ALL PRIVILEGES") || (strings.Contains(g, "INSERT") && strings.Contains(g, "CREATE"))
	res.CanWrite = &ok
	if !ok {
		res.Note = "没有发现 INSERT / CREATE 权限，goInception 可能无法写入回滚备份"
	}
	return res, nil
}
