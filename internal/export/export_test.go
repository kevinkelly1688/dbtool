// @author: kevinkelly
// @license: Apache Licence
// @file: export_test.go
// @time: 2026/10/28

package export

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
)

var sample = Result{
	Columns: []string{"id", "name", "note", "created"},
	Rows: [][]any{
		{int64(1), "张三", "he said \"hi\", ok", time.Date(2026, 10, 6, 9, 30, 0, 0, time.UTC)},
		{int64(2), nil, "it's <b>&\n多行", nil},
		{3.5, []byte("bytes"), "", true},
	},
	Dialect: "mysql",
}

func render(t *testing.T, format string, r Result) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := Write(&b, format, r); err != nil {
		t.Fatalf("%s: %v", format, err)
	}
	return b.Bytes()
}

func TestCSV(t *testing.T) {
	out := render(t, CSV, sample)
	recs, err := csv.NewReader(bytes.NewReader(out)).ReadAll()
	if err != nil {
		t.Fatalf("标准库应能读回: %v\n%s", err, out)
	}
	if len(recs) != 4 || recs[0][1] != "name" || recs[1][2] != `he said "hi", ok` || recs[2][1] != "null" || recs[2][2] != "it's <b>&\n多行" || recs[3][0] != "3.5" {
		t.Errorf("CSV 内容: %q", recs)
	}
	// 每个字段都加引号
	if !strings.HasPrefix(string(out), `"id","name","note","created"`+"\r\n") || !strings.Contains(string(out), `"1","张三"`) {
		t.Errorf("应全部加引号: %q", out[:60])
	}
}

func TestJSON(t *testing.T) {
	out := render(t, JSON, sample)
	var rows []map[string]any
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatalf("应是合法 JSON: %v\n%s", err, out)
	}
	if len(rows) != 3 || rows[0]["name"] != "张三" || rows[0]["created"] != "2026-10-06 09:30:00" || rows[1]["name"] != nil || rows[2]["name"] != "bytes" || rows[0]["id"].(float64) != 1 {
		t.Errorf("JSON 内容: %v", rows)
	}
	if !strings.Contains(string(out), "张三") || strings.Contains(string(out), `\u`) {
		t.Errorf("不应转义中文: %s", out)
	}
	if !strings.Contains(string(out), "<b>&") {
		t.Error("< > & 不应被转义")
	}
	// 键顺序同列顺序
	if strings.Index(string(out), `"id"`) > strings.Index(string(out), `"name"`) || strings.Index(string(out), `"name"`) > strings.Index(string(out), `"note"`) {
		t.Errorf("键顺序: %s", out)
	}
	if got := string(render(t, JSON, Result{Columns: []string{"a"}})); got != "[]" {
		t.Errorf("空结果: %q", got)
	}
}

func TestXML(t *testing.T) {
	out := render(t, XML, sample)
	var doc struct {
		Fields []string `xml:"fields>field"`
		Rows   []struct {
			ID   string `xml:"id,attr"`
			Cols []struct {
				XMLName xml.Name
				Text    string `xml:",chardata"`
			} `xml:",any"`
		} `xml:"data>row"`
	}
	if err := xml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("应是合法 XML: %v\n%s", err, out)
	}
	if len(doc.Fields) != 4 || len(doc.Rows) != 3 || doc.Rows[1].ID != "2" || doc.Rows[1].Cols[1].Text != "(null)" || doc.Rows[1].Cols[2].Text != "it's <b>&\n多行" || doc.Rows[0].Cols[0].XMLName.Local != "column-1" {
		t.Errorf("XML 内容: %+v", doc)
	}
	// 控制字符不会让文件无法解析
	bad := render(t, XML, Result{Columns: []string{"a"}, Rows: [][]any{{"x\x00y\x1bz"}}})
	if err := xml.Unmarshal(bad, new(struct{})); err != nil {
		t.Errorf("含控制字符也应是合法 XML: %v", err)
	}
}

func TestXLSX(t *testing.T) {
	out := render(t, XLSX, sample)
	f, err := excelize.OpenReader(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("应是合法 xlsx: %v", err)
	}
	defer f.Close()
	rows, err := f.GetRows("Sheet1")
	if err != nil || len(rows) != 4 || rows[0][0] != "id" || rows[1][1] != "张三" || rows[1][3] != "2026-10-06 09:30:00" || rows[2][2] != "it's <b>&\n多行" {
		t.Errorf("xlsx 内容: %v %v", rows, err)
	}
	if len(rows[2]) > 1 && rows[2][1] != "" {
		t.Errorf("NULL 写成空串: %q", rows[2][1])
	}
	if err := Write(io.Discard, XLSX, Result{Columns: []string{"a"}, Rows: make([][]any, MaxXLSXRows)}); err == nil {
		t.Error("超过 Excel 行数上限应报错")
	}
}

