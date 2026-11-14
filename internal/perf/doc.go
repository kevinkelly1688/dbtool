// @author: kevinkelly
// @license: Apache Licence
// @file: doc.go
// @time: 2026/10/28

// Package perf 是“SQL 性能工单”：把生产环境的慢 SQL 变成有负责人、有期限、会自动催办的工单。
//
// 工单、团队、路由、SLA、采集配置存放在基础表里（只读写、不改结构），
// 新增的运行记录放在 dbt_perf_*（deploy/sql/017_perf.sql）。
//
// 代码布局：
//
//	types.go        枚举、数据模型、对外结构
//	service.go      业务服务（工单状态机、查询、权限）与 Repo 接口
//	sla.go          级别评估、SLA 时限、巡检升级
//	ingest.go       采集结果入库（建单、刷新指标、回归重开）与采集编排
//	collectors.go   采集来源（接口 + 元库/目标库实现）
//	diagnose.go     自动诊断（文本规则、指标规则、EXPLAIN 规则）
//	fingerprint.go  SQL 指纹
//	board.go        团队排行榜与周报
//	config.go       采集/团队/路由/SLA 配置的增删改查与校验
//	notify.go       状态变化通知（复用 internal/notify）
//	jobs.go         后台任务（server.AddWorker）
//	store.go        Repo 的 GORM 实现
//	http.go         页面、JSON 接口与菜单登记
package perf
