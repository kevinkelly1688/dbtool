// @author: kevinkelly
// @license: Apache Licence
// @file: filestore_local.go
// @time: 2026/10/28

package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// localStore 是本地目录 / NFS 挂载目录存储。Key 是相对根目录（Path + Prefix）的 "/" 分隔路径：
// 不允许空段、"."、".."、绝对路径、反斜杠、冒号（Windows 盘符 / 数据流）、NUL，所以不可能跳出根目录。
// 写入先写同目录下的临时文件，fsync 后 rename，读者看不到半个文件。
// 说明：不解析根目录内部的符号链接，根目录里不要放指向外部的链接。
type localStore struct {
	root string
	base string // 配置的根目录（不含前缀）
	nfs  bool   // 类型为 nfs：base 必须是已存在的（已挂载的）目录，平台不替用户创建
}

func newLocalStore(cfg StoreConfig) (*localStore, error) {
	if strings.TrimSpace(cfg.Path) == "" {
		return nil, errors.New("本地 / NFS 存储需要填写目录")
	}
	root := filepath.Clean(cfg.Path)
	if p := cleanKey(cfg.Prefix); p != "" {
		if err := checkKey(p); err != nil {
			return nil, fmt.Errorf("路径前缀无效：%w", err)
		}
		root = filepath.Join(root, filepath.FromSlash(p))
	}
	return &localStore{root: root, base: filepath.Clean(cfg.Path), nfs: strings.EqualFold(strings.TrimSpace(cfg.Type), "nfs")}, nil
}

// checkKey 校验存储 Key（本地和对象存储共用）。
func checkKey(key string) error {
	if key == "" {
		return errors.New("Key 不能为空")
	}
	if len(key) > 1024 {
		return errors.New("Key 太长")
	}
	if strings.ContainsAny(key, "\x00\\:") {
		return errors.New("Key 含不允许的字符")
	}
	if strings.HasPrefix(key, "/") {
		return errors.New("Key 不能是绝对路径")
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return errors.New("Key 不能含空段、. 或 ..")
		}
	}
	return nil
}

func (s *localStore) resolve(key string) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	full := filepath.Join(s.root, filepath.FromSlash(key))
	pfx := s.root
	if !strings.HasSuffix(pfx, string(filepath.Separator)) {
		pfx += string(filepath.Separator)
	}
	if !strings.HasPrefix(full, pfx) {
		return "", errors.New("Key 超出存储根目录")
	}
	return full, nil
}

func (s *localStore) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	full, err := s.resolve(key)
	if err != nil {
		return err
	}
	if err := s.checkBase(); err != nil {
		return err
	}
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("创建目录失败：%w", err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败：%w", err)
	}
	tmpName := tmp.Name()
	fail := func(e error) error {
		tmp.Close()
		os.Remove(tmpName)
		return e
	}
	n, err := io.Copy(tmp, ctxReader{ctx, r})
	if err != nil {
		return fail(fmt.Errorf("写入失败：%w", err))
	}
	if size >= 0 && n != size {
		return fail(fmt.Errorf("写入大小 %d 与声明的 %d 不一致", n, size))
	}
	if err := tmp.Sync(); err != nil {
		return fail(fmt.Errorf("fsync 失败：%w", err))
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("关闭文件失败：%w", err)
	}
	if err := os.Rename(tmpName, full); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("重命名失败：%w", err)
	}
	return nil
}

