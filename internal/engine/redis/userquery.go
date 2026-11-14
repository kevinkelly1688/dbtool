// @author: kevinkelly
// @license: Apache Licence
// @file: userquery.go
// @time: 2026/10/28

package redis

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// 在线查询（用户手输命令）用的只读命令检查与受限执行。
//
// 与 whitelist.go 的区别：whitelist.go 是平台自己巡检 / 采集用的“元数据命令”白名单（GET、HGETALL 这类读业务数据的命令一律拒绝）；
// 这里是给用户在线查询页用的“读数据命令”白名单（固定在代码里，不可配置），并对返回量大的命令做限量：
//   - SCAN / HSCAN / SSCAN / ZSCAN 的 COUNT 夹到上限，缺省补默认值；
//   - LRANGE / ZRANGE / ZREVRANGE / XRANGE / ZRANGEBYSCORE 等区间类命令收窄到上限（负下标先 LLEN / ZCARD 探测再换算）；
//   - HGETALL / HKEYS / HVALS / SMEMBERS / SINTER 等全量命令先 HLEN / SCARD 探测，超过阈值拒绝并提示改用 *SCAN；
//   - 一律拒绝写命令、KEYS、阻塞命令、订阅、脚本、CONFIG / INFO / CLIENT、SELECT、AUTH 等。
//
// 先 ParseUserCommand（纯静态检查与改写，不连服务端），再 Client.RunUser（探测 + 执行）。

// UserError 是命令检查 / 探测不通过的业务原因，原文可直接展示给用户。
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

func uerr(format string, a ...any) error { return &UserError{Msg: fmt.Sprintf(format, a...)} }

// 在线查询的限量。
const (
	UserScanCountMax     = 1000     // SCAN 类命令 COUNT 上限
	UserScanCountDefault = 100      // 缺省补的 COUNT
	UserRangeMax         = 1000     // 区间类命令单次最多返回的元素数
	UserFullReadMax      = 5000     // 全量命令（HGETALL / SMEMBERS …）允许的最大元素数
	UserKeysMax          = 500      // 多键 / 多成员命令的最大键数（MGET / EXISTS / HMGET …）
	UserBitcountMaxBytes = 16 << 20 // 不带区间的 BITCOUNT 允许的最大字符串长度
	userLPosMaxLen       = 100000   // LPOS 缺省最多比较的元素数
)

type probeKind int

const (
	probeCard   probeKind = iota // 先取元素个数，超过阈值拒绝
	probeStrlen                  // 先取字符串长度
	probeRange                   // 先取长度，换算负下标并收窄
)

type userProbe struct {
	kind   probeKind
	lenCmd string   // HLEN / SCARD / LLEN / ZCARD / STRLEN
	keys   []string // probeCard 可以有多个键（SINTER 等），合计超限就拒绝
	hint   string   // 超限时的建议
	si, ei int      // probeRange：Args 里 start / stop 的下标
}

// UserCommand 是通过静态检查、已按限量改写的命令。
type UserCommand struct {
	Name      string          // 大写命令名（OBJECT / MEMORY 带子命令，如 "OBJECT ENCODING"）
	Args      []string        // 要发送的参数，Args[0] 为大写命令名
	Orig      []string        // 用户原始参数
	Notices   []string        // 静态改写产生的提示（COUNT 夹到上限、补 LIMIT 等）
	Flags     map[string]bool // 选项：WITHSCORES / WITHVALUES / NOVALUES / WITHDIST / WITHCOORD / WITHHASH
	IndexBase int64           // LRANGE / ZRANGE 的起始下标（结果表“index”列的起点）
	probes    []userProbe
}

func (c *UserCommand) notice(format string, a ...any) {
	c.Notices = append(c.Notices, fmt.Sprintf(format, a...))
}