func TestSQLFormat(t *testing.T) {
	out := string(render(t, SQL, sample))
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if lines[0] != `INSERT INTO your_table_name (id, name, note, created) VALUES (1, '张三', 'he said "hi", ok', '2026-10-06 09:30:00');` {
		t.Errorf("第一行: %s", lines[0])
	}
	if !strings.Contains(out, "(2, NULL, 'it''s <b>&") {
		t.Errorf("NULL 与单引号转义: %s", out)
	}
	if !strings.Contains(out, "(3.5, 'bytes', NULL, 1);") {
		t.Errorf("空串写成 NULL、数字原样: %s", out)
	}
	if !strings.Contains(string(render(t, SQL, Result{Columns: []string{"a"}, Rows: [][]any{{`a\b`}}, Dialect: "mysql"})), `'a\\b'`) {
		t.Error("MySQL 方言要转义反斜杠")
	}
	if !strings.Contains(string(render(t, SQL, Result{Columns: []string{"a"}, Rows: [][]any{{`a\b`}}, Dialect: "mssql"})), `'a\b'`) {
		t.Error("SQL Server 方言不转义反斜杠")
	}
	if err := Write(io.Discard, "pdf", sample); err == nil {
		t.Error("不支持的格式")
	}
}

func TestFormatsAndNames(t *testing.T) {
	for _, f := range Formats {
		if !ValidFormat(f) || FormatLabel(f) == "" {
			t.Errorf("%s", f)
		}
	}
	if ValidFormat("pdf") || ValidFormat("") {
		t.Error("非法格式")
	}
	used := map[string]bool{}
	a := InstanceFileName("a/b", 1, "csv", used)
	b := InstanceFileName("a:b", 2, "csv", used) // 替换后与 a/b 同名
	c := InstanceFileName("A_B", 3, "csv", used) // 大小写不同也算同名
	if a != "a_b.csv" || b != "a_b_2.csv" || c != "A_B_3.csv" {
		t.Errorf("实例文件名去重: %s %s %s", a, b, c)
	}
	if got := InstanceFileName("CON", 4, "csv", used); got != "_CON.csv" {
		t.Errorf("保留设备名: %s", got)
	}
	for in, want := range map[string]string{"": "x", "...": "x", "a.b. ": "a.b", "../etc/passwd": "__etc_passwd", "名字\x00": "名字_"} {
		if got := SafeName(in, "x"); got != want {
			t.Errorf("SafeName(%q)=%q 期望 %q", in, got, want)
		}
	}
	if n := len([]rune(SafeName(strings.Repeat("长", 200), "x"))); n != 80 {
		t.Errorf("长度限制: %d", n)
	}
	name, base := SingleArchiveName("shop/../db", time.Date(2026, 10, 6, 9, 30, 1, 0, time.UTC))
	if !strings.HasPrefix(name, "shop___db_20261006093001_") || !strings.HasSuffix(name, ".zip") || strings.Contains(name, "/") || strings.TrimSuffix(name, ".zip") != base {
		t.Errorf("压缩包名: %s %s", name, base)
	}
	if n := BatchArchiveName(42, time.Date(2026, 10, 6, 9, 30, 1, 0, time.UTC)); !strings.HasPrefix(n, "42_20261006093001_") {
		t.Errorf("批量压缩包名: %s", n)
	}
	if CheckName(name) != nil {
		t.Error("生成的压缩包名必须能通过 CheckName")
	}
}

