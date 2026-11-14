// @author: kevinkelly
// @license: Apache Licence
// @file: i18n_test.go
// @time: 2026/10/28

package slowlog

import (
	"strings"
	"testing"

	"dbtool/internal/i18n"
)

// 英文页面：模板里的文案走目录翻译；含行内标签的整句（重置 Redis 慢日志的确认说明）里实例名占位元素保留。
func TestEnglishPages(t *testing.T) {
	env, _ := newEnv(t)
	root := env.Login("root")
	for _, p := range []struct {
		path string
		keys []string
	}{
		{"/slowlogs", []string{"慢 SQL 总次数", "每日累计耗时", "采集器状态", "去智能调优 →"}},
		{"/slowquery", []string{"慢日志统计", "慢日志明细", "命令统计", "重置 Redis 慢日志", "在智能调优中打开"}},
	} {
		code, zh := root.Page(p.path)
		if code != 200 {
			t.Fatalf("%s = %d", p.path, code)
		}
		code, en := root.Page(p.path + "?lang=en")
		if code != 200 || !strings.Contains(en, `<html lang="en"`) {
			t.Fatalf("%s?lang=en = %d", p.path, code)
		}
		for _, k := range p.keys {
			if !strings.Contains(zh, k) {
				t.Errorf("中文页 %s 缺 %q", p.path, k)
			}
			if want := i18n.T(i18n.EN, k); want == k {
				t.Errorf("英文目录缺 %q", k)
			} else if !strings.Contains(en, want) {
				t.Errorf("英文页 %s 应有 %q", p.path, want)
			}
		}
	}
	// 中文页的确认说明与迁移前逐字一致，实例名占位元素带着 id
	_, zh := root.Page("/slowquery")
	if !strings.Contains(zh, `<p>将对实例 <b id="rq-mname"></b> 执行 <code>SLOWLOG RESET</code>，清空实例上的慢日志（重置前先采集入库）；不可撤销。</p>`) {
		t.Error("重置确认说明的中文输出变了")
	}
	_, en := root.Page("/slowquery?lang=en")
	if !strings.Contains(en, `<b id="rq-mname"></b>`) || !strings.Contains(en, "<code>SLOWLOG RESET</code>") {
		t.Error("英文页的确认说明应保留实例名占位元素和 <code>")
	}
}
