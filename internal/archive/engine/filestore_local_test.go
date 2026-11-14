// @author: kevinkelly
// @license: Apache Licence
// @file: filestore_local_test.go
// @time: 2026/10/28

package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestLocal(t *testing.T, prefix string) (*localStore, string) {
	t.Helper()
	root := t.TempDir()
	st, err := NewFileStore(StoreConfig{Type: "local", Path: root, Prefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	return st.(*localStore), root
}

func TestLocalStoreRoundTrip(t *testing.T) {
	st, root := newTestLocal(t, "pre")
	ctx := t.Context()
	data := []byte("backup bytes")
	if err := st.Put(ctx, "a/b/c.csv", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "pre", "a", "b", "c.csv")); err != nil {
		t.Errorf("文件应在前缀目录下：%v", err)
	}
	fi, err := st.Stat(ctx, "a/b/c.csv")
	if err != nil || fi.Size != int64(len(data)) || fi.Key != "a/b/c.csv" {
		t.Fatalf("stat %+v %v", fi, err)
	}
	rc, err := st.Get(ctx, "a/b/c.csv")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Errorf("get %q", got)
	}
	// 覆盖写
	if err := st.Put(ctx, "a/b/c.csv", strings.NewReader("new"), 3); err != nil {
		t.Fatal(err)
	}
	fi, _ = st.Stat(ctx, "a/b/c.csv")
	if fi.Size != 3 {
		t.Errorf("覆盖后大小 %d", fi.Size)
	}
	// 不存在
	if _, err := st.Stat(ctx, "nope"); !errors.Is(err, ErrNotExist) {
		t.Errorf("Stat 不存在应是 ErrNotExist：%v", err)
	}
	if _, err := st.Get(ctx, "nope"); !errors.Is(err, ErrNotExist) {
		t.Errorf("Get 不存在应是 ErrNotExist：%v", err)
	}
	if err := st.Delete(ctx, "a/b/c.csv"); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, "a/b/c.csv"); err != nil {
		t.Errorf("重复删除应成功：%v", err)
	}
	// 目录当文件
	st.Put(ctx, "d/x", strings.NewReader("x"), 1)
	if _, err := st.Stat(ctx, "d"); err == nil {
		t.Errorf("目录不是文件")
	}
}

func TestLocalStoreRejectsTraversal(t *testing.T) {
	st, root := newTestLocal(t, "")
	ctx := t.Context()
	// 在根目录外放一个“秘密”文件，确认任何 Key 都读不到
	outside := filepath.Join(filepath.Dir(root), "secret.txt")
	os.WriteFile(outside, []byte("secret"), 0o600)
	defer os.Remove(outside)
	bad := []string{"", "..", "../secret.txt", "a/../../secret.txt", "a/..", "/etc/passwd", "/secret.txt", "a//b", "./a", "a/./b", "a\\b", "..\\secret.txt",
		"C:\\Windows\\win.ini", "C:/x", "a:b", "a\x00b", strings.Repeat("a/", 600)}
	for _, k := range bad {
		if err := st.Put(ctx, k, strings.NewReader("x"), 1); err == nil {
			t.Errorf("Put(%q) 应被拒绝", k)
		}
		if _, err := st.Get(ctx, k); err == nil {
			t.Errorf("Get(%q) 应被拒绝", k)
		}
		if _, err := st.Stat(ctx, k); err == nil {
			t.Errorf("Stat(%q) 应被拒绝", k)
		}
		if err := st.Delete(ctx, k); err == nil {
			t.Errorf("Delete(%q) 应被拒绝", k)
		}
	}
	if b, _ := os.ReadFile(outside); string(b) != "secret" {
		t.Errorf("根目录外的文件被改动了")
	}
	if _, err := NewFileStore(StoreConfig{Type: "local", Path: root, Prefix: "../x"}); err != nil {
		// ".." 会被清理成 "_"，不应出错也不应跳出
		t.Errorf("前缀应被清理：%v", err)
	}
	s2, _ := NewFileStore(StoreConfig{Type: "nfs", Path: root, Prefix: "../x"})
	if !strings.HasPrefix(s2.(*localStore).root, root) {
		t.Errorf("前缀跳出了根目录：%s", s2.(*localStore).root)
	}
}

