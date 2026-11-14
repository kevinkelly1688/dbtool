// @author: kevinkelly
// @license: Apache Licence
// @file: cmdkeys.go
// @time: 2026/10/28

package redis

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// 写命令的静态知识：硬禁止清单、命令 → 键位置表、命令行分词。
//
// 这些是 Redis 协议本身的事实，与“管理员定义的规则”（internal/redisrule）无关：
//   - 硬禁止清单写死在代码里，任何规则都无法放行；
//   - 键位置表既用来把命令里的“键参数”取出来去匹配规则，也是“支持哪些写命令”的唯一清单——表里没有的命令（含所有无键命令）
//     一律不可能被规则放行。
// 只读白名单（whitelist.go）与这里互不影响。

// hardForbidden 是任何规则都不能放行的命令（大写）。
var hardForbidden = map[string]bool{
	"FLUSHALL": true, "FLUSHDB": true, "SHUTDOWN": true, "CONFIG": true, "DEBUG": true, "KEYS": true,
	"SCRIPT": true, "EVAL": true, "EVALSHA": true, "EVAL_RO": true, "EVALSHA_RO": true, "FCALL": true, "FCALL_RO": true, "FUNCTION": true,
	"MODULE": true, "SLAVEOF": true, "REPLICAOF": true, "ACL": true, "CLIENT": true, "MONITOR": true,
	"SAVE": true, "BGSAVE": true, "BGREWRITEAOF": true, "MIGRATE": true, "SWAPDB": true, "SELECT": true, "AUTH": true,
	"WAIT": true, "WAITAOF": true, "FAILOVER": true, "PSYNC": true, "SYNC": true,
	"SUBSCRIBE": true, "PSUBSCRIBE": true, "SSUBSCRIBE": true, "UNSUBSCRIBE": true, "PUNSUBSCRIBE": true, "SUNSUBSCRIBE": true,
	"MULTI": true, "EXEC": true, "DISCARD": true, "WATCH": true, "UNWATCH": true, "RESET": true, "HELLO": true, "QUIT": true,
	"MOVE": true, "BLPOP": true, "BRPOP": true, "BLMOVE": true, "BRPOPLPUSH": true, "BZPOPMIN": true, "BZPOPMAX": true,
	"BLMPOP": true, "BZMPOP": true, "XREAD": true, "XREADGROUP": true,
}

// HardForbiddenList 返回硬禁止命令清单（排序，供页面与文档展示）。SORT 在带 STORE 时也属于硬禁止，另见 IsHardForbidden。
func HardForbiddenList() []string {
	out := make([]string, 0, len(hardForbidden)+1)
	for k := range hardForbidden {
		out = append(out, k)
	}
	out = append(out, "SORT … STORE")
	sortStrings(out)
	return out
}

func sortStrings(a []string) { sort.Strings(a) }

// IsHardForbidden 判断命令是否属于硬禁止（args[0] 是命令名，大小写不敏感）。SORT 只有带 STORE 才算。
func IsHardForbidden(args []string) bool {
	if len(args) == 0 {
		return false
	}
	name := strings.ToUpper(args[0])
	if hardForbidden[name] {
		return true
	}
	if name == "SORT" || name == "SORT_RO" {
		for _, a := range args[1:] {
			if strings.EqualFold(a, "STORE") {
				return true
			}
		}
	}
	return false
}

// IsHardForbiddenName 只按命令名判断（规则编辑器选命令时用）。
func IsHardForbiddenName(name string) bool {
	return hardForbidden[strings.ToUpper(strings.TrimSpace(name))]
}

// CmdInfo 描述一个受支持的写命令。
type CmdInfo struct {
	Name    string
	MinArgs int // 含命令名本身
	MaxArgs int // -1 不限
	// 键位置（下标从 0 起，args[0] 是命令名）：First..Last 每隔 Step 个一个键；Last < 0 表示从末尾数起（-1 = 最后一个参数）。
	First, Last, Step int
	// Validate 做命令专有的语法校验（可为 nil）。
	Validate func(args []string) error
	// Multi 为真表示一条命令可能涉及多个键（DEL、MSET…），规则里的“键数上限”主要约束它们。
	Multi bool
}

// Keys 取出命令里的键参数（调用前应已通过 Validate 与参数个数检查）。
func (ci *CmdInfo) Keys(args []string) []string {
	last := ci.Last
	if last < 0 {
		last = len(args) + last
	}
	step := ci.Step
	if step < 1 {
		step = 1
	}
	var out []string
	for i := ci.First; i <= last && i < len(args); i += step {
		out = append(out, args[i])
	}
	return out
}