// 拒绝原因分类。
var (
	denyBlocking  = setOf("BLPOP", "BRPOP", "BRPOPLPUSH", "BLMOVE", "BLMPOP", "BZPOPMIN", "BZPOPMAX", "BZMPOP", "XREAD", "XREADGROUP", "WAIT", "WAITAOF")
	denySub       = setOf("MONITOR", "SUBSCRIBE", "PSUBSCRIBE", "SSUBSCRIBE", "UNSUBSCRIBE", "PUNSUBSCRIBE", "SUNSUBSCRIBE", "PUBLISH", "SPUBLISH", "SYNC", "PSYNC")
	denyScript    = setOf("EVAL", "EVALSHA", "EVAL_RO", "EVALSHA_RO", "SCRIPT", "FUNCTION", "FCALL", "FCALL_RO")
	denyAdmin     = setOf("CONFIG", "INFO", "CLIENT", "DEBUG", "COMMAND", "SLOWLOG", "LATENCY", "ROLE", "CLUSTER", "ACL", "MODULE", "SHUTDOWN", "FAILOVER", "SWAPDB", "RESET", "LOLWUT", "MONITOR", "REPLCONF", "READONLY", "READWRITE")
	denyFlush     = setOf("FLUSHALL", "FLUSHDB")
	denyPersist   = setOf("SAVE", "BGSAVE", "BGREWRITEAOF", "LASTSAVE")
	denyReplicate = setOf("SLAVEOF", "REPLICAOF", "MIGRATE")
	denyAuth      = setOf("AUTH", "HELLO")
	denyTx        = setOf("MULTI", "EXEC", "DISCARD", "WATCH", "UNWATCH")
	denyWrite     = setOf(
		"SET", "SETNX", "SETEX", "PSETEX", "MSET", "MSETNX", "APPEND", "INCR", "INCRBY", "INCRBYFLOAT", "DECR", "DECRBY", "GETSET", "GETDEL", "GETEX",
		"SETRANGE", "SETBIT", "BITFIELD", "BITOP", "DEL", "UNLINK", "EXPIRE", "PEXPIRE", "EXPIREAT", "PEXPIREAT", "PERSIST", "RENAME", "RENAMENX",
		"MOVE", "COPY", "RESTORE", "RESTORE-ASKING", "TOUCH", "OBJECT FREQ-SET",
		"HSET", "HSETNX", "HMSET", "HDEL", "HINCRBY", "HINCRBYFLOAT", "HGETDEL", "HSETEX", "HEXPIRE",
		"LPUSH", "RPUSH", "LPUSHX", "RPUSHX", "LPOP", "RPOP", "LINSERT", "LSET", "LREM", "LTRIM", "RPOPLPUSH", "LMOVE", "LMPOP",
		"SADD", "SREM", "SPOP", "SMOVE", "SINTERSTORE", "SUNIONSTORE", "SDIFFSTORE",
		"ZADD", "ZREM", "ZINCRBY", "ZPOPMIN", "ZPOPMAX", "ZMPOP", "ZREMRANGEBYRANK", "ZREMRANGEBYSCORE", "ZREMRANGEBYLEX", "ZUNIONSTORE", "ZINTERSTORE",
		"ZDIFFSTORE", "ZRANGESTORE", "XADD", "XDEL", "XTRIM", "XGROUP", "XACK", "XCLAIM", "XAUTOCLAIM", "XSETID", "PFADD", "PFMERGE",
		"GEOADD", "GEORADIUS", "GEORADIUSBYMEMBER", "GEOSEARCHSTORE", "SELECT")
)

