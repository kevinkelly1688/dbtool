-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 040_query_apply_ext.sql
-- @time: 2026/10/28

-- 查询权限申请的扩展信息（dbtool 新增的旁路表）。需要在元数据库里执行一次；脚本可重复执行。
-- 不改基础表 dbt_query_privileges_apply：申请理由放在一对一的旁路表里，读取时二次查询。
-- 不执行也能用：只是提交申请时不再要求填写“申请理由”，申请列表 / 详情也不显示理由。
CREATE TABLE IF NOT EXISTS dbt_query_apply_ext (
  apply_id   INT          NOT NULL COMMENT 'dbt_query_privileges_apply.apply_id',
  reason     VARCHAR(500) NOT NULL DEFAULT '' COMMENT '申请理由（提交时必填，至少 5 个字）',
  created_at DATETIME     NOT NULL,
  PRIMARY KEY (apply_id)
) ENGINE=InnoDB COMMENT='dbtool 查询权限申请扩展：申请理由';
