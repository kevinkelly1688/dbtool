// @author: kevinkelly
// @license: Apache Licence
// @file: owner_other.go
// @time: 2026/10/28

//go:build windows

package giconfig

import "os"

// Windows 上没有 Unix 属主概念（开发环境用），什么也不做。
type fileOwnerInfo struct{}

func ownerOf(os.FileInfo) *fileOwnerInfo { return nil }

func (o *fileOwnerInfo) apply(string) {}

func syncDir(string) {}
