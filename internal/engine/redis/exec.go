// @author: kevinkelly
// @license: Apache Licence
// @file: exec.go
// @time: 2026/10/28

package redis

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// 写客户端：执行经过检查的写命令（Redis 工单上线）。
//
// 与只读客户端（client.go / whitelist.go）完全分开，只读白名单一个字也没有放宽：
//   - 只接受 Approved——它只能由 Approve（= ParseCommand）创建，硬禁止命令、无键命令、未知命令都创建不出来；
//   - 命令发出前再做一次静态检查，防止调用方绕过 Approve 构造出别的值；
//   - 回复大小、单条命令超时都有上限；库由连接时指定（Dial 的 SELECT），不接受 SELECT 命令；
//   - 执行前值快照（Snapshot）只用固定的几条读命令，且只对调用方指定的命令调用。
// “这条命令该不该放行”（管理员规则）由调用方（internal/sqlflow + internal/redisrule）负责，这里只管“能不能执行”。

const (
	// MaxReplyBytes 是单条写命令回复的字节上限（GETDEL / LPOP count 之类可能返回数据）。
	MaxReplyBytes = 1 << 20
	// ExecTimeout 是单条写命令的超时。
	ExecTimeout = 30 * time.Second
	// SnapshotValueMax 是执行前值快照里单个值的字节上限，超过只记“未记录”。
	SnapshotValueMax = 4096
	// SnapshotMaxKeys 是一条命令最多记录执行前值的键数。
	SnapshotMaxKeys = 10
	// SnapshotMaxFields 是哈希命令最多记录的字段数。
	SnapshotMaxFields = 20
	snapshotTextMax   = 3000
)

// Approved 是通过静态检查的命令。零值不可用。
type Approved struct{ p *Parsed }

// Approve 对命令做静态检查并返回可执行的 Approved。
func Approve(args []string) (Approved, error) {
	p, err := ParseCommand(args)
	if err != nil {
		return Approved{}, err
	}
	return Approved{p: p}, nil
}

func (a Approved) valid() bool    { return a.p != nil }
func (a Approved) Name() string   { return a.p.Name }
func (a Approved) Keys() []string { return append([]string(nil), a.p.Keys...) }
func (a Approved) Args() []string { return append([]string(nil), a.p.Args...) }

var errReplyTooLarge = errors.New("redis 回复超过大小上限")

// capReader 限制一条命令回复最多读多少字节。
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, errReplyTooLarge
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

// ExecClient 是到某个库的写连接，非并发安全。
type ExecClient struct {
	c   *Client
	cap *capReader
}

// OpenExec 连接并选库（db 为 0..1023）。连接超时沿用 Config，单条命令超时为 ExecTimeout。
func (e *Engine) OpenExec(ctx context.Context, db int) (*ExecClient, error) {
	if db < 0 || db > 1023 {
		return nil, fmt.Errorf("库编号 %d 不合法", db)
	}
	cfg := e.cfg
	cfg.DB = db
	cfg.CmdTimeout = ExecTimeout
	c, err := Dial(ctx, cfg)
	if err != nil {
		return nil, err
	}
	cr := &capReader{r: c.conn, left: MaxReplyBytes}
	c.r = bufio.NewReaderSize(cr, 32<<10)
	return &ExecClient{c: c, cap: cr}, nil
}

// Close 关闭连接。
func (x *ExecClient) Close() error { return x.c.Close() }

// Exec 执行一条已批准的命令，返回原始回复（string / int64 / nil / []any）。服务端错误回复以 *ServerError 返回。
func (x *ExecClient) Exec(ctx context.Context, a Approved) (any, error) {
	if !a.valid() {
		return nil, errors.New("命令未经检查，拒绝执行")
	}
	// 再做一次静态检查：Approved 理论上只能由 Approve 创建，这里防御式地再确认。
	if _, err := ParseCommand(a.p.Args); err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, ExecTimeout)
	defer cancel()
	x.cap.left = MaxReplyBytes
	v, err := x.c.exec(cctx, a.p.Args)
	if err != nil && errors.Is(err, errReplyTooLarge) {
		return nil, errReplyTooLarge
	}
	return v, err
}

// ---- 回复整形 ----

