// @author: kevinkelly
// @license: Apache Licence
// @file: kv.go
// @time: 2026/10/28

package redistest

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// KV 是一个带数据的假 Redis：支持工单上线用到的常见写命令和少量读命令（TYPE / GET / HGET / HMGET / STRLEN / PTTL / HLEN / LLEN …）。
// 数据按库分开；因为 Server 的 Handler 看不到连接，“当前库”是整个服务端共用的最近一次 SELECT（测试里串行使用即可）。
// 不是 Redis 的完整实现，只覆盖测试需要的语义。
type KV struct {
	mu      sync.Mutex
	db      int
	data    map[int]map[string]*entry
	Fail    map[string]string // 命令名（大写）→ 返回的错误文本，用来模拟某条命令失败
	AuthErr string            // 非空时 AUTH 返回该错误
}

type entry struct {
	typ  string // string / hash / list / set / zset
	str  string
	hash map[string]string
	list []string
	set  map[string]bool
	ttl  int64 // 毫秒，-1 表示无
}

// NewKV 创建空数据的假 Redis 处理器。
func NewKV() *KV { return &KV{data: map[int]map[string]*entry{}, Fail: map[string]string{}} }

// Serve 在本机随机端口启动服务端。
func (k *KV) Serve() (*Server, error) { return New(k.Handle) }

func (k *KV) store() map[string]*entry {
	m := k.data[k.db]
	if m == nil {
		m = map[string]*entry{}
		k.data[k.db] = m
	}
	return m
}

// SetString / SetHash 预置数据（在 db 库里）。
func (k *KV) SetString(db int, key, val string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	save := k.db
	k.db = db
	k.store()[key] = &entry{typ: "string", str: val, ttl: -1}
	k.db = save
}

func (k *KV) SetHash(db int, key string, h map[string]string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	save := k.db
	k.db = db
	k.store()[key] = &entry{typ: "hash", hash: h, ttl: -1}
	k.db = save
}

// Get 读一个字符串键（测试断言用）。
func (k *KV) Get(db int, key string) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	e := k.data[db][key]
	if e == nil || e.typ != "string" {
		return "", false
	}
	return e.str, true
}

// Exists 判断键是否存在。
func (k *KV) Exists(db int, key string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.data[db][key] != nil
}

