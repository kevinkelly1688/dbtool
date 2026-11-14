// @author: kevinkelly
// @license: Apache Licence
// @file: eval.go
// @time: 2026/10/28

package redisrule

import (
	"errors"
	"fmt"
	"strings"

	"dbtool/internal/engine/redis"
)

// 一次请求的命令条数上限。
const MaxLines = 200

// 检查级别（与 internal/review 的 errlevel 一致）。
const (
	LevelOK    = 0
	LevelWarn  = 1
	LevelError = 2
)

// Target 是命令将要执行的位置。
type Target struct {
	InstanceID uint
	DB         int
}

// Verdict 是一条命令的检查结果。
type Verdict struct {
	Level        int      `json:"level"`
	Status       string   `json:"status"`  // 通过 / 警告 / 拒绝
	Message      string   `json:"message"` // 拒绝原因或警告内容；通过时为空
	Name         string   `json:"name"`    // 大写命令名（解析失败时为空）
	Keys         []string `json:"keys"`
	RuleID       uint     `json:"rule_id"`
	RuleName     string   `json:"rule_name"`
	RecordBefore bool     `json:"record_before"`
	Args         []string `json:"-"` // 分词结果（通过 / 警告时用于执行）
}

// Allowed 命令是否允许（通过或仅警告）。
func (v Verdict) Allowed() bool { return v.Level != LevelError }

func reject(format string, a ...any) Verdict {
	return Verdict{Level: LevelError, Status: "拒绝", Message: fmt.Sprintf(format, a...)}
}

// Line 是请求里的一行命令。
type Line struct {
	No   int    // 行号（从 1 起，空行也占行号）
	Text string // 原文（去掉首尾空白）
}

// SplitLines 按行切分命令文本：空行忽略，行号保留；超过 MaxLines 条报错。
func SplitLines(text string) ([]Line, error) {
	var out []Line
	for i, l := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(strings.ReplaceAll(l, "\r", ""))
		if t == "" {
			continue
		}
		out = append(out, Line{No: i + 1, Text: t})
	}
	if len(out) == 0 {
		return nil, errors.New("命令不能为空")
	}
	if len(out) > MaxLines {
		return nil, fmt.Errorf("一次最多提交 %d 条命令，当前 %d 条", MaxLines, len(out))
	}
	return out, nil
}

// EvaluateLine 对一行命令文本做完整检查：分词 → 静态检查（硬禁止、受支持、语法）→ 规则匹配 → 警告。
func EvaluateLine(rules []Rule, t Target, line string) Verdict {
	args, err := redis.TokenizeLine(line)
	if err != nil {
		return reject("语法错误：%s", err)
	}
	if len(args) == 0 {
		return reject("命令为空")
	}
	return Evaluate(rules, t, args)
}

