// @author: kevinkelly
// @license: Apache Licence
// @file: doc.go
// @time: 2026/10/28

// Package inspect 实现“实例运维 › 性能巡检”：MySQL 系与 SQL Server 实例的只读巡检、报告查看，
// 以及多计划的定时巡检（周期、错峰并发、通知、保留天数）。
//
// 报告存放在这些表：dbt_sql_mysql_inspection_report、dbt_sql_mssql_inspection_report、dbt_sql_mssql_inspection_item，
// 新增 dbt_inspect_plan / dbt_inspect_plan_instance / dbt_inspect_run / dbt_inspect_run_item（deploy/sql/013_inspect.sql）。
package inspect
