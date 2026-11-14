// @author: kevinkelly
// @license: Apache Licence
// @file: main.go
// @time: 2026/10/28

// dbt-admin：dbtool 的初始化与管理员命令行工具（初始化库、创建 / 重置管理员、生成密钥）。
// 用法与安全设计见 docs/admin-cli.md。
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"dbtool/internal/dbtadmin"
	"dbtool/internal/expiry"
)

// version 可在构建时用 -ldflags "-X main.version=..." 覆盖。
var version = "dev"

func main() {
	if err := expiry.Check(); err != nil {
		fmt.Fprintln(os.Stderr, "错误：", err)
		os.Exit(dbtadmin.ExitFailed)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := dbtadmin.Main(ctx, os.Args[1:], dbtadmin.DefaultEnv(version))
	stop()
	os.Exit(code)
}
