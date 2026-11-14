// @author: kevinkelly
// @license: Apache Licence
// @file: doc.go
// @time: 2026/10/28

// Package slowlog 实现两个慢日志页面和后台采集器：
//
//   - “性能优化 › 慢日志检索”（/slowlogs）：平台采集的 dbt_slow_logs 表，按“实例 × 库 × SQL 指纹 × 天”聚合，可看当天最近 100 条；
//   - “性能优化 › 慢 SQL 洞察”（/slowquery）：慢日志统计、明细、历史趋势，并可对选中的 SQL 获取优化建议。
//     数据源按实例类型选择：pt-query-digest 推送的 dbt_mysql_slow_query_review(_history) 表、平台采集的 dbt_slow_logs、
//     阿里云 RDS（只定义接口，未配置时提示）；Redis 暂不支持；
//   - 采集器（collector.go）：定时从实例的 mysql.slow_log / Doris 审计日志拉取增量写入 dbt_slow_logs，默认开启（只采实例上“慢日志采集”开关打开的实例，环境变量 SLOWLOG_COLLECT_ENABLE=0 可整体关闭）。
package slowlog
