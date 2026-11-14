-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 001_notify_channel.sql
-- @time: 2026/10/28

-- 机器人通知渠道配置（dbtool 新增的表）。
-- 需要在元数据库里执行一次；脚本可重复执行。
-- webhook_url 与 secret 在写入前已用 SECRET_KEY 加密，库里不会出现明文。
CREATE TABLE IF NOT EXISTS dbt_notify_channel (
  id           INT UNSIGNED NOT NULL AUTO_INCREMENT,
  group_id     INT UNSIGNED NOT NULL DEFAULT 0 COMMENT '资源组 ID；0 表示全局（所有资源组都会发）',
  name         VARCHAR(50)  NOT NULL COMMENT '渠道名称，便于识别',
  channel_type VARCHAR(16)  NOT NULL COMMENT 'dingtalk / lark / feishu / slack',
  webhook_url  VARCHAR(1500) NOT NULL COMMENT '机器人 webhook 地址（加密）',
  secret       VARCHAR(600) NOT NULL DEFAULT '' COMMENT '签名密钥（加密）；钉钉“加签”、飞书/Lark“签名校验”使用，可为空',
  events       VARCHAR(255) NOT NULL DEFAULT '' COMMENT '订阅的事件，逗号分隔；空表示全部',
  mentions     TEXT         NOT NULL COMMENT '@ 规则，JSON 数组',
  enabled      TINYINT(1)   NOT NULL DEFAULT 1,
  created_by   VARCHAR(30)  NOT NULL DEFAULT '',
  create_time  DATETIME     NOT NULL,
  sys_time     DATETIME     NOT NULL,
  PRIMARY KEY (id),
  KEY idx_group (group_id)
) ENGINE=InnoDB COMMENT='dbtool 机器人通知渠道';