func setOf(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// denyReason 返回点名拒绝的原因；不在任何分类里返回空串（走“不在白名单”的通用提示）。
func denyReason(name string) string {
	switch {
	case name == "KEYS":
		return "KEYS 会在大库上长时间阻塞 Redis，已禁止；请改用 SCAN 0 MATCH 模式 COUNT 100 分批读取（或用左侧 key 浏览）"
	case name == "SELECT":
		return "SELECT 已禁止：库由页面顶部的库下拉选择，连接时已指定"
	case name == "SORT" || name == "SORT_RO":
		return fmt.Sprintf("%s 可能带 STORE 写入或对大集合排序，已禁止", name)
	case denyAuth[name]:
		return fmt.Sprintf("%s 已禁止：在线查询不允许认证 / 切换协议，也不要在命令里携带密码", name)
	case denyBlocking[name]:
		return fmt.Sprintf("%s 是阻塞命令，会占住连接，已禁止", name)
	case denySub[name]:
		return fmt.Sprintf("%s 是订阅 / 复制 / 监控类命令，在线查询不支持", name)
	case denyScript[name]:
		return fmt.Sprintf("%s 可以执行脚本，已禁止", name)
	case denyFlush[name]:
		return fmt.Sprintf("%s 会清空数据，已禁止", name)
	case denyPersist[name]:
		return fmt.Sprintf("%s 属于持久化管理命令，已禁止", name)
	case denyReplicate[name]:
		return fmt.Sprintf("%s 属于复制 / 迁移命令，已禁止", name)
	case denyTx[name]:
		return fmt.Sprintf("%s 是事务命令，在线查询一次只执行一条只读命令", name)
	case denyAdmin[name]:
		return fmt.Sprintf("%s 属于运维命令，已禁止；实例巡检、慢查询请使用对应的 DBA 页面", name)
	case denyWrite[name]:
		return fmt.Sprintf("%s 会修改数据，在线查询只允许只读命令；数据变更请通过「SQL 上线」提交 Redis 工单", name)
	}
	return ""
}

type userCheck func(c *UserCommand) error

// userTable 是只读命令白名单（代码里固定，不可配置）。
var userTable map[string]userCheck

func init() {
	arity := func(min, max int) userCheck {
		return func(c *UserCommand) error {
			n := len(c.Args) - 1
			if n < min || (max >= 0 && n > max) {
				return uerr("命令 %s 的参数个数不对", c.Name)
			}
			return nil
		}
	}
	multi := func(min int) userCheck { // 多键 / 多成员：参数个数有上限
		return func(c *UserCommand) error {
			n := len(c.Args) - 1
			if n < min {
				return uerr("命令 %s 的参数个数不对", c.Name)
			}
			if n > UserKeysMax+1 {
				return uerr("命令 %s 一次最多 %d 个键 / 成员", c.Name, UserKeysMax)
			}
			return nil
		}
	}
	full := func(lenCmd, scanCmd string) userCheck { // 全量读取：先探测元素个数
		return func(c *UserCommand) error {
			if err := arity(1, 1)(c); err != nil {
				return err
			}
			c.probes = append(c.probes, userProbe{kind: probeCard, lenCmd: lenCmd, keys: []string{c.Args[1]},
				hint: fmt.Sprintf("请改用 %s %s 0 COUNT 100 分页读取", scanCmd, quoteCLIArg(c.Args[1]))})
			return nil
		}
	}
	setOps := func(c *UserCommand) error {
		if err := multi(1)(c); err != nil {
			return err
		}
		c.probes = append(c.probes, userProbe{kind: probeCard, lenCmd: "SCARD", keys: append([]string(nil), c.Args[1:]...),
			hint: "请改用 SSCAN 分页读取单个集合，或缩小集合"})
		return nil
	}
	userTable = map[string]userCheck{
		// 字符串
		"GET": arity(1, 1), "MGET": multi(1), "STRLEN": arity(1, 1), "GETRANGE": arity(3, 3), "EXISTS": multi(1), "TYPE": arity(1, 1),
		"TTL": arity(1, 1), "PTTL": arity(1, 1), "EXPIRETIME": arity(1, 1), "PEXPIRETIME": arity(1, 1),
		// 哈希
		"HGET": arity(2, 2), "HMGET": multi(2), "HGETALL": full("HLEN", "HSCAN"), "HLEN": arity(1, 1), "HKEYS": full("HLEN", "HSCAN"),
		"HVALS": full("HLEN", "HSCAN"), "HEXISTS": arity(2, 2), "HSTRLEN": arity(2, 2), "HSCAN": checkHScan, "HRANDFIELD": checkRandField,
		// 列表
		"LRANGE": checkLRange, "LLEN": arity(1, 1), "LINDEX": arity(2, 2), "LPOS": checkLPos,
		// 集合
		"SMEMBERS": full("SCARD", "SSCAN"), "SCARD": arity(1, 1), "SISMEMBER": arity(2, 2), "SMISMEMBER": multi(2), "SRANDMEMBER": checkRandMember,
		"SSCAN": checkSScan, "SINTER": setOps, "SUNION": setOps, "SDIFF": setOps,
		// 有序集合
		"ZRANGE": checkZRange, "ZREVRANGE": checkZRevRange, "ZRANGEBYSCORE": checkZByScore, "ZREVRANGEBYSCORE": checkZByScore, "ZRANGEBYLEX": checkZByLex,
		"ZCARD": arity(1, 1), "ZSCORE": arity(2, 2), "ZMSCORE": multi(2), "ZRANK": checkZRank, "ZREVRANK": checkZRank, "ZCOUNT": arity(3, 3),
		"ZLEXCOUNT": arity(3, 3), "ZSCAN": checkZScan,
		// 其他
		"SCAN": checkScan2, "DBSIZE": arity(0, 0), "XLEN": arity(1, 1), "XRANGE": checkXRange, "XREVRANGE": checkXRange, "BITCOUNT": checkBitcount,
		"GETBIT": arity(2, 2), "PFCOUNT": multi(1), "OBJECT": checkObject, "MEMORY": checkMemory, "GEOPOS": multi(2), "GEODIST": checkGeoDist,
		"GEORADIUS_RO": checkGeoRadiusRO, "GEOSEARCH": checkGeoSearch,
	}
}

// UserCommandNames 返回在线查询放行的命令名（排序后），用于帮助文档与测试。
func UserCommandNames() []string {
	out := make([]string, 0, len(userTable))
	for n := range userTable {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ParseUserCommand 检查并改写一条已分词的命令：名称在白名单内、参数形态合法、返回量受限。不连服务端。
func ParseUserCommand(args []string) (*UserCommand, error) {
	if len(args) == 0 {
		return nil, uerr("命令为空")
	}
	name := strings.ToUpper(args[0])
	if r := denyReason(name); r != "" {
		return nil, &UserError{Msg: r}
	}
	chk, ok := userTable[name]
	if !ok {
		return nil, uerr("命令 %s 不在在线查询的只读白名单内（白名单为固定列表，不可配置）", name)
	}
	for _, a := range args[1:] {
		if u := strings.ToUpper(a); u == "AUTH" || u == "AUTH2" {
			return nil, uerr("命令参数里含 AUTH，疑似携带密码，已拒绝")
		}
	}
	c := &UserCommand{Name: name, Orig: append([]string(nil), args...), Flags: map[string]bool{}}
	c.Args = append([]string{name}, args[1:]...)
	if err := chk(c); err != nil {
		return nil, err
	}
	return c, nil
}

// FormatUserCommand 把参数还原成 redis-cli 风格的一行命令（需要时加引号、转义），用于“实际执行的命令”展示与日志。
func FormatUserCommand(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = quoteCLIArg(a)
	}
	return strings.Join(parts, " ")
}

// QuoteCLIArg 是 TokenizeLine 的逆运算：无需引号的原样返回，否则用双引号包起来并转义。
func QuoteCLIArg(s string) string { return quoteCLIArg(s) }

func quoteCLIArg(s string) string {
	plain := s != ""
	for i := 0; i < len(s) && plain; i++ {
		ch := s[i]
		if ch <= ' ' || ch >= 0x7f || ch == '"' || ch == '\'' || ch == '\\' {
			plain = false
		}
	}
	if plain {
		return s
	}
	var sb strings.Builder
	sb.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			fmt.Fprintf(&sb, "\\x%02x", s[i])
			i++
			continue
		}
		switch {
		case r == '"':
			sb.WriteString(`\"`)
		case r == '\\':
			sb.WriteString(`\\`)
		case r == '\n':
			sb.WriteString(`\n`)
		case r == '\r':
			sb.WriteString(`\r`)
		case r == '\t':
			sb.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&sb, "\\x%02x", r)
		default:
			sb.WriteString(s[i : i+size])
		}
		i += size
	}
	sb.WriteByte('"')
	return sb.String()
}