// Keys 返回库里全部键（排序）。
func (k *KV) Keys(db int) []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []string
	for key := range k.data[db] {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// TTL 返回键的剩余毫秒（-1 无过期，-2 不存在）。
func (k *KV) TTL(db int, key string) int64 {
	k.mu.Lock()
	defer k.mu.Unlock()
	e := k.data[db][key]
	if e == nil {
		return -2
	}
	return e.ttl
}

func wrongType() Err { return Err("WRONGTYPE Operation against a key holding the wrong kind of value") }

// Handle 是 Handler。
func (k *KV) Handle(args []string) any {
	k.mu.Lock()
	defer k.mu.Unlock()
	name := strings.ToUpper(args[0])
	if msg, ok := k.Fail[name]; ok {
		return Err(msg)
	}
	st := func() map[string]*entry { return k.store() }
	switch name {
	case "AUTH":
		if k.AuthErr != "" {
			return Err(k.AuthErr)
		}
		return Simple("OK")
	case "PING":
		return Simple("PONG")
	case "CONFIG":
		if len(args) >= 3 && strings.EqualFold(args[1], "GET") && strings.EqualFold(args[2], "databases") {
			return []any{"databases", "16"}
		}
		return []any{}
	case "INFO":
		return "# Keyspace\r\n"
	case "SELECT":
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 0 || n >= 16 {
			return Err("ERR DB index is out of range")
		}
		k.db = n
		return Simple("OK")
	case "TYPE":
		if e := st()[args[1]]; e != nil {
			return Simple(e.typ)
		}
		return Simple("none")
	case "PTTL":
		e := st()[args[1]]
		if e == nil {
			return int64(-2)
		}
		return e.ttl
	case "STRLEN":
		e := st()[args[1]]
		if e == nil {
			return int64(0)
		}
		if e.typ != "string" {
			return wrongType()
		}
		return int64(len(e.str))
	case "GET":
		e := st()[args[1]]
		if e == nil {
			return nil
		}
		if e.typ != "string" {
			return wrongType()
		}
		return e.str
	case "HLEN":
		if e := st()[args[1]]; e != nil && e.typ == "hash" {
			return int64(len(e.hash))
		}
		return int64(0)
	case "LLEN":
		if e := st()[args[1]]; e != nil && e.typ == "list" {
			return int64(len(e.list))
		}
		return int64(0)
	case "SCARD":
		if e := st()[args[1]]; e != nil && e.typ == "set" {
			return int64(len(e.set))
		}
		return int64(0)
	case "HGET":
		e := st()[args[1]]
		if e == nil || e.typ != "hash" {
			return nil
		}
		if v, ok := e.hash[args[2]]; ok {
			return v
		}
		return nil
	case "HMGET":
		e := st()[args[1]]
		out := make([]any, 0, len(args)-2)
		for _, f := range args[2:] {
			if e != nil && e.typ == "hash" {
				if v, ok := e.hash[f]; ok {
					out = append(out, v)
					continue
				}
			}
			out = append(out, nil)
		}
		return out
	case "SET":
		return k.set(args)
	case "SETEX", "PSETEX":
		ms, _ := strconv.ParseInt(args[2], 10, 64)
		if name == "SETEX" {
			ms *= 1000
		}
		st()[args[1]] = &entry{typ: "string", str: args[3], ttl: ms}
		return Simple("OK")
	case "SETNX":
		if st()[args[1]] != nil {
			return int64(0)
		}
		st()[args[1]] = &entry{typ: "string", str: args[2], ttl: -1}
		return int64(1)
	case "MSET":
		for i := 1; i+1 < len(args); i += 2 {
			st()[args[i]] = &entry{typ: "string", str: args[i+1], ttl: -1}
		}
		return Simple("OK")
	case "APPEND":
		e := st()[args[1]]
		if e == nil {
			e = &entry{typ: "string", ttl: -1}
			st()[args[1]] = e
		}
		if e.typ != "string" {
			return wrongType()
		}
		e.str += args[2]
		return int64(len(e.str))
	case "INCR", "DECR", "INCRBY", "DECRBY":
		e := st()[args[1]]
		if e == nil {
			e = &entry{typ: "string", str: "0", ttl: -1}
			st()[args[1]] = e
		}
		if e.typ != "string" {
			return wrongType()
		}
		cur, err := strconv.ParseInt(e.str, 10, 64)
		if err != nil {
			return Err("ERR value is not an integer or out of range")
		}
		d := int64(1)
		if name == "INCRBY" || name == "DECRBY" {
			d, _ = strconv.ParseInt(args[2], 10, 64)
		}
		if name == "DECR" || name == "DECRBY" {
			d = -d
		}
		cur += d
		e.str = strconv.FormatInt(cur, 10)
		return cur
	case "DEL", "UNLINK":
		n := int64(0)
		for _, key := range args[1:] {
			if st()[key] != nil {
				delete(st(), key)
				n++
			}
		}
		return n
	case "EXPIRE", "PEXPIRE":
		e := st()[args[1]]
		if e == nil {
			return int64(0)
		}
		v, _ := strconv.ParseInt(args[2], 10, 64)
		if name == "EXPIRE" {
			v *= 1000
		}
		e.ttl = v
		return int64(1)
	case "PERSIST":
		if e := st()[args[1]]; e != nil && e.ttl >= 0 {
			e.ttl = -1
			return int64(1)
		}
		return int64(0)
	case "RENAME":
		e := st()[args[1]]
		if e == nil {
			return Err("ERR no such key")
		}
		delete(st(), args[1])
		st()[args[2]] = e
		return Simple("OK")
	case "HSET", "HMSET":
		e := st()[args[1]]
		if e == nil {
			e = &entry{typ: "hash", hash: map[string]string{}, ttl: -1}
			st()[args[1]] = e
		}
		if e.typ != "hash" {
			return wrongType()
		}
		added := int64(0)
		for i := 2; i+1 < len(args); i += 2 {
			if _, ok := e.hash[args[i]]; !ok {
				added++
			}
			e.hash[args[i]] = args[i+1]
		}
		if name == "HMSET" {
			return Simple("OK")
		}
		return added
	case "HDEL":
		e := st()[args[1]]
		if e == nil || e.typ != "hash" {
			return int64(0)
		}
		n := int64(0)
		for _, f := range args[2:] {
			if _, ok := e.hash[f]; ok {
				delete(e.hash, f)
				n++
			}
		}
		if len(e.hash) == 0 {
			delete(st(), args[1])
		}
		return n
	case "LPUSH", "RPUSH":
		e := st()[args[1]]
		if e == nil {
			e = &entry{typ: "list", ttl: -1}
			st()[args[1]] = e
		}
		if e.typ != "list" {
			return wrongType()
		}
		for _, v := range args[2:] {
			if name == "LPUSH" {
				e.list = append([]string{v}, e.list...)
			} else {
				e.list = append(e.list, v)
			}
		}
		return int64(len(e.list))
	case "SADD":
		e := st()[args[1]]
		if e == nil {
			e = &entry{typ: "set", set: map[string]bool{}, ttl: -1}
			st()[args[1]] = e
		}
		if e.typ != "set" {
			return wrongType()
		}
		n := int64(0)
		for _, v := range args[2:] {
			if !e.set[v] {
				e.set[v] = true
				n++
			}
		}
		return n
	}
	return Err(fmt.Sprintf("ERR unknown command '%s'", args[0]))
}

func (k *KV) set(args []string) any {
	key, val := args[1], args[2]
	ttl := int64(-1)
	nx, xx, get := false, false, false
	for i := 3; i < len(args); i++ {
		switch strings.ToUpper(args[i]) {
		case "NX":
			nx = true
		case "XX":
			xx = true
		case "GET":
			get = true
		case "KEEPTTL":
		case "EX", "PX":
			v, _ := strconv.ParseInt(args[i+1], 10, 64)
			if strings.EqualFold(args[i], "EX") {
				v *= 1000
			}
			ttl = v
			i++
		case "EXAT", "PXAT":
			i++
		}
	}
	old := k.store()[key]
	if (nx && old != nil) || (xx && old == nil) {
		if get && old != nil && old.typ == "string" {
			return old.str
		}
		return nil
	}
	k.store()[key] = &entry{typ: "string", str: val, ttl: ttl}
	if get {
		if old != nil && old.typ == "string" {
			return old.str
		}
		return nil
	}
	return Simple("OK")
}
