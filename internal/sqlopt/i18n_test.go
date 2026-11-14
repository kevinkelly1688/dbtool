// @author: kevinkelly
// @license: Apache Licence
// @file: i18n_test.go
// @time: 2026/10/28

package sqlopt

import (
	"strings"
	"testing"

	"dbtool/internal/i18n"
)

// 英文页面：模板里的文案走目录翻译，中文页保持原样。
func TestEnglishPages(t *testing.T) {
	h := newHTTPEnv(t)
	root := h.Login("root")
	for _, p := range []struct {
		path string
		keys []string
	}{
		{"/sqlanalyze", []string{"分析选项", "统计汇总", "最近报告", "尚未选择文件"}},
		{"/sqladvisor", []string{"优化方式", "获取优化建议", "暂无结果", "显示执行计划"}},
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
}
