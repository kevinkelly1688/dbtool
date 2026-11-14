// @author: kevinkelly
// @license: Apache Licence
// @file: doc_test.go
// @time: 2026/10/28

package settings

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// docs/settings-reference.md 由登记表生成：登记表是唯一事实来源，文档不手写。
// 改了登记表后运行：UPDATE_DOCS=1 go test ./internal/settings -run TestReferenceDocUpToDate

func renderReference() string {
	var b strings.Builder
	b.WriteString("# 系统设置参考\n\n")
	b.WriteString("> 本文由 `internal/settings/registry.go` 生成，**不要手改**（改完登记表后运行 `UPDATE_DOCS=1 go test ./internal/settings -run TestReferenceDocUpToDate`）。\n")
	b.WriteString("> 设计与规则见 [settings-design.md](settings-design.md)；页面入口：系统管理 › 系统配置（仅超级用户）；命令行：`dbt-admin settings`。\n\n")
	for _, g := range Groups {
		fmt.Fprintf(&b, "## %s（`%s`）\n\n%s\n\n", g.Label, g.Key, g.Desc)
		b.WriteString("| 键 | 名称 | 类型 | 默认值 | 取值 | 说明 |\n|---|---|---|---|---|---|\n")
		for _, d := range defs {
			if d.Group != g.Key {
				continue
			}
			def := d.Default
			if def == "" {
				def = "（空）"
			}
			if d.Type == TypeSecret {
				def = "—"
			}
			var vals []string
			switch {
			case d.Type == TypeEnum:
				for _, o := range d.Options {
					vals = append(vals, "`"+o.Value+"`")
				}
			case d.Ranged:
				vals = append(vals, strconv.FormatFloat(d.Min, 'f', -1, 64)+" ~ "+strconv.FormatFloat(d.Max, 'f', -1, 64))
			}
			if d.Unit != "" {
				vals = append(vals, d.Unit)
			}
			note := d.Help
			if d.Risk != "" {
				note += " ⚠ " + d.Risk
			}
			if d.Locked != "" {
				note += " 页面只读：" + d.Locked
			}
			if d.Hidden {
				note += " 内部状态，页面不显示。"
			}
			cell := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "|", "\\|"), "\n", " ") }
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s | %s |\n", d.Key, cell(d.Label), d.Type, cell(def), cell(strings.Join(vals, " ")), cell(strings.TrimSpace(note)))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func TestReferenceDocUpToDate(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "settings-reference.md")
	want := renderReference()
	if os.Getenv("UPDATE_DOCS") == "1" {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("缺少 docs/settings-reference.md：UPDATE_DOCS=1 go test ./internal/settings -run TestReferenceDocUpToDate 生成")
	}
	if strings.ReplaceAll(string(got), "\r\n", "\n") != want {
		t.Fatal("docs/settings-reference.md 与登记表不一致：UPDATE_DOCS=1 go test ./internal/settings -run TestReferenceDocUpToDate 重新生成")
	}
}