func TestZip(t *testing.T) {
	var b bytes.Buffer
	if err := ZipSingle(&b, "x.csv", func(w io.Writer) error { return Write(w, CSV, sample) }); err != nil {
		t.Fatal(err)
	}
	zr, _ := zip.NewReader(bytes.NewReader(b.Bytes()), int64(b.Len()))
	if len(zr.File) != 1 || zr.File[0].Name != "x.csv" {
		t.Errorf("单文件包: %+v", zr.File)
	}

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "b.csv"), []byte("b"), 0o600)
	os.WriteFile(filepath.Join(dir, "a.csv"), []byte("a"), 0o600)
	b.Reset()
	if err := ZipDir(&b, dir, "42"); err != nil {
		t.Fatal(err)
	}
	zr, _ = zip.NewReader(bytes.NewReader(b.Bytes()), int64(b.Len()))
	if len(zr.File) != 2 || zr.File[0].Name != "42/a.csv" || zr.File[1].Name != "42/b.csv" {
		t.Errorf("目录包（保留工单号这一层，顺序稳定）: %+v", zr.File)
	}
	// 解包还原
	zp := filepath.Join(t.TempDir(), "p.zip")
	os.WriteFile(zp, b.Bytes(), 0o600)
	out := t.TempDir()
	if err := Unzip(zp, out); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(out, "42", "a.csv")); string(got) != "a" {
		t.Errorf("解包: %q", got)
	}
	// zip-slip
	var evil bytes.Buffer
	zw := zip.NewWriter(&evil)
	w, _ := zw.Create("../evil.txt")
	w.Write([]byte("x"))
	zw.Close()
	ep := filepath.Join(t.TempDir(), "e.zip")
	os.WriteFile(ep, evil.Bytes(), 0o600)
	if err := Unzip(ep, t.TempDir()); err == nil {
		t.Error("应拒绝逃出目录的条目")
	}
}

