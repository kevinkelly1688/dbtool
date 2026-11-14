// @author: kevinkelly
// @license: Apache Licence
// @file: i18n.go
// @time: 2026/10/28

// Package i18n 是界面多语言的核心：以中文原文为键的目录、请求语言的判定与翻译。
//
// 约定：
//   - 源码里写的就是中文，中文原文即键；zh-CN 恒等返回原文，目录里只有其它语言的译文（web/i18n/<lang>.json）。
//   - 缺译一律回落到中文原文，所以中文界面不可能出现“裸键”，迁移可以按页面逐步推进。
//   - 带参数的文案用具名占位 {name}：T(lang, "共 {n} 条", "n", 3)。
//   - 同形异义的词用 “词|上下文” 区分（上下文是小写 ASCII 标识），如 "关闭|dialog"；展示时去掉 |上下文。
package i18n

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"dbtool/web"
)

// 支持的语言代码。
const (
	ZH = "zh-CN"
	EN = "en"
)

// Lang 描述一种界面语言（切换器用）。Name 用该语言自己的写法，不翻译。
type Lang struct {
	Code  string
	Name  string
	Short string
}

// Langs 是支持的语言，顺序即切换器里的顺序。新增语言：在这里加一项并放入 web/i18n/<code>.json。
var Langs = []Lang{
	{ZH, "中文", "中"},
	{EN, "English", "EN"},
}

// Normalize 把 cookie / 参数里的语言写法规整成支持的代码；不认识的返回 false（白名单，避免注入到页面属性里）。
func Normalize(s string) (string, bool) {
	switch strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "_", "-")) {
	case "zh", "zh-cn", "zh-hans", "zh-hans-cn":
		return ZH, true
	case "en", "en-us", "en-gb":
		return EN, true
	}
	return "", false
}

var defaultLang atomic.Value // string

func init() { defaultLang.Store(ZH) }

// SetDefault 设置系统默认语言（没有 cookie 的访客、后台任务生成的文案用它）；不认识的值忽略。
func SetDefault(lang string) {
	if l, ok := Normalize(lang); ok {
		defaultLang.Store(l)
	}
}

// Default 返回系统默认语言，缺省 zh-CN。
func Default() string { return defaultLang.Load().(string) }

// ---- 目录 ----

type catalog struct {
	msgs map[string]map[string]string // 语言 → 中文原文 → 译文
	json map[string][]byte            // 语言 → 下发给前端的 JSON
	tag  map[string]string            // 语言 → ETag
	pats map[string][]pattern         // 语言 → 带格式动词键的匹配器（见 TP）

	conflicts []string // 跨文件译文不一致的键（i18n 的测试要求为空）
}

var cat atomic.Pointer[catalog]

func init() {
	c, err := load(web.FS)
	if err != nil {
		panic("i18n: " + err.Error()) // 内嵌目录损坏是构建期缺陷，i18n 的测试会先发现
	}
	cat.Store(c)
}

// load 读取 fsys 里 i18n/<语言>/*.json（扁平的 {"中文原文": "译文"}，按文件名排序合并，一个模块一个文件，
// 便于并行维护）。zh-CN 不需要文件。同一个键在多个文件里译文不一致时记入 conflicts，后读的文件生效。
func load(fsys fs.FS) (*catalog, error) {
	c := &catalog{msgs: map[string]map[string]string{}, json: map[string][]byte{}, tag: map[string]string{}, pats: map[string][]pattern{}}
	for _, l := range Langs {
		if l.Code == ZH {
			continue
		}
		dir := "i18n/" + l.Code
		ents, err := fs.ReadDir(fsys, dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // 还没有这种语言的目录：全部回落中文
			}
			return nil, err
		}
		m := map[string]string{}
		src := map[string]string{} // 键 → 来自哪个文件（冲突提示用）
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			raw, err := fs.ReadFile(fsys, dir+"/"+e.Name())
			if err != nil {
				return nil, err
			}
			part := map[string]string{}
			if err := json.Unmarshal(raw, &part); err != nil {
				return nil, fmt.Errorf("%s/%s: %w", dir, e.Name(), err)
			}
			for k, v := range part {
				if strings.TrimSpace(v) == "" { // 空译文等同缺译
					continue
				}
				if old, dup := m[k]; dup && old != v {
					c.conflicts = append(c.conflicts, fmt.Sprintf("%s: %q 在 %s 与 %s 里译文不同", l.Code, k, src[k], e.Name()))
				}
				m[k], src[k] = v, e.Name()
			}
		}
		c.msgs[l.Code] = m
		c.pats[l.Code] = buildPatterns(m)
		out, _ := json.Marshal(m) // map 序列化按键排序，输出稳定
		c.json[l.Code] = out
		c.tag[l.Code] = fmt.Sprintf(`"%x"`, hash(out))
	}
	sort.Strings(c.conflicts)
	return c, nil
}

func hash(b []byte) uint64 { // FNV-1a，只用来做 ETag
	h := uint64(14695981039346656037)
	for _, x := range b {
		h ^= uint64(x)
		h *= 1099511628211
	}
	return h
}