var cmdTable = map[string]*CmdInfo{}

func reg(name string, min, max, first, last, step int, multi bool, v func([]string) error) {
	cmdTable[name] = &CmdInfo{Name: name, MinArgs: min, MaxArgs: max, First: first, Last: last, Step: step, Multi: multi, Validate: v}
}

func one(name string, min, max int, v func([]string) error) { reg(name, min, max, 1, 1, 1, false, v) }

func even(from int) func([]string) error {
	return func(a []string) error {
		if (len(a)-from)%2 != 0 {
			return fmt.Errorf("%s 的参数个数不对（需要成对的参数）", strings.ToUpper(a[0]))
		}
		return nil
	}
}

func atoiArg(a []string, i int, what string) error {
	if _, err := strconv.ParseInt(a[i], 10, 64); err != nil {
		return fmt.Errorf("%s 需要整数", what)
	}
	return nil
}

func init() {
	// 字符串
	one("SET", 3, -1, validateSet)
	one("SETEX", 4, 4, func(a []string) error { return atoiArg(a, 2, "SETEX 的过期秒数") })
	one("PSETEX", 4, 4, func(a []string) error { return atoiArg(a, 2, "PSETEX 的过期毫秒数") })
	one("SETNX", 3, 3, nil)
	one("GETSET", 3, 3, nil)
	one("GETDEL", 2, 2, nil)
	one("GETEX", 2, -1, nil)
	one("APPEND", 3, 3, nil)
	one("INCR", 2, 2, nil)
	one("DECR", 2, 2, nil)
	one("INCRBY", 3, 3, nil)
	one("DECRBY", 3, 3, nil)
	one("INCRBYFLOAT", 3, 3, nil)
	one("SETRANGE", 4, 4, nil)
	one("SETBIT", 4, 4, nil)
	one("PFADD", 2, -1, nil)
	// 过期
	one("EXPIRE", 3, 4, func(a []string) error { return atoiArg(a, 2, "EXPIRE 的秒数") })
	one("PEXPIRE", 3, 4, func(a []string) error { return atoiArg(a, 2, "PEXPIRE 的毫秒数") })
	one("EXPIREAT", 3, 4, func(a []string) error { return atoiArg(a, 2, "EXPIREAT 的时间戳") })
	one("PEXPIREAT", 3, 4, func(a []string) error { return atoiArg(a, 2, "PEXPIREAT 的时间戳") })
	one("PERSIST", 2, 2, nil)
	// 哈希
	one("HSET", 4, -1, even(2))
	one("HMSET", 4, -1, even(2))
	one("HSETNX", 4, 4, nil)
	one("HDEL", 3, -1, nil)
	one("HINCRBY", 4, 4, nil)
	one("HINCRBYFLOAT", 4, 4, nil)
	// 列表
	for _, n := range []string{"LPUSH", "RPUSH", "LPUSHX", "RPUSHX"} {
		one(n, 3, -1, nil)
	}
	one("LPOP", 2, 3, nil)
	one("RPOP", 2, 3, nil)
	one("LSET", 4, 4, nil)
	one("LREM", 4, 4, nil)
	one("LTRIM", 4, 4, nil)
	one("LINSERT", 5, 5, nil)
	// 集合
	one("SADD", 3, -1, nil)
	one("SREM", 3, -1, nil)
	one("SPOP", 2, 3, nil)
	// 有序集合
	one("ZADD", 4, -1, nil)
	one("ZREM", 3, -1, nil)
	one("ZINCRBY", 4, 4, nil)
	one("ZREMRANGEBYRANK", 4, 4, nil)
	one("ZREMRANGEBYSCORE", 4, 4, nil)
	one("ZREMRANGEBYLEX", 4, 4, nil)
	one("ZPOPMIN", 2, 3, nil)
	one("ZPOPMAX", 2, 3, nil)
	// 流
	one("XADD", 5, -1, nil)
	one("XDEL", 3, -1, nil)
	one("XTRIM", 4, -1, nil)
	// 多键
	reg("DEL", 2, -1, 1, -1, 1, true, nil)
	reg("UNLINK", 2, -1, 1, -1, 1, true, nil)
	reg("TOUCH", 2, -1, 1, -1, 1, true, nil)
	reg("MSET", 3, -1, 1, -1, 2, true, even(1))
	reg("MSETNX", 3, -1, 1, -1, 2, true, even(1))
	reg("PFMERGE", 2, -1, 1, -1, 1, true, nil)
	reg("SINTERSTORE", 3, -1, 1, -1, 1, true, nil)
	reg("SUNIONSTORE", 3, -1, 1, -1, 1, true, nil)
	reg("SDIFFSTORE", 3, -1, 1, -1, 1, true, nil)
	reg("RENAME", 3, 3, 1, 2, 1, true, nil)
	reg("RENAMENX", 3, 3, 1, 2, 1, true, nil)
	reg("COPY", 3, -1, 1, 2, 1, true, validateCopy)
	reg("SMOVE", 4, 4, 1, 2, 1, true, nil)
	reg("LMOVE", 5, 5, 1, 2, 1, true, nil)
	reg("RPOPLPUSH", 3, 3, 1, 2, 1, true, nil)
	reg("BITOP", 4, -1, 2, -1, 1, true, func(a []string) error {
		switch strings.ToUpper(a[1]) {
		case "AND", "OR", "XOR", "NOT":
			return nil
		}
		return errors.New("BITOP 的操作必须是 AND / OR / XOR / NOT")
	})
}

