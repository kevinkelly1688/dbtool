// @author: kevinkelly
// @license: Apache Licence
// @file: term_linux.go
// @time: 2026/10/28

//go:build linux

package dbtadmin

import (
	"os"
	"syscall"
	"unsafe"
)

// disableEcho 用 TCGETS / TCSETS 关掉终端回显，返回恢复函数（只用标准库 syscall）。
func disableEcho(f *os.File) (restore func(), err error) {
	fd := f.Fd()
	var old syscall.Termios
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCGETS, uintptr(unsafe.Pointer(&old))); e != 0 {
		return nil, e
	}
	nt := old
	nt.Lflag &^= syscall.ECHO
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCSETS, uintptr(unsafe.Pointer(&nt))); e != 0 {
		return nil, e
	}
	return func() {
		syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCSETS, uintptr(unsafe.Pointer(&old)))
	}, nil
}
