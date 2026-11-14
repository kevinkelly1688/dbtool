// @author: kevinkelly
// @license: Apache Licence
// @file: doc.go
// @time: 2026/10/28

// Package partman 实现“数据服务 › 分区维护”（pg-partman，路由 /partman）。
//
// 页面管理配置（dbt_partman_config）与日志（dbt_partman_log）；外部常驻程序（partman daemon）
// 按配置周期性执行 ADD / DROP PARTITION。这里在保持两张表结构不变的前提下，把
// “按配置生成并执行分区语句”也做进来：
//
//   - plan.go     纯函数：根据配置 + 目标表当前的分区状态，计算要创建 / 删除的分区与完整的 ALTER 语句（服务端生成，
//     不接受前端传 DDL）；
//   - service.go  配置 CRUD、预览、手动执行（DROP 必须带确认）、日志、定时维护（RunAuto）；
//   - target.go   目标库访问接口与 MySQL 实现；store.go 元数据库仓库；http.go 路由、页面与菜单。
package partman