// Evaluate 对已分词的命令做检查。
func Evaluate(rules []Rule, t Target, args []string) Verdict {
	p, err := redis.ParseCommand(args)
	if err != nil {
		var hf *redis.ErrHardForbidden
		var un *redis.ErrUnsupported
		switch {
		case errors.As(err, &hf):
			return reject("命中硬禁止命令 %s：任何规则都无法放行", hf.Cmd)
		case errors.As(err, &un):
			return Verdict{Level: LevelError, Status: "拒绝", Name: un.Cmd, Message: fmt.Sprintf("命令 %s 不在受支持的写命令清单里（无键命令、读命令与未知命令不能通过工单执行）", un.Cmd)}
		}
		return reject("语法错误：%s", err)
	}
	v := Verdict{Name: p.Name, Keys: p.Keys, Args: args}

	// 规则匹配：取“走得最远”的规则来解释拒绝原因。
	const (
		stNoCmd = iota // 规则不含该命令
		stScope        // 命令命中，适用范围不符
		stKeys         // 范围符合，键不匹配
		stCount        // 键匹配，数量超限
		stOK
	)
	enabled := 0
	bestStage, bestMsg := -1, ""
	for i := range rules {
		r := &rules[i]
		if !r.Enabled {
			continue
		}
		enabled++
		stage, msg := stNoCmd, ""
		switch {
		case !r.HasCommand(p.Name):
		case !r.InScope(t.InstanceID, t.DB):
			stage, msg = stScope, fmt.Sprintf("规则「%s」允许命令 %s，但不适用于当前实例 / 库（适用范围：%s）", r.Name, p.Name, r.ScopeText())
		default:
			bad := ""
			for _, k := range p.Keys {
				if !matchAny(r.KeyPatterns, k) {
					bad = k
					break
				}
			}
			switch {
			case bad != "":
				stage, msg = stKeys, fmt.Sprintf("键 %s 不匹配规则「%s」允许的键模式（%s）", redis.EscapeBinary(bad), r.Name, strings.Join(r.KeyPatterns, "、"))
			case len(p.Keys) > r.MaxKeys:
				stage, msg = stCount, fmt.Sprintf("命令涉及 %d 个键，超过规则「%s」的上限 %d", len(p.Keys), r.Name, r.MaxKeys)
			default:
				stage = stOK
			}
		}
		if stage == stOK {
			v.Level, v.Status = LevelOK, "通过"
			v.RuleID, v.RuleName, v.RecordBefore = r.ID, r.Name, r.RecordBefore
			v.Message = ""
			warnings(&v, p)
			return v
		}
		if stage > bestStage {
			bestStage, bestMsg = stage, msg
		}
	}
	v.Level, v.Status = LevelError, "拒绝"
	switch {
	case enabled == 0:
		v.Message = "没有任何启用的规则（默认全部拒绝），请联系管理员在「Redis 规则」里定义"
	case bestStage == stNoCmd:
		v.Message = fmt.Sprintf("没有任何启用的规则允许命令 %s", p.Name)
	default:
		v.Message = bestMsg
	}
	return v
}

func matchAny(patterns []string, key string) bool {
	for _, p := range patterns {
		if GlobMatch(p, key) {
			return true
		}
	}
	return false
}

// warnings 给通过的命令补充提醒（不拦截）。
func warnings(v *Verdict, p *redis.Parsed) {
	var w []string
	a := p.Args
	switch p.Name {
	case "SET":
		if !redis.SetHasTTL(a) {
			w = append(w, "SET 没有设置过期时间（EX / PX / EXAT / PXAT / KEEPTTL），键将永不过期")
		}
	case "SETNX", "GETSET", "MSET", "MSETNX":
		w = append(w, fmt.Sprintf("%s 不能设置过期时间，键将永不过期（需要的话请随后加 EXPIRE）", p.Name))
	case "EXPIRE", "PEXPIRE":
		if n := atoi(a[2]); n <= 0 {
			w = append(w, fmt.Sprintf("%s 的时间 ≤ 0，键会被立即删除", p.Name))
		}
	case "EXPIREAT", "PEXPIREAT":
		if n := atoi(a[2]); n <= 0 {
			w = append(w, fmt.Sprintf("%s 的时间戳 ≤ 0，键会被立即删除", p.Name))
		}
	case "PERSIST":
		w = append(w, "PERSIST 会去掉过期时间，键将永不过期")
	case "RENAME", "COPY":
		w = append(w, fmt.Sprintf("%s 会覆盖目标键（若已存在）", p.Name))
	case "DEL", "UNLINK":
		if len(p.Keys) >= 20 {
			w = append(w, fmt.Sprintf("一次删除 %d 个键，请确认", len(p.Keys)))
		}
	}
	if len(w) > 0 {
		v.Level, v.Status, v.Message = LevelWarn, "警告", strings.Join(w, "；")
	}
}

func atoi(s string) int64 {
	var n int64
	neg := false
	for i, c := range s {
		if i == 0 && c == '-' {
			neg = true
			continue
		}
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
		if n > 1<<50 {
			break
		}
	}
	if neg {
		return -n
	}
	return n
}