func (s *localStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	full, err := s.resolve(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	if fi, err := f.Stat(); err == nil && fi.IsDir() {
		f.Close()
		return nil, fmt.Errorf("%s 是目录", key)
	}
	return f, nil
}

func (s *localStore) Stat(ctx context.Context, key string) (FileInfo, error) {
	full, err := s.resolve(key)
	if err != nil {
		return FileInfo{}, err
	}
	fi, err := os.Stat(full)
	if err != nil {
		return FileInfo{}, err
	}
	if fi.IsDir() {
		return FileInfo{}, fmt.Errorf("%s 是目录", key)
	}
	return FileInfo{Key: key, Size: fi.Size()}, nil
}

func (s *localStore) Delete(ctx context.Context, key string) error {
	full, err := s.resolve(key)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func probeKey() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return ".dba-archive-probe-" + hex.EncodeToString(b[:])
}

// testStore 是“测试连接”的通用实现：写入、读取、删除一个探测对象。
func testStore(ctx context.Context, fs FileStore) (time.Duration, error) {
	start := time.Now()
	key := probeKey()
	payload := []byte("dbtool archive probe " + time.Now().UTC().Format(time.RFC3339Nano))
	if err := fs.Put(ctx, key, bytes.NewReader(payload), int64(len(payload))); err != nil {
		return time.Since(start), fmt.Errorf("写入探测对象失败：%w", err)
	}
	defer fs.Delete(context.WithoutCancel(ctx), key)
	fi, err := fs.Stat(ctx, key)
	if err != nil {
		return time.Since(start), fmt.Errorf("读取探测对象信息失败：%w", err)
	}
	if fi.Size != int64(len(payload)) {
		return time.Since(start), fmt.Errorf("探测对象大小不一致（%d / %d）", fi.Size, len(payload))
	}
	rc, err := fs.Get(ctx, key)
	if err != nil {
		return time.Since(start), fmt.Errorf("读取探测对象失败：%w", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return time.Since(start), fmt.Errorf("读取探测对象失败：%w", err)
	}
	if !bytes.Equal(got, payload) {
		return time.Since(start), errors.New("探测对象内容不一致")
	}
	if err := fs.Delete(ctx, key); err != nil {
		return time.Since(start), fmt.Errorf("删除探测对象失败：%w", err)
	}
	return time.Since(start), nil
}

// checkBase 对 NFS 类型：配置的目录必须已经存在且是目录。挂载没生效时这个目录往往不存在（或只是个空挂载点），
// 此时 MkdirAll 会静默地把备份写到本地盘上；所以 NFS 不创建基础目录，只允许在它下面建前缀 / 子目录。
func (s *localStore) checkBase() error {
	if !s.nfs {
		return nil
	}
	fi, err := os.Stat(s.base)
	switch {
	case err != nil:
		return fmt.Errorf("NFS 挂载目录 %s 不存在或无法访问：请先确认 NFS 已挂载并生效（平台不会为 NFS 类型创建目录）：%w", s.base, err)
	case !fi.IsDir():
		return fmt.Errorf("%s 不是目录", s.base)
	}
	return nil
}

// checkMount 在能读到系统挂载表时，确认 NFS 目录不是直接落在根文件系统上（挂载没生效的典型表现：写入会占满本地盘）。
// 读不到挂载表（Windows、没有 /proc 的容器）时不判断。
func (s *localStore) checkMount() error {
	mounts, ok := readMounts()
	if !ok {
		return nil
	}
	abs, err := filepath.Abs(s.base)
	if err != nil {
		return nil
	}
	m, found := mountOf(mounts, filepath.ToSlash(abs))
	if found && !isRootMount(m.Point) {
		return nil
	}
	return fmt.Errorf("目录 %s 没有挂载到任何非根文件系统（所在挂载点：%s）：NFS 可能没有挂上，写入会落到本地盘，请检查 mount / fstab", s.base, orRoot(m.Point))
}

func orRoot(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

func (s *localStore) Test(ctx context.Context) (time.Duration, error) {
	if s.nfs {
		if err := s.checkBase(); err != nil {
			return 0, err
		}
		if err := s.checkMount(); err != nil {
			return 0, err
		}
	}
	// 本地目录类型自动创建；NFS 只创建基础目录下面的前缀目录
	if err := os.MkdirAll(s.root, 0o750); err != nil {
		return 0, fmt.Errorf("目录不可用：%w", err)
	}
	return testStore(ctx, s)
}

// Free 返回根目录所在文件系统的可用字节数；目录还不存在时取最近的已存在上级目录。
func (s *localStore) Free(ctx context.Context) (int64, bool, error) {
	p := s.root
	for i := 0; i < 64; i++ {
		if _, err := os.Stat(p); err == nil {
			n, ok := diskFree(p)
			return n, ok, nil
		}
		up := filepath.Dir(p)
		if up == p {
			break
		}
		p = up
	}
	return 0, false, nil
}
