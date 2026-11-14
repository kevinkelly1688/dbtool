-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 015_slowlog.sql
-- @time: 2026/10/28

-- 慢日志采集器的最近一次采集状态（dbtool 新增的表）。需要在元数据库里执行一次；脚本可重复执行。
-- 不执行也能用：采集器照常工作，只是“查慢日志”页面的采集器状态只显示本进程内的记录（重启后清空）。
-- dbt_slow_logs / dbt_mysql_slow_query_review(_history) 是基础表（由 deploy/bootstrap/001_base_schema.sql 建），
-- dbtool 只读写、不改结构。后两张由 pt-query-digest 的推送脚本写入：脚本里 --review / --history 的 t= 必须是
-- dbt_mysql_slow_query_review / dbt_mysql_slow_query_review_history（不要写不带 dbt_ 前缀的旧表名：新库里没有这些表，会报表不存在）。
CREATE TABLE IF NOT EXISTS dbt_slowlog_collect_state (
  instance_id INT          NOT NULL COMMENT 'dbt_sql_instance.id',
  last_run_at DATETIME     NOT NULL COMMENT '最近一次采集开始时间',
  last_ok_at  DATETIME     NULL     COMMENT '最近一次成功采集时间',
  status      VARCHAR(16)  NOT NULL DEFAULT '' COMMENT 'ok / error / skipped',
  last_error  VARCHAR(500) NOT NULL DEFAULT '' COMMENT '最近一次失败原因（不含密码）',
  last_rows   INT          NOT NULL DEFAULT 0 COMMENT '最近一次写入 dbt_slow_logs 的条数',
  total_rows  BIGINT       NOT NULL DEFAULT 0 COMMENT '本表创建以来累计写入的条数（进程重启后从库里的值继续累加）',
  duration_ms INT          NOT NULL DEFAULT 0 COMMENT '最近一次采集耗时',
  updated_at  DATETIME     NOT NULL,
  PRIMARY KEY (instance_id)
) ENGINE=InnoDB COMMENT='dbtool 慢日志采集状态';

-- 建议（按需手工执行，不是必须）：dbt_slow_logs 很大时，下面两个索引能让“查慢日志”的聚合与采集游标查询走索引。
-- 先 SHOW INDEX FROM dbt_slow_logs 确认是否已经建过。
-- ALTER TABLE dbt_slow_logs ADD INDEX idx_inst_start (instance_id, start_time);
-- ALTER TABLE dbt_slow_logs ADD INDEX idx_inst_micro (instance_id, updated_at_micro);
