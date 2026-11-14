// @author: kevinkelly
// @license: Apache Licence
// @file: fileio_test.go
// @time: 2026/10/28

package giconfig

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOSFilesReadMissingAndTooLarge(t *testing.T) {
	dir := t.TempDir()
	data, info, err := osFiles{}.Read(filepath.Join(dir, "nope.toml"))
	if err != nil || info.Exists || data != nil {
		t.Fatalf("不存在应返回 Exists=false：%+v %v", info, err)
	}
	if _, _, err := (osFiles{}).Read(dir); err == nil || !strings.Contains(err.Error(), "不是普通文件") {
		t.Fatalf("读目录应报错：%v", err)
	}
	big := filepath.Join(dir, "big.toml")
	if err := os.WriteFile(big, make([]byte, MaxContentBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (osFiles{}).Read(big); err == nil || !strings.Contains(err.Error(), "过大") {
		t.Fatalf("%v", err)
	}
}

func TestOSFilesWriteAtomicKeepsPermAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte("old = 1\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := (osFiles{}).Write(p, []byte("new = 2\n")); err != nil {
		t.Fatal(err)
	}
	got, info, err := osFiles{}.Read(p)
	if err != nil || string(got) != "new = 2\n" || !info.Exists {
		t.Fatalf("%q %v", got, err)
	}
	bak, err := os.ReadFile(p + BackupSuffix)
	if err != nil || string(bak) != "old = 1\n" {
		t.Fatalf("旧内容应备份：%q %v", bak, err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o640 {
			t.Fatalf("权限应保持 0640，实际 %v", st.Mode().Perm())
		}
		if st, _ := os.Stat(p + BackupSuffix); st.Mode().Perm() != 0o640 {
			t.Fatalf("备份文件权限应与原文件一致：%v", st.Mode().Perm())
		}
	}
	// 目录里不能留下临时文件
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".giconfig-") {
			t.Fatalf("残留临时文件 %s", e.Name())
		}
	}
	// 内容相同：不重复备份、不报错
	if err := (osFiles{}).Write(p, []byte("new = 2\n")); err != nil {
		t.Fatal(err)
	}
	if bak, _ := os.ReadFile(p + BackupSuffix); string(bak) != "old = 1\n" {
		t.Fatalf("内容没变不应覆盖备份：%q", bak)
	}
}

func TestOSFilesWriteCreatesNew(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "new.toml")
	if err := (osFiles{}).Write(p, []byte("a = 1\n")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "a = 1\n" {
		t.Fatal(string(b))
	}
	if _, err := os.Stat(p + BackupSuffix); err == nil {
		t.Fatal("新建文件不需要备份")
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o640 {
			t.Fatalf("新文件默认 0640：%v", st.Mode().Perm())
		}
	}
	// 目录不存在
	if err := (osFiles{}).Write(filepath.Join(dir, "no", "such", "c.toml"), []byte("x")); err == nil {
		t.Fatal("目录不存在应报错")
	}
	// 目标是目录
	if err := (osFiles{}).Write(dir, []byte("x")); err == nil {
		t.Fatal("目标是目录应报错")
	}
}

func TestOSFilesWriteFollowsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 创建符号链接需要特权")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "real.toml")
	link := filepath.Join(dir, "link.toml")
	_ = os.WriteFile(real, []byte("a = 1\n"), 0o600)
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	if err := (osFiles{}).Write(link, []byte("a = 2\n")); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Lstat(link); st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("符号链接不应被替换成普通文件")
	}
	if b, _ := os.ReadFile(real); string(b) != "a = 2\n" {
		t.Fatalf("应改到真实文件：%q", b)
	}
}

func TestOSFilesCheckWritable(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.toml")
	if err := (osFiles{}).CheckWritable(p); err != nil {
		t.Fatalf("目录可写、文件不存在应通过：%v", err)
	}
	_ = os.WriteFile(p, []byte("x"), 0o600)
	if err := (osFiles{}).CheckWritable(p); err != nil {
		t.Fatal(err)
	}
	if err := (osFiles{}).CheckWritable(filepath.Join(dir, "no", "c.toml")); err == nil {
		t.Fatal("目录不存在应报错")
	}
	if err := (osFiles{}).CheckWritable(dir); err == nil {
		t.Fatal("路径是目录应报错")
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("检查不应留下文件：%v", ents)
	}
}

func TestValidConfigPath(t *testing.T) {
	ok := map[string]string{
		"/etc/goinception/config.toml":      "/etc/goinception/config.toml",
		" /etc//goinception/./config.TOML ": "/etc/goinception/config.TOML",
		"/opt/a/../gi/c.toml":               "/opt/gi/c.toml",
	}
	for in, want := range ok {
		if got, err := validConfigPath(in); err != nil || got != want {
			t.Errorf("%q → %q %v，期望 %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "config.toml", "./config.toml", "/etc/passwd", "/etc/a\n.toml", "/etc/\x00a.toml", "/" + strings.Repeat("a", 260) + ".toml", "/etc/goinception/"} {
		if got, err := validConfigPath(in); err == nil {
			t.Errorf("%q 应被拒绝，得到 %q", in, got)
		}
	}
}