// validateSet 校验 SET key value [NX|XX] [GET] [EX s|PX ms|EXAT t|PXAT t|KEEPTTL]。
func validateSet(a []string) error {
	ttl := false
	for i := 3; i < len(a); i++ {
		switch strings.ToUpper(a[i]) {
		case "NX", "XX", "GET":
		case "KEEPTTL":
			if ttl {
				return errors.New("SET 的过期选项只能出现一个")
			}
			ttl = true
		case "EX", "PX", "EXAT", "PXAT":
			if ttl {
				return errors.New("SET 的过期选项只能出现一个")
			}
			ttl = true
			if i+1 >= len(a) {
				return fmt.Errorf("SET %s 缺少数值", strings.ToUpper(a[i]))
			}
			if n, err := strconv.ParseInt(a[i+1], 10, 64); err != nil || n <= 0 {
				return fmt.Errorf("SET %s 需要正整数", strings.ToUpper(a[i]))
			}
			i++
		default:
			return fmt.Errorf("SET 不支持的选项 %q", a[i])
		}
	}
	return nil
}

// SetHasTTL 判断一条 SET 是否带过期（EX/PX/EXAT/PXAT/KEEPTTL）。
func SetHasTTL(a []string) bool {
	for i := 3; i < len(a); i++ {
		switch strings.ToUpper(a[i]) {
		case "EX", "PX", "EXAT", "PXAT", "KEEPTTL":
			return true
		}
	}
	return false
}

// validateCopy 拒绝 COPY … DB n（会写到别的库，绕开“库”的规则范围）。
func validateCopy(a []string) error {
	for _, x := range a[3:] {
		switch strings.ToUpper(x) {
		case "DB":
			return errors.New("COPY 不支持 DB 选项（不能跨库复制）")
		case "REPLACE":
		default:
			if _, err := strconv.Atoi(x); err != nil {
				return fmt.Errorf("COPY 不支持的选项 %q", x)
			}
		}
	}
	return nil
}

// LookupCmd 返回受支持的写命令信息；不在表里（含无键命令、未知命令）返回 nil。
func LookupCmd(name string) *CmdInfo { return cmdTable[strings.ToUpper(strings.TrimSpace(name))] }

