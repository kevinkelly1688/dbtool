// @author: kevinkelly
// @license: Apache Licence
// @file: term_other.go
// @time: 2026/10/28

//go:build !linux && !windows

package dbtadmin

import "os"

func disableEcho(*os.File) (func(), error) { return nil, errNoEcho }
