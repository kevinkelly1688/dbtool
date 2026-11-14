// @author: kevinkelly
// @license: Apache Licence
// @file: free_windows.go
// @time: 2026/10/28

//go:build windows

package engine

import (
	"syscall"
	"unsafe"
)

var procGetDiskFreeSpaceEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

// diskFree 返回 path 所在卷对当前用户的可用字节数（只用标准库，不引入 golang.org/x/sys）。
func diskFree(path string) (int64, bool) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, false
	}
	var freeToCaller, total, totalFree uint64
	r, _, _ := procGetDiskFreeSpaceEx.Call(uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&freeToCaller)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&totalFree)))
	if r == 0 {
		return 0, false
	}
	return int64(freeToCaller), true
}