// ---- 参数解析辅助 ----

const maxSafeInt = 1 << 50 // 下标 / 偏移的绝对值上限，避免做加法时溢出

func (c *UserCommand) intArg(i int, what string) (int64, error) {
	n, err := strconv.ParseInt(c.Args[i], 10, 64)
	if err != nil || n > maxSafeInt || n < -maxSafeInt {
		return 0, uerr("命令 %s 的%s必须是整数", c.Name, what)
	}
	return n, nil
}

// parseOpts 从 Args[from:] 起按 spec（选项名 → 参数个数）解析选项，返回 选项名 → 位置。重复的选项取最后一个。
func (c *UserCommand) parseOpts(from int, spec map[string]int) (map[string]int, error) {
	pos := map[string]int{}
	for i := from; i < len(c.Args); {
		opt := strings.ToUpper(c.Args[i])
		n, ok := spec[opt]
		if !ok {
			return nil, uerr("命令 %s 不支持选项 %s", c.Name, c.Args[i])
		}
		if i+n >= len(c.Args) {
			return nil, uerr("命令 %s 的选项 %s 缺少参数", c.Name, opt)
		}
		pos[opt] = i
		i += n + 1
	}
	return pos, nil
}

// capCount 处理 COUNT 类选项：存在则要求是正整数并夹到 max；不存在且 def>0 则补上。
func (c *UserCommand) capCount(pos map[string]int, opt string, def, max int) error {
	p, ok := pos[opt]
	if !ok {
		if def > 0 {
			c.Args = append(c.Args, opt, strconv.Itoa(def))
			c.notice("未指定 %s，已补默认值 %d", opt, def)
		}
		return nil
	}
	n, err := c.intArg(p+1, fmt.Sprintf("%s 值", opt))
	if err != nil {
		return err
	}
	if n < 1 {
		return uerr("命令 %s 的 %s 必须是正整数", c.Name, opt)
	}
	if n > int64(max) {
		c.Args[p+1] = strconv.Itoa(max)
		c.notice("%s %d 超过上限，已改为 %d", opt, n, max)
	}
	return nil
}

// capLimit 处理 LIMIT offset count：缺省补 LIMIT 0 N；count 为负（表示全部）或超限夹到 N。
func (c *UserCommand) capLimit(pos map[string]int) error {
	p, ok := pos["LIMIT"]
	if !ok {
		c.Args = append(c.Args, "LIMIT", "0", strconv.Itoa(UserRangeMax))
		c.notice("未带 LIMIT，已自动加上 LIMIT 0 %d（单次最多返回 %d 条）", UserRangeMax, UserRangeMax)
		return nil
	}
	if _, err := c.intArg(p+1, "LIMIT 偏移"); err != nil {
		return err
	}
	n, err := c.intArg(p+2, "LIMIT 条数")
	if err != nil {
		return err
	}
	if n < 0 || n > UserRangeMax {
		c.Args[p+2] = strconv.Itoa(UserRangeMax)
		c.notice("LIMIT 条数超过上限，已改为 %d", UserRangeMax)
	}
	return nil
}

// rangeIdx 处理下标区间（start / stop 在 Args 的位置 si / ei）：两端都非负时静态收窄，含负数时登记探测，执行前换算成绝对下标。
func (c *UserCommand) rangeIdx(si, ei int, lenCmd string) error {
	s, err := c.intArg(si, "起始下标")
	if err != nil {
		return err
	}
	e, err := c.intArg(ei, "结束下标")
	if err != nil {
		return err
	}
	c.IndexBase = max(s, 0)
	if s >= 0 && e >= 0 {
		if e >= s && e-s+1 > UserRangeMax {
			c.Args[ei] = strconv.FormatInt(s+UserRangeMax-1, 10)
			c.notice("区间过大，已收窄为 %d..%d（单次最多 %d 条，请调整 start / stop 分页）", s, s+UserRangeMax-1, UserRangeMax)
		}
		return nil
	}
	c.probes = append(c.probes, userProbe{kind: probeRange, lenCmd: lenCmd, keys: []string{c.Args[1]}, si: si, ei: ei})
	return nil
}

