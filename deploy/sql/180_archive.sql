-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 180_archive.sql
-- @time: 2026/10/28

-- 定时数据归档（dbtool 新增的表）。
-- 需要在元数据库里执行一次；脚本可重复执行（CREATE TABLE IF NOT EXISTS）。
-- 不修改基础表：dbt_archive_config / dbt_archive_log 保持原样（本功能不迁移其数据，也不写 dbt_archive_log）。
-- 表不存在时：「数据归档」页面提示“请先执行 deploy/sql/180_archive.sql”，不报错，也不登记定时调度。

-- 归档任务。配置里的结构化字段放 JSON 文本列（目标、参数、调度），查询用到的字段是普通列。
CREATE TABLE IF NOT EXISTS dbt_archive_task (
  id                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  name              VARCHAR(50)   NOT NULL COMMENT '任务名称（唯一）',
  resource_group_id INT UNSIGNED  NOT NULL DEFAULT 0 COMMENT '所属资源组（审批流按它取）',
  instance_id       INT UNSIGNED  NOT NULL COMMENT '来源实例 dbt_sql_instance.id',
  db_name           VARCHAR(64)   NOT NULL,
  tbl_name          VARCHAR(64)   NOT NULL,
  time_column       VARCHAR(64)   NOT NULL DEFAULT '',
  time_unit         VARCHAR(12)   NOT NULL DEFAULT '' COMMENT '时间单位：空 = date / datetime / timestamp 列；epoch_s = int / bigint 的 10 位秒；epoch_ms = 13 位毫秒',
  retention_days    INT           NOT NULL DEFAULT 0,
  extra_where       VARCHAR(600)  NOT NULL DEFAULT '' COMMENT '附加条件（≤500 字，已通过安全检查）',
  mode              VARCHAR(8)    NOT NULL DEFAULT 'move' COMMENT 'move 归档并删除 / copy 仅备份 / purge 仅清理',
  purge_reason      VARCHAR(500)  NOT NULL DEFAULT '' COMMENT '不备份理由（mode=purge 必填）',
  dest_table        TEXT          NULL COMMENT '库表备份去向 JSON（engine.TableDest），无则空',
  dest_file         TEXT          NULL COMMENT '文件备份去向 JSON（engine.FileDest），无则空',
  params            TEXT          NULL COMMENT '稳定性参数 JSON（engine.Params，含执行窗口）',
  schedule          TEXT          NULL COMMENT '调度 JSON（kind/time/weekdays/month_day/cron）',
  status            VARCHAR(10)   NOT NULL DEFAULT 'draft' COMMENT 'draft / pending / rejected / enabled / paused / disabled',
  pause_kind        VARCHAR(10)   NOT NULL DEFAULT '' COMMENT 'manual / auto / structure',
  pause_reason      VARCHAR(1000) NOT NULL DEFAULT '',
  pause_at          DATETIME      NULL,
  audit_id          BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '审批流 dbt_workflow_audit.audit_id（workflow_type=3, workflow_id=本表 id）',
  reject_reason     VARCHAR(1000) NOT NULL DEFAULT '',
  checkpoint        TEXT          NULL COMMENT '断点 JSON：最后处理的主键值数组',
  fingerprint       VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '审批 / 试运行时的源表结构指纹',
  spec_hash         VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '关键字段哈希（试运行 token 校验用）',
  cum_rows          BIGINT        NOT NULL DEFAULT 0 COMMENT '累计归档行数（move/purge 为删除行数，copy 为备份行数）',
  cum_runs          INT           NOT NULL DEFAULT 0,
  last_run_id       BIGINT UNSIGNED NOT NULL DEFAULT 0,
  next_run_at       DATETIME      NULL COMMENT '下次计划运行时间（仅已启用）',
  history           MEDIUMTEXT    NULL COMMENT '变更历史 JSON 数组（最近 100 条）',
  version           INT UNSIGNED  NOT NULL DEFAULT 1 COMMENT '乐观并发版本号',
  creator           VARCHAR(30)   NOT NULL DEFAULT '',
  creator_id        INT UNSIGNED  NOT NULL DEFAULT 0,
  creator_display   VARCHAR(50)   NOT NULL DEFAULT '',
  created_at        DATETIME      NOT NULL,
  updated_at        DATETIME      NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_name (name),
  KEY idx_table (instance_id, db_name, tbl_name),
  KEY idx_status (status),
  KEY idx_rg (resource_group_id),
  KEY idx_audit (audit_id)
) ENGINE=InnoDB COMMENT='数据归档任务';