func TestLocalStorePutAtomic(t *testing.T) {
	st, root := newTestLocal(t, "")
	ctx := t.Context()
	// 声明大小与实际不符：不留下目标文件，也不留下临时文件
	if err := st.Put(ctx, "x/y.bin", strings.NewReader("12345"), 10); err == nil {
		t.Fatal("大小不一致应失败")
	}
	if _, err := os.Stat(filepath.Join(root, "x", "y.bin")); !os.IsNotExist(err) {
		t.Errorf("失败的 Put 不应留下目标文件")
	}
	ents, _ := os.ReadDir(filepath.Join(root, "x"))
	if len(ents) != 0 {
		t.Errorf("失败的 Put 不应留下临时文件：%v", ents)
	}
	// 读取中途出错
	if err := st.Put(ctx, "x/z.bin", io.MultiReader(strings.NewReader("ab"), errReader{}), -1); err == nil {
		t.Fatal("读取错误应失败")
	}
	if ents, _ := os.ReadDir(filepath.Join(root, "x")); len(ents) != 0 {
		t.Errorf("临时文件应被清理：%v", ents)
	}
	// ctx 取消
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := st.Put(cctx, "x/c.bin", strings.NewReader("abc"), 3); err == nil {
		t.Errorf("取消的 ctx 应失败")
	}
	// 未知大小 -1 可以
	if err := st.Put(ctx, "x/u.bin", strings.NewReader("abc"), -1); err != nil {
		t.Errorf("size=-1：%v", err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestLocalStoreTestAndFree(t *testing.T) {
	st, root := newTestLocal(t, "sub/dir")
	d, err := st.Test(t.Context())
	if err != nil || d < 0 {
		t.Fatalf("Test：%v", err)
	}
	if ents, _ := os.ReadDir(filepath.Join(root, "sub", "dir")); len(ents) != 0 {
		t.Errorf("探测对象应被删除：%v", ents)
	}
	free, ok, err := st.Free(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if ok && free <= 0 {
		t.Errorf("可用空间应为正：%d", free)
	}
	// 目录还不存在时取上级
	st2, _ := NewFileStore(StoreConfig{Type: "local", Path: filepath.Join(root, "not", "yet")})
	if _, ok2, _ := st2.Free(t.Context()); ok2 != ok {
		t.Errorf("不存在的目录应取上级目录的可用空间")
	}
	// 配置校验
	if _, err := NewFileStore(StoreConfig{Type: "local"}); err == nil {
		t.Errorf("缺目录应报错")
	}
	if _, err := NewFileStore(StoreConfig{Type: "ftp"}); err == nil {
		t.Errorf("未知类型应报错")
	}
	// 测试连接失败
	f := filepath.Join(root, "afile")
	os.WriteFile(f, []byte("x"), 0o600)
	bad, _ := NewFileStore(StoreConfig{Type: "local", Path: f})
	if _, err := bad.Test(t.Context()); err == nil {
		t.Errorf("根路径是文件时测试连接应失败")
	}
}

// S8-B6：NFS 类型的“测试连接”不得替用户创建目录（挂载没生效时 MkdirAll 会静默写到本地盘）；本地目录类型保持自动创建。
func TestNFSStoreTestDoesNotCreateBaseDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "mnt", "backup")
	st, err := NewFileStore(StoreConfig{Type: "nfs", Path: missing})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Test(t.Context()); err == nil || !strings.Contains(err.Error(), "挂载") {
		t.Fatalf("目录不存在应报错并提示挂载：%v", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("NFS 的 Test 不得创建目录：%v", err)
	}
	// 写入同样不能把不存在的挂载点重新建出来
	if err := st.Put(t.Context(), "a/b.csv", strings.NewReader("x"), 1); err == nil {
		t.Fatal("挂载目录不存在时不应写入")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("NFS 的 Put 不得创建挂载目录：%v", err)
	}

	// 目录已存在：通过；前缀子目录可以由平台创建
	dir := t.TempDir()
	st2, _ := NewFileStore(StoreConfig{Type: "nfs", Path: dir, Prefix: "dba/archive"})
	if _, err := st2.Test(t.Context()); err != nil {
		t.Fatalf("已存在且可写的目录应通过：%v", err)
	}
	// 路径是文件
	f := filepath.Join(dir, "afile")
	os.WriteFile(f, []byte("x"), 0o600)
	st3, _ := NewFileStore(StoreConfig{Type: "nfs", Path: f})
	if _, err := st3.Test(t.Context()); err == nil {
		t.Fatal("路径是文件应报错")
	}
	// 本地目录类型仍自动创建
	loc := filepath.Join(t.TempDir(), "new", "dir")
	st4, _ := NewFileStore(StoreConfig{Type: "local", Path: loc})
	if _, err := st4.Test(t.Context()); err != nil {
		t.Fatalf("local 应自动创建目录：%v", err)
	}
}

func TestNFSStoreMountCheck(t *testing.T) {
	dir := t.TempDir()
	old := readMounts
	defer func() { readMounts = old }()
	root := filepath.VolumeName(dir) + "/" // Linux "/"，Windows "C:/"
	// 目录只落在根文件系统上：挂载没生效
	readMounts = func() ([]mountEntry, bool) { return []mountEntry{{Point: root, FSType: "ext4"}}, true }
	st, _ := NewFileStore(StoreConfig{Type: "nfs", Path: dir})
	if _, err := st.Test(t.Context()); err == nil || !strings.Contains(err.Error(), "没有挂载") {
		t.Fatalf("目录在根文件系统上应提示 NFS 没挂上：%v", err)
	}
	// 目录本身（或其上级）是一个挂载点
	readMounts = func() ([]mountEntry, bool) {
		return []mountEntry{{Point: root, FSType: "ext4"}, {Point: filepath.ToSlash(dir), FSType: "nfs4"}}, true
	}
	if _, err := st.Test(t.Context()); err != nil {
		t.Fatalf("挂载点存在应通过：%v", err)
	}
	// 读不到挂载表（如 Windows / 容器）：跳过检查
	readMounts = func() ([]mountEntry, bool) { return nil, false }
	if _, err := st.Test(t.Context()); err != nil {
		t.Fatalf("读不到挂载表时不判断挂载：%v", err)
	}
}

func TestParseMountInfo(t *testing.T) {
	in := "36 35 98:0 / /mnt/nfs\\040x rw,noatime shared:1 - nfs4 srv:/export rw\n" +
		"22 1 8:1 / / rw - ext4 /dev/sda1 rw\n" +
		"bad line\n"
	got := parseMountInfo(in)
	if len(got) != 2 || got[0].Point != "/mnt/nfs x" || got[0].FSType != "nfs4" || got[1].Point != "/" || got[1].FSType != "ext4" {
		t.Fatalf("%+v", got)
	}
}
