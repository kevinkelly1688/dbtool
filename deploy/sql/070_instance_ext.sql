-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 070_instance_ext.sql
-- @time: 2026/10/28

-- 实例管理的旁路扩展表（dbtool 新增）。需要在元数据库里执行一次，然后重启 dbtool；脚本可重复执行。
-- 不改任何基础表（dbt_sql_instance 等原样保留）。
-- 表不存在时：实例仍可正常使用，只是没有“停用 / 启用”和“环境”这两项（页面上的相关按钮会提示先执行本脚本）。

-- 实例的停用状态与环境。没有记录 = 启用、环境未设置。
-- 停用 = 平台不再连接该实例，且它不再出现在在线查询 / SQL 上线 / 巡检 / 慢日志采集等页面的实例下拉里；已有数据保留。
-- 它与 dbt_sql_instance.is_enable（慢日志采集开关）是两回事。
CREATE TABLE IF NOT EXISTS dbt_instance_ext (
  instance_id INT UNSIGNED NOT NULL COMMENT 'dbt_sql_instance.id',
  disabled    TINYINT(1)   NOT NULL DEFAULT 0 COMMENT '1=已停用',
  env         VARCHAR(8)   NOT NULL DEFAULT '' COMMENT 'prod 生产 / test 测试 / 空 未设置',
  disabled_by VARCHAR(30)  NOT NULL DEFAULT '' COMMENT '最近一次停用的操作人',
  disabled_at DATETIME     NULL COMMENT '最近一次停用的时间',
  updated_at  DATETIME     NOT NULL,
  PRIMARY KEY (instance_id),
  KEY idx_disabled (disabled)
) ENGINE=InnoDB COMMENT='dbtool 实例扩展：停用状态与环境';
