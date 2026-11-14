// @author: kevinkelly
// @license: Apache Licence
// @file: run_files_test.go
// @time: 2026/10/28

package engine_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dbtool/internal/archive/engine"
	"dbtool/internal/archive/engine/enginetest"
)

func readBackup(t *testing.T, h *enginetest.Harness, f engine.ManifestFile, m *engine.Manifest) string {
	t.Helper()
	b, ok := h.Files.Object(f.Key)
	if !ok {
		t.Fatalf("存储里没有 %s", f.Key)
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != f.SHA256 || int64(len(b)) != f.Size {
		t.Fatalf("%s 的 sha256 / 大小与清单不一致", f.Key)
	}
	rc, err := engine.OpenBackup(bytes.NewReader(b), m.Enc != "", m.Gzip, h.FileKey)
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

func TestRunFileOnlyAllFormats(t *testing.T) {
	cases := []struct {
		format  string
		gz, enc bool
		ext     string
	}{
		{"csv", false, false, ".csv"}, {"csv", true, false, ".csv.gz"}, {"csv", false, true, ".csv.enc"}, {"csv", true, true, ".csv.gz.enc"},
		{"sql", false, false, ".sql"}, {"sql", true, true, ".sql.gz.enc"},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s gz=%v enc=%v", c.format, c.gz, c.enc), func(t *testing.T) {
			h := enginetest.New(enginetest.Options{Rows: 450})
			spec := h.FileSpec(c.format, c.gz, c.enc)
			tmp := t.TempDir()
			res, err := newEng().Run(t.Context(), spec, h.Policy(), h.EnvFor(spec, tmp), nil, h.Sink)
			if err != nil {
				t.Fatal(err)
			}
			if res.Result != engine.ResultSuccess || res.Deleted != 450 || res.Backed != 450 || h.Src.Len() != 0 {
				t.Fatalf("%+v", res)
			}
			m := res.Manifest
			if m == nil || len(m.Files) != 1 || m.Rows != 450 || m.Format != c.format || m.Gzip != c.gz || (m.Enc != "") != c.enc || m.Table != "orders" {
				t.Fatalf("清单：%+v", m)
			}
			if m.MinPK[0] != "1" || m.MaxPK[0] != "450" || len(m.Cols) != 4 {
				t.Errorf("清单范围 / 列：%+v", m)
			}
			if want := "prod-db01/shop/orders/2026/10/08/1/part-0001" + c.ext; m.Files[0].Key != want {
				t.Errorf("文件 Key %q want %q", m.Files[0].Key, want)
			}
			if m.ManifestKey != "prod-db01/shop/orders/2026/10/08/1/manifest.json" {
				t.Errorf("清单 Key %q", m.ManifestKey)
			}
			if _, ok := h.Files.Object(m.ManifestKey); !ok {
				t.Errorf("清单应已上传")
			}
			text := readBackup(t, h, m.Files[0], m)
			switch c.format {
			case "csv":
				recs, err := csv.NewReader(strings.NewReader(text)).ReadAll()
				if err != nil || len(recs) != 451 || recs[0][0] != "id" || recs[1][0] != "1" || recs[450][0] != "450" {
					t.Errorf("csv 解析：%v 行数 %d", err, len(recs))
				}
			case "sql":
				if n := strings.Count(text, "INSERT INTO `orders`"); n != 5 { // 每批 100 行 = 1 条语句，共 5 批
					t.Errorf("INSERT 语句数 %d", n)
				}
				if !strings.Contains(text, "(1, '2025-01-02 10:00:00', 1, 'p1')") {
					t.Errorf("sql 内容：%.200s", text)
				}
			}
			// 本地临时文件已清理
			ents, _ := os.ReadDir(tmp)
			if len(ents) != 0 {
				t.Errorf("本地临时文件残留：%v", ents)
			}
			if c.enc {
				b, _ := h.Files.Object(m.Files[0].Key)
				if bytes.Contains(b, []byte("p123")) {
					t.Errorf("加密文件不应含明文")
				}
			}
		})
	}
}

