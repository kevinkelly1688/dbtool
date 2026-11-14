// @author: kevinkelly
// @license: Apache Licence
// @file: free_unix.go
// @time: 2026/10/28

//go:build linux || darwin || freebsd

package engine

import "syscall"

// diskFree 返回 path 所在文件系统对普通用户的可用字节数。
func diskFree(path string) (int64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), true
}