-- 运行记录（定时 / 手动 / 恢复 / 试运行 / 跳过）。running 的记录带租约，进程崩溃后租约过期，启动时标为 interrupted。
CREATE TABLE IF NOT EXISTS dbt_archive_run (
  id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  task_id        BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '0 = 任务已删除或未保存的草稿试运行',
  task_name      VARCHAR(50)   NOT NULL DEFAULT '' COMMENT '任务名快照',
  instance_id    INT UNSIGNED  NOT NULL DEFAULT 0,
  instance_name  VARCHAR(100)  NOT NULL DEFAULT '',
  db_name        VARCHAR(64)   NOT NULL DEFAULT '',
  tbl_name       VARCHAR(64)   NOT NULL DEFAULT '',
  resource_group_id INT UNSIGNED NOT NULL DEFAULT 0,
  trigger_kind   VARCHAR(10)   NOT NULL COMMENT 'schedule / manual / restore / dryrun',
  result         VARCHAR(20)   NOT NULL DEFAULT 'running' COMMENT 'running / paused / success / window_end / load_abort / limit_reached / failed / verify_failed / stopped / structure_changed / interrupted / skipped',
  by_user        VARCHAR(30)   NOT NULL DEFAULT '',
  started_at     DATETIME      NOT NULL,
  finished_at    DATETIME      NULL,
  duration_ms    BIGINT        NOT NULL DEFAULT 0,
  scanned        BIGINT        NOT NULL DEFAULT 0,
  backed         BIGINT        NOT NULL DEFAULT 0,
  deleted        BIGINT        NOT NULL DEFAULT 0,
  batches        INT           NOT NULL DEFAULT 0,
  peak_threads   INT           NOT NULL DEFAULT 0,
  peak_lag       DOUBLE        NOT NULL DEFAULT 0,
  throttle_count INT           NOT NULL DEFAULT 0,
  pk_cols        VARCHAR(500)  NOT NULL DEFAULT '' COMMENT '主键列 JSON',
  min_pk         VARCHAR(1000) NOT NULL DEFAULT '' COMMENT '本次处理的主键范围下界 JSON（恢复用）',
  max_pk         VARCHAR(1000) NOT NULL DEFAULT '' COMMENT '上界 JSON',
  dest_table     VARCHAR(200)  NOT NULL DEFAULT '' COMMENT '库表备份的实际表名 db.table',
  dest_instance_id INT UNSIGNED NOT NULL DEFAULT 0,
  restore_of     BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'trigger=restore 时被恢复的运行',
  check_id       BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'trigger=dryrun 时关联的试运行结果',
  err            TEXT          NULL,
  note           VARCHAR(500)  NOT NULL DEFAULT '' COMMENT '跳过原因等说明',
  auto_pause     TINYINT(1)    NOT NULL DEFAULT 0,
  lease_owner    VARCHAR(80)   NOT NULL DEFAULT '' COMMENT '执行进程标识 host:pid:启动时间',
  lease_until    DATETIME      NULL COMMENT '租约到期时间；running 记录由心跳续期',
  heartbeat_at   DATETIME      NULL,
  progress       TEXT          NULL COMMENT '最近一次进度 JSON（节流落库）',
  spec           MEDIUMTEXT    NULL COMMENT '运行时的 Spec 快照 JSON（恢复 / 追溯用，不含凭据）',
  manifest       MEDIUMTEXT    NULL COMMENT '文件备份清单 JSON（engine.Manifest）',
  verify         TEXT          NULL COMMENT '校验结果 JSON 数组',
  throttles      TEXT          NULL COMMENT '限流事件 JSON 数组',
  backups        MEDIUMTEXT    NULL COMMENT '备份位置 JSON 数组',
  created_at     DATETIME      NOT NULL,
  PRIMARY KEY (id),
  KEY idx_task (task_id, id),
  KEY idx_state (result, lease_until),
  KEY idx_started (started_at),
  KEY idx_restore (restore_of)
) ENGINE=InnoDB COMMENT='数据归档运行记录';

-- 运行事件（引擎 Sink.Event 落库，每个运行保留最近 N 条，由后台清理）。
CREATE TABLE IF NOT EXISTS dbt_archive_event (
  id      BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  run_id  BIGINT UNSIGNED NOT NULL,
  seq     INT UNSIGNED    NOT NULL,
  at      DATETIME(3)     NOT NULL,
  kind    VARCHAR(16)     NOT NULL DEFAULT 'log',
  msg     VARCHAR(1000)   NOT NULL DEFAULT '',
  fields  TEXT            NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_run_seq (run_id, seq)
) ENGINE=InnoDB COMMENT='数据归档运行事件';

-- 批吞吐采样点（吞吐曲线）。每个运行最多保留约 200 个点（采样落库）。
CREATE TABLE IF NOT EXISTS dbt_archive_batch_stat (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  run_id        BIGINT UNSIGNED NOT NULL,
  t_ms          BIGINT          NOT NULL DEFAULT 0 COMMENT '距运行开始的毫秒数',
  rows_per_sec  DOUBLE          NOT NULL DEFAULT 0,
  batch_size    INT             NOT NULL DEFAULT 0,
  throttled     TINYINT(1)      NOT NULL DEFAULT 0,
  PRIMARY KEY (id),
  KEY idx_run (run_id, t_ms)
) ENGINE=InnoDB COMMENT='数据归档批吞吐采样';

