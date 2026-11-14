// @author: kevinkelly
// @license: Apache Licence
// @file: main.go
// @time: 2026/10/28

// dbt-i18n 是界面多语言的开发辅助命令（不随产品发布）：
//
//	dbt-i18n missing [-root .] [-lang en] [路径片段 ...]   列出缺译文的键，输出可直接填写的 JSON 骨架（值为空）
//	dbt-i18n stats   [-root .] [-lang en]                  按目录统计：文件数 / 缺译键数
//
// “缺译”的口径与 internal/i18n 的闸门测试一致：模板 / JS 里显式翻译调用的键，以及 Go 文件里面向用户的中文字面量（日志除外）。
// 骨架填好译文后放进 web/i18n/<语言>/<模块>.json；值为空的条目按缺译处理。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"dbtool/internal/i18n"
	"dbtool/internal/i18n/extract"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd := os.Args[1]
	fl := flag.NewFlagSet(cmd, flag.ExitOnError)
	root := fl.String("root", ".", "仓库根目录")
	lang := fl.String("lang", i18n.EN, "目标语言")
	_ = fl.Parse(os.Args[2:])
	msgs := i18n.Messages(*lang)

	need, err := required(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	switch cmd {
	case "missing":
		out := map[string]string{}
		for file, keys := range need {
			if !matches(file, fl.Args()) {
				continue
			}
			for _, k := range keys {
				if _, ok := msgs[k]; !ok {
					out[k] = ""
				}
			}
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
	case "stats":
		type row struct{ files, missing int }
		byDir := map[string]*row{}
		for file, keys := range need {
			d := filepath.ToSlash(filepath.Dir(file))
			r := byDir[d]
			if r == nil {
				r = &row{}
				byDir[d] = r
			}
			miss := 0
			for _, k := range keys {
				if _, ok := msgs[k]; !ok {
					miss++
				}
			}
			if miss > 0 {
				r.files++
				r.missing += miss
			}
		}
		var dirs []string
		for d, r := range byDir {
			if r.missing > 0 {
				dirs = append(dirs, d)
			}
		}
		sort.Slice(dirs, func(i, j int) bool { return byDir[dirs[i]].missing > byDir[dirs[j]].missing })
		total := 0
		for _, d := range dirs {
			fmt.Printf("%6d 键 %4d 文件  %s\n", byDir[d].missing, byDir[d].files, d)
			total += byDir[d].missing
		}
		fmt.Printf("%6d 键（合计，按文件计，同一个键在多个文件里重复计数）\n", total)
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "用法: dbt-i18n missing|stats [-root .] [-lang en] [路径片段 ...]")
	os.Exit(2)
}

func matches(file string, pats []string) bool {
	if len(pats) == 0 {
		return true
	}
	for _, p := range pats {
		if strings.Contains(file, p) {
			return true
		}
	}
	return false
}

// required 返回每个文件里需要有译文的键（口径同闸门测试）。
func required(root string) (map[string][]string, error) {
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
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		var web string
		switch {
		case strings.HasPrefix(rel, "web/templates/") && strings.HasSuffix(rel, ".html"):
			web = strings.TrimPrefix(rel, "web/")
		case strings.HasPrefix(rel, "web/static/js/") && strings.HasSuffix(rel, ".js"):
			web = strings.TrimPrefix(rel, "web/")
		case extract.InGoScope(rel):
		default:
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		switch {
		case strings.HasSuffix(rel, ".html"):
			add(web, extract.TemplateKeys(string(b)))
		case strings.HasSuffix(rel, ".js"):
			add(web, extract.JSKeys(string(b)))
		default:
			var keys []string
			for _, l := range extract.GoLiterals(rel, string(b)) {
				if !l.Log {
					keys = append(keys, l.Text)
				}
			}
			add(rel, keys)
		}
		return nil
	})
	return out, err
}
