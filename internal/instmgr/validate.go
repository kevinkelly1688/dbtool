// @author: kevinkelly
// @license: Apache Licence
// @file: validate.go
// @time: 2026/10/28

package instmgr

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// secret 是不能出现在日志、审计和预览里的参数（密码）：执行时还原成明文，展示时显示为 '******'。
type secret string

// plainArgs 把 secret 还原成普通字符串，交给驱动。
func plainArgs(args []any) []any {
	out := make([]any, len(args))
	for i, a := range args {
		if s, ok := a.(secret); ok {
			out[i] = string(s)
		} else {
			out[i] = a
		}
	}
	return out
}

// quoteLit 把字符串转成 MySQL 字面量，仅用于展示（执行时一律走参数占位符）。
func quoteLit(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\x00", `\0`, "\n", `\n`, "\r", `\r`, "\x1a", `\Z`)
	return "'" + r.Replace(s) + "'"
}

// fmtStmt 把带 ? 的语句与参数合成一条给人看的语句：字符串加引号，数字原样，secret 打码。
func fmtStmt(sql string, args ...any) string {
	var b strings.Builder
	i := 0
	for _, c := range sql {
		if c != '?' || i >= len(args) {
			b.WriteRune(c)
			continue
		}
		switch v := args[i].(type) {
		case secret:
			b.WriteString("'******'")
		case string:
			b.WriteString(quoteLit(v))
		case int:
			b.WriteString(strconv.Itoa(v))
		case int64:
			b.WriteString(strconv.FormatInt(v, 10))
		case float64:
			b.WriteString(strconv.FormatFloat(v, 'g', -1, 64))
		default:
			b.WriteString(quoteLit(fmt.Sprint(v)))
		}
		i++
	}
	return b.String()
}

// hasControl 判断是否含控制字符（含换行、NUL）。
func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// cleanText 去掉首尾空白并校验长度（按字符）与控制字符。
func cleanText(label, s string, max int, required bool) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		if required {
			return "", bad(fmt.Sprintf("%s不能为空", label))
		}
		return "", nil
	}
	if utf8.RuneCountInString(s) > max {
		return "", bad(fmt.Sprintf("%s过长（最多 %d 个字符）", label, max))
	}
	if hasControl(s) {
		return "", bad(fmt.Sprintf("%s不能包含换行等控制字符", label))
	}
	return s, nil
}

// quoteIdent 给库名、表名、列名加反引号（内部反引号加倍）。名字必须非空、不超过 64 个字符且没有控制字符。
func quoteIdent(name string) (string, error) {
	if name == "" || utf8.RuneCountInString(name) > 64 || hasControl(name) || !utf8.ValidString(name) {
		// 用 %s + strconv.Quote（与 %q 输出一致）：目录里的格式串匹配时，参数带着引号原样代入
		return "", bad(fmt.Sprintf("名称 %s 不合法（1 到 64 个字符，不能含控制字符）", strconv.Quote(name)))
	}
	return "`" + strings.ReplaceAll(name, "`", "``") + "`", nil
}

// dbNameRe 是创建数据库时的库名白名单（同原型：字母开头，字母或数字结尾，中间可含下划线与中划线，最长 64）。
var dbNameRe = regexp.MustCompile(`^[A-Za-z]([A-Za-z0-9_-]{0,62}[A-Za-z0-9])?$`)

func validNewDBName(name string) error {
	if !dbNameRe.MatchString(name) {
		return bad("数据库名只能由字母、数字、下划线、中划线组成，以字母开头、字母或数字结尾，最长 64 位")
	}
	switch strings.ToLower(name) {
	case "information_schema", "performance_schema", "mysql", "sys", "test":
		return bad("不能使用系统库的名字")
	}
	return nil
}

var (
	identWordRe  = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	accountRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,31}$`)
	hostRe       = regexp.MustCompile(`^[A-Za-z0-9._%:/-]{1,60}$`)
	varNameRe    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	binlogNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,255}$`)
)

func validAccountUser(user string) error {
	if !accountRe.MatchString(user) {
		return bad("账号名只能由字母、数字、下划线、点、中划线组成，以字母或数字开头，最长 32 位")
	}
	return nil
}

func validHost(host string) error {
	if !hostRe.MatchString(host) {
		return bad(fmt.Sprintf("主机 %s 不合法（只能是 IP / 主机名 / 含 %% 的通配，不能含空格和引号）", strconv.Quote(host)))
	}
	return nil
}

// splitHosts 拆分用 | 分隔的多个主机，去重并逐个校验。
func splitHosts(s string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, h := range strings.Split(s, "|") {
		h = strings.TrimSpace(h)
		if h == "" || seen[h] {
			continue
		}
		if err := validHost(h); err != nil {
			return nil, err
		}
		seen[h] = true
		out = append(out, h)
	}
	if len(out) == 0 {
		return nil, bad("主机不能为空")
	}
	if len(out) > 20 {
		return nil, bad("一次最多创建 20 个主机的账号")
	}
	return out, nil
}

// validPassword：8 到 32 位，只含可见 ASCII 字符，且同时含英文、数字和特殊符号（同原型提示）。
func validPassword(pw string) error {
	if len(pw) < 8 || len(pw) > 32 {
		return bad("密码长度需要 8 到 32 位")
	}
	var letter, digit, special bool
	for _, r := range pw {
		switch {
		case r > unicode.MaxASCII || r < 0x21 || r == 0x7f:
			return bad("密码只能包含英文、数字和英文标点，不能有空格或中文")
		case unicode.IsLetter(r):
			letter = true
		case unicode.IsDigit(r):
			digit = true
		default:
			special = true
		}
	}
	if !letter || !digit || !special {
		return bad("密码必须同时包含英文、数字和特殊符号")
	}
	return nil
}

// likeEscape 转义 LIKE 里的通配符（配合 ESCAPE '!'）。
func likeEscape(s string) string {
	return strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(s)
}

// escapeDBPattern 转义库级授权里的通配符：GRANT ... ON `my_db`.* 里 _ 和 % 是通配符，不转义会授权到其他库。
func escapeDBPattern(db string) string {
	return strings.NewReplacer(`_`, `\_`, `%`, `\%`).Replace(db)
}

// clip 截断到最多 n 个字符。
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}
