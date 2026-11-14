// @author: kevinkelly
// @license: Apache Licence
// @file: whitelist.go
// @time: 2026/10/28

package redis

import (
	"fmt"
	"strconv"
	"strings"
)

// 命令白名单：平台只读采集，任何不在白名单里的命令都在发出前就被拒绝，不会到达服务端。
//
// 规则：
//   - 命令名（及子命令）不区分大小写；
//   - 只允许只读命令：INFO / CONFIG GET / SLOWLOG GET|LEN / DBSIZE / SCAN / TYPE / MEMORY USAGE / CLIENT LIST / LATENCY / ROLE ……
//   - FLUSHALL、FLUSHDB、CONFIG SET、DEBUG、KEYS、SHUTDOWN、SAVE、EVAL 等一律拒绝（KEYS 会在大库上长时间阻塞，用 SCAN 代替）；
//   - SLOWLOG RESET 不在白名单里，只能走 Client.SlowlogReset（调用方负责权限与审计）。

// ErrNotAllowed 表示命令不在只读白名单里。
type ErrNotAllowed struct{ Cmd string }

func (e *ErrNotAllowed) Error() string {
	return fmt.Sprintf("命令 %s 不在只读白名单内，已拒绝执行", e.Cmd)
}

// 无子命令的只读命令，及其允许的最多参数个数（不含命令名）。-1 表示不限制（由专门的校验函数处理）。
var simpleReadOnly = map[string]int{
	"PING":     1,
	"DBSIZE":   0,
	"ROLE":     0,
	"TIME":     0,
	"LASTSAVE": 0,
	"TYPE":     1,
	"TTL":      1,
	"PTTL":     1,
	"STRLEN":   1,
	"LLEN":     1,
	"HLEN":     1,
	"SCARD":    1,
	"ZCARD":    1,
	"XLEN":     1,
	"SELECT":   1,
	"INFO":     1,
}

// 允许的 INFO 段。
var infoSections = map[string]bool{
	"server": true, "clients": true, "memory": true, "persistence": true, "stats": true, "replication": true, "cpu": true,
	"modules": true, "errorstats": true, "cluster": true, "keyspace": true, "commandstats": true, "latencystats": true,
	"default": true, "all": true, "everything": true,
}

// 子命令白名单：命令 → 允许的子命令。
var subReadOnly = map[string]map[string]bool{
	"CONFIG":  {"GET": true},
	"SLOWLOG": {"GET": true, "LEN": true},
	"MEMORY":  {"USAGE": true, "STATS": true, "DOCTOR": true},
	"CLIENT":  {"LIST": true, "GETNAME": true, "INFO": true},
	"LATENCY": {"LATEST": true, "HISTORY": true, "DOCTOR": true},
	"COMMAND": {"INFO": true, "COUNT": true},
	"OBJECT":  {"ENCODING": true, "IDLETIME": true, "FREQ": true},
	"CLUSTER": {"INFO": true, "NODES": true},
}

const (
	maxScanCount     = 1000  // SCAN 单次 COUNT 上限
	maxSlowlogGet    = 10000 // SLOWLOG GET 单次条数上限
	maxCommandInfoOf = 16    // COMMAND INFO 一次最多查多少个命令
)

