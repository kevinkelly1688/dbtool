-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 250_query_favorite.sql
-- @time: 2026/10/28

-- 在线查询的“收藏”（dbtool 新增）。需要在元数据库里执行一次（升级时 install.sh 会自动执行 sql/*.sql）；脚本可重复执行。
-- 每个人只能看到和管理自己的收藏；执行历史不需要新表，沿用 dbt_query_log / dbt_query_event。
-- 表不存在时：查询页的“收藏”标签提示“请先执行 250_query_favorite.sql”，其它功能不受影响。

CREATE TABLE IF NOT EXISTS dbt_query_favorite (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  owner         VARCHAR(150)    NOT NULL COMMENT '收藏人（dbt_sql_users.username）',
  remark        VARCHAR(200)    NOT NULL DEFAULT '' COMMENT '注释',
  sql_text      TEXT            NOT NULL COMMENT '收藏的 SQL',
  sql_md5       CHAR(32)        NOT NULL COMMENT 'sql_text 去掉首尾空白后的 md5：同一个人同一条 SQL 只存一份',
  instance_name VARCHAR(100)    NOT NULL DEFAULT '' COMMENT '收藏时所在的实例（仅展示，不参与权限）',
  db_name       VARCHAR(128)    NOT NULL DEFAULT '' COMMENT '收藏时所在的库（仅展示）',
  use_count     INT UNSIGNED    NOT NULL DEFAULT 0 COMMENT '被“使用”的次数',
  created_at    DATETIME        NOT NULL,
  updated_at    DATETIME        NOT NULL,
  last_used_at  DATETIME        NULL COMMENT '最后一次被“使用”的时间',
  PRIMARY KEY (id),
  UNIQUE KEY uk_owner_sql (owner, sql_md5),
  KEY idx_owner_used (owner, last_used_at)
) ENGINE=InnoDB COMMENT='dbtool 在线查询收藏的 SQL（每人自己的）';
