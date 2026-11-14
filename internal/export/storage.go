// @author: kevinkelly
// @license: Apache Licence
// @file: storage.go
// @time: 2026/10/28

package export

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrNotFound 文件不存在。
var ErrNotFound = errors.New("文件不存在")

// Storage 是导出文件的存放位置。目前只实现本地目录；
// 配置为 SFTP / S3 兼容 / Azure Blob 这些类型时系统会明确提示“暂只支持本地存储”（见 sqlflow）。
type Storage interface {
	Save(name string, r io.Reader) error
	Open(name string) (io.ReadSeekCloser, int64, error)
	Exists(name string) bool
	Delete(name string) error
}

// LocalStorage 把文件存在一个目录里。所有文件名都必须是纯文件名（不含路径），防止通过被篡改的工单记录读写目录之外的文件。
type LocalStorage struct{ Dir string }

func NewLocalStorage(dir string) (*LocalStorage, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("导出目录未配置")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("创建导出目录失败: %w", err)
	}
	return &LocalStorage{Dir: dir}, nil
}

// CheckName 校验文件名：非空、不含路径分隔符与 ..、不以点开头、长度合理。
func CheckName(name string) error {
	switch {
	case name == "" || len(name) > 255:
		return errors.New("文件名不合法")
	case strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") || strings.HasPrefix(name, ".") || strings.ContainsRune(name, 0):
		return errors.New("文件名不合法")
	case filepath.Base(name) != name:
		return errors.New("文件名不合法")
	}
	return nil
}

func (s *LocalStorage) path(name string) (string, error) {
	if err := CheckName(name); err != nil {
		return "", err
	}
	return filepath.Join(s.Dir, name), nil
}

// Save 先写临时文件再改名，避免读到写了一半的文件。
func (s *LocalStorage) Save(name string, r io.Reader) error {
	p, err := s.path(name)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, ".upload-*")
	if err != nil {
		return err
	}
	_, err = io.Copy(tmp, r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	_ = os.Chmod(tmp.Name(), 0o640)
	return os.Rename(tmp.Name(), p)
}

func (s *LocalStorage) Open(name string) (io.ReadSeekCloser, int64, error) {
	p, err := s.path(name)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

func (s *LocalStorage) Exists(name string) bool {
	p, err := s.path(name)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

func (s *LocalStorage) Delete(name string) error {
	p, err := s.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
