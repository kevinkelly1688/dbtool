// @author: kevinkelly
// @license: Apache Licence
// @file: merge_test.go
// @time: 2026/10/28

package export

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/xuri/excelize/v2"
)

var (
	mergeCols = []string{"id", "name", "note"}
	mergeA    = Result{Columns: mergeCols, Dialect: "mysql", Rows: [][]any{
		{int64(1), "alice", nil},
		{int64(2), `b"ob`, "x,y"},
	}}
	mergeB = Result{Columns: mergeCols, Dialect: "mssql", Rows: [][]any{
		{int64(3), "中文", time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC)},
	}}
)

func buildMerged(t *testing.T, format string, parts ...Result) []byte {
	t.Helper()
	var buf bytes.Buffer
	mw, err := NewMergeWriter(&buf, format)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range parts {
		if err := mw.Add(p); err != nil {
			t.Fatalf("%s Add(%d): %v", format, i, err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestMergedFileName(t *testing.T) {
	if got := MergedFileName(123, "csv"); got != "merged_123.csv" {
		t.Errorf("合并文件名 = %q", got)
	}
}

func TestNewMergeWriterRejectsUnknownFormat(t *testing.T) {
	if _, err := NewMergeWriter(&bytes.Buffer{}, "pdf"); err == nil {
		t.Error("不支持的格式应报错")
	}
}

// 没有实例名称列：合并文件就是各实例导出的数据依次接在一起，表头只有一份。
func TestMergeCSV(t *testing.T) {
	got := string(buildMerged(t, CSV, mergeA, mergeB))
	want := "\"id\",\"name\",\"note\"\r\n" +
		"\"1\",\"alice\",\"null\"\r\n" +
		"\"2\",\"b\"\"ob\",\"x,y\"\r\n" +
		"\"3\",\"中文\",\"2026-10-01 08:30:00\"\r\n"
	if got != want {
		t.Errorf("CSV 合并结果不对：\n%q\n期望\n%q", got, want)
	}
}

func TestMergeJSON(t *testing.T) {
	out := buildMerged(t, JSON, mergeA, mergeB)
	var objs []map[string]any
	if err := json.Unmarshal(out, &objs); err != nil {
		t.Fatalf("JSON 不合法: %v\n%s", err, out)
	}
	if len(objs) != 3 || len(objs[0]) != 3 || objs[0]["name"] != "alice" || objs[2]["name"] != "中文" || objs[0]["note"] != nil {
		t.Errorf("JSON 内容不对: %v", objs)
	}
	if !strings.HasPrefix(string(out), "[\n  {\n    \"id\": 1,\n    \"name\": \"alice\",") {
		t.Errorf("JSON 的键顺序不对：\n%s", out)
	}
}

func TestMergeXML(t *testing.T) {
	out := buildMerged(t, XML, mergeA, mergeB)
	s := string(out)
	for _, want := range []string{
		"<fields><field>id</field><field>name</field><field>note</field></fields>",
		`<row id="1"><column-1>1</column-1><column-2>alice</column-2><column-3>(null)</column-3></row>`,
		`<row id="3"><column-1>3</column-1><column-2>中文</column-2>`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("XML 里没有 %q:\n%s", want, s)
		}
	}
	if err := xml.Unmarshal(out, new(struct{})); err != nil {
		t.Errorf("XML 不合法: %v", err)
	}
	if strings.Count(s, "<fields>") != 1 {
		t.Error("字段列表只应写一次")
	}
}

func TestMergeSQL(t *testing.T) {
	got := string(buildMerged(t, SQL, mergeA, mergeB))
	want := "INSERT INTO your_table_name (id, name, note) VALUES (1, 'alice', NULL);\n" +
		"INSERT INTO your_table_name (id, name, note) VALUES (2, 'b\"ob', 'x,y');\n" +
		"INSERT INTO your_table_name (id, name, note) VALUES (3, '中文', '2026-10-01 08:30:00');\n"
	if got != want {
		t.Errorf("SQL 合并结果不对：\n%s\n期望\n%s", got, want)
	}
}

func readXLSX(t *testing.T, data []byte) [][]string {
	t.Helper()
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := f.GetRows("Sheet1")
	if err != nil {
		t.Fatal(err)
	}
	for i := range rows { // GetRows 会丢掉行末的空单元格，补齐到表头的宽度再比较
		for len(rows[i]) < len(rows[0]) {
			rows[i] = append(rows[i], "")
		}
	}
	return rows
}

func TestMergeXLSX(t *testing.T) {
	rows := readXLSX(t, buildMerged(t, XLSX, mergeA, mergeB))
	if len(rows) != 4 {
		t.Fatalf("行数 = %d，期望 4（表头 + 3 行）: %v", len(rows), rows)
	}
	if strings.Join(rows[0], "|") != "id|name|note" || strings.Join(rows[1], "|") != "1|alice|" || strings.Join(rows[3], "|") != "3|中文|2026-10-01 08:30:00" {
		t.Errorf("Excel 内容不对: %v", rows)
	}
}

// 只有一个实例时，合并文件与单实例导出的文件完全一样（Excel 比单元格）：合并只是把数据接在一起，没有额外的列或格式。
func TestMergeSingleInstanceEqualsPlainExport(t *testing.T) {
	for _, format := range Formats {
		t.Run(format, func(t *testing.T) {
			var plain bytes.Buffer
			if err := Write(&plain, format, mergeA); err != nil {
				t.Fatal(err)
			}
			merged := buildMerged(t, format, mergeA)
			if format == XLSX {
				p, m := readXLSX(t, plain.Bytes()), readXLSX(t, merged)
				if len(p) != len(m) {
					t.Fatalf("行数 %d，期望 %d", len(m), len(p))
				}
				for i := range p {
					if strings.Join(p[i], "|") != strings.Join(m[i], "|") {
						t.Errorf("第 %d 行 %v，期望 %v", i+1, m[i], p[i])
					}
				}
				return
			}
			if !bytes.Equal(plain.Bytes(), merged) {
				t.Errorf("合并结果与单实例导出不同：\n%q\n期望\n%q", merged, plain.Bytes())
			}
		})
	}
}

// 多个实例合并 = 把所有行放进一个结果再导出一次。
func TestMergeEqualsOneBigExport(t *testing.T) {
	all := Result{Columns: mergeCols, Dialect: "mysql", Rows: append(append([][]any{}, mergeA.Rows...), mergeB.Rows...)}
	for _, format := range []string{CSV, JSON, XML, SQL} {
		var plain bytes.Buffer
		if err := Write(&plain, format, all); err != nil {
			t.Fatal(err)
		}
		// SQL 的转义按实例的类型：这里两个实例都用 mysql，才能与“一次导出”逐字节比较
		b := mergeB
		b.Dialect = "mysql"
		if got := buildMerged(t, format, mergeA, b); !bytes.Equal(got, plain.Bytes()) {
			t.Errorf("%s：合并结果与把所有行一次导出不同：\n%q\n期望\n%q", format, got, plain.Bytes())
		}
	}
}

func TestMergeColumnsMismatchWritesNothing(t *testing.T) {
	for _, format := range Formats {
		var buf bytes.Buffer
		mw, _ := NewMergeWriter(&buf, format)
		if err := mw.Add(mergeA); err != nil {
			t.Fatal(err)
		}
		before := buf.Len()
		err := mw.Add(Result{Columns: []string{"id", "name", "other"}, Rows: [][]any{{int64(9), "z", "q"}}})
		var ce *ColumnsError
		if !errors.As(err, &ce) || !Recoverable(err) {
			t.Fatalf("%s: 列不一致应返回 *ColumnsError，实得 %v", format, err)
		}
		if !strings.Contains(err.Error(), "id, name, note") || !strings.Contains(err.Error(), "id, name, other") {
			t.Errorf("%s: 错误信息要写明两边的列: %v", format, err)
		}
		if buf.Len() != before {
			t.Errorf("%s: 列不一致时不应写入任何内容", format)
		}
		// 列个数不同也不行
		if err := mw.Add(Result{Columns: []string{"id", "name"}, Rows: [][]any{{int64(9), "z"}}}); !errors.As(err, &ce) {
			t.Errorf("%s: 列个数不同应返回 *ColumnsError: %v", format, err)
		}
		// 后面的实例照常能写进去
		if err := mw.Add(mergeB); err != nil {
			t.Errorf("%s: 列不一致之后写下一个实例应成功: %v", format, err)
		}
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}
		if mw.Rows() != 3 {
			t.Errorf("%s: 数据行数 = %d，期望 3", format, mw.Rows())
		}
	}
}

func TestMergeColumnsCompareIgnoresCase(t *testing.T) {
	var buf bytes.Buffer
	mw, _ := NewMergeWriter(&buf, CSV)
	_ = mw.Add(Result{Columns: []string{"ID", "Name"}, Rows: [][]any{{"1", "x"}}})
	if err := mw.Add(Result{Columns: []string{"id", "name"}, Rows: [][]any{{"2", "y"}}}); err != nil {
		t.Errorf("列名只有大小写不同应视为一致: %v", err)
	}
	if !strings.HasPrefix(buf.String(), "\"ID\",\"Name\"\r\n") {
		t.Errorf("表头以第一个实例为准: %q", buf.String())
	}
	if !SameColumns([]string{"A", "b"}, []string{"a", "B"}) || SameColumns([]string{"a"}, []string{"a", "b"}) {
		t.Error("SameColumns 判断不对")
	}
}

func TestMergeEmptyResults(t *testing.T) {
	empty := Result{Columns: mergeCols}
	for format, want := range map[string]string{
		JSON: "[]",
		CSV:  "\"id\",\"name\",\"note\"\r\n",
		SQL:  "",
	} {
		if got := string(buildMerged(t, format, empty, empty)); got != want {
			t.Errorf("%s: 没有数据行时的内容 = %q，期望 %q", format, got, want)
		}
	}
	if x := string(buildMerged(t, XML, empty)); !strings.Contains(x, "</fields><data></data></tabledata>") {
		t.Errorf("XML 没有数据行时应只有字段列表: %s", x)
	}
	if rows := readXLSX(t, buildMerged(t, XLSX, empty)); len(rows) != 1 {
		t.Errorf("Excel 没有数据行时应只有表头: %v", rows)
	}
	// 一次都没有成功 Add：什么也不写
	var buf bytes.Buffer
	mw, _ := NewMergeWriter(&buf, JSON)
	if err := mw.Close(); err != nil || buf.Len() != 0 {
		t.Errorf("没有 Add 过就 Close 不应写任何内容: %v %q", err, buf.String())
	}
}

func TestMergeXLSXRowLimit(t *testing.T) {
	var buf bytes.Buffer
	mw, _ := NewMergeWriter(&buf, XLSX)
	one := Result{Columns: []string{"v"}, Rows: [][]any{{"1"}}}
	if err := mw.Add(one); err != nil {
		t.Fatal(err)
	}
	mw.xrow = MaxXLSXRows - 2 // 还能放 2 行
	err := mw.Add(Result{Columns: []string{"v"}, Rows: [][]any{{"1"}, {"2"}, {"3"}}})
	var le *LimitError
	if !errors.As(err, &le) || !Recoverable(err) || !strings.Contains(err.Error(), "1048576") {
		t.Fatalf("超过 Excel 行数上限应返回 *LimitError: %v", err)
	}
	if err := mw.Add(Result{Columns: []string{"v"}, Rows: [][]any{{"1"}, {"2"}}}); err != nil {
		t.Errorf("刚好放得下应成功: %v", err)
	}
}

func TestRecoverableOtherErrors(t *testing.T) {
	if Recoverable(errors.New("disk full")) || Recoverable(nil) {
		t.Error("普通错误不是可跳过的错误")
	}
}

// 追加后的文件必须与“两个实例一次写完”逐字节相同（Excel 比单元格内容）。
func TestAppendMergedEqualsDirectBuild(t *testing.T) {
	for _, format := range Formats {
		t.Run(format, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "m."+format)
			if err := AppendMerged(path, format, mergeA); err != nil { // 文件不存在：新建
				t.Fatal(err)
			}
			if err := AppendMerged(path, format, mergeB); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(path)
			want := buildMerged(t, format, mergeA, mergeB)
			if format == XLSX {
				g, w := readXLSX(t, got), readXLSX(t, want)
				if len(g) != len(w) {
					t.Fatalf("Excel 行数 %d，期望 %d", len(g), len(w))
				}
				for i := range w {
					if strings.Join(g[i], "|") != strings.Join(w[i], "|") {
						t.Errorf("Excel 第 %d 行 %v，期望 %v", i+1, g[i], w[i])
					}
				}
				return
			}
			if !bytes.Equal(got, want) {
				t.Errorf("追加结果与一次写完不同：\n%s\n期望\n%s", got, want)
			}
		})
	}
}

