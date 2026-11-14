// @author: kevinkelly
// @license: Apache Licence
// @file: i18n_test.go
// @time: 2026/10/28

package i18n

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/fstest"
)

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"zh-CN": ZH, "zh": ZH, "ZH_cn": ZH, " zh-Hans ": ZH,
		"en": EN, "en-US": EN, "EN_gb": EN,
	} {
		if got, ok := Normalize(in); !ok || got != want {
			t.Errorf("Normalize(%q) = %q,%v，应为 %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "fr", "zh-TW", `en"><script>`, "../en"} {
		if got, ok := Normalize(in); ok {
			t.Errorf("Normalize(%q) 不该被接受，得到 %q", in, got)
		}
	}
}

func TestTranslateAndFallback(t *testing.T) {
	if got := T(EN, "首页"); got != "Home" {
		t.Errorf("en 首页 = %q", got)
	}
	if got := T(ZH, "首页"); got != "首页" {
		t.Errorf("zh 首页 = %q", got)
	}
	// 缺译回落中文原文；不认识的语言同样回落
	if got := T(EN, "目录里绝对没有的句子"); got != "目录里绝对没有的句子" {
		t.Errorf("缺译应回落原文：%q", got)
	}
	if got := T("fr", "首页"); got != "首页" {
		t.Errorf("未知语言应回落原文：%q", got)
	}
	// 占位符：译文与回落都要代入
	if got := T(EN, "请求失败（HTTP {status}）", "status", 502); got != "Request failed (HTTP 502)" {
		t.Errorf("en 占位符 = %q", got)
	}
	if got := T(ZH, "共 {n} 条", "n", 3); got != "共 3 条" {
		t.Errorf("zh 占位符 = %q", got)
	}
	// 参数里带花括号的值不会被二次代入
	if got := T(ZH, "{a}-{b}", "a", "{b}", "b", "x"); got != "{b}-x" {
		t.Errorf("参数不应被二次代入：%q", got)
	}
	// 奇数个参数：多余的忽略，不 panic
	if got := T(ZH, "共 {n} 条", "n"); got != "共 {n} 条" {
		t.Errorf("奇数参数 = %q", got)
	}
}

func TestContextSuffix(t *testing.T) {
	for in, want := range map[string]string{
		"关闭|dialog":    "关闭",
		"关闭":           "关闭",
		"审核 | 查询":      "审核 | 查询", // 带空格的竖线是正文，不是上下文
		"A|B":          "A|B",
		"启用|state_on":  "启用",
		"a|b|c.d-e_f1": "a|b",
		"中文|中文":        "中文|中文",
		"x|Upper":      "x|Upper",
		"含|竖线的句子":      "含|竖线的句子",
		"开启|switch-on": "开启",
	} {
		if got := Base(in); got != want {
			t.Errorf("Base(%q) = %q，应为 %q", in, got, want)
		}
	}
	if got := T(EN, "关闭|dialog"); got != "关闭" && got != "Close" {
		t.Errorf("上下文键缺译应回落去掉后缀的原文：%q", got)
	}
}

func TestLoadMergesFilesAndReportsConflicts(t *testing.T) {
	fsys := fstest.MapFS{
		"i18n/en/a.json":  {Data: []byte(`{"甲": "A", "乙": "B", "空": "  "}`)},
		"i18n/en/b.json":  {Data: []byte(`{"乙": "B", "丙": "C"}`)},
		"i18n/en/c.json":  {Data: []byte(`{"甲": "A2"}`)},
		"i18n/en/skip.md": {Data: []byte(`not json`)},
	}
	c, err := load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	m := c.msgs[EN]
	if m["甲"] != "A2" || m["乙"] != "B" || m["丙"] != "C" {
		t.Errorf("合并结果不对：%v", m)
	}
	if _, ok := m["空"]; ok {
		t.Error("空译文应当视为缺译")
	}
	if len(c.conflicts) != 1 {
		t.Errorf("应只有“甲”一处冲突：%v", c.conflicts)
	}
	// 没有该语言的目录：不报错，全部回落
	c2, err := load(fstest.MapFS{})
	if err != nil || len(c2.msgs) != 0 {
		t.Errorf("无目录应当是空目录：%v %v", c2, err)
	}
	// 目录损坏必须报错而不是悄悄吞掉
	if _, err := load(fstest.MapFS{"i18n/en/x.json": {Data: []byte(`{`)}}); err == nil {
		t.Error("坏 JSON 应当报错")
	}
}

func TestJSONAndETag(t *testing.T) {
	body, tag := JSON(EN)
	if len(body) < 10 || tag == "" || tag == `"empty"` {
		t.Fatalf("en 目录 / ETag 不应为空：%d %q", len(body), tag)
	}
	if b, tg := JSON(ZH); string(b) != "{}" || tg == tag {
		t.Errorf("中文不下发目录：%s %s", b, tg)
	}
	if _, tag2 := JSON(EN); tag2 != tag {
		t.Error("ETag 应稳定")
	}
}

func TestContextAndDefault(t *testing.T) {
	if got := FromContext(context.Background()); got != Default() {
		t.Errorf("无语言的 ctx 应取默认语言：%q", got)
	}
	if got := FromContext(WithLang(context.Background(), EN)); got != EN {
		t.Errorf("FromContext = %q", got)
	}
	SetDefault("en")
	defer SetDefault(ZH)
	if Default() != EN {
		t.Error("SetDefault(en) 未生效")
	}
	SetDefault("klingon") // 不认识的值忽略
	if Default() != EN {
		t.Error("不认识的默认语言应被忽略")
	}
}

func TestErrors(t *testing.T) {
	e := Errorf("实例 {name} 不存在", "name", "db1")
	if e.Error() != "实例 db1 不存在" {
		t.Errorf("Error() 恒为中文：%q", e.Error())
	}
	if got := Localize(ZH, e); got != "实例 db1 不存在" {
		t.Errorf("zh = %q", got)
	}
	// 经 %w 包裹后仍能本地化；底层错误也能穿透
	base := errors.New("底层")
	w := fmt.Errorf("外层: %w", Wrap(base, "请求失败（HTTP {status}）", "status", 500))
	if !errors.Is(w, base) {
		t.Error("Wrap 应保留底层错误")
	}
	if got := Localize(EN, w); got != "Request failed (HTTP 500)" {
		t.Errorf("en = %q", got)
	}
	// 普通错误按原文查目录
	if got := Localize(EN, errors.New("首页")); got != "Home" {
		t.Errorf("普通错误按原文翻译：%q", got)
	}
	if Localize(EN, nil) != "" {
		t.Error("nil 错误应得到空串")
	}
}

func TestTP(t *testing.T) {
	c, err := load(fstest.MapFS{"i18n/en/t.json": {Data: []byte(`{
		"实例 %s 不存在": "Instance %s does not exist",
		"共 %d 条，耗时 %.1f 秒": "%d rows in %.1f s",
		"用户 %s 对 %s 无权限": "User %[1]s has no access to %[2]s",
		"保存失败：%s": "Save failed: %s",
		"已通过": "Approved",
		"完成 100%% 了": "100%% done"
	}`)}})
	if err != nil {
		t.Fatal(err)
	}
	old := cat.Load()
	cat.Store(c)
	defer cat.Store(old)

	cases := map[string]string{
		"实例 db1 不存在":          "Instance db1 does not exist",
		"共 12 条，耗时 0.5 秒":     "12 rows in 0.5 s",
		"用户 bob 对 orders 无权限": "User bob has no access to orders",
		"已通过":                 "Approved",                                 // 精确匹配优先
		"保存失败：实例 db1 不存在":     "Save failed: Instance db1 does not exist", // 参数里的中文递归翻译
		"完成 100% 了":           "100% done",
		"没有对应译文的句子":           "没有对应译文的句子",
		"共 x 条，耗时 0.5 秒":      "共 x 条，耗时 0.5 秒", // %d 不匹配非数字：回落原文
	}
	for in, want := range cases {
		if got := TP(EN, in); got != want {
			t.Errorf("TP(%q) = %q，应为 %q", in, got, want)
		}
		if got := TP(ZH, in); got != in {
			t.Errorf("中文不翻译：TP(zh, %q) = %q", in, got)
		}
	}
	// 错误经 Localize 走同一条路径
	if got := Localize(EN, errors.New("实例 db2 不存在")); got != "Instance db2 does not exist" {
		t.Errorf("Localize 应走 TP：%q", got)
	}
}

func TestTPWrappedQuotedMultiline(t *testing.T) {
	c, err := load(fstest.MapFS{"i18n/en/t.json": {Data: []byte(`{
		"保存失败：%w": "Save failed: %w",
		"不认识的取值 %q，可选 %s": "Unknown value %q, allowed: %s",
		"执行出错：%s": "Execution error: %s",
		"第 %s 行 %s 出错": "Line %s: %s failed"
	}`)}})
	if err != nil {
		t.Fatal(err)
	}
	old := cat.Load()
	cat.Store(c)
	defer cat.Store(old)

	cases := map[string]string{
		"保存失败：连接超时":            "Save failed: 连接超时",
		`不认识的取值 "a\"b"，可选 x,y`: `Unknown value "a\"b", allowed: x,y`,
		"执行出错：第一行\n第二行":        "Execution error: 第一行\n第二行", // 末尾的动词可以带换行
		"第 3 行 abc 出错":         "Line 3: abc failed",
		"第 3\n行 abc 出错":        "第 3\n行 abc 出错", // 非末尾的动词不跨行
	}
	for in, want := range cases {
		if got := TP(EN, in); got != want {
			t.Errorf("TP(%q) = %q，应为 %q", in, got, want)
		}
	}
}
