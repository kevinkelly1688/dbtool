-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 017_perf.sql
-- @time: 2026/10/28

-- SQL 性能工单（dbtool 新增的表）。需要在元数据库里执行一次；脚本可重复执行。
--
-- 性能工单本身沿用基础表 dbt_perf_*（dbt_perf_issue、dbt_perf_issue_log、dbt_perf_issue_stat、dbt_perf_team、dbt_perf_collect_config、
-- dbt_perf_route、dbt_perf_sla_policy、dbt_perf_digest_cursor；由 deploy/bootstrap/001_base_schema.sql 建），
-- dbtool 只读写它们的既有列，不改结构，也不在这里建。
-- 这里只新增后台任务的运行记录表。
--
-- dbt_perf_job_run：按天/按周执行一次的后台任务（每日验证、周报）在这里用 (job, run_key) 的唯一键抢占，
-- 同一个任务同一周期只会有一个 dbtool 进程执行（多实例部署、重启后都不会重复发周报）。
-- 表不存在时：每日验证退回进程内按天去重；周报不会执行（宁可不发，也不能重启一次发一次）。
CREATE TABLE IF NOT EXISTS dbt_perf_job_run (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  job         VARCHAR(32)  NOT NULL COMMENT '任务名：verify 每日验证 / report 周报',
  run_key     VARCHAR(64)  NOT NULL COMMENT '周期键：verify:2026-10-06 / weekly:2026-W41',
  started_at  DATETIME     NOT NULL,
  finished_at DATETIME     NULL,
  result      VARCHAR(500) NOT NULL DEFAULT '' COMMENT '结果摘要或 ERROR 信息',
  PRIMARY KEY (id),
  UNIQUE KEY uk_job_run (job, run_key)
) ENGINE=InnoDB COMMENT='dbtool 性能工单后台任务运行记录';