// ---- 各命令的检查 ----

func (c *UserCommand) need(min, max int) error {
	n := len(c.Args) - 1
	if n < min || (max >= 0 && n > max) {
		return uerr("命令 %s 的参数个数不对", c.Name)
	}
	return nil
}

func (c *UserCommand) scanCursor(i int) error {
	if _, err := strconv.ParseUint(c.Args[i], 10, 64); err != nil {
		return uerr("命令 %s 的游标必须是非负整数（从 0 开始）", c.Name)
	}
	return nil
}

func (c *UserCommand) scanLike(cursorIdx int, spec map[string]int) error {
	if err := c.need(cursorIdx, -1); err != nil {
		return err
	}
	if err := c.scanCursor(cursorIdx); err != nil {
		return err
	}
	pos, err := c.parseOpts(cursorIdx+1, spec)
	if err != nil {
		return err
	}
	if p, ok := pos["MATCH"]; ok && len(c.Args[p+1]) > 512 {
		return uerr("MATCH 模式过长（最多 512 字节）")
	}
	if _, ok := pos["NOVALUES"]; ok {
		c.Flags["NOVALUES"] = true
	}
	return c.capCount(pos, "COUNT", UserScanCountDefault, UserScanCountMax)
}

func checkScan2(c *UserCommand) error {
	return c.scanLike(1, map[string]int{"MATCH": 1, "COUNT": 1, "TYPE": 1})
}
func checkHScan(c *UserCommand) error {
	return c.scanLike(2, map[string]int{"MATCH": 1, "COUNT": 1, "NOVALUES": 0})
}
func checkSScan(c *UserCommand) error { return c.scanLike(2, map[string]int{"MATCH": 1, "COUNT": 1}) }
func checkZScan(c *UserCommand) error { return c.scanLike(2, map[string]int{"MATCH": 1, "COUNT": 1}) }

func checkLRange(c *UserCommand) error {
	if err := c.need(3, 3); err != nil {
		return err
	}
	return c.rangeIdx(2, 3, "LLEN")
}

func checkZRevRange(c *UserCommand) error {
	if err := c.need(3, 4); err != nil {
		return err
	}
	if len(c.Args) == 5 {
		if !strings.EqualFold(c.Args[4], "WITHSCORES") {
			return uerr("ZREVRANGE 只支持可选的 WITHSCORES")
		}
		c.Flags["WITHSCORES"] = true
	}
	return c.rangeIdx(2, 3, "ZCARD")
}

func checkZRange(c *UserCommand) error {
	if err := c.need(3, -1); err != nil {
		return err
	}
	pos, err := c.parseOpts(4, map[string]int{"BYSCORE": 0, "BYLEX": 0, "REV": 0, "WITHSCORES": 0, "LIMIT": 2})
	if err != nil {
		return err
	}
	if _, ok := pos["WITHSCORES"]; ok {
		c.Flags["WITHSCORES"] = true
	}
	_, byScore := pos["BYSCORE"]
	_, byLex := pos["BYLEX"]
	switch {
	case byScore && byLex:
		return uerr("ZRANGE 不能同时使用 BYSCORE 和 BYLEX")
	case byScore || byLex:
		return c.capLimit(pos)
	}
	if _, ok := pos["LIMIT"]; ok {
		return uerr("ZRANGE 的 LIMIT 只能和 BYSCORE / BYLEX 一起使用")
	}
	return c.rangeIdx(2, 3, "ZCARD")
}

func checkZByScore(c *UserCommand) error {
	if err := c.need(3, -1); err != nil {
		return err
	}
	pos, err := c.parseOpts(4, map[string]int{"WITHSCORES": 0, "LIMIT": 2})
	if err != nil {
		return err
	}
	if _, ok := pos["WITHSCORES"]; ok {
		c.Flags["WITHSCORES"] = true
	}
	return c.capLimit(pos)
}

func checkZByLex(c *UserCommand) error {
	if err := c.need(3, -1); err != nil {
		return err
	}
	pos, err := c.parseOpts(4, map[string]int{"LIMIT": 2})
	if err != nil {
		return err
	}
	return c.capLimit(pos)
}

func checkZRank(c *UserCommand) error {
	if err := c.need(2, 3); err != nil {
		return err
	}
	if len(c.Args) == 4 && !strings.EqualFold(c.Args[3], "WITHSCORE") {
		return uerr("%s 只支持可选的 WITHSCORE", c.Name)
	}
	return nil
}