func TestRunTableAndFileBoth(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 300})
	spec := h.Spec()
	spec.File = &engine.FileDest{StoreID: 1, Format: "csv", Gzip: true, PathTemplate: "{db}/{table}/{run_id}"}
	res := mustRun(t, h, spec, h.Policy(), nil)
	if res.Result != engine.ResultSuccess || res.Manifest == nil || res.Manifest.Rows != 300 || len(h.Dst.Table("shop_arch", "orders_bak")) != 300 || h.Src.Len() != 0 {
		t.Fatalf("%+v", res)
	}
	checkOrder(t, h)
	// 库表写失败（文件没问题）→ 不删源；文件里的行不会超出已处理的范围
	h2 := enginetest.New(enginetest.Options{Rows: 300})
	h2.Dst.OnInsert = func(call int, rows []engine.Row) error {
		if call >= 2 {
			return errors.New("db down")
		}
		return nil
	}
	spec = h2.Spec()
	spec.File = &engine.FileDest{StoreID: 1, Format: "csv", PathTemplate: "{db}/{table}/{run_id}"}
	res = mustRun(t, h2, spec, h2.Policy(), nil)
	if res.Result != engine.ResultFailed || h2.Src.Len() != 200 || res.Deleted != 100 {
		t.Fatalf("%+v 剩 %d", res, h2.Src.Len())
	}
	// 失败时也要把已删源行的备份文件上传、生成清单
	if res.Manifest == nil || res.Manifest.Rows != 100 || res.Manifest.MaxPK[0] != "100" {
		t.Fatalf("已处理的批必须有文件备份：%+v", res.Manifest)
	}
}

func TestRunFileRolling(t *testing.T) {
	// 每行 payload 约 20 KB，1 MB 一个文件
	big := enginetest.New(enginetest.Options{Rows: 1})
	big.Src = enginetest.NewMemSource(big.Log, big.Clock, "shop", "orders", enginetest.DefaultColumns(), []string{"id"})
	var seed []engine.Row
	for i := 1; i <= 600; i++ {
		seed = append(seed, engine.Row{int64(i), "2025-01-01 00:00:00", int64(1), randPayload(i)})
	}
	big.Src.Seed(seed...)
	spec := big.FileSpec("csv", true, true)
	spec.File.PartMB = 1
	res := mustRun(t, big, spec, big.Policy(), nil)
	if res.Result != engine.ResultSuccess || res.Deleted != 600 {
		t.Fatalf("%+v", res)
	}
	m := res.Manifest
	if len(m.Files) < 3 {
		t.Fatalf("应滚动出多个文件：%d", len(m.Files))
	}
	var total int64
	seen := map[string]bool{}
	for i, f := range m.Files {
		total += f.Rows
		text := readBackup(t, big, f, m)
		recs, err := csv.NewReader(strings.NewReader(text)).ReadAll()
		if err != nil || int64(len(recs)-1) != f.Rows {
			t.Fatalf("文件 %d：%v 行 %d / 清单 %d", i, err, len(recs)-1, f.Rows)
		}
		for _, r := range recs[1:] {
			if seen[r[0]] {
				t.Fatalf("行 %s 出现在多个文件里", r[0])
			}
			seen[r[0]] = true
		}
	}
	if total != 600 || len(seen) != 600 || m.Rows != 600 {
		t.Errorf("总行数 %d / %d / %d", total, len(seen), m.Rows)
	}
}

func TestRunFileUploadFailureKeepsLocalFile(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 300})
	h.Files.PutErr = func(key string, call int) error { return errors.New("403 AccessDenied") }
	spec := h.FileSpec("csv", false, false)
	tmp := t.TempDir()
	res, err := newEng().Run(t.Context(), spec, h.Policy(), h.EnvFor(spec, tmp), nil, h.Sink)
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != engine.ResultFailed || !res.AutoPause || res.Manifest != nil || !strings.Contains(res.Err, "本地文件") {
		t.Fatalf("上传失败必须报失败并给出本地文件位置：%+v", res)
	}
	var left []string
	filepath.Walk(tmp, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			left = append(left, p)
		}
		return nil
	})
	if len(left) != 1 {
		t.Fatalf("本地备份文件必须保留：%v", left)
	}
	b, _ := os.ReadFile(left[0])
	if !strings.HasPrefix(string(b), "id,created_at,status,payload\n1,") || !strings.Contains(res.Err, filepath.Base(left[0])) {
		t.Errorf("保留的文件内容 / 错误信息：%.60q %s", b, res.Err)
	}
	if bytes.Count(b, []byte("\n")) != 301 {
		t.Errorf("本地文件应包含全部已删除的行：%d 行", bytes.Count(b, []byte("\n")))
	}
	// 即使运行因窗口等原因“正常中断”，收尾上传失败也要变成 failed
	h2 := enginetest.New(enginetest.Options{Rows: 300})
	h2.Files.PutErr = func(key string, call int) error { return errors.New("down") }
	spec = h2.FileSpec("csv", false, false)
	spec.Params.MaxRows = 200
	res, _ = newEng().Run(t.Context(), spec, h2.Policy(), h2.EnvFor(spec, t.TempDir()), nil, nil)
	if res.Result != engine.ResultFailed {
		t.Errorf("limit_reached 但上传失败应是 failed：%+v", res)
	}
}

