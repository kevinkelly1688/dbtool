// @author: kevinkelly
// @license: Apache Licence
// @file: opener.go
// @time: 2026/10/28

package engine

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	drv "github.com/go-sql-driver/mysql"

	eng "dbtool/internal/engine"
)

// archiveSQLMode：严格模式（超长 / 非法值直接报错，不静默截断），但不开 NO_ZERO_DATE，旧表里的 0000-00-00 能原样搬；
// NO_AUTO_VALUE_ON_ZERO 保证主键值 0 不被改成自增值。去掉 ANSI_QUOTES / NO_BACKSLASH_ESCAPES，使引号规则与引擎生成 SQL 时的假设一致。
const archiveSQLMode = "STRICT_TRANS_TABLES,NO_AUTO_VALUE_ON_ZERO"

// poolConfig 是建连接池要用的会话参数。
type poolConfig struct {
	lockWait int
	tz       string // 如 +08:00；空表示不设置
	sqlMode  bool
	maxOpen  int
	lenient  bool // 兼容实现（PolarDB-X 等）：锁等待参数连上后逐条 SET，不认识就忽略，不让建连失败
}

// bestEffortConnector 在每个新连接建好后逐条执行 SET，失败（变量不存在 / 不允许改）直接忽略。
type bestEffortConnector struct {
	driver.Connector
	sets []string
}

func (c *bestEffortConnector) Connect(ctx context.Context) (driver.Conn, error) {
	cn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	if ex, ok := cn.(driver.ExecerContext); ok {
		for _, q := range c.sets {
			_, _ = ex.ExecContext(ctx, q, nil)
		}
	}
	return cn, nil
}

// openPool 用 database/sql + go-sql-driver/mysql 打开连接池。会话参数通过 DSN 的系统变量参数在每个新连接上执行 SET。
func openPool(conn eng.Conn, pc poolConfig) (*sql.DB, error) {
	cfg := drv.NewConfig()
	cfg.User, cfg.Passwd = conn.User, conn.Password
	cfg.Net, cfg.Addr = "tcp", fmt.Sprintf("%s:%d", conn.Host, conn.Port)
	cfg.Timeout = 10 * time.Second
	charset := conn.Charset
	if charset == "" {
		charset = "utf8mb4"
	}
	cfg.Params = map[string]string{"charset": charset}
	var sets []string
	if pc.lockWait > 0 {
		w := strconv.Itoa(pc.lockWait)
		if pc.lenient {
			sets = append(sets, "SET SESSION lock_wait_timeout = "+w, "SET SESSION innodb_lock_wait_timeout = "+w)
		} else {
			cfg.Params["lock_wait_timeout"] = w
			cfg.Params["innodb_lock_wait_timeout"] = w
		}
	}
	if pc.tz != "" {
		cfg.Params["time_zone"] = "'" + pc.tz + "'"
	}
	if pc.sqlMode {
		cfg.Params["sql_mode"] = "'" + archiveSQLMode + "'"
	}
	if conn.SSL {
		cfg.TLSConfig = "skip-verify"
		if conn.VerifySSL {
			cfg.TLSConfig = "true"
		}
	}
	var db *sql.DB
	if len(sets) > 0 {
		conr, err := drv.NewConnector(cfg)
		if err != nil {
			return nil, err
		}
		db = sql.OpenDB(&bestEffortConnector{Connector: conr, sets: sets})
	} else {
		var err error
		if db, err = sql.Open("mysql", cfg.FormatDSN()); err != nil {
			return nil, err
		}
	}
	n := pc.maxOpen
	if n < 1 {
		n = 2
	}
	db.SetMaxOpenConns(n)
	db.SetMaxIdleConns(n)
	db.SetConnMaxLifetime(5 * time.Minute)
	return db, nil
}

