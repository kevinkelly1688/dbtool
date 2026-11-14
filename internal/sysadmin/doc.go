// @author: kevinkelly
// @license: Apache Licence
// @file: doc.go
// @time: 2026/10/28

// Package sysadmin 实现“系统管理”里的几个页面：审批流程配置、资源组管理、用户管理、权限组管理、系统审计。
// 所有写操作在同一个事务里同时写 dbt_audit_log。
package sysadmin