// Check 校验一条命令是否允许发送。args[0] 是命令名。
func Check(args []string) error {
	if len(args) == 0 {
		return &ErrNotAllowed{Cmd: "(空命令)"}
	}
	for _, a := range args {
		if strings.ContainsRune(a, 0) {
			return fmt.Errorf("命令参数含非法字符")
		}
	}
	name := strings.ToUpper(args[0])
	if maxArgs, ok := simpleReadOnly[name]; ok {
		if len(args)-1 > maxArgs {
			return fmt.Errorf("命令 %s 的参数过多", name)
		}
		switch name {
		case "INFO":
			if len(args) == 2 && !infoSections[strings.ToLower(args[1])] {
				return fmt.Errorf("INFO 不支持的段 %q", args[1])
			}
		case "SELECT":
			if len(args) != 2 {
				return fmt.Errorf("SELECT 需要库编号")
			}
			if n, err := strconv.Atoi(args[1]); err != nil || n < 0 || n > 1023 {
				return fmt.Errorf("SELECT 的库编号不合法")
			}
		case "TYPE", "TTL", "PTTL", "STRLEN", "LLEN", "HLEN", "SCARD", "ZCARD", "XLEN":
			if len(args) != 2 {
				return fmt.Errorf("命令 %s 需要一个 key", name)
			}
		}
		return nil
	}
	switch name {
	case "SCAN":
		return checkScan(args)
	case "KEYS", "FLUSHALL", "FLUSHDB", "DEBUG", "SHUTDOWN", "SAVE", "BGSAVE", "BGREWRITEAOF", "REPLICAOF", "SLAVEOF",
		"MIGRATE", "EVAL", "EVALSHA", "SCRIPT", "FUNCTION", "MODULE", "ACL", "MONITOR", "SYNC", "PSYNC", "DEL", "UNLINK",
		"SET", "GET", "MGET", "HGETALL", "SMEMBERS", "LRANGE", "RENAME", "EXPIRE", "AUTH", "FAILOVER", "RESET":
		// 点名拒绝：GET / HGETALL 等读数据的命令也不放行，巡检只看元数据，不读业务数据
		return &ErrNotAllowed{Cmd: name}
	}
	subs, ok := subReadOnly[name]
	if !ok {
		return &ErrNotAllowed{Cmd: name}
	}
	if len(args) < 2 {
		return &ErrNotAllowed{Cmd: name}
	}
	sub := strings.ToUpper(args[1])
	if !subs[sub] {
		return &ErrNotAllowed{Cmd: name + " " + sub}
	}
	switch name + " " + sub {
	case "CONFIG GET":
		if len(args) < 3 || len(args) > 6 {
			return fmt.Errorf("CONFIG GET 需要 1 到 4 个参数名")
		}
	case "SLOWLOG GET":
		if len(args) > 3 {
			return fmt.Errorf("SLOWLOG GET 参数过多")
		}
		if len(args) == 3 {
			n, err := strconv.Atoi(args[2])
			if err != nil || n < -1 || n > maxSlowlogGet {
				return fmt.Errorf("SLOWLOG GET 的条数需在 1 到 %d 之间", maxSlowlogGet)
			}
		}
	case "SLOWLOG LEN":
		if len(args) != 2 {
			return fmt.Errorf("SLOWLOG LEN 不带参数")
		}
	case "MEMORY USAGE":
		if len(args) != 3 && !(len(args) == 5 && strings.EqualFold(args[3], "SAMPLES")) {
			return fmt.Errorf("MEMORY USAGE 的参数不合法")
		}
		if len(args) == 5 {
			if n, err := strconv.Atoi(args[4]); err != nil || n < 0 || n > 100 {
				return fmt.Errorf("MEMORY USAGE SAMPLES 需在 0 到 100 之间")
			}
		}
	case "CLIENT LIST":
		// CLIENT LIST [TYPE normal|master|replica|pubsub] [ID ...]：只放行无参和 TYPE
		if len(args) != 2 && !(len(args) == 4 && strings.EqualFold(args[2], "TYPE")) {
			return fmt.Errorf("CLIENT LIST 的参数不合法")
		}
	case "COMMAND INFO":
		if len(args) < 3 || len(args)-2 > maxCommandInfoOf {
			return fmt.Errorf("COMMAND INFO 需要 1 到 %d 个命令名", maxCommandInfoOf)
		}
	case "OBJECT ENCODING", "OBJECT IDLETIME", "OBJECT FREQ":
		if len(args) != 3 {
			return fmt.Errorf("OBJECT %s 需要一个 key", sub)
		}
	case "LATENCY HISTORY":
		if len(args) != 3 {
			return fmt.Errorf("LATENCY HISTORY 需要事件名")
		}
	}
	return nil
}

func checkScan(args []string) error {
	// SCAN cursor [MATCH pattern] [COUNT n] [TYPE t]
	if len(args) < 2 {
		return fmt.Errorf("SCAN 需要游标")
	}
	if _, err := strconv.ParseUint(args[1], 10, 64); err != nil {
		return fmt.Errorf("SCAN 游标不合法")
	}
	i := 2
	for i < len(args) {
		opt := strings.ToUpper(args[i])
		if i+1 >= len(args) {
			return fmt.Errorf("SCAN 选项 %s 缺少值", opt)
		}
		switch opt {
		case "MATCH", "TYPE":
		case "COUNT":
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 1 || n > maxScanCount {
				return fmt.Errorf("SCAN COUNT 需在 1 到 %d 之间", maxScanCount)
			}
		default:
			return fmt.Errorf("SCAN 不支持选项 %s", opt)
		}
		i += 2
	}
	return nil
}
