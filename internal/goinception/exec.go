// @author: kevinkelly
// @license: Apache Licence
// @file: exec.go
// @time: 2026/10/28

package goinception

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	drv "github.com/go-sql-driver/mysql"

	"dbtool/internal/review"
)

// ForbiddenMarkers 是不允许出现在用户 SQL 里的 goInception 控制语句。用户 SQL 会被拼进
// “inception_magic_start; … inception_magic_commit;” 脚本，里面若自带这些标记，就能提前结束脚本、
// 把后面的内容当成 goInception 自己的命令来执行。
var ForbiddenMarkers = []string{"inception_magic_start", "inception_magic_commit"}

// CheckSQLSafe 拒绝含 goInception 控制标记的 SQL。
func CheckSQLSafe(sqlText string) error {
	low := strings.ToLower(sqlText)
	for _, m := range ForbiddenMarkers {
		if strings.Contains(low, m) {
			return fmt.Errorf("SQL 中不允许出现 %s", m)
		}
	}
	return nil
}

// CheckOpts 是检查选项。
type CheckOpts struct {
	RealRowCount bool // dbt_setting 的 review.exact_row_count：用真实行数代替估算
}

// ExecOpts 是执行选项。
type ExecOpts struct {
	Backup bool // 是否生成回滚备份
}

func buildScript(t Target, db, stmt, options string) (string, error) {
	for name, v := range map[string]string{"账号": t.User, "密码": t.Password, "地址": t.Host} {
		if err := headerSafe(name, v); err != nil {
			return "", err
		}
	}
	if err := CheckSQLSafe(stmt); err != nil {
		return "", err
	}
	stmt = strings.TrimRight(strings.TrimSpace(stmt), "; \t\r\n")
	var b strings.Builder
	fmt.Fprintf(&b, "/*--user='%s';--password='%s';--host='%s';--port=%d;%s*/\n", t.User, t.Password, t.Host, t.Port, options)
	b.WriteString("inception_magic_start;\n")
	if db != "" {
		b.WriteString("use `" + strings.ReplaceAll(db, "`", "``") + "`;\n")
	}
	b.WriteString(stmt + ";\n")
	b.WriteString("inception_magic_commit;")
	return b.String(), nil
}

// BuildCheckScript 生成审核脚本（--check=1）。
func BuildCheckScript(t Target, db, stmt string, o CheckOpts) (string, error) {
	opt := "--check=1;"
	if o.RealRowCount {
		opt += "--real_row_count=true;"
	}
	return buildScript(t, db, stmt, opt)
}

// BuildExecuteScript 生成执行脚本（--execute=1），忽略警告、按需备份、限速。
func BuildExecuteScript(t Target, db, stmt string, o ExecOpts) (string, error) {
	backup := "--backup=0"
	if o.Backup {
		backup = "--backup=1"
	}
	return buildScript(t, db, stmt, "--execute=1;--ignore-warnings=1;"+backup+";--sleep=200;--sleep_rows=100")
}

// runRows 执行脚本并按位置读出所有结果行。出错但已经读到部分行时，同时返回行和错误
// （执行到一半连接断开等场景，调用方需要保留已有结果）。
func (c *Client) runRows(ctx context.Context, script string, timeout time.Duration) ([]review.Row, error) {
	conn, err := c.open()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	rows, err := conn.QueryContext(ctx, script)
	if err != nil {
		return nil, fmt.Errorf("goInception 请求失败: %w", err)
	}
	defer rows.Close()
	var out []review.Row
	for {
		cols, err := rows.Columns()
		if err != nil {
			return out, err
		}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return out, err
			}
			for i, v := range vals {
				if b, ok := v.([]byte); ok {
					vals[i] = string(b)
				}
			}
			out = append(out, review.RowFromColumns(vals))
		}
		if err := rows.Err(); err != nil {
			return out, err
		}
		if !rows.NextResultSet() {
			break
		}
	}
	return out, nil
}

// Check 让 goInception 审核一批语句，返回每条语句的审核结果。
func (c *Client) Check(ctx context.Context, t Target, db, stmt string, o CheckOpts) ([]review.Row, error) {
	script, err := BuildCheckScript(t, db, stmt, o)
	if err != nil {
		return nil, err
	}
	return c.runRows(ctx, script, 5*time.Minute)
}

// Execute 让 goInception 执行一批语句，返回每条语句的执行结果。执行时间不设上限（由调用方的 ctx 控制）。
func (c *Client) Execute(ctx context.Context, t Target, db, stmt string, o ExecOpts) ([]review.Row, error) {
	script, err := BuildExecuteScript(t, db, stmt, o)
	if err != nil {
		return nil, err
	}
	return c.runRows(ctx, script, 0)
}