func TestLocalStorage(t *testing.T) {
	root := t.TempDir()
	st, err := NewLocalStorage(filepath.Join(root, "exports"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save("a.zip", strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	if !st.Exists("a.zip") || st.Exists("b.zip") {
		t.Error("Exists")
	}
	f, size, err := st.Open("a.zip")
	if err != nil || size != 5 {
		t.Fatalf("%v %d", err, size)
	}
	got, _ := io.ReadAll(f)
	f.Close()
	if string(got) != "hello" {
		t.Errorf("内容: %q", got)
	}
	if _, _, err := st.Open("nope.zip"); err != ErrNotFound {
		t.Errorf("不存在: %v", err)
	}
	// 覆盖写；目录里不留临时文件
	st.Save("a.zip", strings.NewReader("v2"))
	ents, _ := os.ReadDir(st.Dir)
	if len(ents) != 1 {
		t.Errorf("不应留下临时文件: %v", ents)
	}
	if err := st.Delete("a.zip"); err != nil || st.Exists("a.zip") {
		t.Errorf("删除: %v", err)
	}
	if err := st.Delete("a.zip"); err != nil {
		t.Errorf("重复删除不报错: %v", err)
	}
	// 文件名来自数据库记录，必须当作不可信输入
	secret := filepath.Join(root, "secret.txt")
	os.WriteFile(secret, []byte("s"), 0o600)
	for _, bad := range []string{"../secret.txt", `..\secret.txt`, "/etc/passwd", "a/b.zip", ".hidden", "", "a\x00b", strings.Repeat("a", 300)} {
		if st.Exists(bad) {
			t.Errorf("%q 不应存在", bad)
		}
		if _, _, err := st.Open(bad); err == nil {
			t.Errorf("%q 应拒绝打开", bad)
		}
		if err := st.Save(bad, strings.NewReader("x")); err == nil {
			t.Errorf("%q 应拒绝写入", bad)
		}
		if err := st.Delete(bad); err == nil {
			t.Errorf("%q 应拒绝删除", bad)
		}
	}
	if got, _ := os.ReadFile(secret); string(got) != "s" {
		t.Error("目录外的文件不应被动到")
	}
	if _, err := NewLocalStorage(" "); err == nil {
		t.Error("目录为空应报错")
	}
}

func TestPrepareMySQL(t *testing.T) {
	ok := map[string]string{
		"select id, name from t where id > 1;": "select id, name from t where id > 1",
		"-- 说明\nSELECT 1":                      "SELECT 1",
		"with a as (select 1) select * from a": "with a as (select 1) select * from a",
	}
	for in, want := range ok {
		p, err := Prepare("mysql", in)
		if err != nil || p.SQL != want || p.Batch {
			t.Errorf("%q: %+v %v", in, p, err)
		}
	}
	bad := map[string]string{
		"":                                      "不能为空",
		"update t set a=1":                      "违规语句",
		"delete from t":                         "违规语句",
		"select 1; select 2":                    "只支持一条",
		"select * from t into outfile '/tmp/x'": "写出文件",
		"select * from t for update":            "加锁",
		"select load_file('/etc/passwd')":       "load_file",
		"select sleep(100)":                     "sleep",
		"select * from mysql.user":              "mysql.user",
		"show databases":                        "违规语句",
		"select 1 into dumpfile '/tmp/y'":       "写出文件",
		"select * from t lock in share mode":    "加锁",
	}
	for in, want := range bad {
		if _, err := Prepare("mysql", in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q 应报错含 %q: %v", in, want, err)
		}
	}
	p, _ := Prepare("mysql", "select a from t where x=1 order by a")
	if got := p.CountSQL("mysql"); got != "SELECT COUNT(*) FROM (select a from t where x=1 order by a) t" {
		t.Errorf("统计语句: %s", got)
	}
}

// 导出：DECLARE @t TABLE + INSERT INTO @t 攒中间结果再 SELECT 的批处理放行；写真表 / 临时表仍然拒绝。
func TestPrepareMSSQLTableVarInsert(t *testing.T) {
	sql := "DECLARE @areas TABLE (Area varchar(10));\n" +
		"INSERT INTO @areas SELECT DISTINCT Area FROM dbo.Settings;\n" +
		"SELECT a.Area, COUNT(1) AS n FROM dbo.Users u CROSS APPLY (SELECT TOP 1 x.Area FROM @areas x WHERE u.UserName LIKE x.Area + '%') a GROUP BY a.Area"
	p, err := Prepare("mssql", sql)
	if err != nil || !p.Batch || p.CountSQL("mssql") != "" {
		t.Fatalf("写表变量的批处理应放行（按批处理导出）: %+v %v", p, err)
	}
	for _, in := range []string{
		"DECLARE @t TABLE (a INT); INSERT INTO dbo.T SELECT 1; SELECT a FROM @t",
		"DECLARE @t TABLE (a INT); INSERT INTO #t SELECT 1; SELECT a FROM @t",
	} {
		if _, err := Prepare("mssql", in); err == nil || !strings.Contains(err.Error(), "INSERT 只能写入") {
			t.Errorf("%q 应拒绝并说明只能写表变量: %v", in, err)
		}
	}
}

// INSERT … OUTPUT 会把写入的行当成第一个结果集导出，脱敏按 SELECT 列表匹配，OUTPUT 里换个别名就漏：一律拒绝。
func TestPrepareMSSQLRejectsInsertOutput(t *testing.T) {
	for _, in := range []string{
		"DECLARE @t TABLE (x VARCHAR(20), y INT); INSERT INTO @t (x, y) OUTPUT inserted.x AS leak SELECT phone, 1 FROM users; SELECT 1 AS a",
		"declare @t table (x varchar(20)); insert into @t output inserted.x select phone from users; select x from @t",
	} {
		if _, err := Prepare("mssql", in); err == nil || !strings.Contains(err.Error(), "output") {
			t.Errorf("%q 应拒绝: %v", in, err)
		}
	}
}

func TestPrepareMSSQL(t *testing.T) {
	p, err := Prepare("mssql", "select id from dbo.T where id > 1 order by id;\nGO")
	if err != nil || p.Batch || p.SQL != "select id from dbo.T where id > 1 order by id" {
		t.Fatalf("%+v %v", p, err)
	}
	// 统计行数要去掉最外层 ORDER BY（否则 SQL Server 报 1033）
	if got := p.CountSQL("mssql"); got != "SELECT COUNT(*) FROM (select id from dbo.T where id > 1) t" {
		t.Errorf("统计语句: %s", got)
	}
	// 批处理：保留整段，没有统计语句
	b, err := Prepare("mssql", "declare @d date = '2026-01-01';\nselect * from dbo.T where d = @d")
	if err != nil || !b.Batch || b.CountSQL("mssql") != "" || !strings.Contains(b.SQL, "declare @d") {
		t.Errorf("批处理: %+v %v", b, err)
	}
	bad := map[string]string{
		"update dbo.T set a=1":                  "不允许 update",
		"select 1\nupdate dbo.T set c=1":        "update",
		"select * into #t from dbo.T":           "into",
		"exec sp_who":                           "不允许 exec",
		"declare c cursor for select 1":         "DECLARE @变量",
		"set nocount on; select 1":              "SET @变量",
		"select 1; select 2":                    "一条查询语句",
		"select * from openrowset('a','b','c')": "openrowset",
		"waitfor delay '00:00:10'; select 1":    "waitfor",
		"use other; select 1":                   "只允许 SELECT",
		"drop table dbo.T":                      "不允许 drop",
	}
	for in, want := range bad {
		if _, err := Prepare("mssql", in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q 应报错含 %q: %v", in, want, err)
		}
	}
	// 字符串、注释、标识符里的同名单词不算
	for _, in := range []string{"select 'update delete' as x from dbo.T where id=1", "select id from dbo.T -- drop table x\nwhere id=1", "select [update], [delete] from dbo.T where id=1"} {
		if _, err := Prepare("mssql", in); err != nil {
			t.Errorf("%q 不应被拒绝: %v", in, err)
		}
	}
}
