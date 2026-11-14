-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 230_masking_policy.sql
-- @time: 2026/10/28

-- 数据脱敏策略表（dbtool 新增）。需要在元数据库里执行一次，然后重启 dbtool；脚本可重复执行。
-- “系统管理 › 数据脱敏”页面和在线查询都只读这张表。
-- 表不存在时：在线查询不脱敏，脱敏页提示先执行本脚本。

-- 一行 = 对一个字段的一条脱敏规则。
--   scope_db / scope_table 为 * 表示“该实例所有库表里的同名字段”（要同时为 *）；
--   PostgreSQL / SQL Server 的 scope_table 写成 架构.表；MongoDB 的 scope_table 是集合名、field_path 是点号路径（contacts[].mobile 表示数组元素）。
CREATE TABLE IF NOT EXISTS dbt_masking_policy (
  id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  instance_id  INT UNSIGNED    NOT NULL COMMENT 'dbt_sql_instance.id',
  scope_db     VARCHAR(128)    NOT NULL COMMENT '库名；* = 任意库',
  scope_table  VARCHAR(255)    NOT NULL COMMENT '表名（架构.表 / 集合）；* = 任意表',
  field_path   VARCHAR(255)    NOT NULL COMMENT '字段名；MongoDB 为点号路径',
  rule_kind    VARCHAR(16)     NOT NULL COMMENT 'keep 保留首尾 / three 三段式 / whole 整体隐藏 / regex 自定义正则',
  keep_prefix  SMALLINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'keep：保留前几位',
  keep_suffix  SMALLINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'keep：保留后几位',
  hide_group   TINYINT UNSIGNED  NOT NULL DEFAULT 0 COMMENT 'three：1 前段 2 中段 3 后段；regex：要隐藏的分组',
  rule_regex   VARCHAR(255)    NOT NULL DEFAULT '' COMMENT 'regex：正则',
  active       TINYINT(1)      NOT NULL DEFAULT 1 COMMENT '1=启用',
  updated_by   VARCHAR(30)     NOT NULL DEFAULT '',
  created_at   DATETIME        NOT NULL,
  updated_at   DATETIME        NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_scope (instance_id, scope_db, scope_table, field_path),
  KEY idx_instance_active (instance_id, active)
) ENGINE=InnoDB COMMENT='dbtool 数据脱敏策略（字段级）';
