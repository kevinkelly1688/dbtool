// @author: kevinkelly
// @license: Apache Licence
// @file: redis.go
// @time: 2026/10/28

package query

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"dbtool/internal/engine"
	redisx "dbtool/internal/engine/redis"
	"dbtool/internal/model"
)

// Redis 的在线查询：用命令代替 SQL，只读。
//
//   - “库”= Redis 的 db 编号（"0".."N-1"，N 取 CONFIG GET databases，取不到回落 16），库级查询权限沿用 dbt_query_privileges（db_name 存 "0".."15"）；
//   - 命令按 redis-cli 规则分词、一次只执行一条，只允许固定的只读白名单（见 engine/redis/userquery.go），返回量受限；
//   - 结果统一整形成二维表（shapeRedis）；Redis 没有列脱敏，结果不做脱敏；
//   - 查询日志、失败 / 被拦截记录、行数上限、超时、库权限、实例停用与资源组可见规则全部沿用 MySQL / SQL Server 的流水线。
//
// MySQL / SQL Server 的代码路径不受影响：只在实例类型为 redis 时进入本文件的分支。

const dbTypeRedis = "redis"

func isRedis(dbType string) bool { return dbType == dbTypeRedis }

const (
	redisMaxCommandBytes = 64 << 10 // 命令文本上限
	redisMaxCell         = 64 << 10 // 单个单元格显示上限，超出截断
	redisMaxReplyBytes   = 4 << 20  // 结果表所有单元格合计上限，超出标记“已截断”
	redisUserConcurrency = 3        // 每个用户同时执行的 Redis 命令数上限
	redisDefaultKeyPage  = 200      // key 浏览每页默认条数
)

// redisPrep 是通过检查的 Redis 命令。
type redisPrep struct {
	cmd    *redisx.UserCommand
	db     int
	dbName string // 规范化后的库编号（"3"）
}

// redisDialer / redisLister 由 redis 引擎实现（测试里用真实引擎连接假服务端）。
type redisDialer interface {
	DialDB(ctx context.Context, db int) (*redisx.Client, error)
}
type redisLister interface {
	DatabaseList(ctx context.Context) ([]redisx.DBInfo, error)
}

// normalizeRedisDB 把 "3" / "db3" 规范成 (3, "3")。
func normalizeRedisDB(s string) (int, string, error) {
	s = strings.TrimSpace(s)
	if len(s) > 2 && strings.EqualFold(s[:2], "db") {
		s = s[2:]
	}
	if s == "" {
		return 0, "", newErr(400, "bad_request", "请选择数据库（Redis 的 db 编号）")
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 1023 {
		return 0, "", newErr(400, "bad_request", "Redis 库编号不合法（应为 0 到 1023 的整数）")
	}
	return n, strconv.Itoa(n), nil
}

func sameRedisDB(a, b string) bool {
	_, x, e1 := normalizeRedisDB(a)
	_, y, e2 := normalizeRedisDB(b)
	return e1 == nil && e2 == nil && x == y
}

// checkRedisCmd 完成命令文本的静态检查：非空、不超长、只有一条、redis-cli 规则分词、白名单与限量改写。不访问 Redis。
func checkRedisCmd(text string) (*redisx.UserCommand, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, newErr(400, "bad_query", "命令为空")
	}
	if len(text) > redisMaxCommandBytes {
		return nil, newErr(400, "bad_query", fmt.Sprintf("命令过长（上限 %d KB）", redisMaxCommandBytes>>10))
	}
	lines := 0
	for _, ln := range strings.Split(text, "\n") {
		if strings.TrimSpace(ln) != "" {
			lines++
		}
	}
	if lines > 1 {
		return nil, newErr(400, "bad_query", fmt.Sprintf("检测到 %d 行命令，在线查询一次只能执行一条；请选中其中一条再运行（值里的换行请写成 \\n）。", lines))
	}
	args, err := redisx.TokenizeLine(text)
	if err != nil {
		return nil, newErr(400, "bad_query", fmt.Sprintf("命令解析失败：%s", err.Error()))
	}
	if len(args) == 0 {
		return nil, newErr(400, "bad_query", "命令为空")
	}
	cmd, err := redisx.ParseUserCommand(args)
	if err != nil {
		var ue *redisx.UserError
		if errors.As(err, &ue) {
			return nil, newErr(400, "bad_query", ue.Msg)
		}
		return nil, newErr(400, "bad_query", err.Error())
	}
	return cmd, nil
}

