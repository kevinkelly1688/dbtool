// @author: kevinkelly
// @license: Apache Licence
// @file: localize_test.go
// @time: 2026/10/28

package localize

import (
	"encoding/json"
	"reflect"
	"testing"

	"dbtool/internal/i18n"
)

// 中文请求原样返回；英文请求翻译服务端生成的文案，skipKeys 子树与非字符串值保持原样，多行文案逐行翻译。
func TestValue(t *testing.T) {
	const key = "巡检报告不存在"
	want := i18n.TP(i18n.EN, key)
	if want == key {
		t.Skip("英文目录里没有测试用的键")
	}
	in := map[string]any{
		"title": key,
		"sql":   key,
		"list":  []string{key, "用户数据"},
		"notes": "加载中…\n暂无结果",
		"n":     int64(9007199254740993), // 超过 float64 精度的整数不能被改写
		"ok":    true,
	}
	if got := Value(i18n.ZH, in); !reflect.DeepEqual(got, in) {
		t.Errorf("中文应原样返回: %v", got)
	}
	got, ok := Value(i18n.EN, in, "sql").(map[string]any)
	if !ok {
		t.Fatalf("英文应返回 JSON 树")
	}
	if got["title"] != want {
		t.Errorf("title = %v，期望 %q", got["title"], want)
	}
	if got["sql"] != key {
		t.Errorf("skipKeys 里的键不应翻译: %v", got["sql"])
	}
	if l := got["list"].([]any); l[0] != want || l[1] != "用户数据" {
		t.Errorf("list = %v", l)
	}
	if l, r := i18n.TP(i18n.EN, "加载中…"), i18n.TP(i18n.EN, "暂无结果"); l == "加载中…" || r == "暂无结果" || got["notes"] != l+"\n"+r {
		t.Errorf("多行文案应逐行翻译: %q", got["notes"])
	}
	raw, _ := json.Marshal(got)
	var back map[string]any
	_ = json.Unmarshal(raw, &back)
	if n, ok := got["n"].(json.Number); !ok || n.String() != "9007199254740993" {
		t.Errorf("大整数被改写: %v", got["n"])
	}
	if got["ok"] != true {
		t.Errorf("布尔值被改写: %v", got["ok"])
	}
	// 无法 JSON 编码的值原样返回
	ch := make(chan int)
	if v := Value(i18n.EN, ch); v != any(ch) {
		t.Error("无法编码时应原样返回")
	}
}
