-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 210_notify_email.sql
-- @time: 2026/10/28

-- 邮件通知渠道的 SMTP 配置（dbtool 新增的表）。
-- 需要在元数据库里执行一次；脚本可重复执行（CREATE TABLE IF NOT EXISTS）。不修改基础表，也不改 dbt_notify_* 的已有表。
-- 与 dbt_notify_channel 一对一，只有 channel_type = 'email' 的渠道才有这一行：
--   config  SMTP 主机 / 端口 / 加密方式 / 是否校验证书 / 用户名 / 是否允许明文认证 / 发件人 / 固定收件人 / 主题前缀，JSON 文本；
--           不含密码——SMTP 密码与机器人的“签名密钥”共用 dbt_notify_channel.secret（同一套加密，页面不回显，编辑时留空 = 不修改）；
--           dbt_notify_channel.webhook_url 对邮件渠道为空。
-- 没有这张表时：其它渠道照常工作；页面不能新建邮件渠道（保存时提示先执行本脚本）。
CREATE TABLE IF NOT EXISTS dbt_notify_email (
  channel_id  INT UNSIGNED NOT NULL COMMENT 'dbt_notify_channel.id',
  config      TEXT         NOT NULL COMMENT 'SMTP 配置 JSON（不含密码）',
  update_time DATETIME     NOT NULL,
  PRIMARY KEY (channel_id)
) ENGINE=InnoDB COMMENT='dbtool 邮件通知渠道的 SMTP 配置（与 dbt_notify_channel 一对一）';
