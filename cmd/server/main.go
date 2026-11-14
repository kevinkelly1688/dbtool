// @author: kevinkelly
// @license: Apache Licence
// @file: main.go
// @time: 2026/10/28

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "dbtool/internal/archive" // 数据归档
	_ "dbtool/internal/authx"   // 登录方式 / 登录安全 / 两步验证
	_ "dbtool/internal/binlog"  // Binlog 解析 / 回滚
	"dbtool/internal/config"
	_ "dbtool/internal/dict"              // 数据字典
	_ "dbtool/internal/duty"              // 值班表：DBA 值班班次与临时调班，顶栏显示当前值班人
	_ "dbtool/internal/engine/clickhouse" // 注册 ClickHouse 引擎（在线查询 / 数据字典 / 实例连通性测试）
	_ "dbtool/internal/engine/dm"         // 注册达梦引擎（在线查询 / 数据字典 / 实例连通性测试）
	_ "dbtool/internal/engine/mongo"      // 注册 MongoDB 引擎（在线查询 / 数据字典 / 实例连通性测试）
	_ "dbtool/internal/engine/mssql"      // 注册 SQL Server 引擎（SQL 工单与批量上线；在线查询尚未支持）
	_ "dbtool/internal/engine/mysql"      // 注册 MySQL 系引擎（mysql/polardb/selectdb/doris/adb）
	_ "dbtool/internal/engine/oracle"     // 注册 Oracle 引擎（在线查询 / 数据字典 / 实例连通性测试）
	_ "dbtool/internal/engine/postgres"   // 注册 PostgreSQL 引擎（在线查询 / 数据字典 / 实例连通性测试）
	_ "dbtool/internal/engine/redis"      // 注册 Redis 引擎（实例连通性测试 / 健康探测；巡检与慢查询直接用其客户端）
	_ "dbtool/internal/envadmin"          // 环境管理：实例环境字典（仅超级用户）
	"dbtool/internal/expiry"              // 使用期限
	_ "dbtool/internal/firewalladmin"     // SQL 防火墙管理（仅超级用户）
	_ "dbtool/internal/giconfig"          // goInception 配置
	_ "dbtool/internal/inspect"           // 性能巡检（含定时巡检）
	_ "dbtool/internal/instmgr"           // 实例管理：列表/会话/数据库/账号/参数
	_ "dbtool/internal/maskadmin"         // 数据脱敏管理（仅超级用户）
	"dbtool/internal/notify"
	_ "dbtool/internal/partman"  // 分区维护
	_ "dbtool/internal/perf"     // 性能工单
	_ "dbtool/internal/queryfav" // 在线查询的收藏（每人自己的）
	"dbtool/internal/redisrule"
	_ "dbtool/internal/redisrule/admin" // Redis 上线规则（仅超级用户）
	_ "dbtool/internal/retention"       // 数据增长清理（默认全部关闭，需 dbt_setting 显式开启）
	"dbtool/internal/server"
	_ "dbtool/internal/slowlog" // 慢查日志 / 查慢日志
	"dbtool/internal/sqlflow"
	_ "dbtool/internal/sqlopt" // SQL 分析 / 优化工具
	"dbtool/internal/store"
	_ "dbtool/internal/sysadmin"  // 系统管理：资源组/用户/权限组/审批流程/系统审计
	_ "dbtool/internal/sysconfig" // 系统配置：本系统自己的设置中心（仅超级用户）
)

// version 可在构建时用 -ldflags "-X main.version=..." 覆盖（make dist 会注入）。
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "打印版本并退出")
	configFile := flag.String("config", "", "配置文件路径（TOML，可指定监听端口等）；不给则依次找 $"+config.ConfigFileEnv+"、./dbtool.toml、可执行文件所在目录、/etc/dbtool/dbtool.toml")
	flag.Parse()
	if *showVersion {
		fmt.Printf("dbtool %s\n", version)
		return
	}
	if err := expiry.Check(); err != nil {
		slog.Error("无法启动", "err", err)
		os.Exit(1)
	}
	cfg, err := config.LoadFile(*configFile)
	if err != nil {
		slog.Error("加载配置失败", "err", err)
		os.Exit(1)
	}
	if cfg.ConfigFile != "" {
		slog.Info("已加载配置文件", "file", cfg.ConfigFile)
	}
	if cfg.Version == "" { // 配置文件 [site] version 优先，没写就用构建时注入的版本
		cfg.Version = version
	}
	for _, w := range cfg.Warnings {
		slog.Warn(w)
	}
	st, err := store.Open(cfg)
	if err != nil {
		slog.Error("初始化存储失败", "err", err)
		os.Exit(1)
	}

	// SQL 工单：执行与定时调度都在后台进行，所以服务在这里构造，以便统一启动/停止。
	sqlSvc := sqlflow.NewService(store.NewSQLRepo(st), sqlflow.Options{
		Notifier:         notify.NewDispatcher(st, st),
		RequireInception: cfg.RequireGoInception,
		BatchDBTypes:     cfg.BatchDBTypes,
		ExportBaseDir:    cfg.ExportBaseDir,
	})
	sqlSvc.BaseURL = cfg.PublicURL
	if names, err := st.UndecryptableConfigItems(); err == nil && len(names) > 0 {
		slog.Warn("系统设置（dbt_setting）里有敏感项的密文用当前 SECRET_KEY / MIRAGE_* 解不开：它们会被当成未设置。"+
			"多半是这些项当初用另一把密钥保存的；请核对密钥，或在“平台设置 › 系统配置”页重新填写", "items", names)
	}
	if names, err := st.UndecryptableInstances(); err == nil && len(names) > 0 {
		slog.Warn("有实例的账号 / 密码看起来是密文，但用当前 SECRET_KEY / MIRAGE_* 解不开：这些实例会连不上目标库（用密文当账号密码去登录）。"+
			"多半是当初用另一把密钥（例如公开缺省密钥）加密的，而当前部署用了另一把；用 dbt-admin check-secrets 核对并按提示处理", "count", len(names), "instances", names)
	}
	sqlSvc.SetRedisRules(redisrule.SourceOf(redisrule.NewGormRepo(st.GormDB()))) // Redis 工单按管理员规则检查；规则表没建时全部拒绝

	r, err := server.New(cfg, st, server.Options{SQLFlow: sqlSvc})
	if err != nil {
		slog.Error("初始化服务失败", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// 运行中越过使用期限：走下面同一套优雅停止流程
	expiry.Watch(ctx, stop)
	waitWorkers := server.RunWorkers(ctx) // 功能模块登记的后台任务（定时巡检、采集等）
	schedDone := make(chan struct{})
	go func() { sqlSvc.StartScheduler(ctx, 15*time.Second); close(schedDone) }()

	srv := &http.Server{Addr: cfg.Listen, Handler: r, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = srv.Shutdown(sh)
	}()
	slog.Info("dbtool 启动", "listen", cfg.Listen, "来源", cfg.ListenFrom)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("服务退出", "err", err)
		os.Exit(1)
	}
	// 正在执行的工单不能被中途丢掉（SQL 已经发到目标库）：停止接收新请求后，等它们跑完，最多等 10 分钟。
	waitWorkers()
	<-schedDone // 调度器先停下，之后不会再有新的后台执行加进来
	done := make(chan struct{})
	go func() { sqlSvc.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Minute):
		slog.Warn("仍有工单在执行，进程即将退出；这些工单会停在“执行中”，请到目标库确认实际执行情况")
	}
}
