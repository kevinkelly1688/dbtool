// @author: kevinkelly
// @license: Apache Licence
// @file: validate.go
// @time: 2026/10/28

package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// 专门校验：登记时通过 Def.check 挂到具体设置上。

func checkRegex(v string) error {
	if _, err := regexp.Compile(v); err != nil {
		return fmt.Errorf("不是合法的正则表达式（Go / RE2 语法）：%v", err)
	}
	return nil
}

func checkJSONObject(v string) error {
	var m map[string]any
	if err := json.Unmarshal([]byte(v), &m); err != nil {
		return errors.New("不是合法的 JSON 对象（形如 {\"规则名\": \"级别\"}）")
	}
	return nil
}

// checkCSVOf 校验逗号分隔的列表，每一项必须在 allowed 里（空项忽略）。
func checkCSVOf(allowed ...string) func(string) error {
	set := map[string]bool{}
	for _, a := range allowed {
		set[a] = true
	}
	return func(v string) error {
		for _, p := range strings.Split(v, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if !set[p] {
				return fmt.Errorf("不认识的取值 %q，只能是：%s（逗号分隔）", p, strings.Join(allowed, "、"))
			}
		}
		return nil
	}
}

// checkUintList 校验逗号分隔的正整数列表（用户 ID 等）。
func checkUintList(v string) error {
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if n, err := strconv.ParseUint(p, 10, 32); err != nil || n == 0 {
			return fmt.Errorf("%q 不是有效的 ID（需要正整数，逗号分隔）", p)
		}
	}
	return nil
}

var (
	windowRE = regexp.MustCompile(`^(\d{1,2}):(\d{2})-(\d{1,2}):(\d{2})$`)
	quietRE  = regexp.MustCompile(`^(\d{1,2})-(\d{1,2})$`)
)

func checkWindow(v string) error {
	m := windowRE.FindStringSubmatch(v)
	if m == nil {
		return errors.New("格式应为 HH:MM-HH:MM，例如 01:00-05:00（可跨零点）")
	}
	for _, i := range []int{1, 3} {
		if h, _ := strconv.Atoi(m[i]); h > 23 {
			return errors.New("小时需在 0 到 23 之间")
		}
	}
	for _, i := range []int{2, 4} {
		if mm, _ := strconv.Atoi(m[i]); mm > 59 {
			return errors.New("分钟需在 0 到 59 之间")
		}
	}
	return nil
}

func checkQuietHours(v string) error {
	m := quietRE.FindStringSubmatch(v)
	if m == nil {
		return errors.New("格式应为 起-止 两个小时数，例如 22-9")
	}
	for _, s := range m[1:] {
		if h, _ := strconv.Atoi(s); h > 24 {
			return errors.New("小时需在 0 到 24 之间")
		}
	}
	return nil
}

func checkFloat(lo, hi float64) func(string) error {
	return func(v string) error {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return errors.New("需要一个数字")
		}
		if f < lo || f > hi {
			return fmt.Errorf("需要在 %v 到 %v 之间", lo, hi)
		}
		return nil
	}
}

// checkExportPath 校验导出目录：不能为根目录，不能含 ..（相对路径相对于 EXPORT_BASE_DIR）。
func checkExportPath(v string) error {
	if len(v) > 255 {
		return errors.New("路径太长（最多 255 个字符）")
	}
	norm := strings.ReplaceAll(v, "\\", "/")
	for _, seg := range strings.Split(norm, "/") {
		if seg == ".." {
			return errors.New("路径里不能含 ..")
		}
	}
	if strings.Trim(norm, "/ .") == "" {
		return errors.New("不能是根目录或空路径")
	}
	return nil
}

// wmTemplateRE / wmVars：水印内容模板里的变量（与 authx 的 WatermarkVars 一致，authx 校验时仍有自己的完整规则）。
var wmTemplateRE = regexp.MustCompile(`\{([^{}]*)\}`)

func checkWatermarkTemplate(v string) error {
	known := map[string]bool{"账号": true, "姓名": true, "部门": true, "IP": true, "日期": true, "时间": true} // i18n-ignore 水印模板里的变量名，用户在模板里照这个写
	for _, m := range wmTemplateRE.FindAllStringSubmatch(v, -1) {
		if !known[m[1]] {
			return fmt.Errorf("不认识的变量 {%s}，可用：{账号} {姓名} {部门} {IP} {日期} {时间}", m[1])
		}
	}
	if strings.ContainsAny(wmTemplateRE.ReplaceAllString(v, ""), "{}") {
		return errors.New("花括号不成对：变量要写成 {账号} 这样的形式")
	}
	return nil
}