func TestRunFileReadbackMismatchFails(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 200})
	h.Files.GetMangle = func(key string, data []byte) []byte {
		if len(data) > 0 {
			data[len(data)/2] ^= 0xff
		}
		return data
	}
	res := mustRun(t, h, h.FileSpec("csv", false, false), h.Policy(), nil)
	if res.Result != engine.ResultFailed || !strings.Contains(res.Err, "回读校验") || res.Manifest != nil {
		t.Fatalf("%+v", res)
	}
}

func TestRunFileDoesNotDuplicateRowsWhenDeleteRetries(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 300})
	h.Src.OnDelete = func(call int, req engine.DeleteReq) error {
		if call == 2 {
			return errors.New("connection lost")
		}
		return nil
	}
	spec := h.FileSpec("csv", false, false)
	res := mustRun(t, h, spec, h.Policy(), nil)
	if res.Result != engine.ResultSuccess || res.Manifest.Rows != 300 || h.Src.Len() != 0 {
		t.Fatalf("%+v", res)
	}
	text := readBackup(t, h, res.Manifest.Files[0], res.Manifest)
	if n := strings.Count(text, "\n"); n != 301 {
		t.Errorf("删除失败后重读同一批，文件里不应重复写：%d 行", n)
	}
}

func TestRunFileCopyMode(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 200})
	spec := h.FileSpec("sql", true, false)
	spec.Mode = engine.ModeCopy
	res := mustRun(t, h, spec, h.Policy(), nil)
	if res.Result != engine.ResultSuccess || res.Deleted != 0 || h.Src.Len() != 200 || res.Manifest == nil || res.Manifest.Rows != 200 {
		t.Fatalf("%+v", res)
	}
}

func TestRunFileKeysDoNotOverwriteEarlierRun(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 300})
	spec := h.FileSpec("csv", false, false)
	spec.File.PathTemplate = "{db}/{table}" // 故意不带 run_id
	spec.Mode = engine.ModeCopy
	r1 := mustRun(t, h, spec, h.Policy(), nil)
	spec.Checkpoint = nil
	r2 := mustRun(t, h, spec, h.Policy(), nil)
	if r1.Manifest.Files[0].Key == r2.Manifest.Files[0].Key || r1.Manifest.ManifestKey == r2.Manifest.ManifestKey {
		t.Fatalf("第二次运行覆盖了第一次的备份：%s / %s", r1.Manifest.Files[0].Key, r2.Manifest.Files[0].Key)
	}
	if _, ok := h.Files.Object(r1.Manifest.Files[0].Key); !ok {
		t.Errorf("第一次的备份丢了")
	}
}

// ---- 恢复 ----

func TestRestore(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 500})
	spec := h.Spec()
	res := mustRun(t, h, spec, h.Policy(), nil)
	if res.Result != engine.ResultSuccess || h.Src.Len() != 0 {
		t.Fatalf("%+v", res)
	}
	rng := engine.RestoreRange{DestInstanceID: 1, DestDB: "shop_arch", DestTable: res.DestTable, MinPK: res.MinPK, MaxPK: res.MaxPK}
	// 先在源表里放回一部分（模拟“只丢了一部分”）和一行内容已被改过的行
	h.Src.Seed(engine.Row{int64(10), "keep-me", int64(0), "newer-value"})
	rspec := spec
	rspec.RunID = 2
	ctl := engine.NewControl()
	rres, err := newEng().Restore(t.Context(), rspec, rng, h.EnvFor(rspec, t.TempDir()), ctl, h.Sink)
	if err != nil {
		t.Fatal(err)
	}
	if rres.Result != engine.ResultSuccess || rres.Scanned != 500 || rres.Backed != 499 || rres.Deleted != 0 || rres.Batches != 5 {
		t.Fatalf("%+v", rres)
	}
	if h.Src.Len() != 500 {
		t.Errorf("恢复后源表应有 500 行：%d", h.Src.Len())
	}
	for _, r := range h.Src.Rows() {
		if r[0] == int64(10) && r[1] != "keep-me" {
			t.Errorf("INSERT IGNORE 不能覆盖已有行：%v", r)
		}
	}
	// 幂等：再恢复一次，什么都不补
	rres, err = newEng().Restore(t.Context(), rspec, rng, h.EnvFor(rspec, t.TempDir()), nil, nil)
	if err != nil || rres.Result != engine.ResultSuccess || rres.Backed != 0 || rres.Scanned != 500 || h.Src.Len() != 500 {
		t.Fatalf("重复恢复应只补缺：%v %+v", err, rres)
	}
	// 按范围恢复一部分
	h2 := enginetest.New(enginetest.Options{Rows: 500})
	res = mustRun(t, h2, h2.Spec(), h2.Policy(), nil)
	rng = engine.RestoreRange{DestDB: "shop_arch", DestTable: res.DestTable, MinPK: []string{"201"}, MaxPK: []string{"300"}}
	rres, _ = newEng().Restore(t.Context(), h2.Spec(), rng, h2.EnvFor(h2.Spec(), t.TempDir()), nil, nil)
	if rres.Result != engine.ResultSuccess || rres.Backed != 100 || h2.Src.Len() != 100 || !h2.Src.Has("201") || !h2.Src.Has("300") || h2.Src.Has("200") || h2.Src.Has("301") {
		t.Fatalf("按范围恢复：%+v 剩 %d", rres, h2.Src.Len())
	}
}

