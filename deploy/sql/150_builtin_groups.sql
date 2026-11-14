-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 150_builtin_groups.sql
-- @time: 2026/10/28

-- 内置权限组（dbtool 新增）：给权限组扩展表 dbt_auth_group_ext 加 builtin 列，并把名称恰为 DBA / TL / RD / QA / Default 的权限组标成内置。
-- 内置组不能改名、不能删除（可以调整权限和说明）；不影响组内成员和权限，也不改 dbt_auth_group 等基础表。
-- 需要在元数据库里执行一次；可重复执行（幂等）。
-- 依赖 080_sysadmin_ext.sql 先建好 dbt_auth_group_ext；没有这张表时本脚本什么也不做，系统照常运行（所有权限组按普通组处理）。
-- MySQL 5.7 的加列语句不支持 IF NOT EXISTS，所以用 information_schema 判断后动态执行。

SET @has_tbl := (SELECT COUNT(*) FROM information_schema.tables
                  WHERE table_schema = DATABASE() AND table_name = 'dbt_auth_group_ext');
SET @has_col := (SELECT COUNT(*) FROM information_schema.columns
                  WHERE table_schema = DATABASE() AND table_name = 'dbt_auth_group_ext' AND column_name = 'builtin');

SET @ddl := IF(@has_tbl = 1 AND @has_col = 0,
  'ALTER TABLE dbt_auth_group_ext ADD COLUMN builtin TINYINT(1) NOT NULL DEFAULT 0 COMMENT ''1=内置权限组：不能改名、不能删除'' AFTER description',
  'SELECT 1');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

-- 按名称匹配 dbt_auth_group（基础表，只读）；已有扩展行只更新 builtin，说明等其他字段不动。
SET @mark := IF(@has_tbl = 1,
  'INSERT INTO dbt_auth_group_ext (group_id, builtin, updated_by, updated_at) SELECT id, 1, ''system'', NOW() FROM dbt_auth_group WHERE name IN (''DBA'', ''TL'', ''RD'', ''QA'', ''Default'') ON DUPLICATE KEY UPDATE builtin = 1',
  'SELECT 1');
PREPARE stmt FROM @mark;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;