// SupportedCmds 返回受支持的写命令名（排序）。
func SupportedCmds() []string {
	out := make([]string, 0, len(cmdTable))
	for k := range cmdTable {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// 参数规模上限：单条命令全部参数的总字节数，防止把整块数据塞进一个工单。
const MaxCommandBytes = 1 << 20

// Parsed 是一条经过静态检查的命令：命令名、支持信息与键参数。
type Parsed struct {
	Args  []string
	Name  string // 大写命令名
	Info  *CmdInfo
	Keys  []string
	Multi bool
}

// ErrHardForbidden 表示命中硬禁止命令。
type ErrHardForbidden struct{ Cmd string }

func (e *ErrHardForbidden) Error() string {
	return fmt.Sprintf("命令 %s 属于硬禁止命令，任何规则都无法放行", e.Cmd)
}

// ErrUnsupported 表示命令不在受支持的写命令清单里（含无键命令、未知命令）。
type ErrUnsupported struct{ Cmd string }

func (e *ErrUnsupported) Error() string {
	return fmt.Sprintf("命令 %s 不在受支持的写命令清单里（无键命令、读命令与未知命令不能通过工单执行）", e.Cmd)
}

// ParseCommand 对一条已分词的命令做静态检查（不涉及管理员规则）：非空、参数规模、硬禁止、受支持、参数个数与专有语法，并取出键参数。
// 错误类型：*ErrHardForbidden / *ErrUnsupported / 其余为语法错误。
func ParseCommand(args []string) (*Parsed, error) {
	if len(args) == 0 {
		return nil, errors.New("命令为空")
	}
	total := 0
	for _, a := range args {
		total += len(a)
	}
	if total > MaxCommandBytes {
		return nil, fmt.Errorf("命令过大（%d 字节，上限 %d）", total, MaxCommandBytes)
	}
	name := strings.ToUpper(args[0])
	if IsHardForbidden(args) {
		return nil, &ErrHardForbidden{Cmd: name}
	}
	info := cmdTable[name]
	if info == nil {
		return nil, &ErrUnsupported{Cmd: name}
	}
	if len(args) < info.MinArgs || (info.MaxArgs >= 0 && len(args) > info.MaxArgs) {
		return nil, fmt.Errorf("命令 %s 的参数个数不对", name)
	}
	if info.Validate != nil {
		if err := info.Validate(args); err != nil {
			return nil, err
		}
	}
	keys := info.Keys(args)
	if len(keys) == 0 {
		return nil, &ErrUnsupported{Cmd: name}
	}
	return &Parsed{Args: args, Name: name, Info: info, Keys: keys, Multi: info.Multi}, nil
}

// TokenizeLine 按 redis-cli 的规则把一行命令拆成参数（对应 sdssplitargs）：
//   - 空白分隔；
//   - 双引号内支持转义 \n \r \t \b \a \xHH 与 \"、\\ 等（其余 \c 取 c）；
//   - 单引号内只支持 \' 转义；
//   - 引号关闭后必须紧跟空白或行尾；引号没有关闭报错。
//
// 结果按字节处理（\xHH 可产生任意字节），所以返回的字符串可能不是合法 UTF-8。空行返回 (nil, nil)。
func TokenizeLine(line string) ([]string, error) {
	var out []string
	b := []byte(line)
	i, n := 0, len(b)
	for {
		for i < n && isSpace(b[i]) {
			i++
		}
		if i >= n {
			return out, nil
		}
		var cur []byte
		inDouble, inSingle, done := false, false, false
		for !done {
			switch {
			case inDouble:
				if i >= n {
					return nil, errors.New("双引号没有闭合")
				}
				switch {
				case b[i] == '\\' && i+3 < n && b[i+1] == 'x' && isHex(b[i+2]) && isHex(b[i+3]):
					cur = append(cur, hexVal(b[i+2])<<4|hexVal(b[i+3]))
					i += 3
				case b[i] == '\\' && i+1 < n:
					i++
					switch b[i] {
					case 'n':
						cur = append(cur, '\n')
					case 'r':
						cur = append(cur, '\r')
					case 't':
						cur = append(cur, '\t')
					case 'b':
						cur = append(cur, '\b')
					case 'a':
						cur = append(cur, 7)
					default:
						cur = append(cur, b[i])
					}
				case b[i] == '"':
					if i+1 < n && !isSpace(b[i+1]) {
						return nil, errors.New("右引号后面必须是空白或行尾")
					}
					done = true
				default:
					cur = append(cur, b[i])
				}
			case inSingle:
				if i >= n {
					return nil, errors.New("单引号没有闭合")
				}
				switch {
				case b[i] == '\\' && i+1 < n && b[i+1] == '\'':
					i++
					cur = append(cur, '\'')
				case b[i] == '\'':
					if i+1 < n && !isSpace(b[i+1]) {
						return nil, errors.New("右引号后面必须是空白或行尾")
					}
					done = true
				default:
					cur = append(cur, b[i])
				}
			default:
				if i >= n {
					done = true
					break
				}
				switch b[i] {
				case ' ', '\n', '\r', '\t', 0:
					done = true
				case '"':
					inDouble = true
				case '\'':
					inSingle = true
				default:
					cur = append(cur, b[i])
				}
			}
			if i < n {
				i++
			}
		}
		out = append(out, string(cur))
	}
}

func isSpace(c byte) bool { return c == ' ' || c == '\n' || c == '\r' || c == '\t' || c == 0 }
func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
func hexVal(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}