var (
	oscShaRe = regexp.MustCompile(`^[0-9a-fA-F]{1,64}$`)
	oscCmds  = map[string]bool{"get": true, "kill": true, "pause": true, "resume": true}
)

// Osc 控制在线改表（pt-osc / gh-ost）：get 查进度，kill / pause / resume 控制执行。
// 命令与 sqlsha1 都经过白名单/格式校验后才拼进语句。
func (c *Client) Osc(ctx context.Context, command, sqlsha1 string) ([]map[string]any, error) {
	command = strings.ToLower(strings.TrimSpace(command))
	if !oscCmds[command] {
		return nil, errors.New("不支持的 OSC 命令，可用：get / kill / pause / resume")
	}
	if !oscShaRe.MatchString(sqlsha1) {
		return nil, errors.New("sqlsha1 格式不合法")
	}
	q := "inception " + command + " osc '" + sqlsha1 + "';"
	if command == "get" {
		q = "inception get osc_percent '" + sqlsha1 + "';"
	}
	conn, err := c.open()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("goInception 请求失败: %w", err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := map[string]any{}
		for i, col := range cols {
			if b, ok := vals[i].([]byte); ok {
				m[col] = string(b)
			} else {
				m[col] = vals[i]
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---- 回滚备份读取 ----

// BackupConfig 是 goInception 备份库的连接信息，对应 dbt_setting 的 inception_remote_backup_*。
type BackupConfig struct {
	Host     string
	Port     int
	User     string
	Password string
}

// Backup 从 goInception 的备份库读取回滚语句。
type Backup struct{ cfg BackupConfig }

func NewBackup(cfg BackupConfig) *Backup {
	if cfg.Port == 0 {
		cfg.Port = 3306
	}
	return &Backup{cfg: cfg}
}

var (
	identRe = regexp.MustCompile(`^[A-Za-z0-9_$]{1,128}$`)
	seqRe   = regexp.MustCompile(`^[0-9_]{1,64}$`)
)

func (b *Backup) open() (*sql.DB, error) {
	cfg := drv.NewConfig()
	cfg.InterpolateParams = true // 参数由驱动转义后以文本形式发送（备份库只读，不需要预处理语句）
	cfg.User, cfg.Passwd = b.cfg.User, b.cfg.Password
	cfg.Net, cfg.Addr = "tcp", fmt.Sprintf("%s:%d", b.cfg.Host, b.cfg.Port)
	cfg.Timeout = 10 * time.Second
	cfg.Params = map[string]string{"charset": "utf8mb4"}
	return sql.Open("mysql", cfg.FormatDSN())
}

// Rollback 是一条语句对应的回滚语句。
type Rollback struct {
	SQL      string // 原语句
	Rollback string // 回滚语句（可能有多条，换行分隔）
}

// Rollbacks 按执行顺序倒序返回回滚语句。没有备份的行（backup_dbname 为空或 "None"）被跳过。
// backup_dbname、sequence 来自存储的执行结果，仍按不可信输入处理：校验格式后才用于拼标识符，值用参数传递。
func (b *Backup) Rollbacks(ctx context.Context, rows []review.Row) ([]Rollback, error) {
	conn, err := b.open()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var out []Rollback
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		if r.BackupDBName == "" || r.BackupDBName == "None" {
			continue
		}
		if !identRe.MatchString(r.BackupDBName) {
			return nil, fmt.Errorf("备份库名不合法：%q", r.BackupDBName)
		}
		opid := strings.ReplaceAll(r.Sequence, "'", "")
		if !seqRe.MatchString(opid) {
			return nil, fmt.Errorf("备份序号不合法：%q", r.Sequence)
		}
		var table string
		err := conn.QueryRowContext(ctx, "select tablename from `"+r.BackupDBName+"`.`$_$Inception_backup_information$_$` where opid_time = ?", opid).Scan(&table)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("读取备份信息失败: %w", err)
		}
		if !identRe.MatchString(table) {
			return nil, fmt.Errorf("备份表名不合法：%q", table)
		}
		rs, err := conn.QueryContext(ctx, "select rollback_statement from `"+r.BackupDBName+"`.`"+table+"` where opid_time = ?", opid)
		if err != nil {
			return nil, fmt.Errorf("读取回滚语句失败: %w", err)
		}
		var parts []string
		for rs.Next() {
			var s sql.NullString
			if err := rs.Scan(&s); err != nil {
				rs.Close()
				return nil, err
			}
			parts = append(parts, s.String)
		}
		rs.Close()
		if err := rs.Err(); err != nil {
			return nil, err
		}
		out = append(out, Rollback{SQL: r.SQL, Rollback: strings.Join(parts, "\n")})
	}
	return out, nil
}
