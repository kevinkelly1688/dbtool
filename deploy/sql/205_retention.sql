-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 205_retention.sql
-- @time: 2026/10/28

-- 数据增长清理（dbtool 新增的表）。
-- 需要在元数据库里执行一次；脚本可重复执行（CREATE TABLE IF NOT EXISTS）。
-- 不修改基础表。清理功能默认关闭（系统设置 retention.enabled），不执行本脚本不影响其它功能：
--   - dbt_retention_run 不存在：清理照常执行，只是页面 / 接口看不到每轮汇总（审计日志与服务日志里仍有）；
--   - dbt_export_purged 不存在：导出文件清理（retention_export_files_days）不会执行（无处记录“已清理”标记，宁可不删）。

-- 每轮清理的汇总。
CREATE TABLE IF NOT EXISTS dbt_retention_run (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  started_at  DATETIME     NOT NULL,
  finished_at DATETIME     NOT NULL,
  trigger_by  VARCHAR(12)  NOT NULL DEFAULT 'schedule' COMMENT 'schedule / manual',
  status      VARCHAR(10)  NOT NULL DEFAULT 'ok' COMMENT 'ok / partial（有目标出错） / failed / skipped',
  deleted     BIGINT       NOT NULL DEFAULT 0 COMMENT '本轮删除的行数 + 文件数 + 备份对象数合计',
  summary     TEXT         NULL COMMENT '每个目标的明细（文本）',
  PRIMARY KEY (id),
  KEY idx_started (started_at)
) ENGINE=InnoDB COMMENT='dbtool 数据清理每轮汇总';

-- 已按保留期清理文件的导出工单。基础表 dbt_sql_workflow 不加列，所以“文件已清理”标记放在这里：
-- 工单页 / 下载接口据此显示“文件已按保留期清理”，而不是“文件不存在”。工单记录本身不动。
CREATE TABLE IF NOT EXISTS dbt_export_purged (
  workflow_id BIGINT UNSIGNED NOT NULL COMMENT 'dbt_sql_workflow.id（is_offline_export=1）',
  file_name   VARCHAR(255) NOT NULL DEFAULT '' COMMENT '被清理的文件名（dbt_sql_workflow.file_name 的快照）',
  size_bytes  BIGINT       NOT NULL DEFAULT 0,
  existed     TINYINT(1)   NOT NULL DEFAULT 1 COMMENT '清理时文件是否还在；0 = 早已被人工删除',
  purged_at   DATETIME     NOT NULL,
  PRIMARY KEY (workflow_id),
  KEY idx_purged (purged_at)
) ENGINE=InnoDB COMMENT='dbtool 导出文件清理标记';