func TestAppendMergedMismatchLeavesFileUntouched(t *testing.T) {
	bad := Result{Columns: []string{"id", "name"}, Rows: [][]any{{int64(1), "z"}}}
	for _, format := range Formats {
		t.Run(format, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "m."+format)
			if err := AppendMerged(path, format, mergeA); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			err := AppendMerged(path, format, bad)
			var ce *ColumnsError
			if !errors.As(err, &ce) {
				t.Fatalf("列不一致应返回 *ColumnsError: %v", err)
			}
			if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
				t.Error("列不一致时文件不应被改动")
			}
			if _, err := os.Stat(path + ".tmp"); err == nil {
				t.Error("不应留下临时文件")
			}
		})
	}
}

func TestAppendMergedAfterEmptyFirstResult(t *testing.T) {
	empty := Result{Columns: mergeCols}
	for _, format := range Formats {
		t.Run(format, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "m."+format)
			if err := AppendMerged(path, format, empty); err != nil { // 第一个实例没有数据
				t.Fatal(err)
			}
			if err := AppendMerged(path, format, mergeB); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(path)
			want := buildMerged(t, format, mergeB)
			if format == XLSX {
				if g := readXLSX(t, got); len(g) != 2 || g[1][0] != "3" {
					t.Errorf("Excel 内容 %v", g)
				}
				return
			}
			if !bytes.Equal(got, want) {
				t.Errorf("第一个实例为空时追加结果不对:\n%s\n期望\n%s", got, want)
			}
		})
	}
}

