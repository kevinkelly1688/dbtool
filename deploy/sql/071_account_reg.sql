-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 071_account_reg.sql
-- @time: 2026/10/28

-- 账号管理的“使用人 / 用途 / 到期日”登记（dbtool 新增）。需要在元数据库里执行一次；脚本可重复执行。
-- 基础表 dbt_instance_account 只有备注和加密的密码，不加列，所以另存一张旁路表；
-- 两者按 (实例, 账号, 主机) 对应。
-- 表不存在时：账号登记仍可用（只有备注），页面上的“使用人 / 用途 / 到期日”不可录入。

CREATE TABLE IF NOT EXISTS dbt_account_reg (
  instance_id   INT UNSIGNED NOT NULL COMMENT 'dbt_sql_instance.id',
  user_name     VARCHAR(64)  NOT NULL COMMENT '数据库账号（明文，不含密码）',
  host          VARCHAR(128) NOT NULL COMMENT '账号的主机部分',
  owner         VARCHAR(30)  NOT NULL DEFAULT '' COMMENT '使用人 dbt_sql_users.username',
  owner_display VARCHAR(50)  NOT NULL DEFAULT '',
  purpose       VARCHAR(255) NOT NULL DEFAULT '' COMMENT '用途',
  expire_date   DATE         NULL COMMENT '到期日；空=不限期',
  updated_by    VARCHAR(30)  NOT NULL DEFAULT '',
  updated_at    DATETIME     NOT NULL,
  PRIMARY KEY (instance_id, user_name, host),
  KEY idx_expire (expire_date)
) ENGINE=InnoDB COMMENT='dbtool 数据库账号登记：使用人 / 用途 / 到期日';