func checkXRange(c *UserCommand) error {
	if err := c.need(3, 5); err != nil {
		return err
	}
	pos, err := c.parseOpts(4, map[string]int{"COUNT": 1})
	if err != nil {
		return err
	}
	return c.capCount(pos, "COUNT", UserRangeMax, UserRangeMax)
}

func checkRandMember(c *UserCommand) error {
	if err := c.need(1, 2); err != nil {
		return err
	}
	return c.capRandCount(2)
}

func checkRandField(c *UserCommand) error {
	if err := c.need(1, 3); err != nil {
		return err
	}
	if len(c.Args) == 4 {
		if !strings.EqualFold(c.Args[3], "WITHVALUES") {
			return uerr("HRANDFIELD 只支持可选的 WITHVALUES")
		}
		c.Flags["WITHVALUES"] = true
	}
	return c.capRandCount(2)
}

func (c *UserCommand) capRandCount(i int) error {
	if i >= len(c.Args) {
		return nil
	}
	n, err := c.intArg(i, "数量")
	if err != nil {
		return err
	}
	if n > UserRangeMax || n < -UserRangeMax {
		v := int64(UserRangeMax)
		if n < 0 {
			v = -v
		}
		c.Args[i] = strconv.FormatInt(v, 10)
		c.notice("数量超过上限，已改为 %d", v)
	}
	return nil
}

func checkLPos(c *UserCommand) error {
	if err := c.need(2, -1); err != nil {
		return err
	}
	pos, err := c.parseOpts(3, map[string]int{"RANK": 1, "COUNT": 1, "MAXLEN": 1})
	if err != nil {
		return err
	}
	if p, ok := pos["COUNT"]; ok {
		n, err := c.intArg(p+1, "COUNT 值")
		if err != nil {
			return err
		}
		if n < 0 {
			return uerr("LPOS 的 COUNT 不能为负")
		}
		if n == 0 || n > UserRangeMax {
			c.Args[p+1] = strconv.Itoa(UserRangeMax)
			c.notice("COUNT 已限制为最多 %d 个匹配", UserRangeMax)
		}
	}
	if p, ok := pos["MAXLEN"]; ok {
		n, err := c.intArg(p+1, "MAXLEN 值")
		if err != nil {
			return err
		}
		if n < 0 {
			return uerr("LPOS 的 MAXLEN 不能为负")
		}
		if n == 0 || n > 1000000 {
			c.Args[p+1] = strconv.Itoa(userLPosMaxLen)
			c.notice("MAXLEN 已限制为最多比较 %d 个元素", userLPosMaxLen)
		}
	} else {
		c.Args = append(c.Args, "MAXLEN", strconv.Itoa(userLPosMaxLen))
		c.notice("已限制最多比较 %d 个元素（MAXLEN）", userLPosMaxLen)
	}
	return nil
}

func checkBitcount(c *UserCommand) error {
	n := len(c.Args) - 1
	if n != 1 && n != 3 && n != 4 {
		return uerr("BITCOUNT 的参数个数不对（key [start end [BYTE|BIT]]）")
	}
	if n == 4 {
		if u := strings.ToUpper(c.Args[4]); u != "BYTE" && u != "BIT" {
			return uerr("BITCOUNT 的第 4 个参数只能是 BYTE 或 BIT")
		}
	}
	if n == 1 {
		c.probes = append(c.probes, userProbe{kind: probeStrlen, lenCmd: "STRLEN", keys: []string{c.Args[1]},
			hint: "请带上 start end 区间分段统计"})
	}
	return nil
}

func checkObject(c *UserCommand) error {
	if len(c.Args) < 2 {
		return uerr("OBJECT 需要子命令，在线查询只支持 ENCODING / IDLETIME / FREQ")
	}
	sub := strings.ToUpper(c.Args[1])
	if sub != "ENCODING" && sub != "IDLETIME" && sub != "FREQ" {
		return uerr("OBJECT %s 不在白名单内，在线查询只支持 OBJECT ENCODING / IDLETIME / FREQ", sub)
	}
	if len(c.Args) != 3 {
		return uerr("OBJECT %s 需要一个 key", sub)
	}
	c.Args[1] = sub
	c.Name = "OBJECT " + sub
	return nil
}

func checkMemory(c *UserCommand) error {
	if len(c.Args) < 2 {
		return uerr("MEMORY 需要子命令，在线查询只支持 MEMORY USAGE")
	}
	if sub := strings.ToUpper(c.Args[1]); sub != "USAGE" {
		return uerr("MEMORY %s 不在白名单内，在线查询只支持 MEMORY USAGE key [SAMPLES n]", sub)
	}
	c.Args[1] = "USAGE"
	c.Name = "MEMORY USAGE"
	switch len(c.Args) {
	case 3:
	case 5:
		if !strings.EqualFold(c.Args[3], "SAMPLES") {
			return uerr("MEMORY USAGE 的第 3 个参数只能是 SAMPLES")
		}
		if n, err := strconv.Atoi(c.Args[4]); err != nil || n < 0 || n > 100 {
			return uerr("MEMORY USAGE SAMPLES 需在 0 到 100 之间")
		}
	default:
		return uerr("MEMORY USAGE 的参数不合法")
	}
	return nil
}