// Summarize 把回复压成一行摘要（用于工单执行结果）：整数 (integer) n、nil (nil)、字符串带引号转义并截断、数组显示个数与前几项。
func Summarize(v any) string {
	switch x := v.(type) {
	case nil:
		return "(nil)"
	case int64:
		return "(integer) " + strconv.FormatInt(x, 10)
	case string:
		if x == "OK" {
			return "OK"
		}
		return quoteTrunc(x, 200)
	case []any:
		parts := make([]string, 0, 4)
		for i, it := range x {
			if i >= 3 {
				break
			}
			parts = append(parts, Summarize(it))
		}
		s := fmt.Sprintf("(array, %d 项)", len(x)) // i18n-ignore: 拼进结果单元格的值，属于数据输出
		if len(parts) > 0 {
			s += " " + strings.Join(parts, ", ")
			if len(x) > 3 {
				s += ", …"
			}
		}
		return s
	case *ServerError:
		return "(error) " + x.Msg
	}
	return fmt.Sprint(v)
}

// ReplyInt 返回整数回复的值（用作“影响行数”）；不是整数返回 0。
func ReplyInt(v any) int64 {
	if n, ok := v.(int64); ok {
		return n
	}
	return 0
}

// EscapeBinary 把字符串里的不可见字符与非法 UTF-8 转成 \xHH，并用双引号包起来（同 redis-cli 的展示）。
func EscapeBinary(s string) string {
	var sb strings.Builder
	sb.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&sb, `\x%02x`, s[i])
		case r == '"' || r == '\\':
			sb.WriteByte('\\')
			sb.WriteRune(r)
		case r == '\n':
			sb.WriteString(`\n`)
		case r == '\r':
			sb.WriteString(`\r`)
		case r == '\t':
			sb.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&sb, `\x%02x`, r)
		default:
			sb.WriteRune(r)
		}
		i += size
	}
	sb.WriteByte('"')
	return sb.String()
}

func quoteTrunc(s string, max int) string {
	cut := false
	if len(s) > max {
		at := max
		for at > 0 && !utf8.RuneStart(s[at]) { // 不要把多字节字符切成两半
			at--
		}
		s = s[:at]
		cut = true
	}
	q := EscapeBinary(s)
	if cut {
		q += "…"
	}
	return q
}

// ---- 审计用的命令摘要 ----

var secretKeyRe = regexp.MustCompile(`(?i)pass|pwd|secret|token|credential|private`)

// Redact 生成写进审计的命令摘要：命令名 + 键（每个最多 64 字符）+ 其余参数（每个最多 24 字符，并标注总长度）。
// 键名看起来是密码 / 令牌（含 pass、pwd、secret、token…）时，值参数一律不记录。
func Redact(args []string) string {
	if len(args) == 0 {
		return ""
	}
	keyIdx := map[int]bool{}
	secret := false
	if p, err := ParseCommand(args); err == nil {
		last := p.Info.Last
		if last < 0 {
			last = len(args) + last
		}
		step := p.Info.Step
		if step < 1 {
			step = 1
		}
		for i := p.Info.First; i <= last && i < len(args); i += step {
			keyIdx[i] = true
			if secretKeyRe.MatchString(args[i]) {
				secret = true
			}
		}
	}
	parts := []string{strings.ToUpper(args[0])}
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case keyIdx[i]:
			parts = append(parts, cutRunes(sanitize(a), 64))
		case secret:
			parts = append(parts, "***")
		case len([]rune(a)) > 24:
			parts = append(parts, strings.TrimSuffix(cutRunes(sanitize(a), 24), "…")+fmt.Sprintf("…(%d 字节)", len(a))) // i18n-ignore: 命令摘要，会写进工单 / 审计，不随界面语言变化
		default:
			parts = append(parts, sanitize(a))
		}
	}
	return strings.Join(parts, " ")
}

func sanitize(s string) string {
	if utf8.ValidString(s) && !strings.ContainsAny(s, "\n\r\t\x00 ") {
		return s
	}
	return EscapeBinary(s)
}

func cutRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ---- 执行前值快照 ----

// Snapshot 在执行写命令之前，记录它将要改动的键的现状，写进工单详情供人工恢复。只读、限量：
//   - 最多 SnapshotMaxKeys 个键；字符串只记 < 4KB 的值，哈希只记命令里点名的字段（最多 20 个）且每个 < 4KB；其他类型只记类型与长度；
//   - 返回文本最长约 3KB；任何读取失败只在文本里标注，不影响执行。
//
// 只有规则里勾选“记录执行前值”的命令才会调用它（原值会明文显示在工单详情里）。
func (x *ExecClient) Snapshot(ctx context.Context, a Approved) string {
	if !a.valid() {
		return ""
	}
	keys := a.p.Keys
	var sb strings.Builder
	for i, k := range keys {
		if i >= SnapshotMaxKeys {
			fmt.Fprintf(&sb, "\n…其余 %d 个键未记录", len(keys)-SnapshotMaxKeys) // i18n-ignore: 执行前快照文本，写进工单详情，不随界面语言变化
			break
		}
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(x.snapshotKey(ctx, a, k))
		if sb.Len() > snapshotTextMax {
			sb.WriteString("\n…（超长，已截断）") // i18n-ignore: 执行前快照文本
			break
		}
	}
	return sb.String()
}

