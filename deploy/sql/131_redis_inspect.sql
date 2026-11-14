-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 131_redis_inspect.sql
-- @time: 2026/10/28

-- Redis 性能巡检报告（dbtool 新增的表）。需要在元数据库里执行一次；脚本可重复执行。
-- 结构与基础表 dbt_sql_mysql_inspection_report 一致（健康评分 + result JSON），这样列表、详情、趋势、计划运行、通知的处理方式相同；
-- 计划与运行记录沿用 dbt_inspect_plan / dbt_inspect_run / dbt_inspect_run_item（013_inspect.sql，run_item.report_kind = 'redis'）。
-- 表不存在时，Redis 之外的巡检功能照常工作；立即巡检 Redis 实例会提示执行本脚本。
-- 注意：这张表不写字符集 / 排序规则，沿用库的默认值，和 dbt_sql_mysql_inspection_report / dbt_sql_mssql_inspection_report 一致——列表用 UNION ALL 把三种报告合并，
-- 字符串列的排序规则不同会报 Illegal mix of collations。（项目策略：所有表、所有字段都不指定，见 docs/charset-policy.md）

CREATE TABLE IF NOT EXISTS dbt_inspect_redis_report (
  id              INT UNSIGNED NOT NULL AUTO_INCREMENT,
  instance_id     INT UNSIGNED NOT NULL COMMENT 'dbt_sql_instance.id',
  status          VARCHAR(10)  NOT NULL DEFAULT 'running' COMMENT 'running / finish / failed / cancelled',
  trigger_type    VARCHAR(10)  NOT NULL DEFAULT 'manual' COMMENT 'manual / schedule',
  operator        VARCHAR(30)  NOT NULL DEFAULT '',
  health_score    INT          NOT NULL DEFAULT 100,
  alert_count     INT          NOT NULL DEFAULT 0,
  attention_count INT          NOT NULL DEFAULT 0,
  failed_count    INT          NOT NULL DEFAULT 0 COMMENT '未采集的检查项数',
  total_items     INT          NOT NULL DEFAULT 0 COMMENT '采集步骤总数（INFO / CONFIG / SLOWLOG / COMMAND / 大 key 抽样）',
  done_items      INT          NOT NULL DEFAULT 0,
  result          LONGTEXT     NULL COMMENT '巡检结果 JSON：server / checks[] / inspection_report',
  error_message   TEXT         NULL,
  start_time      DATETIME     NOT NULL,
  finish_time     DATETIME     NULL,
  PRIMARY KEY (id),
  KEY idx_instance_time (instance_id, start_time),
  KEY idx_status (status),
  KEY idx_start (start_time)
) ENGINE=InnoDB COMMENT='Redis 性能巡检报告';
