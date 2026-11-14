-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 092_notify_log.sql
-- @time: 2026/10/28

-- 通知发送记录（dbtool 新增的表）。
-- 需要在元数据库里执行一次；脚本可重复执行。
-- 每次向机器人发送（含测试发送）记一条：渠道、事件、结果、耗时、错误摘要（不含 webhook 地址）。保留 30 天，dbtool 写入时自动清理。
-- 没有这张表时通知照常发送，只是管理页不显示最近发送与发送记录。
CREATE TABLE IF NOT EXISTS dbt_notify_log (
  id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  channel_id   INT UNSIGNED    NOT NULL,
  channel_name VARCHAR(50)     NOT NULL DEFAULT '' COMMENT '发送时的渠道名（渠道删除后仍可读）',
  event        VARCHAR(32)     NOT NULL DEFAULT '' COMMENT '事件名，如 pending / perf_overdue',
  kind         VARCHAR(32)     NOT NULL DEFAULT '' COMMENT '工单类别，如 sql_workflow',
  ok           TINYINT(1)      NOT NULL DEFAULT 1,
  cost_ms      INT             NOT NULL DEFAULT 0,
  error        VARCHAR(500)    NOT NULL DEFAULT '' COMMENT '失败原因摘要',
  mentions     INT             NOT NULL DEFAULT 0 COMMENT '本次 @ 的对象数',
  is_test      TINYINT(1)      NOT NULL DEFAULT 0 COMMENT '管理页的测试发送',
  created_at   DATETIME        NOT NULL,
  PRIMARY KEY (id),
  KEY idx_channel (channel_id, id),
  KEY idx_time (created_at)
) ENGINE=InnoDB COMMENT='dbtool 通知发送记录（保留 30 天）';
