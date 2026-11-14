// @author: kevinkelly
// @license: Apache Licence
// @file: fileio.go
// @time: 2026/10/28

package giconfig

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// FileInfo 是配置文件的基本信息。
type FileInfo struct {
	Exists  bool
	Size    int64
	ModTime time.Time
	Mode    os.FileMode
}

// FileStore 是对磁盘上 config.toml 的读写。测试里用内存假实现。
type FileStore interface {
	// Read 读取文件。文件不存在时 info.Exists=false 且 err=nil。
	Read(path string) ([]byte, FileInfo, error)
	// Write 原子地写入（临时文件 + rename），保留原文件的权限；文件已存在且内容不同时，先把旧内容备份到 path+BackupSuffix。
	Write(path string, data []byte) error
	// CheckWritable 检查能不能写（目录可创建临时文件、已有文件可读）。
	CheckWritable(path string) error
}

// BackupSuffix 是覆盖前保存旧内容的备份文件后缀（只保留最近一份）。
const BackupSuffix = ".dbtool.bak"

type osFiles struct{}

func (osFiles) Read(path string) ([]byte, FileInfo, error) {
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, FileInfo{}, nil
	}
	if err != nil {
		return nil, FileInfo{}, err
	}
	if !st.Mode().IsRegular() {
		return nil, FileInfo{}, fmt.Errorf("%s 不是普通文件", path)
	}
	if st.Size() > MaxContentBytes {
		return nil, FileInfo{}, fmt.Errorf("%s 过大（超过 %d KB），不是正常的 goInception 配置文件", path, MaxContentBytes/1024)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, FileInfo{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxContentBytes+1))
	if err != nil {
		return nil, FileInfo{}, err
	}
	return data, FileInfo{Exists: true, Size: st.Size(), ModTime: st.ModTime(), Mode: st.Mode().Perm()}, nil
}

// atomicWrite 在 dir 里写临时文件，设置权限与属主后 rename 到 target。
func atomicWrite(target string, data []byte, perm os.FileMode, owner *fileOwnerInfo) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".giconfig-*.tmp")
	if err != nil {
		return wrapf(err, "在 %s 创建临时文件失败（dbtool 运行用户需要目录写权限）：%s", dir)
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, perm); err != nil {
		return err
	}
	if owner != nil {
		owner.apply(name)
	}
	if err := os.Rename(name, target); err != nil {
		return wrapf(err, "替换配置文件失败（如果文件是单独挂载进容器的，请改为挂载所在目录）：%s")
	}
	ok = true
	syncDir(dir)
	return nil
}

func (osFiles) Write(path string, data []byte) error {
	target := path
	if real, err := filepath.EvalSymlinks(path); err == nil {
		target = real // 符号链接：改链接指向的真实文件，不破坏链接
	}
	perm := os.FileMode(0o640)
	var owner *fileOwnerInfo
	if st, err := os.Stat(target); err == nil {
		if !st.Mode().IsRegular() {
			return fmt.Errorf("%s 不是普通文件", path)
		}
		perm = st.Mode().Perm()
		owner = ownerOf(st)
		if old, err := os.ReadFile(target); err == nil && string(old) != string(data) {
			if err := atomicWrite(target+BackupSuffix, old, perm, owner); err != nil {
				return wrapf(err, "备份旧配置文件失败：%s")
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return atomicWrite(target, data, perm, owner)
}

func (osFiles) CheckWritable(path string) error {
	dir := filepath.Dir(path)
	st, err := os.Stat(dir)
	if err != nil {
		return wrapf(err, "目录 %s 不可访问：%s", dir)
	}
	if !st.IsDir() {
		return fmt.Errorf("%s 不是目录", dir)
	}
	tmp, err := os.CreateTemp(dir, ".giconfig-check-*.tmp")
	if err != nil {
		return wrapf(err, "目录 %s 不可写（原子替换需要在该目录创建临时文件）：%s", dir)
	}
	name := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(name)
	if fi, err := os.Stat(path); err == nil {
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("%s 不是普通文件", path)
		}
		f, err := os.Open(path)
		if err != nil {
			return wrapf(err, "配置文件 %s 不可读：%s", path)
		}
		_ = f.Close()
	}
	return nil
}

// validConfigPath 校验配置文件路径：绝对路径、.toml 结尾、没有控制字符，返回清理后的路径。
func validConfigPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("请填写配置文件路径")
	}
	if len(p) > 255 {
		return "", errors.New("配置文件路径过长（最多 255 个字符）")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("配置文件路径含有控制字符")
		}
	}
	if !filepath.IsAbs(p) && !strings.HasPrefix(p, "/") {
		return "", errors.New("配置文件路径必须是绝对路径")
	}
	if strings.HasPrefix(p, "/") {
		p = path.Clean(p) // Unix 风格路径保持正斜杠（也让单元测试与操作系统无关）
	} else {
		p = filepath.Clean(p)
	}
	if !strings.HasSuffix(strings.ToLower(p), ".toml") {
		return "", errors.New("配置文件必须是 .toml 文件")
	}
	return p, nil
}
