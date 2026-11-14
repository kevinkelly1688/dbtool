// @author: kevinkelly
// @license: Apache Licence
// @file: lint_test.go
// @time: 2026/10/28

package i18n

import (
	_ "embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"dbtool/internal/i18n/extract"
	"dbtool/web"
)

// 本文件是界面多语言的“闸门”：
//
//  1. 已迁移的模板 / JS 里不许再出现没被 t / th / t() 包起来的中文；
//  2. 已迁移的 Go 文件里每个面向用户的中文字面量（日志除外）在目录里都有译文；
//  3. 代码里显式翻译调用用到的键，目录里都有译文；译文的占位符、行内标签与原文一致，且不含中文；
//  4. 目录文件之间不许对同一个键给出不同译文。
//
// pending.txt 列出尚未迁移的文件（模板 / JS 相对 web/，Go 相对仓库根），只许减不许增：
// 迁完一个文件就把它删掉；名单里已经干净的文件测试会要求你删掉它。
// 排查用的环境变量：
//
//	I18N_REGEN=1    按现状重写 pending.txt（提交前看一眼 diff）
//	I18N_SHOW=x     打印路径含 x 的模板 / JS 文件里没被包裹的中文
//	I18N_MISSING=x  打印路径含 x 的文件里缺英文译文的键

//go:embed pending.txt
var pendingTxt string

type srcFile struct{ name, body string }

