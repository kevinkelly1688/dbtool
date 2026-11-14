// @author: kevinkelly
// @license: Apache Licence
// @file: backup_test.go
// @time: 2026/10/28

package engine

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"
)

var testKey = func() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i * 3)
	}
	return k
}()

func TestCSVFormat(t *testing.T) {
	f := csvFormatter{}
	cols := []string{"id", "name", "note", "blob", "n"}
	kinds := []colKind{kindNumber, kindText, kindText, kindBinary, kindNumber}
	rows := []Row{
		{int64(1), []byte("plain"), nil, []byte{0xde, 0xad}, 1.5},
		{int64(2), "has,comma", "line1\nline2", []byte{}, nil},
		{int64(3), `say "hi"`, "", []byte("x"), int64(-4)},
		{int64(4), ` lead`, `\N`, nil, uint64(18446744073709551615)},
	}
	got := string(f.header("t", cols)) + string(f.rows("t", cols, kinds, rows))
	want := "id,name,note,blob,n\n" +
		"1,plain,\\N,0xdead,1.5\n" +
		"2,\"has,comma\",\"line1\nline2\",0x,\\N\n" +
		"3,\"say \"\"hi\"\"\",\"\",0x78,-4\n" +
		"4,\" lead\",\"\\N\",\\N,18446744073709551615\n"
	if got != want {
		t.Errorf("csv\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestSQLFormat(t *testing.T) {
	f := sqlFormatter{}
	cols := []string{"id", "name", "b", "d"}
	kinds := []colKind{kindNumber, kindText, kindBinary, kindNumber}
	rows := []Row{
		{int64(1), "it's", []byte{1, 2}, []byte("12.50")},
		{int64(2), "a\nb\\c\"d\x00", nil, nil},
		{int64(3), nil, []byte{}, []byte("1e5")},
		{int64(4), "x", []byte("y"), []byte("evil'; drop")},
	}
	got := string(f.header("t`x", cols)) + string(f.rows("t`x", cols, kinds, rows))
	for _, want := range []string{
		"-- table: t`x",
		"INSERT INTO `t``x` (`id`, `name`, `b`, `d`) VALUES\n",
		"(1, 'it\\'s', 0x0102, 12.50),\n",
		"(2, 'a\\nb\\\\c\\\"d\\0', NULL, NULL),\n",
		"(3, NULL, '', 1e5),\n",
		"(4, 'x', 0x79, 'evil\\'; drop');\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("sql 缺少 %q:\n%s", want, got)
		}
	}
	// 分语句：每 sqlRowsPerStmt 行一条
	many := make([]Row, sqlRowsPerStmt*2+1)
	for i := range many {
		many[i] = Row{int64(i), "x", nil, nil}
	}
	out := string(f.rows("t", cols, kinds, many))
	if n := strings.Count(out, "INSERT INTO"); n != 3 {
		t.Errorf("应分成 3 条语句，得到 %d", n)
	}
	if strings.Count(out, ";\n") != 3 {
		t.Errorf("每条语句以 ; 结尾")
	}
}

func TestValueHelpers(t *testing.T) {
	for _, c := range []struct {
		typ  string
		want colKind
	}{
		{"bigint(20) unsigned", kindNumber}, {"int", kindNumber}, {"decimal(10,2)", kindNumber}, {"double", kindNumber}, {"year", kindNumber},
		{"varchar(10)", kindText}, {"datetime", kindText}, {"json", kindText}, {"enum('a','b')", kindText}, {"text", kindText},
		{"blob", kindBinary}, {"varbinary(16)", kindBinary}, {"BIT(1)", kindBinary}, {"geometry", kindBinary}, {"longblob", kindBinary},
	} {
		if got := kindOf(c.typ); got != c.want {
			t.Errorf("kindOf(%q)=%v want %v", c.typ, got, c.want)
		}
	}
	if s, null := valueText(time.Date(2026, 1, 2, 3, 4, 5, 123000000, time.UTC), kindText); null || s != "2026-01-02 03:04:05.123" {
		t.Errorf("time: %q", s)
	}
	if s, _ := valueText(float32(0.1), kindNumber); s != "0.1" {
		t.Errorf("float32: %q", s)
	}
	if _, null := valueText(nil, kindText); !null {
		t.Errorf("nil 是 NULL")
	}
	// valKey：数值与文本的十进制表示视为相同，NULL 与空串不同
	if valKey(int64(5)) != valKey([]byte("5")) || valKey(nil) == valKey("") || valKey("a") == valKey("b") {
		t.Errorf("valKey 语义不对")
	}
	if rowKey(Row{"ab", "c"}) == rowKey(Row{"a", "bc"}) {
		t.Errorf("rowKey 必须无歧义")
	}
}

func TestEncryptRoundTrip(t *testing.T) {
	aead, err := newGCM(testKey)
	if err != nil {
		t.Fatal(err)
	}
	var file bytes.Buffer
	file.WriteString(encMagic)
	parts := []string{"first chunk", "", strings.Repeat("x", 100000), "最后一块"}
	for i, p := range parts {
		c, err := encryptChunk(aead, uint64(i), []byte(p))
		if err != nil {
			t.Fatal(err)
		}
		file.Write(c)
	}
	raw := file.Bytes()
	dr, err := NewDecryptReader(bytes.NewReader(raw), testKey)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(dr)
	if err != nil || string(got) != strings.Join(parts, "") {
		t.Fatalf("解密不一致：%v len=%d", err, len(got))
	}
	// 密文里不含明文
	if bytes.Contains(raw, []byte("first chunk")) {
		t.Errorf("密文泄漏明文")
	}
	// 随机 nonce：同样的明文两次加密结果不同
	a, _ := encryptChunk(aead, 0, []byte("same"))
	b, _ := encryptChunk(aead, 0, []byte("same"))
	if bytes.Equal(a, b) {
		t.Errorf("nonce 必须随机")
	}
	// 错误密钥
	wrong := bytes.Repeat([]byte{9}, 32)
	dr, _ = NewDecryptReader(bytes.NewReader(raw), wrong)
	if _, err := io.ReadAll(dr); err == nil || !strings.Contains(err.Error(), "解密失败") {
		t.Errorf("错误密钥应解密失败：%v", err)
	}
	// 篡改一个字节
	bad := append([]byte(nil), raw...)
	bad[len(encMagic)+4+12+3] ^= 1
	dr, _ = NewDecryptReader(bytes.NewReader(bad), testKey)
	if _, err := io.ReadAll(dr); err == nil {
		t.Errorf("篡改应被发现")
	}
	// 调换块顺序（AAD 含块序号）：把前两块对调
	c0, _ := encryptChunk(aead, 0, []byte("A"))
	c1, _ := encryptChunk(aead, 1, []byte("B"))
	swapped := append(append([]byte(encMagic), c1...), c0...)
	dr, _ = NewDecryptReader(bytes.NewReader(swapped), testKey)
	if _, err := io.ReadAll(dr); err == nil {
		t.Errorf("调换块顺序应被发现")
	}
	// 截断
	dr, _ = NewDecryptReader(bytes.NewReader(raw[:len(raw)-5]), testKey)
	if _, err := io.ReadAll(dr); err == nil || !strings.Contains(err.Error(), "截断") {
		t.Errorf("截断应被发现：%v", err)
	}
	// 文件头不对 / 密钥长度不对
	if _, err := NewDecryptReader(strings.NewReader("not an archive file"), testKey); err == nil {
		t.Errorf("文件头不对应报错")
	}
	if _, err := newGCM(testKey[:16]); err == nil {
		t.Errorf("密钥必须 32 字节")
	}
}

func TestOpenBackupGzipMembers(t *testing.T) {
	// 多个 gzip 成员首尾相接，等于一个合法的 gzip 文件
	var all bytes.Buffer
	for _, s := range []string{"id,name\n", "1,a\n2,b\n", "3,c\n"} {
		z, err := gzipBytes([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		all.Write(z)
	}
	rc, err := OpenBackup(bytes.NewReader(all.Bytes()), false, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "id,name\n1,a\n2,b\n3,c\n" {
		t.Errorf("%q", got)
	}
	zr, _ := gzip.NewReader(bytes.NewReader(all.Bytes()))
	if b, _ := io.ReadAll(zr); string(b) != string(got) {
		t.Errorf("标准 gzip 读取应一致")
	}
}

// ---- backupWriter：用内存 FileStore ----

type memFS struct {
	mu         sync.Mutex
	objs       map[string][]byte
	puts       int
	failPuts   int // 前 N 次 Put 失败
	corruptGet bool
	statErr    error
}

func newMemFS() *memFS { return &memFS{objs: map[string][]byte{}} }

func (m *memFS) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.puts++
	if m.puts <= m.failPuts {
		return errors.New("simulated put failure")
	}
	m.objs[key] = b
	return nil
}
func (m *memFS) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	if !ok {
		return nil, ErrNotExist
	}
	b = append([]byte(nil), b...)
	if m.corruptGet && len(b) > 0 {
		b[0] ^= 0xff
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (m *memFS) Stat(ctx context.Context, key string) (FileInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.statErr != nil {
		return FileInfo{}, m.statErr
	}
	b, ok := m.objs[key]
	if !ok {
		return FileInfo{}, fmt.Errorf("%s: %w", key, ErrNotExist)
	}
	return FileInfo{Key: key, Size: int64(len(b))}, nil
}
func (m *memFS) Delete(ctx context.Context, key string) error    { return nil }
func (m *memFS) Test(ctx context.Context) (time.Duration, error) { return 0, nil }
func (m *memFS) Free(ctx context.Context) (int64, bool, error)   { return 0, false, nil }

func mkRows(from, to int) []Row {
	var rows []Row
	for i := from; i <= to; i++ {
		rows = append(rows, Row{int64(i), fmt.Sprintf("name-%d", i)})
	}
	return rows
}

func newTestBW(t *testing.T, st FileStore, dest FileDest) *backupWriter {
	t.Helper()
	bw, err := newBackupWriter(st, dest, testKey, "inst/shop/orders/2026/10/08/7", t.TempDir(), "tag", "orders",
		[]string{"id", "name"}, []colKind{kindNumber, kindText}, func(context.Context, time.Duration) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return bw
}

func decodeObject(t *testing.T, b []byte, enc, gz bool) string {
	t.Helper()
	rc, err := OpenBackup(bytes.NewReader(b), enc, gz, testKey)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	out, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestBackupWriterPlainCSV(t *testing.T) {
	fs := newMemFS()
	bw := newTestBW(t, fs, FileDest{Format: "csv"})
	ctx := t.Context()
	for _, rg := range [][2]int{{1, 3}, {4, 6}} {
		n, err := bw.Append(ctx, mkRows(rg[0], rg[1]))
		if err != nil || n != 3 {
			t.Fatalf("append: %d %v", n, err)
		}
	}
	m, err := bw.Finish(ctx, TableInfo{Name: "orders", Columns: []ColumnInfo{{Name: "id", Type: "bigint"}}}, []string{"1"}, []string{"6"}, FileDest{Format: "csv"})
	if err != nil || m == nil {
		t.Fatalf("finish: %v", err)
	}
	if len(m.Files) != 1 || m.Files[0].Key != "inst/shop/orders/2026/10/08/7/part-0001.csv" || m.Files[0].Rows != 6 || m.Rows != 6 || m.Enc != "" ||
		m.Format != "csv" || m.Table != "orders" || m.MinPK[0] != "1" || m.MaxPK[0] != "6" {
		t.Fatalf("manifest: %+v", m)
	}
	obj := fs.objs[m.Files[0].Key]
	sum := sha256.Sum256(obj)
	if m.Files[0].SHA256 != hex.EncodeToString(sum[:]) || m.Files[0].Size != int64(len(obj)) {
		t.Errorf("manifest 的 sha256 / 大小与对象不一致")
	}
	want := "id,name\n1,name-1\n2,name-2\n3,name-3\n4,name-4\n5,name-5\n6,name-6\n"
	if got := decodeObject(t, obj, false, false); got != want {
		t.Errorf("内容：\n%s", got)
	}
	// 清单本身也存进了存储，且内容与返回值一致
	mb, ok := fs.objs[m.ManifestKey]
	if !ok || m.ManifestKey != "inst/shop/orders/2026/10/08/7/manifest.json" {
		t.Fatalf("清单没有上传：%q", m.ManifestKey)
	}
	var back Manifest
	if err := json.Unmarshal(mb, &back); err != nil || back.Rows != 6 || len(back.Files) != 1 || back.Files[0].SHA256 != m.Files[0].SHA256 {
		t.Errorf("清单内容：%v %+v", err, back)
	}
}

func TestBackupWriterRollingGzipEnc(t *testing.T) {
	for _, mode := range []struct {
		name    string
		gz, enc bool
		ext     string
	}{{"gzip", true, false, ".csv.gz"}, {"enc", false, true, ".csv.enc"}, {"gzip+enc", true, true, ".csv.gz.enc"}} {
		t.Run(mode.name, func(t *testing.T) {
			fs := newMemFS()
			dest := FileDest{Format: "csv", Gzip: mode.gz, Encrypt: mode.enc, PartMB: 1}
			bw := newTestBW(t, fs, dest)
			ctx := t.Context()
			// 每批约 0.4 MB（随机不可压缩数据会让 gzip 也有体积；这里用较长的不同内容）
			rnd := func(seed int64) string {
				b := make([]byte, 64*1024)
				rand.New(rand.NewSource(seed)).Read(b)
				return hex.EncodeToString(b) // 128 KiB，gzip 压不动
			}
			var all []string
			next := 1
			for b := 0; b < 12; b++ {
				var rows []Row
				for i := 0; i < 4; i++ {
					name := "marker-" + rnd(int64(next))
					rows = append(rows, Row{int64(next), name})
					all = append(all, fmt.Sprintf("%d,%s\n", next, name))
					next++
				}
				if n, err := bw.Append(ctx, rows); err != nil || n != 4 {
					t.Fatalf("append: %d %v", n, err)
				}
			}
			m, err := bw.Finish(ctx, TableInfo{Name: "orders"}, []string{"1"}, []string{fmt.Sprint(next - 1)}, dest)
			if err != nil {
				t.Fatal(err)
			}
			if len(m.Files) < 3 {
				t.Fatalf("应滚动出多个文件：%d", len(m.Files))
			}
			var rowsSum int64
			var joined strings.Builder
			for i, f := range m.Files {
				if !strings.HasSuffix(f.Key, fmt.Sprintf("part-%04d%s", i+1, mode.ext)) {
					t.Errorf("文件名 %s", f.Key)
				}
				obj := fs.objs[f.Key]
				if int64(len(obj)) != f.Size {
					t.Errorf("大小")
				}
				text := decodeObject(t, obj, mode.enc, mode.gz)
				if !strings.HasPrefix(text, "id,name\n") {
					t.Errorf("每个文件都应有表头：%.30q", text)
				}
				joined.WriteString(strings.TrimPrefix(text, "id,name\n"))
				rowsSum += f.Rows
				if mode.enc && !bytes.HasPrefix(obj, []byte(encMagic)) {
					t.Errorf("加密文件应以魔数开头")
				}
				if mode.enc && bytes.Contains(obj, []byte("marker-")) {
					t.Errorf("加密文件泄漏明文")
				}
			}
			if rowsSum != 48 || m.Rows != 48 {
				t.Errorf("总行数 %d / %d", rowsSum, m.Rows)
			}
			if joined.String() != strings.Join(all, "") {
				t.Errorf("还原的内容与写入的不一致")
			}
			if mode.enc && m.Enc != "aes-256-gcm" {
				t.Errorf("Enc=%q", m.Enc)
			}
			// 本地临时文件都已清理
			if left := bw.LocalLeftovers(); len(left) != 0 {
				t.Errorf("残留 %v", left)
			}
		})
	}
}

func TestBackupWriterSkipsExistingKeys(t *testing.T) {
	fs := newMemFS()
	fs.objs["inst/shop/orders/2026/10/08/7/part-0001.csv"] = []byte("old backup")
	fs.objs["inst/shop/orders/2026/10/08/7/manifest.json"] = []byte("old manifest")
	bw := newTestBW(t, fs, FileDest{Format: "csv"})
	bw.Append(t.Context(), mkRows(1, 2))
	m, err := bw.Finish(t.Context(), TableInfo{}, nil, nil, FileDest{Format: "csv"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Files[0].Key != "inst/shop/orders/2026/10/08/7/part-0002.csv" || m.ManifestKey != "inst/shop/orders/2026/10/08/7/manifest-2.json" {
		t.Errorf("不能覆盖已有备份：%s %s", m.Files[0].Key, m.ManifestKey)
	}
	if string(fs.objs["inst/shop/orders/2026/10/08/7/part-0001.csv"]) != "old backup" {
		t.Errorf("旧备份被覆盖")
	}
}

func TestBackupWriterReadbackMismatch(t *testing.T) {
	fs := newMemFS()
	fs.corruptGet = true
	bw := newTestBW(t, fs, FileDest{Format: "csv"})
	bw.Append(t.Context(), mkRows(1, 3))
	_, err := bw.Finish(t.Context(), TableInfo{}, nil, nil, FileDest{Format: "csv"})
	if err == nil || !strings.Contains(err.Error(), "回读校验") || !strings.Contains(err.Error(), "本地文件保留") {
		t.Fatalf("回读不一致应失败，并提示本地文件位置：%v", err)
	}
}

func TestBackupWriterPutRetry(t *testing.T) {
	fs := newMemFS()
	fs.failPuts = 2 // 前两次失败，第三次成功
	bw := newTestBW(t, fs, FileDest{Format: "csv"})
	bw.Append(t.Context(), mkRows(1, 3))
	m, err := bw.Finish(t.Context(), TableInfo{}, nil, nil, FileDest{Format: "csv"})
	if err != nil || m == nil {
		t.Fatalf("重试后应成功：%v", err)
	}
	fs2 := newMemFS()
	fs2.failPuts = 100
	bw2 := newTestBW(t, fs2, FileDest{Format: "csv"})
	bw2.Append(t.Context(), mkRows(1, 3))
	_, err = bw2.Finish(t.Context(), TableInfo{}, nil, nil, FileDest{Format: "csv"})
	if err == nil || !strings.Contains(err.Error(), "本地文件保留") {
		t.Fatalf("持续失败应报错并保留本地文件：%v", err)
	}
}

func TestBackupWriterNoRows(t *testing.T) {
	bw := newTestBW(t, newMemFS(), FileDest{Format: "sql"})
	m, err := bw.Finish(t.Context(), TableInfo{}, nil, nil, FileDest{Format: "sql"})
	if m != nil || err != nil {
		t.Errorf("没有写过行时不应生成清单：%v %v", m, err)
	}
}

func TestBackupWriterStatErrorIsNotTreatedAsMissing(t *testing.T) {
	fs := newMemFS()
	fs.statErr = errors.New("network down")
	bw := newTestBW(t, fs, FileDest{Format: "csv"})
	if _, err := bw.Append(t.Context(), mkRows(1, 2)); err == nil {
		t.Errorf("无法确认对象是否存在时不应冒险写入（可能覆盖已有备份）")
	}
}