-- 备份存储。AK / SK 用 mirage 加密保存（与实例密码同一套 internal/crypto），SK 永不回显。
CREATE TABLE IF NOT EXISTS dbt_archive_store (
  id             INT UNSIGNED NOT NULL AUTO_INCREMENT,
  name           VARCHAR(50)  NOT NULL,
  type           VARCHAR(8)   NOT NULL COMMENT 'local / nfs / s3 / oss',
  path           VARCHAR(500) NOT NULL DEFAULT '' COMMENT 'local / nfs：根目录',
  endpoint       VARCHAR(300) NOT NULL DEFAULT '',
  region         VARCHAR(64)  NOT NULL DEFAULT '',
  bucket         VARCHAR(100) NOT NULL DEFAULT '',
  prefix         VARCHAR(200) NOT NULL DEFAULT '',
  ak             VARCHAR(500) NOT NULL DEFAULT '' COMMENT 'mirage 加密',
  sk             VARCHAR(800) NOT NULL DEFAULT '' COMMENT 'mirage 加密',
  path_style     TINYINT(1)   NOT NULL DEFAULT 0,
  retention_days INT          NOT NULL DEFAULT 0 COMMENT '0 = 永久',
  enabled        TINYINT(1)   NOT NULL DEFAULT 1,
  status         VARCHAR(8)   NOT NULL DEFAULT 'unknown' COMMENT 'ok / err / unknown',
  last_test_at   DATETIME     NULL,
  last_test_ok   TINYINT(1)   NOT NULL DEFAULT 0,
  last_test_ms   INT          NOT NULL DEFAULT 0,
  last_test_msg  VARCHAR(500) NOT NULL DEFAULT '',
  free_bytes     BIGINT       NULL,
  updated_by     VARCHAR(30)  NOT NULL DEFAULT '',
  created_at     DATETIME     NOT NULL,
  updated_at     DATETIME     NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_name (name)
) ENGINE=InnoDB COMMENT='数据归档备份存储';

-- 安全策略（单行 JSON，id 固定为 1）。没有记录时使用内置默认值。
CREATE TABLE IF NOT EXISTS dbt_archive_policy (
  id          INT UNSIGNED NOT NULL,
  content     TEXT         NOT NULL COMMENT '策略 JSON',
  updated_by  VARCHAR(30)  NOT NULL DEFAULT '',
  updated_at  DATETIME     NOT NULL,
  PRIMARY KEY (id)
) ENGINE=InnoDB COMMENT='数据归档安全策略';

-- 试运行结果。token 在提交申请时校验：7 天有效、spec_hash 与任务当前关键字段一致。
CREATE TABLE IF NOT EXISTS dbt_archive_check (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  token       VARCHAR(64)  NOT NULL,
  task_id     BIGINT UNSIGNED NOT NULL DEFAULT 0,
  spec_hash   VARCHAR(64)  NOT NULL,
  created_by  VARCHAR(30)  NOT NULL DEFAULT '',
  created_at  DATETIME     NOT NULL,
  expires_at  DATETIME     NOT NULL,
  blocked     TINYINT(1)   NOT NULL DEFAULT 0 COMMENT '报告里有红色项',
  warn_count  INT          NOT NULL DEFAULT 0,
  big         TINYINT(1)   NOT NULL DEFAULT 0 COMMENT '命中占表比例超过 30%',
  fingerprint VARCHAR(64)  NOT NULL DEFAULT '',
  report      MEDIUMTEXT   NULL COMMENT 'engine.CheckReport JSON',
  PRIMARY KEY (id),
  UNIQUE KEY uk_token (token),
  KEY idx_task (task_id),
  KEY idx_expires (expires_at)
) ENGINE=InnoDB COMMENT='数据归档试运行结果';

-- 升级：给已有的 dbt_archive_task 补 time_unit 列（时间列是 int / bigint 里的 10 位秒 / 13 位毫秒时间戳时用，旧任务默认空，等于原来的 datetime 行为）。
-- MySQL 5.7 的加列语句不支持 IF NOT EXISTS，所以用 information_schema 判断后动态执行。新装的库建表时已带这一列，这里什么也不做。
-- 程序启动后发现缺这一列，「数据归档」页面会提示先执行本脚本（与表不存在时一样），不会报错。
SET @has_col := (SELECT COUNT(*) FROM information_schema.columns
                  WHERE table_schema = DATABASE() AND table_name = 'dbt_archive_task' AND column_name = 'time_unit');
SET @ddl := IF(@has_col = 0,
  'ALTER TABLE dbt_archive_task ADD COLUMN time_unit VARCHAR(12) NOT NULL DEFAULT '''' COMMENT ''时间单位：空 = date / datetime / timestamp 列，epoch_s = int / bigint 的 10 位秒，epoch_ms = 13 位毫秒'' AFTER time_column',
  'SELECT 1');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;
