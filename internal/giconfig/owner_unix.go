// @author: kevinkelly
// @license: Apache Licence
// @file: owner_unix.go
// @time: 2026/10/28

//go:build !windows

package giconfig

import (
	"os"
	"syscall"
)

// fileOwnerInfo 记录原文件的属主，替换文件后尽量恢复，避免 goInception 因属主/属组变化读不到配置。
type fileOwnerInfo struct{ uid, gid int }

func ownerOf(fi os.FileInfo) *fileOwnerInfo {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return &fileOwnerInfo{uid: int(st.Uid), gid: int(st.Gid)}
}

// apply 先尝试完整恢复属主与属组（需要 root），不行就只恢复属组（dbtool 用户在该组里即可）；都失败也不报错。
func (o *fileOwnerInfo) apply(path string) {
	if err := os.Chown(path, o.uid, o.gid); err != nil {
		_ = os.Chown(path, -1, o.gid)
	}
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}
