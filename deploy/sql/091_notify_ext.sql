-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 091_notify_ext.sql
-- @time: 2026/10/28

-- 通知渠道的扩展信息（dbtool 新增的表）。
-- 需要在元数据库里执行一次；脚本可重复执行。
-- 与 dbt_notify_channel 一对一，不改 001 建的表：
--   group_ids  一个渠道关联多个资源组（dbt_notify_channel.group_id 仍保存第一个，旧代码照常按单个资源组读取）；
--   duty_users “DBA 值班”@ 目标的值班人员；
--   events_v2  订阅是在“细分事件”模型下保存的（性能工单超时、巡检告警是独立事件）；旧渠道为 0，
--              它们订阅了“执行异常”仍会收到性能超时与巡检告警，行为和升级前一致。
-- 没有这张表时通知照常工作（渠道只能关联一个资源组，没有值班人员）。
CREATE TABLE IF NOT EXISTS dbt_notify_channel_ext (
  channel_id  INT UNSIGNED  NOT NULL COMMENT 'dbt_notify_channel.id',
  group_ids   VARCHAR(255)  NOT NULL DEFAULT '' COMMENT '关联的资源组 ID，逗号分隔；空表示只看 dbt_notify_channel.group_id',
  duty_users  VARCHAR(1000) NOT NULL DEFAULT '' COMMENT 'DBA 值班人员用户名，逗号分隔',
  events_v2   TINYINT(1)    NOT NULL DEFAULT 0 COMMENT '订阅按细分事件模型保存',
  update_time DATETIME      NOT NULL,
  PRIMARY KEY (channel_id)
) ENGINE=InnoDB COMMENT='dbtool 通知渠道扩展信息（与 dbt_notify_channel 一对一）';