func webFiles(t *testing.T) (tpls, js []srcFile) {
	t.Helper()
	err := fs.WalkDir(web.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		switch {
		case strings.HasPrefix(p, "templates/") && strings.HasSuffix(p, ".html"):
			b, _ := fs.ReadFile(web.FS, p)
			tpls = append(tpls, srcFile{p, string(b)})
		case strings.HasPrefix(p, "static/js/") && strings.HasSuffix(p, ".js"):
			b, _ := fs.ReadFile(web.FS, p)
			js = append(js, srcFile{p, string(b)})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return
}

// goFiles 返回纳入闸门的 Go 源文件（相对仓库根的路径 → 内容）。
func goFiles(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	root := "../../"
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := filepath.ToSlash(strings.TrimPrefix(filepath.ToSlash(p), root))
		if !extract.InGoScope(rel) {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func pendingSet() map[string]bool {
	m := map[string]bool{}
	for _, l := range strings.Split(pendingTxt, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			m[l] = true
		}
	}
	return m
}

func scanWeb(t *testing.T) map[string][]extract.Finding {
	tpls, js := webFiles(t)
	out := map[string][]extract.Finding{}
	for _, f := range tpls {
		out[f.name] = extract.ScanTemplate(f.name, f.body)
	}
	for _, f := range js {
		out[f.name] = extract.ScanJS(f.name, f.body)
	}
	return out
}

// requiredKeys 返回每个文件“需要有译文”的键：模板 / JS 里显式翻译调用的第一个参数；Go 文件里面向用户的中文字面量。
func requiredKeys(t *testing.T) map[string][]string {
	out := map[string][]string{}
	add := func(file string, keys []string) {
		seen := map[string]bool{}
		for _, k := range keys {
			if !seen[k] {
				seen[k] = true
				out[file] = append(out[file], k)
			}
		}
	}
	tpls, js := webFiles(t)
	for _, f := range tpls {
		add(f.name, extract.TemplateKeys(f.body))
	}
	for _, f := range js {
		add(f.name, extract.JSKeys(f.body))
	}
	for name, body := range goFiles(t) {
		var keys []string
		for _, l := range extract.GoLiterals(name, body) {
			if !l.Log {
				keys = append(keys, l.Text)
			}
		}
		add(name, keys)
	}
	return out
}

func missingByFile(t *testing.T, lang string) map[string][]string {
	msgs := Messages(lang)
	out := map[string][]string{}
	for f, keys := range requiredKeys(t) {
		for _, k := range keys {
			if _, ok := msgs[k]; !ok {
				out[f] = append(out[f], k)
			}
		}
		sort.Strings(out[f])
	}
	return out
}

func quoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = strconv.Quote(s)
	}
	return out
}

// 已迁移的模板 / JS 不许再有未包裹的中文；pending.txt 里的文件迁完必须移出名单（棘轮只收紧）。
func TestNoUnwrappedChinese(t *testing.T) {
	all := scanWeb(t)
	if os.Getenv("I18N_REGEN") != "" {
		var names []string
		for n, fs := range all {
			if len(fs) > 0 {
				names = append(names, n)
			}
		}
		for f, ks := range missingByFile(t, EN) { // 缺译的 Go 文件（模板 / JS 的缺译不进名单，必须补齐）
			if strings.HasSuffix(f, ".go") && len(ks) > 0 {
				names = append(names, f)
			}
		}
		sort.Strings(names)
		body := "# 尚未迁移到多语言的文件（模板 / JS 相对 web/，Go 相对仓库根）。只许减不许增；迁完一个文件就删掉对应行。\n" + strings.Join(names, "\n") + "\n"
		if err := os.WriteFile("pending.txt", []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Skipf("pending.txt 已重写：%d 个文件", len(names))
	}
	pend := pendingSet()
	var names []string
	for n := range all {
		names = append(names, n)
	}
	sort.Strings(names)
	if show := os.Getenv("I18N_SHOW"); show != "" {
		for _, n := range names {
			if strings.Contains(n, show) {
				for _, f := range all[n] {
					t.Logf("%s:%d: %s", f.File, f.Line, f.Text)
				}
			}
		}
	}
	for _, n := range names {
		fs := all[n]
		switch {
		case len(fs) > 0 && !pend[n]:
			show := fs
			if len(show) > 8 {
				show = show[:8]
			}
			var b strings.Builder
			for _, f := range show {
				b.WriteString(fmt.Sprintf("\n  %s:%d: %s", f.File, f.Line, f.Text))
			}
			t.Errorf("%s 有 %d 处未翻译的中文（用 t / th 包起来，或在行内注明 i18n-ignore）：%s", n, len(fs), b.String())
		case len(fs) == 0 && pend[n]:
			t.Errorf("%s 已经没有未翻译的中文，请把它从 internal/i18n/pending.txt 删掉", n)
		}
	}
	for n := range pend {
		if _, ok := all[n]; !ok && !strings.HasSuffix(n, ".go") {
			t.Errorf("pending.txt 里的 %s 不存在，请删掉这一行", n)
		}
	}
}

var (
	phRe   = regexp.MustCompile(`\{[A-Za-z_][A-Za-z0-9_]*\}`)
	tagRe  = regexp.MustCompile(`</?[a-zA-Z][a-zA-Z0-9]*/?>`)
	verbRe = regexp.MustCompile(`%(?:\[\d+\])?[-+# 0]*\d*(?:\.\d+)?[sdvqfgxtTw]`)
)

func sortedMatches(re *regexp.Regexp, s string) string {
	ms := re.FindAllString(s, -1)
	sort.Strings(ms)
	return strings.Join(ms, ",")
}

// verbSig 取格式动词的类型序列（忽略 %[n] 重排），原文与译文的动词多重集必须一致。
func verbSig(s string) string {
	var vs []string
	for _, m := range verbRe.FindAllString(s, -1) {
		v := m[len(m)-1:]
		if v == "w" { // %w 与 %s 等价
			v = "s"
		}
		vs = append(vs, v)
	}
	sort.Strings(vs)
	return strings.Join(vs, "")
}

// 用到的键必须有英文译文（pending.txt 里列出的 Go 文件除外）；译文的占位符 / 行内标签 / 格式动词与原文一致，且不含中文。
func TestCatalogComplete(t *testing.T) {
	if c := Conflicts(); len(c) > 0 {
		t.Errorf("目录文件之间的译文冲突：%s", "\n  "+strings.Join(c, "\n  "))
	}
	pend := pendingSet()
	for _, l := range Langs {
		if l.Code == ZH {
			continue
		}
		miss := missingByFile(t, l.Code)
		if show := os.Getenv("I18N_MISSING"); show != "" {
			for f, ks := range miss {
				if strings.Contains(f, show) {
					for _, k := range ks {
						t.Logf("%s\t%s", f, strconv.Quote(k))
					}
				}
			}
		}
		var files []string
		for f := range miss {
			files = append(files, f)
		}
		sort.Strings(files)
		for _, f := range files {
			if pend[f] {
				continue
			}
			t.Errorf("%s 缺 %d 条 %s 译文（加到 web/i18n/%s/*.json）：%s", f, len(miss[f]), l.Code, l.Code, "\n  "+strings.Join(quoteAll(miss[f]), "\n  "))
		}
		for f := range pend {
			if strings.HasSuffix(f, ".go") && len(miss[f]) == 0 {
				t.Errorf("%s 的翻译键已齐全，请把它从 internal/i18n/pending.txt 删掉", f)
			}
		}
		for k, v := range Messages(l.Code) {
			if sortedMatches(phRe, Base(k)) != sortedMatches(phRe, v) {
				t.Errorf("%s: %q 的 {占位符} 与译文 %q 不一致", l.Code, k, v)
			}
			if sortedMatches(tagRe, k) != sortedMatches(tagRe, v) {
				t.Errorf("%s: %q 的行内标签与译文 %q 不一致", l.Code, k, v)
			}
			if verbSig(k) != verbSig(v) {
				t.Errorf("%s: %q 的格式动词（%%s %%d …）与译文 %q 不一致", l.Code, k, v)
			}
			if extract.HasHan(v) {
				t.Errorf("%s: %q 的译文里还有中文：%q", l.Code, k, v)
			}
		}
	}
}