// checkRedis 是 checkFor 的 Redis 版本：返回规范化后的命令行与提示。
func (s *Service) checkRedis(text string) (stmt, notice string, err error) {
	cmd, err := checkRedisCmd(text)
	if err != nil {
		return "", "", err
	}
	return redisx.FormatUserCommand(cmd.Args), strings.Join(cmd.Notices, "\n"), nil
}

// prepareRedis 是 prepare 的 Redis 分支：库编号、命令检查、库权限、行数上限。
func (s *Service) prepareRedis(ctx context.Context, p Principal, req Request, inst *model.Instance, pol Policy) (*prepared, error) {
	dbn, dbName, err := normalizeRedisDB(req.DB)
	if err != nil {
		return nil, err
	}
	cmd, err := checkRedisCmd(req.SQL)
	if err != nil {
		return nil, err
	}
	line := redisx.FormatUserCommand(cmd.Args)
	dec, err := s.auth.AuthorizeQuery(ctx, p, inst, dbName, line, pol.AdminLimit)
	if err != nil {
		return nil, err
	}
	limit := dec.MaxRows
	if req.Limit > 0 && (limit <= 0 || req.Limit < limit) {
		limit = req.Limit
	}
	return &prepared{inst: inst, pol: pol, stmt: line, run: line, kind: "redis", notice: strings.Join(cmd.Notices, "\n"), limit: limit, dec: dec,
		redis: &redisPrep{cmd: cmd, db: dbn, dbName: dbName}}, nil
}

// authorizeRedis：Redis 只有库级权限（dbt_query_privileges 的 priv_type=1，db_name 为 "0".."15" 或 "*"），行数上限取该授权的 limit_num。
func (a *PrivAuthorizer) authorizeRedis(p Principal, inst *model.Instance, db string) (Decision, error) {
	privs, err := a.repo.ValidQueryPrivileges(p.Username, inst.ID)
	if err != nil {
		return Decision{}, err
	}
	for _, pv := range privs {
		if pv.PrivType == 1 && (pv.DBName == "*" || sameRedisDB(pv.DBName, db)) {
			return Decision{MaxRows: pv.LimitNum, Source: fmt.Sprintf("db%s 库权限", db), PrivCheck: true}, nil
		}
	}
	return Decision{}, noPriv(fmt.Sprintf("你无 Redis db%s 的查询权限，请先申请。", db), db, "")
}

// ---- 并发限制 ----

var redisSlots = struct {
	sync.Mutex
	n map[string]int
}{n: map[string]int{}}

// acquireRedisSlot 限制每个用户同时执行的 Redis 命令数（命令和 key 浏览共用）。
func acquireRedisSlot(user string) (func(), error) {
	redisSlots.Lock()
	defer redisSlots.Unlock()
	if redisSlots.n[user] >= redisUserConcurrency {
		return nil, newErr(429, "busy", fmt.Sprintf("你同时执行的 Redis 命令已达 %d 条，请等待其中一条完成后再试", redisUserConcurrency))
	}
	redisSlots.n[user]++
	return func() {
		redisSlots.Lock()
		defer redisSlots.Unlock()
		if redisSlots.n[user] <= 1 {
			delete(redisSlots.n, user)
		} else {
			redisSlots.n[user]--
		}
	}, nil
}