func checkWatermarkText(v string) error {
	if utf8.RuneCountInString(v) > 60 {
		return errors.New("水印内容最长 60 个字符")
	}
	if !strings.Contains(v, "{账号}") && !strings.Contains(v, "{姓名}") { // i18n-ignore 水印模板里的变量名
		return errors.New("水印内容必须包含 {账号} 或 {姓名}，否则截图无法追溯到具体的人")
	}
	return checkWatermarkTemplate(v)
}

func checkWatermarkExtra(v string) error {
	if utf8.RuneCountInString(v) > 40 {
		return errors.New("附加文字最长 40 个字符")
	}
	return checkWatermarkTemplate(v)
}

// ---- 值规整 ----

// normBool 把各种写法规整成 true / false；不认识返回 ok=false。
func normBool(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes", "on", "y", "t":
		return "true", true
	case "false", "0", "no", "off", "n", "f":
		return "false", true
	}
	return "", false
}

// normList 把逗号分隔的列表规整：去空白、去空项、去重（保持顺序）。
func normList(s string) string {
	var out []string
	seen := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return strings.Join(out, ",")
}

// textClean 检查文本是否可以保存：合法 UTF-8、没有控制字符（multiline 时允许换行 / 制表符）。
func textClean(v string, multiline bool) error {
	if !utf8.ValidString(v) {
		return errors.New("值不是合法的 UTF-8 文本")
	}
	for _, r := range v {
		if r == '\n' || r == '\r' || r == '\t' {
			if !multiline {
				return errors.New("值里不能有换行或制表符")
			}
			continue
		}
		if unicode.IsControl(r) {
			return errors.New("值里不能有控制字符")
		}
	}
	return nil
}

// ---- 指向服务器程序 / 文件 / 外部库的设置 ----

var winDrive = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

// checkAbsPath 要求绝对路径，且不含 .. 段（避免用相对跳转绕开目录约束）。
func checkAbsPath(v string) error {
	if !strings.HasPrefix(v, "/") && !winDrive.MatchString(v) {
		return errors.New("需要绝对路径（以 / 开头；Windows 为盘符开头）")
	}
	for _, seg := range strings.FieldsFunc(v, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return errors.New("路径里不能有 ..")
		}
	}
	return nil
}

// checkSoarPath 要求 SOAR 程序是绝对路径、文件名必须是 soar（Windows 可带 .exe）：
// 页面能改 = 能让服务执行服务器上的程序，所以不允许指向 sh / python 之类的任意程序。
func checkSoarPath(v string) error {
	if err := checkAbsPath(v); err != nil {
		return err
	}
	base := v
	if i := strings.LastIndexAny(v, `/\`); i >= 0 {
		base = v[i+1:]
	}
	if !strings.EqualFold(strings.TrimSuffix(strings.ToLower(base), ".exe"), "soar") {
		return errors.New("文件名必须是 soar")
	}
	return nil
}

// SoarDSNAddr 解析 SOAR 测试环境 DSN（用户:密码@主机:端口/库名）里的地址；端口缺省 3306。
func SoarDSNAddr(dsn string) (host string, port int, err error) {
	bad := errors.New("格式应为 用户:密码@主机:端口/库名")
	at := strings.LastIndex(dsn, "@")
	if at <= 0 || strings.Contains(dsn, "://") {
		return "", 0, bad
	}
	if user, _, _ := strings.Cut(dsn[:at], ":"); user == "" {
		return "", 0, bad
	}
	hostport, _, _ := strings.Cut(dsn[at+1:], "/")
	hostport = strings.TrimSpace(hostport)
	if hostport == "" {
		return "", 0, bad
	}
	port = 3306
	if h, p, e := net.SplitHostPort(hostport); e == nil {
		n, e := strconv.Atoi(p)
		if e != nil || n < 1 || n > 65535 {
			return "", 0, errors.New("端口需要在 1 到 65535 之间")
		}
		host, port = h, n
	} else {
		host = strings.Trim(hostport, "[]")
	}
	if host == "" {
		return "", 0, bad
	}
	return host, port, nil
}

func checkSoarDSN(v string) error {
	_, _, err := SoarDSNAddr(v)
	return err
}
