-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 019_btnperm.sql
-- @time: 2026/10/28

-- 按钮级权限（dbtool 新增的表）。
-- 需要在元数据库里执行一次，然后重启 dbtool；脚本可重复执行。
-- 不改任何基础表（dbt_auth_permission / dbt_auth_group_permissions 等原样保留）：按钮权限是在原权限之上的补充。
-- 表不存在时系统按“没有任何按钮授予、兼容模式”运行，行为与升级前完全一致。

-- 权限组 → 按钮权限码（dbt_auth_group.id 为 group_id）。权限码取自 internal/btnperm 登记表，如 sqlworkflow.execute_group。
CREATE TABLE IF NOT EXISTS dbt_group_perm (
  id        INT UNSIGNED NOT NULL AUTO_INCREMENT,
  group_id  INT          NOT NULL COMMENT 'dbt_auth_group.id',
  perm_code VARCHAR(64)  NOT NULL COMMENT '按钮权限码（登记表）',
  PRIMARY KEY (id),
  UNIQUE KEY uk_group_perm (group_id, perm_code),
  KEY idx_perm (perm_code)
) ENGINE=InnoDB COMMENT='dbtool 权限组的按钮权限';

-- 单用户单独授权 / 禁用：禁用优先于一切授权；到期后自动失效（expire_at 为空表示长期有效）。
CREATE TABLE IF NOT EXISTS dbt_user_perm (
  id         INT UNSIGNED NOT NULL AUTO_INCREMENT,
  user_id    INT          NOT NULL COMMENT 'dbt_sql_users.id',
  perm_code  VARCHAR(64)  NOT NULL COMMENT '按钮权限码（登记表）',
  effect     ENUM('allow','deny') NOT NULL COMMENT 'allow 单独授权 / deny 单独禁用',
  expire_at  DATETIME     NULL COMMENT '到期时间（本地时间）；NULL=长期',
  reason     VARCHAR(200) NOT NULL DEFAULT '' COMMENT '原因（必填，同时写入 dbt_audit_log）',
  created_by VARCHAR(30)  NOT NULL DEFAULT '',
  created_at DATETIME     NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_user_perm (user_id, perm_code),
  KEY idx_expire (expire_at)
) ENGINE=InnoDB COMMENT='dbtool 单用户按钮权限的授权 / 禁用';

-- 全局设置：mode = compat（兼容，默认）/ strict（严格）；deny_display = hide（隐藏，默认）/ disable（置灰）。
CREATE TABLE IF NOT EXISTS dbt_perm_setting (
  skey       VARCHAR(32)  NOT NULL,
  svalue     VARCHAR(64)  NOT NULL DEFAULT '',
  updated_by VARCHAR(30)  NOT NULL DEFAULT '',
  updated_at DATETIME     NOT NULL,
  PRIMARY KEY (skey)
) ENGINE=InnoDB COMMENT='dbtool 按钮权限的全局设置';
