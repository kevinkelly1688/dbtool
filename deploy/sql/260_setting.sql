-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 260_setting.sql
-- @time: 2026/10/28

-- 系统设置（dbtool 自己的设置体系；设计见 docs/settings-design.md）。
-- 需要在元数据库里执行一次（install.sh upgrade / dbt-admin bootstrap --only-sql 会执行）；脚本可重复执行。

-- 设置值：只存“显式设置过”的项；没有行 = 使用代码里登记的默认值。
CREATE TABLE IF NOT EXISTS dbt_setting (
  skey        VARCHAR(100) NOT NULL COMMENT '设置键：<领域>.<名称>，见 internal/settings/registry.go',
  svalue      MEDIUMTEXT   NOT NULL COMMENT '值；encrypted=1 时是 mirage 密文',
  encrypted   TINYINT(1)   NOT NULL DEFAULT 0 COMMENT '敏感项（密钥 / 密码 / 令牌）加密保存',
  version     INT UNSIGNED NOT NULL DEFAULT 1 COMMENT '每修改一次加 1',
  updated_by  VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '最后修改人；空 = 系统（导入 / 迁移）',
  update_time DATETIME     NOT NULL,
  PRIMARY KEY (skey)
) ENGINE=InnoDB COMMENT='dbtool 系统设置';

-- 变更记录：每次修改一行。敏感项 old_value / new_value 为空，只记“改过”。
CREATE TABLE IF NOT EXISTS dbt_setting_history (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  skey        VARCHAR(100) NOT NULL,
  old_value   TEXT         NOT NULL,
  new_value   TEXT         NOT NULL,
  is_secret   TINYINT(1)   NOT NULL DEFAULT 0,
  action      VARCHAR(16)  NOT NULL DEFAULT 'set' COMMENT 'set 修改 / reset 恢复默认 / import 导入',
  changed_by  VARCHAR(64)  NOT NULL DEFAULT '',
  change_time DATETIME     NOT NULL,
  PRIMARY KEY (id),
  KEY idx_key_time (skey, change_time),
  KEY idx_time (change_time)
) ENGINE=InnoDB COMMENT='dbtool 系统设置变更记录';
