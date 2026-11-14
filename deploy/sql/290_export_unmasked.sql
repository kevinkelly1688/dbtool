-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 290_export_unmasked.sql
-- @time: 2026/10/28

-- 数据导出工单的扩展选项（dbtool 新增）：超级用户提交时选择的“导出原值（不脱敏）”，以及批量导出时选择的“合并成一个文件”。
-- 需要在元数据库里执行一次（升级时 install.sh 会自动执行 sql/*.sql）；脚本可重复执行。
-- 不改 dbt_sql_workflow（与外部程序共用的基础表），选项单独放在下面这张表里；一行 = 这张导出工单的扩展选项，没有这一行的导出工单按默认处理：
-- 按系统的脱敏设置导出，批量导出每个实例一个文件。
-- 表不存在时：导出申请页不出现“导出原值”和“合并成一个文件”选项；只缺 merge_files 这一列时：只不出现“合并成一个文件”。其它功能不受影响。

CREATE TABLE IF NOT EXISTS dbt_sql_export_ext (
  workflow_id INT UNSIGNED NOT NULL COMMENT 'dbt_sql_workflow.id（数据导出工单）',
  unmasked    TINYINT      NOT NULL DEFAULT 0 COMMENT '1 = 提交人（超级用户）选择了导出原值：执行时不做用户配置的脱敏（SQL 防火墙要求强制脱敏的列仍然脱敏）',
  reason      VARCHAR(255) NOT NULL DEFAULT '' COMMENT '导出原值的理由（必填，审批人会看到）',
  set_by      VARCHAR(30)  NOT NULL DEFAULT '' COMMENT '选择导出原值的人（提交人登录名）',
  merge_files TINYINT      NOT NULL DEFAULT 0 COMMENT '1 = 批量导出合并成一个文件：各实例导出的数据依次写进同一个文件，不再每个实例一个文件',
  created_at  DATETIME     NOT NULL,
  PRIMARY KEY (workflow_id)
) ENGINE=InnoDB COMMENT='数据导出工单的扩展：导出原值（不脱敏）、批量导出合并成一个文件';

-- 升级：给已有的 dbt_sql_export_ext 补 merge_files 列（只执行过旧版脚本的库）。
-- MySQL 5.7 的加列语句不支持 IF NOT EXISTS，所以用 information_schema 判断后动态执行。新装的库建表时已带这一列，这里什么也不做。
SET @has_col := (SELECT COUNT(*) FROM information_schema.columns
                  WHERE table_schema = DATABASE() AND table_name = 'dbt_sql_export_ext' AND column_name = 'merge_files');
SET @ddl := IF(@has_col = 0,
  'ALTER TABLE dbt_sql_export_ext ADD COLUMN merge_files TINYINT NOT NULL DEFAULT 0 COMMENT ''1 = 批量导出合并成一个文件：各实例导出的数据依次写进同一个文件，不再每个实例一个文件'' AFTER set_by',
  'SELECT 1');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;
