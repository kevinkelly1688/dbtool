// @author: kevinkelly
// @license: Apache Licence
// @file: doc.go
// @time: 2026/10/28

// Package instmgr 实现顶栏“实例管理”下的五个页面：实例列表、会话管理、数据库管理、账号管理、参数配置。
//
// 业务逻辑（service*.go）只依赖本包定义的 Repo（元数据库）与 Dialer/Target（目标库）两个接口，
// 真实实现分别是 store.go（GORM）与 dialer.go（database/sql + go-sql-driver/mysql），测试用假实现。
package instmgr
