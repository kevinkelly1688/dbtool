// @author: kevinkelly
// @license: Apache Licence
// @file: filestore.go
// @time: 2026/10/28

package engine

import (
	"context"
	"fmt"
	"io"
	"strings"
)

func newFileStore(cfg StoreConfig) (FileStore, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Type)) {
	case "local", "nfs":
		return newLocalStore(cfg)
	case "s3", "oss", "minio":
		return newS3Store(cfg)
	}
	return nil, fmt.Errorf("不支持的备份存储类型：%s（local / nfs / s3 / oss）", quote(cfg.Type))
}

// ctxReader 在每次 Read 前检查 ctx，使大文件拷贝可以被取消。
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// redactor 把错误信息里出现的凭据替换掉（防御：凭据永不进日志 / 错误信息）。
func redact(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	changed := false
	for _, s := range secrets {
		if len(s) >= 4 && strings.Contains(msg, s) {
			msg = strings.ReplaceAll(msg, s, "***")
			changed = true
		}
	}
	if !changed {
		return err
	}
	return fmt.Errorf("%s", msg)
}