func checkGeoDist(c *UserCommand) error {
	if err := c.need(3, 4); err != nil {
		return err
	}
	if len(c.Args) == 5 {
		switch strings.ToLower(c.Args[4]) {
		case "m", "km", "ft", "mi":
		default:
			return uerr("GEODIST 的单位只能是 m / km / ft / mi")
		}
	}
	return nil
}

// geoOpts 解析地理查询里共有的尾部选项，并给 COUNT 加上限（没有 COUNT 就补上）。
func (c *UserCommand) geoOpts(from int, spec map[string]int) error {
	pos, err := c.parseOpts(from, spec)
	if err != nil {
		return err
	}
	for _, f := range []string{"WITHCOORD", "WITHDIST", "WITHHASH"} {
		if _, ok := pos[f]; ok {
			c.Flags[f] = true
		}
	}
	// COUNT n [ANY]：ANY 紧跟在 n 后面，parseOpts 把它当成无参选项，同样放行
	return c.capCount(pos, "COUNT", UserRangeMax, UserRangeMax)
}

func checkGeoRadiusRO(c *UserCommand) error {
	if err := c.need(5, -1); err != nil {
		return err
	}
	return c.geoOpts(6, map[string]int{"WITHCOORD": 0, "WITHDIST": 0, "WITHHASH": 0, "COUNT": 1, "ANY": 0, "ASC": 0, "DESC": 0})
}

func checkGeoSearch(c *UserCommand) error {
	if err := c.need(3, -1); err != nil {
		return err
	}
	return c.geoOpts(2, map[string]int{"FROMMEMBER": 1, "FROMLONLAT": 2, "BYRADIUS": 2, "BYBOX": 3, "WITHCOORD": 0, "WITHDIST": 0, "WITHHASH": 0,
		"COUNT": 1, "ANY": 0, "ASC": 0, "DESC": 0})
}

// ---- 执行 ----

// UserResult 是一次受限执行的结果。
type UserResult struct {
	Reply     any
	Args      []string // 实际发送的命令（探测后可能又改写了区间）
	Notices   []string
	IndexBase int64
}

// DialDB 连接并 SELECT 到指定库（0 起），用于在线查询。
func (e *Engine) DialDB(ctx context.Context, db int) (*Client, error) {
	cfg := e.cfg
	cfg.DB = db
	return Dial(ctx, cfg)
}

// RunUser 先做 cmd 登记的探测（元素个数 / 字符串长度 / 负下标换算），再执行命令。命令必须来自 ParseUserCommand。
// 探测不通过返回 *UserError。
func (c *Client) RunUser(ctx context.Context, cmd *UserCommand) (*UserResult, error) {
	res := &UserResult{Args: append([]string(nil), cmd.Args...), Notices: append([]string(nil), cmd.Notices...), IndexBase: cmd.IndexBase}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, pr := range cmd.probes {
		switch pr.kind {
		case probeCard, probeStrlen:
			var total int64
			for _, k := range pr.keys {
				v, err := c.exec(ctx, []string{pr.lenCmd, k})
				if err != nil {
					return nil, err
				}
				total += asInt(v)
			}
			limit := int64(UserFullReadMax)
			what := "元素"
			if pr.kind == probeStrlen {
				limit, what = UserBitcountMaxBytes, "字节"
			}
			if total > limit {
				return nil, uerr("key 太大：共 %d 个%s，超过在线查询的全量读取阈值 %d；%s", total, what, limit, pr.hint)
			}
		case probeRange:
			v, err := c.exec(ctx, []string{pr.lenCmd, pr.keys[0]})
			if err != nil {
				return nil, err
			}
			n := asInt(v)
			s, _ := strconv.ParseInt(res.Args[pr.si], 10, 64)
			e, _ := strconv.ParseInt(res.Args[pr.ei], 10, 64)
			if s < 0 {
				s += n
			}
			if e < 0 {
				e += n
			}
			s = max(s, 0)
			if e >= n {
				e = n - 1
			}
			if n == 0 || s > e {
				break // 空区间，原样发送，服务端返回空
			}
			if e-s+1 > UserRangeMax {
				e = s + UserRangeMax - 1
				res.Notices = append(res.Notices, fmt.Sprintf("区间过大（共 %d 条），已收窄为 %d..%d（单次最多 %d 条，请分页）", n, s, e, UserRangeMax))
			}
			res.Args[pr.si], res.Args[pr.ei] = strconv.FormatInt(s, 10), strconv.FormatInt(e, 10)
			res.IndexBase = s
		}
	}
	reply, err := c.exec(ctx, res.Args)
	if err != nil {
		return nil, err
	}
	res.Reply = reply
	return res, nil
}

// ---- 批量命令（流水线，用于 key 浏览时一次取多个 TYPE） ----

