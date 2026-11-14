-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 080_sysadmin_ext.sql
-- @time: 2026/10/28

-- 系统管理（资源组 / 用户 / 权限组）的扩展字段（dbtool 新增的旁路表）。
-- 需要在元数据库里执行一次，然后重启 dbtool；脚本可重复执行。
-- 不改任何基础表（dbt_resource_group / dbt_sql_users / dbt_auth_group 原样保留）：扩展字段放在一对一的旁路表里，
-- 读取时 LEFT JOIN / 二次查询。表不存在时系统照常运行，只是这些扩展字段不显示、也不能保存（页面给出提示）。

-- 资源组扩展：负责人（dbt_sql_users.username）与备注。
CREATE TABLE IF NOT EXISTS dbt_resource_group_ext (
  group_id   INT          NOT NULL COMMENT 'dbt_resource_group.group_id',
  leader     VARCHAR(30)  NOT NULL DEFAULT '' COMMENT '负责人用户名（dbt_sql_users.username）；空=未指定',
  remark     VARCHAR(200) NOT NULL DEFAULT '' COMMENT '备注（业务范围说明）',
  updated_by VARCHAR(30)  NOT NULL DEFAULT '',
  updated_at DATETIME     NOT NULL,
  PRIMARY KEY (group_id)
) ENGINE=InnoDB COMMENT='dbtool 资源组扩展字段';

-- 用户扩展：部门、认证方式（空=本地；ldap 等=由外部系统认证，禁止管理员重置密码）、
-- 是否要求下次登录修改密码（管理员“系统生成临时密码”重置后置 1，用户自己改密成功后清零）。
CREATE TABLE IF NOT EXISTS dbt_user_profile (
  user_id              INT         NOT NULL COMMENT 'dbt_sql_users.id',
  dept                 VARCHAR(50) NOT NULL DEFAULT '' COMMENT '部门',
  auth_type            VARCHAR(16) NOT NULL DEFAULT '' COMMENT '创建时选择的认证方式：空/local=本地，ldap=LDAP',
  must_change_password TINYINT(1)  NOT NULL DEFAULT 0 COMMENT '1=下次登录必须先修改密码',
  updated_by           VARCHAR(30) NOT NULL DEFAULT '',
  updated_at           DATETIME    NOT NULL,
  PRIMARY KEY (user_id),
  KEY idx_dept (dept)
) ENGINE=InnoDB COMMENT='dbtool 用户扩展字段';

-- 权限组扩展：说明。
CREATE TABLE IF NOT EXISTS dbt_auth_group_ext (
  group_id    INT          NOT NULL COMMENT 'dbt_auth_group.id',
  description VARCHAR(200) NOT NULL DEFAULT '' COMMENT '说明',
  updated_by  VARCHAR(30)  NOT NULL DEFAULT '',
  updated_at  DATETIME     NOT NULL,
  PRIMARY KEY (group_id)
) ENGINE=InnoDB COMMENT='dbtool 权限组扩展字段';
