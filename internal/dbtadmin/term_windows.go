// @author: kevinkelly
// @license: Apache Licence
// @file: term_windows.go
// @time: 2026/10/28

//go:build windows

package dbtadmin

import (
	"os"
	"syscall"
	"unsafe"
)

const enableEchoInput = 0x0004

var (
	kernel32       = syscall.NewLazyDLL("kernel32.dll")
	procGetConMode = kernel32.NewProc("GetConsoleMode")
	procSetConMode = kernel32.NewProc("SetConsoleMode")
)

// disableEcho 用 SetConsoleMode 去掉 ENABLE_ECHO_INPUT，返回恢复函数。
func disableEcho(f *os.File) (restore func(), err error) {
	h := f.Fd()
	var mode uint32
	if r, _, e := procGetConMode.Call(h, uintptr(unsafe.Pointer(&mode))); r == 0 {
		return nil, e
	}
	if r, _, e := procSetConMode.Call(h, uintptr(mode&^enableEchoInput)); r == 0 {
		return nil, e
	}
	return func() { procSetConMode.Call(h, uintptr(mode)) }, nil
}
