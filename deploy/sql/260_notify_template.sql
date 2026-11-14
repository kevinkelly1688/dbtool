-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 260_notify_template.sql
-- @time: 2026/10/28

-- 通知消息模板（dbtool 新增）。需要在元数据库里执行一次（升级时 install.sh 会自动执行 sql/*.sql）；脚本可重复执行。
-- 管理员在“通知渠道 › 消息模板”里按“消息（事件）× 范围（全局 / 某个渠道）”定制推送的标题、状态色、字段与按钮。
-- 发送时：渠道专属模板 → 全局模板 → 系统默认文案（没有模板 / 表不存在时发原来的文本，行为不变）。
-- 表不存在时：消息模板页提示“请先执行 260_notify_template.sql”，通知照常按系统默认文案发送。

CREATE TABLE IF NOT EXISTS dbt_notify_template (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  event_key   VARCHAR(64)     NOT NULL COMMENT '消息键，如 sql.submitted / sql.finished / priv.changed',
  scope_key   VARCHAR(32)     NOT NULL COMMENT '范围：global 或 c:<渠道 ID>（渠道专属，优先于全局）',
  name        VARCHAR(60)     NOT NULL DEFAULT '' COMMENT '模板名称（管理页展示）',
  enabled     TINYINT(1)      NOT NULL DEFAULT 1 COMMENT '停用后按上一层 / 系统默认发送',
  title       VARCHAR(255)    NOT NULL COMMENT '标题模板，可含 {{变量}}',
  tone        VARCHAR(8)      NOT NULL DEFAULT 'info' COMMENT '状态色：info / ok / warn / crit',
  icon        VARCHAR(16)     NOT NULL DEFAULT '' COMMENT '标题前的图标（一个 emoji 或短文字）',
  blocks      MEDIUMTEXT      NOT NULL COMMENT '内容块 JSON：fields / text / code / link / divider',
  updated_by  VARCHAR(150)    NOT NULL DEFAULT '',
  created_at  DATETIME        NOT NULL,
  updated_at  DATETIME        NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_event_scope (event_key, scope_key)
) ENGINE=InnoDB COMMENT='dbtool 通知消息模板';

CREATE TABLE IF NOT EXISTS dbt_notify_template_hist (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  event_key   VARCHAR(64)     NOT NULL,
  scope_key   VARCHAR(32)     NOT NULL,
  action      VARCHAR(16)     NOT NULL COMMENT 'save / delete',
  snapshot    MEDIUMTEXT      NULL COMMENT '操作后的模板 JSON（delete 时是删除前的内容）',
  note        VARCHAR(200)    NOT NULL DEFAULT '' COMMENT '修改说明',
  operator    VARCHAR(150)    NOT NULL DEFAULT '',
  created_at  DATETIME        NOT NULL,
  PRIMARY KEY (id),
  KEY idx_event_scope (event_key, scope_key, id)
) ENGINE=InnoDB COMMENT='dbtool 通知消息模板的修改历史（每个模板保留最近 30 条）';