// fmtOffset 把 TIMEDIFF 的结果（如 "08:00:00"、"-05:30:00"）转成 "+08:00" 形式。
func fmtOffset(s string) (string, error) {
	s = strings.TrimSpace(s)
	sign := "+"
	if strings.HasPrefix(s, "-") {
		sign = "-"
		s = s[1:]
	} else {
		s = strings.TrimPrefix(s, "+")
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 {
		return "", fmt.Errorf("无法解析服务器时区偏移 %s", quote(s))
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h > 14 || m > 59 {
		return "", fmt.Errorf("无法解析服务器时区偏移 %s", quote(s))
	}
	return fmt.Sprintf("%s%02d:%02d", sign, h, m), nil
}

// serverOffset 取服务器当前时区相对 UTC 的偏移。源端与目标端的会话都固定成这个偏移，
// 这样 TIMESTAMP 列读出来的文本写到目标端后仍是同一时刻，且 NOW() 的语义与服务器一致。
func serverOffset(ctx context.Context, conn eng.Conn) (string, error) {
	db, err := openPool(conn, poolConfig{maxOpen: 1})
	if err != nil {
		return "", err
	}
	defer db.Close()
	var s string
	if err := db.QueryRowContext(ctx, "SELECT TIMEDIFF(NOW(), UTC_TIMESTAMP())").Scan(&s); err != nil {
		return "", err
	}
	return fmtOffset(s)
}

// checkBinlogOn 拒绝 sql_log_bin=0 的会话：归档的写入 / 删除必须进 binlog（从库、备份依赖它）。
// lenient（兼容实现）时读不到该变量不拦截（PolarDB-X 没有这个会话变量）；读到了但不是 1 仍然拒绝。
func checkBinlogOn(ctx context.Context, db *sql.DB, lenient bool) error {
	var v int
	if err := db.QueryRowContext(ctx, "SELECT @@session.sql_log_bin").Scan(&v); err != nil {
		if lenient {
			return nil
		}
		return fmt.Errorf("读取 sql_log_bin 失败：%w", err)
	}
	if v != 1 {
		return errors.New("会话 sql_log_bin 不是 1：归档要求写入 binlog，拒绝运行")
	}
	return nil
}

// dumpHostsQuery 列出正在从本库拉 binlog 的连接（从库 IO 线程）。需要 PROCESS 权限才能看到其他账号的线程。
const dumpHostsQuery = "SELECT host AS host FROM information_schema.processlist WHERE command LIKE 'Binlog Dump%' ORDER BY id"

// hostOfDump 取 processlist.host（形如 10.0.0.5:34567、name:34567、[::1]:34567）里的主机部分，小写。
func hostOfDump(h string) string {
	h = strings.TrimSpace(h)
	if strings.HasPrefix(h, "[") {
		if i := strings.Index(h, "]"); i > 0 {
			return strings.ToLower(h[1:i])
		}
	}
	if i := strings.LastIndex(h, ":"); i > 0 && strings.Count(h, ":") == 1 {
		h = h[:i]
	}
	return strings.ToLower(h)
}

// dumpHosts 返回拉 binlog 的从库主机（去重）；查询失败返回空。
func dumpHosts(ctx context.Context, db *sql.DB) []string {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rs, err := db.QueryContext(qctx, dumpHostsQuery)
	if err != nil {
		return nil
	}
	defer rs.Close()
	var out []string
	seen := map[string]bool{}
	for rs.Next() {
		var h sql.NullString
		if rs.Scan(&h) != nil {
			continue
		}
		if v := hostOfDump(h.String); v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func (o Opener) open(ctx context.Context, spec Spec) (env *Env, err error) {
	if o.Conn == nil {
		return nil, errors.New("Opener.Conn 未设置")
	}
	env = &Env{Now: time.Now, TempDir: o.TempDir}
	defer func() {
		if err != nil {
			env.Close()
			env = nil
		}
	}()
	add := func(db *sql.DB) { env.Closers = append(env.Closers, db) }

	conn, instName, err := o.Conn(ctx, spec.InstanceID)
	if err != nil {
		return nil, err
	}
	env.InstName = instName
	lockWait := spec.Params.LockWaitSeconds
	if lockWait <= 0 {
		lockWait = defLockWait
	}
	tz, err := serverOffset(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("连接源实例失败：%w", err)
	}
	lenient := o.Lenient != nil && o.Lenient(ctx, spec.InstanceID)
	pc := poolConfig{lockWait: lockWait, tz: tz, sqlMode: true, maxOpen: 4, lenient: lenient}
	srcDB, err := openPool(conn, pc)
	if err != nil {
		return nil, fmt.Errorf("连接源实例失败：%w", err)
	}
	add(srcDB)
	if err := checkBinlogOn(ctx, srcDB, lenient); err != nil {
		return nil, fmt.Errorf("源实例：%w", err)
	}
	env.Src = NewMySQLSource(srcDB)

	diskFn := func(id uint) DiskFunc {
		if o.Disk == nil {
			return nil
		}
		return func(ctx context.Context) (float64, bool, error) { return o.Disk(ctx, id) }
	}

	var replicas []*sql.DB
	openReplica := func(c eng.Conn) {
		rdb, oerr := openPool(c, poolConfig{maxOpen: 1})
		if oerr != nil {
			replicas = append(replicas, nil)
			return
		}
		add(rdb)
		replicas = append(replicas, rdb)
	}
	switch {
	case o.Replicas != nil:
		rc, rerr := o.Replicas(ctx, spec.InstanceID)
		if rerr != nil {
			return nil, fmt.Errorf("取从库信息失败：%w", rerr)
		}
		for _, c := range rc {
			openReplica(c)
		}
	case o.SlaveByHost != nil:
		// 自动发现：源库上有哪些从库在拉 binlog（processlist 里的 Binlog Dump 线程），再按主机对应平台里登记的从库实例。
		// 认不出的从库放一个 nil 占位：探针会报“复制延迟未检测”（黄色），而不是当作没有从库静默通过。
		// 发现本身失败（如账号没有 PROCESS 权限看不到别人的线程）时不报错，交给探针自己用 SHOW REPLICAS 判断有没有从库。
		for _, host := range dumpHosts(ctx, srcDB) {
			convs, lerr := o.SlaveByHost(ctx, host)
			if lerr != nil || len(convs) == 0 {
				replicas = append(replicas, nil)
				continue
			}
			for _, c := range convs {
				openReplica(c)
			}
		}
	}
	env.Probe = NewMySQLProbe(srcDB, replicas, diskFn(spec.InstanceID))

	if spec.Dest != nil {
		dconn := conn
		if spec.Dest.InstanceID != spec.InstanceID {
			if dconn, _, err = o.Conn(ctx, spec.Dest.InstanceID); err != nil {
				return nil, fmt.Errorf("归档库表所在实例：%w", err)
			}
		}
		dpc := pc
		dpc.maxOpen = 2
		dpc.lenient = lenient
		if spec.Dest.InstanceID != spec.InstanceID {
			dpc.lenient = o.Lenient != nil && o.Lenient(ctx, spec.Dest.InstanceID)
		}
		dstDB, err2 := openPool(dconn, dpc)
		if err2 != nil {
			return nil, fmt.Errorf("连接归档库表所在实例失败：%w", err2)
		}
		add(dstDB)
		if err = checkBinlogOn(ctx, dstDB, dpc.lenient); err != nil {
			return nil, fmt.Errorf("归档库表所在实例：%w", err)
		}
		env.Dst = NewMySQLDest(dstDB, diskFn(spec.Dest.InstanceID))
	}

	if spec.File != nil {
		if o.Store == nil {
			return nil, errors.New("Opener.Store 未设置")
		}
		cfg, serr := o.Store(ctx, spec.File.StoreID)
		if serr != nil {
			return nil, fmt.Errorf("备份存储：%w", serr)
		}
		fsys, ferr := NewFileStore(cfg)
		if ferr != nil {
			return nil, fmt.Errorf("备份存储「%s」：%w", cfg.Name, ferr)
		}
		env.Files = fsys
		if spec.File.Encrypt {
			if o.FileKey == nil {
				return nil, errors.New("Opener.FileKey 未设置，无法加密")
			}
			key, kerr := o.FileKey()
			if kerr != nil {
				return nil, fmt.Errorf("取加密密钥失败：%w", kerr)
			}
			if len(key) != 32 {
				return nil, fmt.Errorf("加密密钥必须是 32 字节（当前 %d）", len(key))
			}
			env.FileKey = key
		}
	}
	return env, nil
}

func closeEnv(e *Env) error {
	if e == nil {
		return nil
	}
	var first error
	keep := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	if e.Src != nil {
		keep(e.Src.Close())
		e.Src = nil
	}
	if e.Dst != nil {
		keep(e.Dst.Close())
		e.Dst = nil
	}
	for _, c := range e.Closers {
		keep(c.Close())
	}
	e.Closers = nil
	return first
}