func (x *ExecClient) rd(ctx context.Context, args ...string) (any, error) {
	x.cap.left = MaxReplyBytes
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return x.c.exec(cctx, args)
}

func (x *ExecClient) snapshotKey(ctx context.Context, a Approved, k string) string {
	// 本函数拼出的是“执行前快照”文本，会原样写进工单详情（存库的历史文本），不随界面语言变化：整段字面量都标 i18n-ignore。
	head := "键 " + cutRunes(EscapeBinary(k), 80) // i18n-ignore
	tv, err := x.rd(ctx, "TYPE", k)
	if err != nil {
		return head + "：读取失败（" + err.Error() + "）" // i18n-ignore
	}
	typ := asString(tv)
	if typ == "none" {
		return head + "：执行前不存在" // i18n-ignore
	}
	ttl := ""
	if pv, err := x.rd(ctx, "PTTL", k); err == nil {
		switch n := asInt(pv); {
		case n == -1:
			ttl = "，无过期时间" // i18n-ignore
		case n >= 0:
			ttl = "，剩余 " + (time.Duration(n) * time.Millisecond).Round(time.Second).String() // i18n-ignore
		}
	}
	head += "：类型 " + typ + ttl // i18n-ignore
	switch typ {
	case "string":
		lv, err := x.rd(ctx, "STRLEN", k)
		if err != nil {
			return head + "，读取长度失败" // i18n-ignore
		}
		n := asInt(lv)
		if n > SnapshotValueMax {
			return head + fmt.Sprintf("，值 %d 字节，超过 %d 字节，未记录", n, SnapshotValueMax) // i18n-ignore
		}
		vv, err := x.rd(ctx, "GET", k)
		if err != nil {
			return head + "，读取值失败" // i18n-ignore
		}
		return head + "，值 " + EscapeBinary(asString(vv)) // i18n-ignore
	case "hash":
		fields := hashFields(a.p)
		if len(fields) == 0 {
			if lv, err := x.rd(ctx, "HLEN", k); err == nil {
				return head + fmt.Sprintf("，%d 个字段（本命令未点名字段，内容未记录）", asInt(lv)) // i18n-ignore
			}
			return head
		}
		more := ""
		if len(fields) > SnapshotMaxFields {
			more = fmt.Sprintf("（仅记录前 %d 个字段）", SnapshotMaxFields) // i18n-ignore
			fields = fields[:SnapshotMaxFields]
		}
		vals, err := x.rd(ctx, append([]string{"HMGET", k}, fields...)...)
		if err != nil {
			return head + "，读取字段失败" // i18n-ignore
		}
		arr, _ := vals.([]any)
		var parts []string
		for i, f := range fields {
			v := "(nil)"
			if i < len(arr) && arr[i] != nil {
				s := asString(arr[i])
				if len(s) > SnapshotValueMax {
					v = fmt.Sprintf("(%d 字节，未记录)", len(s)) // i18n-ignore
				} else {
					v = EscapeBinary(s)
				}
			}
			parts = append(parts, EscapeBinary(f)+"="+v)
		}
		return head + "，原字段值" + more + " " + strings.Join(parts, "，") // i18n-ignore
	}
	lenCmd := map[string]string{"list": "LLEN", "set": "SCARD", "zset": "ZCARD", "stream": "XLEN"}[typ]
	if lenCmd != "" {
		if lv, err := x.rd(ctx, lenCmd, k); err == nil {
			return head + fmt.Sprintf("，%d 个元素（内容未记录）", asInt(lv)) // i18n-ignore
		}
	}
	return head + "（内容未记录）" // i18n-ignore
}

// hashFields 取哈希写命令里点名的字段。
func hashFields(p *Parsed) []string {
	a := p.Args
	switch p.Name {
	case "HSET", "HMSET", "HSETNX":
		var out []string
		for i := 2; i < len(a); i += 2 {
			out = append(out, a[i])
		}
		return out
	case "HDEL":
		return append([]string(nil), a[2:]...)
	case "HINCRBY", "HINCRBYFLOAT":
		return []string{a[2]}
	}
	return nil
}
