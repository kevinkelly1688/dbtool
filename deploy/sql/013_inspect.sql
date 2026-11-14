-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 013_inspect.sql
-- @time: 2026/10/28

-- 性能巡检的定时计划（dbtool 新增的表）。需要在元数据库里执行一次；脚本可重复执行。
-- 巡检报告沿用基础表 dbt_sql_mysql_inspection_report、dbt_sql_mssql_inspection_report、dbt_sql_mssql_inspection_item
-- （由 deploy/bootstrap/001_base_schema.sql 建），不改它们的结构。
-- 没有外键约束：计划与运行记录由 dbtool 自己维护，删除实例 / 报告时不需要联动。

-- 计划：每个计划自选实例（dbt_inspect_plan_instance）和 / 或标签，自己的周期、错峰并发、通知、报告保留天数。
CREATE TABLE IF NOT EXISTS dbt_inspect_plan (
  id                INT UNSIGNED NOT NULL AUTO_INCREMENT,
  name              VARCHAR(60)  NOT NULL COMMENT '计划名称（唯一）',
  enabled           TINYINT(1)   NOT NULL DEFAULT 1,
  schedule_kind     VARCHAR(8)   NOT NULL DEFAULT 'daily' COMMENT 'daily / weekly / monthly / cron',
  schedule_time     CHAR(5)      NOT NULL DEFAULT '' COMMENT 'HH:MM；cron 类型为空',
  schedule_weekdays VARCHAR(20)  NOT NULL DEFAULT '' COMMENT '每周几，逗号分隔，0=周日 … 6=周六',
  schedule_monthday TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '每月几日（1-28）',
  cron_expr         VARCHAR(100) NOT NULL DEFAULT '' COMMENT '最终生效的 5 段 cron 表达式（由上面几项生成，或用户输入）',
  tag_codes         VARCHAR(500) NOT NULL DEFAULT '' COMMENT '按标签选实例（dbt_sql_instance_tag.tag_code，逗号分隔），运行时动态取',
  mssql_databases   TEXT         NULL COMMENT 'SQL Server 每库检查项的库范围（JSON 数组）；空表示全部用户库',
  concurrency       TINYINT UNSIGNED NOT NULL DEFAULT 2 COMMENT '同时巡检几个实例',
  stagger_seconds   INT UNSIGNED NOT NULL DEFAULT 0 COMMENT '每启动一个实例的间隔（秒，错峰）',
  skip_busy         TINYINT(1)   NOT NULL DEFAULT 1 COMMENT '实例上已有进行中的巡检时跳过；否则等它结束',
  retry_once        TINYINT(1)   NOT NULL DEFAULT 0 COMMENT '失败后 5 分钟重试一次',
  timeout_minutes   SMALLINT UNSIGNED NOT NULL DEFAULT 15 COMMENT '单实例超时（分钟）',
  notify_mode       VARCHAR(8)   NOT NULL DEFAULT 'none' COMMENT 'none / alert（仅告警或失败时）/ always',
  notify_channels   VARCHAR(255) NOT NULL DEFAULT '' COMMENT 'dbt_notify_channel.id，逗号分隔',
  retention_days    SMALLINT UNSIGNED NOT NULL DEFAULT 90 COMMENT '本计划产生的报告保留天数',
  next_run_at       DATETIME     NULL COMMENT '下次运行时间；调度器用带条件的 UPDATE 原子占位，多个 dbtool 实例同时运行也只会领取一次',
  last_run_at       DATETIME     NULL,
  last_run_id       INT UNSIGNED NOT NULL DEFAULT 0,
  created_by        VARCHAR(30)  NOT NULL DEFAULT '',
  create_time       DATETIME     NOT NULL,
  sys_time          DATETIME     NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_name (name),
  KEY idx_due (enabled, next_run_at)
) ENGINE=InnoDB COMMENT='dbtool 定时巡检计划';

CREATE TABLE IF NOT EXISTS dbt_inspect_plan_instance (
  plan_id     INT UNSIGNED NOT NULL,
  instance_id INT UNSIGNED NOT NULL COMMENT 'dbt_sql_instance.id',
  PRIMARY KEY (plan_id, instance_id),
  KEY idx_instance (instance_id)
) ENGINE=InnoDB COMMENT='定时巡检计划选中的实例';

-- 计划的一次运行（定时触发或手动“立即运行”）。心跳用来发现进程中断留下的“运行中”记录。
CREATE TABLE IF NOT EXISTS dbt_inspect_run (
  id              INT UNSIGNED NOT NULL AUTO_INCREMENT,
  plan_id         INT UNSIGNED NOT NULL,
  plan_name       VARCHAR(60)  NOT NULL DEFAULT '' COMMENT '创建时的计划名快照',
  trigger_type    VARCHAR(10)  NOT NULL DEFAULT 'schedule' COMMENT 'schedule / manual',
  operator        VARCHAR(30)  NOT NULL DEFAULT '' COMMENT '手动运行的操作人；定时为“系统”',
  scheduled_at    DATETIME     NOT NULL COMMENT '计划触发时间',
  started_at      DATETIME     NOT NULL,
  finished_at     DATETIME     NULL,
  status          VARCHAR(10)  NOT NULL DEFAULT 'running' COMMENT 'running / finish / failed / skipped',
  instance_total  INT NOT NULL DEFAULT 0,
  ok_count        INT NOT NULL DEFAULT 0,
  failed_count    INT NOT NULL DEFAULT 0,
  skipped_count   INT NOT NULL DEFAULT 0,
  alert_total     INT NOT NULL DEFAULT 0,
  attention_total INT NOT NULL DEFAULT 0,
  note            VARCHAR(1000) NOT NULL DEFAULT '',
  heartbeat_at    DATETIME     NOT NULL,
  PRIMARY KEY (id),
  KEY idx_plan_started (plan_id, started_at),
  KEY idx_status_hb (status, heartbeat_at)
) ENGINE=InnoDB COMMENT='定时巡检计划的运行记录';

-- 一次运行里每个实例的结果，并记录它产生的报告（report_kind = mysql / mssql，对应 dbt_sql_mysql_inspection_report / dbt_sql_mssql_inspection_report 两张报告表）。
CREATE TABLE IF NOT EXISTS dbt_inspect_run_item (
  id              INT UNSIGNED NOT NULL AUTO_INCREMENT,
  run_id          INT UNSIGNED NOT NULL,
  instance_id     INT UNSIGNED NOT NULL,
  instance_name   VARCHAR(50)  NOT NULL DEFAULT '',
  report_kind     VARCHAR(8)   NOT NULL DEFAULT '',
  report_id       INT UNSIGNED NOT NULL DEFAULT 0 COMMENT '0 表示没有产生报告（跳过 / 建报告失败）',
  status          VARCHAR(10)  NOT NULL DEFAULT '' COMMENT 'ok / failed / skipped / cancelled',
  alert_count     INT NOT NULL DEFAULT 0,
  attention_count INT NOT NULL DEFAULT 0,
  message         VARCHAR(255) NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  KEY idx_run (run_id),
  KEY idx_report (report_kind, report_id)
) ENGINE=InnoDB COMMENT='定时巡检运行明细';