// redisFail 把执行 / 连接错误转成业务错误。
func redisFail(ctx, qctx context.Context, err error, pol Policy) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return newErr(499, "canceled", "查询已取消")
	}
	if errors.Is(qctx.Err(), context.DeadlineExceeded) {
		return newErr(504, "timeout", fmt.Sprintf("查询超过 %d 秒已终止，请缩小范围或改用 SCAN 分批读取。", pol.MaxExecSeconds))
	}
	var ue *redisx.UserError
	if errors.As(err, &ue) {
		return newErr(400, "bad_query", ue.Msg)
	}
	if strings.Contains(err.Error(), "i/o timeout") { // 连接上的读写超时比 ctx 的计时器先到
		return newErr(504, "timeout", fmt.Sprintf("查询超过 %d 秒已终止，请缩小范围或改用 SCAN 分批读取。", pol.MaxExecSeconds))
	}
	return newErr(statusTargetError, "engine_error", err.Error())
}

// runRedis 执行已通过检查的命令并整形结果（Execute 的 Redis 分支）。
func (s *Service) runRedis(ctx context.Context, p Principal, req Request, pr *prepared, tr *execTrace) (*Result, error) {
	rp := pr.redis
	release, err := acquireRedisSlot(p.Username)
	if err != nil {
		return nil, err
	}
	defer release()
	e, err := s.open(pr.inst.DBType, s.repo.EngineConn(pr.inst))
	if err != nil {
		return nil, newErr(501, "unsupported_engine", err.Error())
	}
	defer e.Close()
	d, ok := e.(redisDialer)
	if !ok {
		return nil, newErr(501, "unsupported_engine", "Redis 引擎不可用")
	}
	timeout := time.Duration(pr.pol.MaxExecSeconds) * time.Second
	qctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	var ur *redisx.UserResult
	c, err := d.DialDB(qctx, rp.db)
	if err == nil {
		defer c.Close()
		ur, err = c.RunUser(qctx, rp.cmd)
	}
	cost := time.Since(start)
	tr.cost = cost
	if err != nil {
		return nil, redisFail(ctx, qctx, err, pr.pol)
	}

	sh := shapeRedis(rp.cmd, ur, pr.limit)
	notices := append([]string(nil), ur.Notices...)
	if sh.Binary > 0 {
		notices = append(notices, fmt.Sprintf("%d 个值含不可见字符或非 UTF-8 字节，已按 \\xHH 转义显示。", sh.Binary))
	}
	if sh.Cut > 0 {
		notices = append(notices, fmt.Sprintf("%d 个值超过 %d KB，已截断显示。", sh.Cut, redisMaxCell>>10))
	}
	if sh.Truncated {
		notices = append(notices, "结果已按行数 / 回复大小上限截断；可缩小范围，或用 SCAN 系列命令分批读取。")
	}
	executed := redisx.FormatUserCommand(ur.Args)
	res := &Result{
		Columns: sh.Columns, Rows: sh.Rows, RowCount: len(sh.Rows), EffectiveLimit: pr.limit, LimitSource: pr.dec.Source,
		Truncated: sh.Truncated, CostMs: cost.Milliseconds(), MaskedColumns: []string{}, ExecutedSQL: executed,
		Notice: joinNotice(notices...), Kind: "redis", Cursor: sh.Cursor, NextCmd: sh.NextCmd,
	}
	if res.Rows == nil {
		res.Rows = [][]any{}
	}
	log := &model.QueryLog{
		InstanceName: pr.inst.InstanceName, DBName: rp.dbName, SQLLog: executed, EffectRow: int64(len(sh.Rows)),
		CostTime: strconv.FormatFloat(cost.Seconds(), 'f', 3, 64), Username: p.Username, UserDisplay: p.Display,
		PrivCheck: pr.dec.PrivCheck,
	}
	if err := s.repo.InsertQueryLog(log); err != nil {
		slog.Error("写入 dbt_query_log 失败，已拒绝返回 Redis 查询结果", "err", err, "user", p.Username, "instance", pr.inst.InstanceName)
		return nil, newErr(500, "audit_failed", "查询已执行，但审计日志写入失败，结果未返回，请联系管理员。")
	}
	res.QueryLogID = log.ID
	return res, nil
}

// ---- 结果整形 ----

