// @author: kevinkelly
// @license: Apache Licence
// @file: fingerprint.go
// @time: 2026/10/28

package redis

import (
	"strings"
)

// 带子命令的命令：指纹里保留命令名 + 子命令（CONFIG GET ?）。
var twoWord = map[string]bool{
	"CONFIG": true, "CLIENT": true, "SLOWLOG": true, "MEMORY": true, "XINFO": true, "XGROUP": true, "SCRIPT": true,
	"OBJECT": true, "CLUSTER": true, "COMMAND": true, "ACL": true, "FUNCTION": true, "PUBSUB": true, "LATENCY": true, "MODULE": true,
}

// 指纹里原样保留的选项关键字（其余参数——key 名、值、数字——都换成 ?）。
var keepWords = map[string]bool{
	"EX": true, "PX": true, "EXAT": true, "PXAT": true, "NX": true, "XX": true, "GT": true, "LT": true, "CH": true, "INCR": true,
	"KEEPTTL": true, "GET": true, "COUNT": true, "MATCH": true, "TYPE": true, "LIMIT": true, "WITHSCORES": true, "BYSCORE": true,
	"BYLEX": true, "REV": true, "LEFT": true, "RIGHT": true, "BEFORE": true, "AFTER": true, "MAXLEN": true, "MINID": true,
	"STORE": true, "STOREDIST": true, "ASC": true, "DESC": true, "ALPHA": true, "BY": true, "WEIGHTS": true, "AGGREGATE": true,
	"SUM": true, "MIN": true, "MAX": true, "WITHCOORD": true, "WITHDIST": true, "WITHHASH": true, "ASYNC": true, "SYNC": true,
	"REPLACE": true, "FROMMEMBER": true, "FROMLONLAT": true, "BYRADIUS": true, "BYBOX": true, "BLOCK": true, "STREAMS": true,
	"GROUP": true, "NOACK": true, "WITHVALUES": true, "ID": true, "SAMPLES": true,
}

// Fingerprint 把一条 Redis 命令归一成指纹：命令名（大写）+ 参数占位，如 `HGETALL user:1` → `HGETALL ?`。
// 3 个以上连续的占位符折叠成 `? ? ...`（MGET a b c d → MGET ? ? ...），这样参数个数不同的同类命令归为一类。
// 指纹里没有 key 名和值，可以安全地放进报告 / 汇总。
func Fingerprint(args []string) string {
	if len(args) == 0 {
		return ""
	}
	name := strings.ToUpper(args[0])
	rest := args[1:]
	if twoWord[name] && len(rest) > 0 {
		name += " " + strings.ToUpper(rest[0])
		rest = rest[1:]
	}
	parts := []string{name}
	run := 0
	flush := func() {
		switch {
		case run == 1:
			parts = append(parts, "?")
		case run == 2:
			parts = append(parts, "?", "?")
		case run > 2:
			parts = append(parts, "?", "?", "...")
		}
		run = 0
	}
	for _, a := range rest {
		if kw := strings.ToUpper(a); keepWords[kw] && len(a) <= 12 {
			flush()
			parts = append(parts, kw)
			continue
		}
		run++
	}
	flush()
	return strings.Join(parts, " ")
}

// CommandName 返回命令名（带子命令的返回 `CONFIG GET` 这样的两段名），用于按命令聚合统计。
func CommandName(args []string) string {
	if len(args) == 0 {
		return ""
	}
	name := strings.ToUpper(args[0])
	if twoWord[name] && len(args) > 1 {
		name += " " + strings.ToUpper(args[1])
	}
	return name
}
