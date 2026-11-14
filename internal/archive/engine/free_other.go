// @author: kevinkelly
// @license: Apache Licence
// @file: free_other.go
// @time: 2026/10/28

//go:build !linux && !darwin && !freebsd && !windows

package engine

// diskFree 在不支持的平台上取不到可用空间。
func diskFree(path string) (int64, bool) { return 0, false }