// execMany 一次写出多条命令再依次读回复；服务端的错误回复作为 *ServerError 元素保留，不当作整体失败。
func (c *Client) execMany(ctx context.Context, cmds [][]string) ([]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil || c.broken {
		return nil, errors.New("Redis 连接已关闭")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(c.cfg.CmdTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = c.conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = c.conn.SetDeadline(time.Now()) })
	defer stop()
	for _, a := range cmds {
		if err := writeCommand(c.w, a); err != nil {
			c.broken = true
			return nil, c.wrap(ctx, err)
		}
	}
	out := make([]any, 0, len(cmds))
	for range cmds {
		v, err := readValue(c.r, 0)
		if err != nil {
			c.broken = true
			return nil, c.wrap(ctx, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// ---- key 浏览 ----

// KeyEntry 是 key 浏览里的一项。
type KeyEntry struct {
	Key  string
	Type string
}

// KeyPage 是一页 key 浏览结果。
type KeyPage struct {
	Keys    []KeyEntry
	Cursor  string // 下一页游标，"0" 表示已扫完
	Scanned int    // 本次扫描的 SCAN 批数
}

const (
	browseMaxRounds = 60  // 单次最多发起多少轮 SCAN
	browseBatch     = 500 // 每轮 SCAN 的 COUNT
	browseMaxWant   = 500
)

// BrowseKeys 用 SCAN 从 cursor 起收集至少 want 个匹配 match 的 key（单页最多约 want+500 个），再批量取类型。
// 每轮 SCAN 的 COUNT 固定不超过 500、总轮数有上限，所以对大库也只做有限的工作；扫不满时返回游标让调用方继续。
func (c *Client) BrowseKeys(ctx context.Context, match, cursor string, want int) (*KeyPage, error) {
	if want < 1 {
		want = 100
	}
	want = min(want, browseMaxWant)
	if match == "" {
		match = "*"
	}
	if len(match) > 512 {
		return nil, uerr("匹配模式过长（最多 512 字节）")
	}
	if cursor == "" {
		cursor = "0"
	}
	if _, err := strconv.ParseUint(cursor, 10, 64); err != nil {
		return nil, uerr("游标必须是非负整数")
	}
	page := &KeyPage{Cursor: cursor}
	for round := 0; round < browseMaxRounds && len(page.Keys) < want; round++ {
		args := []string{"SCAN", page.Cursor, "COUNT", strconv.Itoa(min(browseBatch, max(want, 50)))}
		if match != "*" {
			args = append(args, "MATCH", match)
		}
		v, err := c.exec(ctx, args)
		if err != nil {
			return nil, err
		}
		arr, ok := v.([]any)
		if !ok || len(arr) != 2 {
			return nil, errors.New("SCAN 回复格式不对")
		}
		page.Scanned++
		page.Cursor = asString(arr[0])
		if ks, ok := arr[1].([]any); ok {
			for _, k := range ks {
				page.Keys = append(page.Keys, KeyEntry{Key: asString(k)})
			}
		}
		if page.Cursor == "0" {
			break
		}
	}
	if len(page.Keys) > 0 {
		cmds := make([][]string, len(page.Keys))
		for i, k := range page.Keys {
			cmds[i] = []string{"TYPE", k.Key}
		}
		for off := 0; off < len(cmds); off += 200 {
			end := min(off+200, len(cmds))
			replies, err := c.execMany(ctx, cmds[off:end])
			if err != nil {
				return nil, err
			}
			for i, r := range replies {
				if s, ok := r.(string); ok {
					page.Keys[off+i].Type = s
				}
			}
		}
	}
	return page, nil
}

// ---- 库列表 ----

// DBInfo 是 db 编号及其键数（Keys=-1 表示取不到）。
type DBInfo struct {
	DB   int
	Keys int64
}

// DatabaseList 返回 0..databases-1：databases 取 CONFIG GET databases，失败回落 16；键数取 INFO keyspace，取不到为 -1。
func (e *Engine) DatabaseList(ctx context.Context) ([]DBInfo, error) {
	c, err := e.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	n := 16
	if cfg, err := c.ConfigGet(ctx, "databases"); err == nil {
		if v, _ := strconv.Atoi(cfg["databases"]); v > 0 && v <= 1024 {
			n = v
		}
	}
	keys := map[int]int64{}
	known := false
	if info, err := c.Info(ctx, "keyspace"); err == nil {
		known = true
		for k, v := range info {
			if !strings.HasPrefix(k, "db") {
				continue
			}
			idx, err := strconv.Atoi(k[2:])
			if err != nil {
				continue
			}
			for _, kv := range strings.Split(v, ",") {
				if a, b, ok := strings.Cut(kv, "="); ok && a == "keys" {
					keys[idx], _ = strconv.ParseInt(b, 10, 64)
				}
			}
		}
	}
	out := make([]DBInfo, n)
	for i := range out {
		out[i] = DBInfo{DB: i, Keys: -1}
		if known {
			out[i].Keys = keys[i] // INFO keyspace 不列出空库，缺省按 0
		}
	}
	return out, nil
}
