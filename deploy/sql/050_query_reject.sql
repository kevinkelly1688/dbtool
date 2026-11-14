-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 050_query_reject.sql
-- @time: 2026/10/28

-- 在线查询的“未成功”记录（dbtool 新增的表）。
-- 需要在元数据库里执行一次，然后重启 dbtool；脚本可重复执行。
-- 不改基础表 dbt_query_log：成功的查询仍只写 dbt_query_log；被拦截 / 失败 / 超时 / 已取消的记录写在这里。
-- 表不存在时在线查询照常工作，只是执行历史里看不到非成功记录，系统审计里看不到被拒绝语句。

-- 被拦截的语句（语句规则、权限等拒绝）。字段固定，系统审计只读它。
CREATE TABLE IF NOT EXISTS dbt_query_reject (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  user_name   VARCHAR(64)  NOT NULL COMMENT 'dbt_sql_users.username',
  instance_id INT          NOT NULL DEFAULT 0 COMMENT 'dbt_sql_instance.id',
  db_name     VARCHAR(128) NOT NULL DEFAULT '',
  sql_text    TEXT         NULL COMMENT '被拦截的语句，最多保留 2000 个字符',
  reason      VARCHAR(500) NOT NULL DEFAULT '' COMMENT '拦截原因（展示给用户的提示）',
  created_at  DATETIME     NOT NULL,
  PRIMARY KEY (id),
  KEY idx_created (created_at),
  KEY idx_user_created (user_name, created_at),
  KEY idx_instance (instance_id)
) ENGINE=InnoDB COMMENT='dbtool 在线查询被拦截的语句';

-- 全部未成功的查询（执行历史用）：status = blocked 被拦截 / failed 引擎报错 / timeout 超时 / canceled 用户取消。
CREATE TABLE IF NOT EXISTS dbt_query_event (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  user_name     VARCHAR(64)  NOT NULL,
  user_display  VARCHAR(64)  NOT NULL DEFAULT '',
  instance_id   INT          NOT NULL DEFAULT 0,
  instance_name VARCHAR(50)  NOT NULL DEFAULT '',
  db_name       VARCHAR(128) NOT NULL DEFAULT '',
  sql_text      TEXT         NULL COMMENT '最多保留 2000 个字符',
  status        VARCHAR(16)  NOT NULL COMMENT 'blocked / failed / timeout / canceled',
  code          VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '错误码，如 bad_query、no_privilege、timeout',
  reason        VARCHAR(500) NOT NULL DEFAULT '',
  cost_ms       INT          NOT NULL DEFAULT 0,
  created_at    DATETIME     NOT NULL,
  PRIMARY KEY (id),
  KEY idx_created (created_at),
  KEY idx_user_created (user_name, created_at),
  KEY idx_status (status, created_at)
) ENGINE=InnoDB COMMENT='dbtool 在线查询未成功的记录';