func TestRestoreFailuresAndControl(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 300})
	res := mustRun(t, h, h.Spec(), h.Policy(), nil)
	spec := h.Spec()
	good := engine.RestoreRange{DestDB: "shop_arch", DestTable: res.DestTable, MinPK: res.MinPK, MaxPK: res.MaxPK}
	env := func() *engine.Env { return h.EnvFor(spec, t.TempDir()) }

	// 参数非法：不开始
	bad := []struct {
		name string
		mut  func(*engine.Spec, *engine.RestoreRange)
	}{
		{"protected", func(s *engine.Spec, r *engine.RestoreRange) { s.DB = "mysql" }},
		{"bad table", func(s *engine.Spec, r *engine.RestoreRange) { s.Table = "a`b" }},
		{"bad backup table", func(s *engine.Spec, r *engine.RestoreRange) { r.DestTable = "x;y" }},
		{"no range", func(s *engine.Spec, r *engine.RestoreRange) { r.MinPK, r.MaxPK = nil, nil }},
		{"range mismatch", func(s *engine.Spec, r *engine.RestoreRange) { r.MaxPK = []string{"1", "2"} }},
	}
	for _, b := range bad {
		s, r := spec, good
		b.mut(&s, &r)
		if _, err := newEng().Restore(t.Context(), s, r, env(), nil, nil); err == nil {
			t.Errorf("%s：应拒绝", b.name)
		}
	}
	if _, err := newEng().Restore(t.Context(), spec, good, &engine.Env{Src: h.Src}, nil, nil); err == nil {
		t.Errorf("缺 Dst")
	}
	// 备份表不存在
	missing := good
	missing.DestTable = "nope"
	if r, err := newEng().Restore(t.Context(), spec, missing, env(), nil, nil); err != nil || r.Result != engine.ResultFailed || !strings.Contains(r.Err, "不存在") {
		t.Errorf("备份表不存在：%v %+v", err, r)
	}
	// 主键列数不一致
	two := good
	two.MinPK, two.MaxPK = []string{"1", "1"}, []string{"9", "9"}
	if r, _ := newEng().Restore(t.Context(), spec, two, env(), nil, nil); r.Result != engine.ResultFailed {
		t.Errorf("主键列数不一致：%+v", r)
	}
	// 持续失败 → failed + AutoPause 风格（连续 3 次）
	h.Src.OnInsertIgnore = func(call int, req engine.InsertReq) error { return errors.New("read only") }
	r, _ := newEng().Restore(t.Context(), spec, good, env(), nil, h.Sink)
	if r.Result != engine.ResultFailed || !r.AutoPause || h.Src.Len() != 0 {
		t.Errorf("%+v", r)
	}
	h.Src.OnInsertIgnore = nil
	// 终止
	ctl := engine.NewControl()
	stopCalls := 0
	h.Src.OnInsertIgnore = func(call int, req engine.InsertReq) error {
		stopCalls++
		if stopCalls == 1 {
			ctl.Stop()
		}
		return nil
	}
	r, _ = newEng().Restore(t.Context(), spec, good, env(), ctl, nil)
	if r.Result != engine.ResultStopped || h.Src.Len() != 100 {
		t.Errorf("终止应在当前批后停止：%+v 剩 %d", r, h.Src.Len())
	}
	// 负载超限等待 → load_abort
	h.Src.OnInsertIgnore = nil
	h.Probe.Threads = func(int) (int, error) { return 999, nil }
	rs := spec
	rs.Params.MaxWaitSeconds = 30
	r, _ = newEng().Restore(t.Context(), rs, good, h.EnvFor(rs, t.TempDir()), nil, nil)
	if r.Result != engine.ResultLoadAbort || r.Err != "" {
		t.Errorf("恢复也要遵守负载保护：%+v", r)
	}
}

// randPayload 生成约 10 KB 的伪随机十六进制串（gzip 压不动一半以上）。
func randPayload(seed int) string {
	b := make([]byte, 5000)
	rand.New(rand.NewSource(int64(seed))).Read(b)
	return fmt.Sprintf("%05d-%s", seed, hex.EncodeToString(b))
}