// Conflicts 返回目录文件之间译文不一致的键，诊断 / 测试用。
func Conflicts() []string { return cat.Load().conflicts }

// Messages 返回某语言的目录（只读，勿修改）；中文与未知语言返回 nil。
func Messages(lang string) map[string]string { return cat.Load().msgs[lang] }

// JSON 返回某语言目录的 JSON 与 ETag（给前端 /i18n/<lang>.json 用）；中文返回 {}。
func JSON(lang string) (body []byte, etag string) {
	c := cat.Load()
	if b, ok := c.json[lang]; ok {
		return b, c.tag[lang]
	}
	return []byte("{}"), `"empty"`
}

// ---- 翻译 ----

var ctxRe = regexp.MustCompile(`\|[a-z][a-z0-9_.-]*$`)

// Base 去掉 “|上下文” 后缀，得到展示用的中文原文。
func Base(key string) string {
	if i := strings.LastIndexByte(key, '|'); i >= 0 && ctxRe.MatchString(key[i:]) {
		return key[:i]
	}
	return key
}

var missing sync.Map // lang+"\x00"+key → struct{}

// T 把中文原文 key 翻译成 lang 的文案。kv 是成对的 {名字, 值}，代入 {名字} 占位。
// lang 为中文、不认识或缺译时回落到原文（参数照常代入）。
func T(lang, key string, kv ...any) string {
	s := Base(key)
	if lang != ZH {
		if m := cat.Load().msgs[lang]; m != nil {
			if v, ok := m[key]; ok {
				s = v
			} else if _, ok := Normalize(lang); ok && hasHan(s) {
				missing.Store(lang+"\x00"+key, struct{}{})
			}
		}
	}
	return fill(s, kv)
}

// Missing 返回已被请求过但目录里没有译文的键（含中文的才算），排序后给诊断 / 测试用。
func Missing(lang string) []string {
	var out []string
	missing.Range(func(k, _ any) bool {
		if l, key, ok := strings.Cut(k.(string), "\x00"); ok && l == lang {
			out = append(out, key)
		}
		return true
	})
	sort.Strings(out)
	return out
}

func hasHan(s string) bool {
	for _, r := range s {
		if r >= 0x4e00 && r <= 0x9fff {
			return true
		}
	}
	return false
}

// Fill 把 s 里的 {名字} 占位按成对参数代入（T 的代入步骤，单独暴露给需要先转义再代入的调用方，如模板的 th）。
func Fill(s string, kv ...any) string { return fill(s, kv) }

func fill(s string, kv []any) string {
	if len(kv) < 2 || !strings.Contains(s, "{") {
		return s
	}
	args := make([]string, 0, len(kv))
	for i := 0; i+1 < len(kv); i += 2 {
		args = append(args, "{"+fmt.Sprint(kv[i])+"}", fmt.Sprint(kv[i+1]))
	}
	return strings.NewReplacer(args...).Replace(s)
}

// N 标记一个“稍后翻译”的中文原文（如菜单标签的登记处），返回原文本身；提取工具据此收录进目录。
func N(key string) string { return key }

// ---- 请求上下文 ----

type ctxKey struct{}

// WithLang 把请求语言放进 ctx，供不持有 *gin.Context 的业务层使用。
func WithLang(ctx context.Context, lang string) context.Context {
	return context.WithValue(ctx, ctxKey{}, lang)
}

// FromContext 取请求语言；ctx 里没有时返回系统默认语言。
func FromContext(ctx context.Context) string {
	if ctx != nil {
		if l, ok := ctx.Value(ctxKey{}).(string); ok && l != "" {
			return l
		}
	}
	return Default()
}

// ---- 带翻译键的错误 ----

// Error 是带翻译键的错误：Error() 恒返回中文（日志与既有断言不变），
// 在 HTTP 边缘用 Localize 按请求语言渲染。
type Error struct {
	key   string
	kv    []any
	cause error
}

// Errorf 构造带键的错误：i18n.Errorf("实例 {name} 不存在", "name", n)。
func Errorf(key string, kv ...any) *Error { return &Error{key: key, kv: kv} }

// Wrap 构造带键的错误并保留底层错误（errors.Is / As 可穿透）。
func Wrap(cause error, key string, kv ...any) *Error { return &Error{key: key, kv: kv, cause: cause} }

func (e *Error) Error() string                 { return T(ZH, e.key, e.kv...) }
func (e *Error) Unwrap() error                 { return e.cause }
func (e *Error) Localize(lang string) string   { return T(lang, e.key, e.kv...) }
func (e *Error) LocalizedKey() (string, []any) { return e.key, e.kv }

// Localize 把任意 error 渲染成 lang 的文案：带键的错误按键翻译（可经 fmt.Errorf("%w") 包裹），
// 实现了 Localize(lang) 的错误类型用自己的实现，其余按 err.Error() 的原文查目录（含带格式动词的键，见 TP）。
func Localize(lang string, err error) string {
	if err == nil {
		return ""
	}
	var l interface{ Localize(lang string) string }
	if errors.As(err, &l) {
		return l.Localize(lang)
	}
	return TP(lang, err.Error())
}
