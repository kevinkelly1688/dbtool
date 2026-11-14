-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 041_query_priv_ext.sql
-- @time: 2026/10/28

-- 已授予查询权限的来源记录（dbtool 新增的旁路表）。需要在元数据库里执行一次；脚本可重复执行。
-- 不改基础表 dbt_query_privileges：授予来源（申请单号 / 管理员直接授予）、直接授予的操作人与原因放在一对一的旁路表里。
-- 不执行也能用：权限列表的“授予来源”列显示“—”，申请详情里不显示“已授予的权限”链接，管理员也仍可直接授予（只是不记来源）。
CREATE TABLE IF NOT EXISTS dbt_query_priv_ext (
  privilege_id INT          NOT NULL COMMENT 'dbt_query_privileges.privilege_id',
  source       VARCHAR(16)  NOT NULL DEFAULT 'apply' COMMENT 'apply=申请单审批通过后自动授予；grant=管理员直接授予',
  apply_id     INT          NOT NULL DEFAULT 0 COMMENT '来源申请单号（source=apply）',
  granted_by   VARCHAR(30)  NOT NULL DEFAULT '' COMMENT '直接授予的操作人账号（source=grant）',
  reason       VARCHAR(500) NOT NULL DEFAULT '' COMMENT '直接授予的原因（source=grant）',
  created_at   DATETIME     NOT NULL,
  PRIMARY KEY (privilege_id),
  KEY idx_apply (apply_id)
) ENGINE=InnoDB COMMENT='dbtool 已授予查询权限的来源';