func TestAppendMergedXMLIDsContinue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.xml")
	_ = AppendMerged(path, XML, mergeA)
	_ = AppendMerged(path, XML, mergeB)
	_ = AppendMerged(path, XML, mergeB)
	data, _ := os.ReadFile(path)
	s := string(data)
	for _, id := range []string{`<row id="1">`, `<row id="2">`, `<row id="3">`, `<row id="4">`} {
		if strings.Count(s, id) != 1 {
			t.Errorf("%s 应只出现一次:\n%s", id, s)
		}
	}
	if err := xml.Unmarshal(data, new(struct{})); err != nil {
		t.Errorf("追加后 XML 不合法: %v", err)
	}
}

func TestAppendMergedRejectsBrokenFile(t *testing.T) {
	dir := t.TempDir()
	for format, content := range map[string]string{
		JSON: "not json at all",
		XML:  "<tabledata><fields><field>id</field><field>name</field><field>note</field></fields><data></data>",
		SQL:  "select 1;\n",
	} {
		path := filepath.Join(dir, "bad."+format)
		_ = os.WriteFile(path, []byte(content), 0o600)
		if err := AppendMerged(path, format, mergeA); err == nil {
			t.Errorf("%s: 内容格式不对的文件应拒绝追加", format)
		}
		if b, _ := os.ReadFile(path); string(b) != content {
			t.Errorf("%s: 拒绝追加时文件不应被改动", format)
		}
	}
}

func TestAppendCSVWithoutTrailingNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.csv")
	_ = os.WriteFile(path, []byte("\"id\",\"name\",\"note\"\r\n\"1\",\"x\",\"y\""), 0o600)
	if err := AppendMerged(path, CSV, mergeB); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "\"y\"\r\n\"3\",") {
		t.Errorf("文件末尾没有换行时，追加的行应另起一行:\n%q", b)
	}
}

func TestCountOccurrencesAcrossChunks(t *testing.T) {
	data := strings.Repeat("ab<row id=\"x", 1000) + "<row id=\""
	want := strings.Count(data, `<row id="`)
	got, err := countOccurrences(strings.NewReader(data), []byte(`<row id="`))
	if err != nil || got != want {
		t.Errorf("计数 = %d（%v），期望 %d", got, err, want)
	}
	got, err = countOccurrences(iotest.OneByteReader(strings.NewReader(data)), []byte(`<row id="`))
	if err != nil || got != want {
		t.Errorf("每次只读 1 字节时计数 = %d（%v），期望 %d", got, err, want)
	}
}
