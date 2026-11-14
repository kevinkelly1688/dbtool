-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 002_sql_schedule.sql
-- @time: 2026/10/28

-- SQL 工单的定时执行计划（dbtool 新增的表）。需要在元数据库里执行一次；脚本可重复执行。
-- 每个工单最多一条计划；到点后由 dbtool 的调度器把它转入执行队列并删除这一行。
CREATE TABLE IF NOT EXISTS dbt_sql_schedule (
  workflow_id INT UNSIGNED NOT NULL COMMENT 'dbt_sql_workflow.id',
  run_at      DATETIME     NOT NULL COMMENT '计划执行时间',
  created_by  VARCHAR(30)  NOT NULL DEFAULT '',
  executor    VARCHAR(16)  NOT NULL DEFAULT '' COMMENT '执行方式 goinception / native，设置定时时由执行人选定',
  create_time DATETIME     NOT NULL,
  PRIMARY KEY (workflow_id),
  KEY idx_run_at (run_at)
) ENGINE=InnoDB COMMENT='dbtool SQL 工单定时执行计划';