type redisShape struct {
	Columns   []string
	Rows      [][]any
	Truncated bool
	Cursor    string
	NextCmd   string // SCAN 系列：下一页的命令（游标换成新的）；游标为 0 时为空
	Binary    int    // 含不可见字符 / 非 UTF-8 的值个数
	Cut       int    // 过长被截断的值个数
	max       int
	bytes     int
}

// redisText 把一个值转成可显示的文本：合法 UTF-8 且没有控制字符（\t \n \r 除外）原样显示，否则把非法字节和控制字符转成 \xHH。
func redisText(s string) (out string, binary bool) {
	if utf8.ValidString(s) {
		clean := true
		for i := 0; i < len(s); i++ {
			if c := s[i]; (c < 0x20 && c != '\t' && c != '\n' && c != '\r') || c == 0x7f {
				clean = false
				break
			}
		}
		if clean {
			return s, false
		}
	}
	var sb strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			fmt.Fprintf(&sb, "\\x%02x", s[i])
		case (r < 0x20 && r != '\t' && r != '\n' && r != '\r') || r == 0x7f:
			fmt.Fprintf(&sb, "\\x%02x", r)
		default:
			sb.WriteString(s[i : i+size])
		}
		i += size
	}
	return sb.String(), true
}

func (sh *redisShape) cell(v any) any {
	switch x := v.(type) {
	case nil:
		return "(nil)"
	case int64:
		return x
	case string:
		s, bin := redisText(x)
		if bin {
			sh.Binary++
		}
		if len(s) > redisMaxCell {
			sh.Cut++
			cut := redisMaxCell
			for cut > 0 && !utf8.RuneStart(s[cut]) {
				cut--
			}
			s = s[:cut] + fmt.Sprintf("…（已截断，共 %d 字节）", len(x)) // i18n-ignore: 拼进结果单元格的值，属于数据输出，不随界面语言变化
		}
		return s
	case []any:
		parts := make([]string, len(x))
		for i, it := range x {
			parts[i] = fmt.Sprint(sh.cell(it))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *redisx.ServerError:
		return "(error) " + x.Msg
	}
	return fmt.Sprint(v)
}

// add 追加一行，返回 false 表示已达行数 / 字节上限（此行及之后的都被丢弃）。
func (sh *redisShape) add(cells ...any) bool {
	if sh.Truncated {
		return false
	}
	if sh.max > 0 && len(sh.Rows) >= sh.max {
		sh.Truncated = true
		return false
	}
	row := make([]any, len(cells))
	n := 0
	for i, c := range cells {
		row[i] = sh.cell(c)
		switch x := row[i].(type) {
		case string:
			n += len(x)
		default:
			n += 8
		}
	}
	if sh.bytes+n > redisMaxReplyBytes {
		sh.Truncated = true
		return false
	}
	sh.bytes += n
	sh.Rows = append(sh.Rows, row)
	return true
}

// shapeRedis 把回复整形成二维表：
//
//	字符串 / 数字 / 状态：(value) 一行；nil 显示 (nil)
//	数组：(index, value)；哈希：(field, value)；带分数的有序集合：(member, score)
//	SCAN 系列：第一行 ("(cursor)", 下一游标)，其后是 key / 成员 / 字段；
//	MGET / HMGET 等按位置对应的回复：(key, value)；Stream：(id, field, value)；地理查询按 WITH* 选项增列。
func shapeRedis(cmd *redisx.UserCommand, ur *redisx.UserResult, maxRows int) *redisShape {
	sh := &redisShape{Rows: [][]any{}, max: maxRows}
	arr, isArr := ur.Reply.([]any)
	if !isArr {
		sh.Columns = []string{"value"}
		sh.add(ur.Reply)
		return sh
	}
	list := func(col string, base int64) {
		sh.Columns = []string{"index", col}
		for i, v := range arr {
			if !sh.add(base+int64(i), v) {
				return
			}
		}
	}
	pairs := func(a, b string, items []any) {
		sh.Columns = []string{a, b}
		for i := 0; i < len(items); i += 2 {
			var v any
			if i+1 < len(items) {
				v = items[i+1]
			}
			if !sh.add(items[i], v) {
				return
			}
		}
	}
	keyed := func(a, b string, keys []string) {
		sh.Columns = []string{a, b}
		for i, v := range arr {
			k := ""
			if i < len(keys) {
				k = keys[i]
			}
			if !sh.add(k, v) {
				return
			}
		}
	}
	args := cmd.Args
	tail := func(n int) []string {
		if len(args) > n {
			return args[n:]
		}
		return nil
	}
	switch cmd.Name {
	case "SCAN", "SSCAN", "HSCAN", "ZSCAN":
		var items []any
		if len(arr) == 2 {
			sh.Cursor = fmt.Sprint(sh.cell(arr[0]))
			items, _ = arr[1].([]any)
			if ci := map[bool]int{true: 1, false: 2}[cmd.Name == "SCAN"]; sh.Cursor != "0" && ci < len(ur.Args) {
				next := append([]string(nil), ur.Args...)
				next[ci] = sh.Cursor
				sh.NextCmd = redisx.FormatUserCommand(next)
			}
		}
		switch {
		case cmd.Name == "HSCAN" && !cmd.Flags["NOVALUES"]:
			sh.Columns = []string{"field", "value"}
			sh.add("(cursor)", sh.Cursor)
			for i := 0; i < len(items); i += 2 {
				var v any
				if i+1 < len(items) {
					v = items[i+1]
				}
				if !sh.add(items[i], v) {
					break
				}
			}
		case cmd.Name == "ZSCAN":
			sh.Columns = []string{"member", "score"}
			sh.add("(cursor)", sh.Cursor)
			for i := 0; i < len(items); i += 2 {
				var v any
				if i+1 < len(items) {
					v = items[i+1]
				}
				if !sh.add(items[i], v) {
					break
				}
			}
		default:
			col := map[string]string{"SCAN": "key", "SSCAN": "member", "HSCAN": "field"}[cmd.Name]
			sh.Columns = []string{"index", col}
			sh.add("(cursor)", sh.Cursor)
			for i, v := range items {
				if !sh.add(int64(i+1), v) {
					break
				}
			}
		}
	case "HGETALL":
		pairs("field", "value", arr)
	case "HRANDFIELD":
		if cmd.Flags["WITHVALUES"] {
			pairs("field", "value", arr)
		} else {
			list("field", 0)
		}
	case "ZRANGE", "ZREVRANGE", "ZRANGEBYSCORE", "ZREVRANGEBYSCORE":
		if cmd.Flags["WITHSCORES"] {
			pairs("member", "score", arr)
		} else {
			list("member", ur.IndexBase)
		}
	case "ZRANGEBYLEX":
		list("member", 0)
	case "MGET":
		keyed("key", "value", tail(1))
	case "HMGET":
		keyed("field", "value", tail(2))
	case "SMISMEMBER":
		keyed("member", "is_member", tail(2))
	case "ZMSCORE":
		keyed("member", "score", tail(2))
	case "GEOPOS":
		sh.Columns = []string{"member", "longitude", "latitude"}
		for i, v := range arr {
			m := ""
			if i+2 < len(args) {
				m = args[i+2]
			}
			var lon, lat any
			if pos, ok := v.([]any); ok && len(pos) == 2 {
				lon, lat = pos[0], pos[1]
			}
			if !sh.add(m, lon, lat) {
				break
			}
		}
	case "GEORADIUS_RO", "GEOSEARCH":
		shapeGeo(sh, cmd, arr)
	case "XRANGE", "XREVRANGE":
		sh.Columns = []string{"id", "field", "value"}
	stream:
		for _, it := range arr {
			ent, ok := it.([]any)
			if !ok || len(ent) < 2 {
				continue
			}
			fv, _ := ent[1].([]any)
			if len(fv) == 0 {
				if !sh.add(ent[0], nil, nil) {
					break stream
				}
				continue
			}
			for i := 0; i < len(fv); i += 2 {
				var v any
				if i+1 < len(fv) {
					v = fv[i+1]
				}
				if !sh.add(ent[0], fv[i], v) {
					break stream
				}
			}
		}
	case "LRANGE":
		list("value", ur.IndexBase)
	case "SMEMBERS", "SINTER", "SUNION", "SDIFF", "SRANDMEMBER":
		list("member", 0)
	case "HKEYS":
		list("field", 0)
	default:
		list("value", 0)
	}
	return sh
}

func shapeGeo(sh *redisShape, cmd *redisx.UserCommand, arr []any) {
	withDist, withHash, withCoord := cmd.Flags["WITHDIST"], cmd.Flags["WITHHASH"], cmd.Flags["WITHCOORD"]
	if !withDist && !withHash && !withCoord {
		sh.Columns = []string{"index", "member"}
		for i, v := range arr {
			if !sh.add(int64(i), v) {
				return
			}
		}
		return
	}
	sh.Columns = []string{"member"}
	if withDist {
		sh.Columns = append(sh.Columns, "distance")
	}
	if withHash {
		sh.Columns = append(sh.Columns, "hash")
	}
	if withCoord {
		sh.Columns = append(sh.Columns, "longitude", "latitude")
	}
	for _, it := range arr {
		ent, ok := it.([]any)
		if !ok || len(ent) == 0 {
			continue
		}
		row := []any{ent[0]}
		i := 1
		if withDist && i < len(ent) {
			row = append(row, ent[i])
			i++
		}
		if withHash && i < len(ent) {
			row = append(row, ent[i])
			i++
		}
		if withCoord {
			var lon, lat any
			if i < len(ent) {
				if pos, ok := ent[i].([]any); ok && len(pos) == 2 {
					lon, lat = pos[0], pos[1]
				}
			}
			row = append(row, lon, lat)
		}
		if !sh.add(row...) {
			return
		}
	}
}

// ---- 库列表、元数据、key 浏览 ----

// RedisDBItem 是库下拉里的一项：库编号与键数（-1 表示取不到）。
type RedisDBItem struct {
	DB   string `json:"db"`
	Keys int64  `json:"keys"`
}

// redisDBItems 取实例的库列表（经实例的库名过滤规则），按编号排序。
func (s *Service) redisDBItems(ctx context.Context, e engine.Engine, inst *model.Instance) ([]RedisDBItem, error) {
	l, ok := e.(redisLister)
	if !ok {
		return nil, errors.New("Redis 引擎不可用")
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	list, err := l.DatabaseList(cctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(list))
	keys := map[string]int64{}
	for i, d := range list {
		names[i] = strconv.Itoa(d.DB)
		keys[names[i]] = d.Keys
	}
	kept, err := filterDBs(names, inst.ShowDBNameRegex, inst.DeniedDBNameRegex)
	if err != nil {
		return nil, err
	}
	sort.Slice(kept, func(i, j int) bool {
		a, _ := strconv.Atoi(kept[i])
		b, _ := strconv.Atoi(kept[j])
		return a < b
	})
	out := make([]RedisDBItem, len(kept))
	for i, n := range kept {
		out[i] = RedisDBItem{DB: n, Keys: keys[n]}
	}
	return out, nil
}

// redisDBNames 是 Databases 的 Redis 分支：返回 "0".."N-1"。
func (s *Service) redisDBNames(ctx context.Context, e engine.Engine, inst *model.Instance) ([]string, error) {
	items, err := s.redisDBItems(ctx, e, inst)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.DB
	}
	return out, nil
}

// RedisDatabases 返回 Redis 实例的库列表及各库键数（页面下拉显示 db0 (键数)；键数取不到为 -1）。
func (s *Service) RedisDatabases(ctx context.Context, p Principal, id uint) (out []RedisDBItem, err error) {
	err = s.withEngine(ctx, p, id, func(e engine.Engine, inst *model.Instance) error {
		if !isRedis(inst.DBType) {
			return newErr(400, "bad_request", "该实例不是 Redis")
		}
		out, err = s.redisDBItems(ctx, e, inst)
		return err
	})
	if out == nil {
		out = []RedisDBItem{}
	}
	return
}

// redisMeta 是 DBMeta 的 Redis 分支：没有表，只带限制策略（页面据此画提示条）。
func (s *Service) redisMeta(ctx context.Context, p Principal, inst *model.Instance, db string) (*DBMeta, error) {
	_, name, err := normalizeRedisDB(db)
	if err != nil {
		return nil, err
	}
	out := &DBMeta{DB: name, Tables: []TableMeta{}}
	if pp, ok := s.auth.(PolicyProvider); ok {
		if info, err := pp.PolicyInfo(ctx, p, inst, name, s.Policy().AdminLimit); err == nil {
			out.Policy = info
		}
	}
	if out.Policy.Mode == "" {
		out.Policy = PolicyInfo{Mode: "unknown", Tables: []string{}}
	}
	return out, nil
}

// RedisKeysReq 是 key 浏览请求。
type RedisKeysReq struct {
	Match  string
	Cursor string
	Count  int
}

// RedisKeyItem 是 key 浏览的一项。Key 是显示用文本（不可见字符已转 \xHH），Arg 是可直接写进命令的 redis-cli 参数（必要时带引号）。
type RedisKeyItem struct {
	Key    string `json:"key"`
	Arg    string `json:"arg"`
	Type   string `json:"type"`
	Binary bool   `json:"binary,omitempty"`
}

// RedisKeysResult 是一页 key。Cursor 为 "0" 表示已扫完。
type RedisKeysResult struct {
	Keys   []RedisKeyItem `json:"keys"`
	Cursor string         `json:"cursor"`
	Done   bool           `json:"done"`
	Match  string         `json:"match"`
}

// RedisKeys 用 SCAN 分页列出库里的 key 与类型（左侧“key 浏览”）。需要对该库有查询权限；每页条数与扫描轮数都有上限，
// 对大库也只做有限的工作。不写查询日志（同库表树，只列名字）。
func (s *Service) RedisKeys(ctx context.Context, p Principal, id uint, db string, req RedisKeysReq) (*RedisKeysResult, error) {
	dbn, dbName, err := normalizeRedisDB(db)
	if err != nil {
		return nil, err
	}
	var out *RedisKeysResult
	err = s.withEngine(ctx, p, id, func(e engine.Engine, inst *model.Instance) error {
		if !isRedis(inst.DBType) {
			return newErr(400, "bad_request", "该实例不是 Redis")
		}
		pol := s.Policy()
		if _, err := s.auth.AuthorizeQuery(ctx, p, inst, dbName, "SCAN 0", pol.AdminLimit); err != nil {
			return err
		}
		release, err := acquireRedisSlot(p.Username)
		if err != nil {
			return err
		}
		defer release()
		d, ok := e.(redisDialer)
		if !ok {
			return newErr(501, "unsupported_engine", "Redis 引擎不可用")
		}
		timeout := time.Duration(min(pol.MaxExecSeconds, 15)) * time.Second
		qctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		c, err := d.DialDB(qctx, dbn)
		if err != nil {
			return redisFail(ctx, qctx, err, pol)
		}
		defer c.Close()
		count := req.Count
		if count <= 0 {
			count = redisDefaultKeyPage
		}
		match := req.Match
		if match == "" {
			match = "*"
		}
		page, err := c.BrowseKeys(qctx, match, req.Cursor, count)
		if err != nil {
			return redisFail(ctx, qctx, err, pol)
		}
		out = &RedisKeysResult{Keys: make([]RedisKeyItem, 0, len(page.Keys)), Cursor: page.Cursor, Done: page.Cursor == "0", Match: match}
		for _, k := range page.Keys {
			text, bin := redisText(k.Key)
			out.Keys = append(out.Keys, RedisKeyItem{Key: text, Arg: redisx.QuoteCLIArg(k.Key), Type: k.Type, Binary: bin})
		}
		return nil
	})
	return out, err
}
