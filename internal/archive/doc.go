// @author: kevinkelly
// @license: Apache Licence
// @file: doc.go
// @time: 2026/10/28

// Package archive 是“数据归档”的服务层：任务（草稿 / 审批 / 启停）、试运行、调度与运行控制、备份存储、安全策略、
// 以及 /archive 页面和 /archive/api/ JSON 接口。执行引擎（分批读 → 写备份 → 校验 → 删源、限流、文件存储）在
// internal/archive/engine，本包通过它的 contract.go 调用，不直接碰源库数据。
//
// 元数据表见 deploy/sql/180_archive.sql（dbt_archive_*），
// 不修改任何基础表；审批复用审批流存储与审批人计算（internal/audit，workflow_type=3“数据归档申请”）。
//
// 表不存在时（没执行 180_archive.sql）页面提示先执行脚本，不报 500，也不登记定时调度。
package archive
