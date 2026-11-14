-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 018_auth.sql
-- @time: 2026/10/28

-- 登录方式、外部身份绑定（dbtool 新增的表）；登录与安全设置放系统设置 dbt_setting（260_setting.sql）。
-- 需要在元数据库里执行一次，然后重启 dbtool；脚本可重复执行。
-- 不改任何基础表：账号锁定沿用 dbt_sql_users.failed_login_count / last_login_failed_at，
-- 两步验证沿用 dbt_2fa_config。

-- 登录方式：LDAP / OIDC / 钉钉 / CAS / 飞书(Lark) / Slack / 企业微信，可同时启用多种、可添加多个同类型。
CREATE TABLE IF NOT EXISTS dbt_auth_provider (
  id              INT UNSIGNED NOT NULL AUTO_INCREMENT,
  kind            VARCHAR(16)  NOT NULL COMMENT 'ldap / oidc / dingtalk / cas / feishu / slack / wecom（Lark 是 feishu 的国际站点）',
  name            VARCHAR(50)  NOT NULL COMMENT '名称（管理员看的）',
  enabled         TINYINT(1)   NOT NULL DEFAULT 0 COMMENT '启用前必须测试通过',
  sort_order      INT          NOT NULL DEFAULT 0 COMMENT '登录页按钮顺序，小的在前',
  btn_label       VARCHAR(100) NOT NULL DEFAULT '' COMMENT '登录页按钮文案；空则用“使用<名称>登录”',
  first_login     VARCHAR(16)  NOT NULL DEFAULT 'existing' COMMENT 'create=外部账号找不到平台用户时自动创建；existing=只允许已有用户',
  auth_groups     VARCHAR(255) NOT NULL DEFAULT '' COMMENT '自动创建用户时加入的权限组 ID（逗号分隔）；空=用全局默认 default_auth_group',
  resource_groups VARCHAR(255) NOT NULL DEFAULT '' COMMENT '自动创建用户时加入的资源组 ID（逗号分隔）；空=用全局默认 default_resource_group',
  config          MEDIUMTEXT   NOT NULL COMMENT '类型相关的参数（JSON）；其中 client_secret / bind_password 等密钥字段用 mirage 加密，接口不回显',
  last_test_ok    TINYINT(1)   NULL COMMENT '最近一次测试是否通过；NULL=从未测试',
  last_test_msg   VARCHAR(1000) NOT NULL DEFAULT '' COMMENT '最近一次测试的结果说明',
  last_test_time  DATETIME     NULL,
  tested_hash     VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '测试通过时的连接配置指纹（HMAC）；配置改动后指纹不符，测试结果即失效',
  last_login_time DATETIME     NULL COMMENT '最近一次通过该方式成功登录',
  created_by      VARCHAR(30)  NOT NULL DEFAULT '',
  create_time     DATETIME     NOT NULL,
  sys_time        DATETIME     NOT NULL,
  PRIMARY KEY (id),
  KEY idx_sort (sort_order, id)
) ENGINE=InnoDB COMMENT='dbtool 登录方式';

-- 外部身份与平台用户的绑定：首次登录按“匹配依据”找到用户后记下 (登录方式, 外部唯一 ID) → 用户，之后即使外部用户名变了也认这个绑定。
CREATE TABLE IF NOT EXISTS dbt_auth_identity (
  id          INT UNSIGNED NOT NULL AUTO_INCREMENT,
  provider_id INT UNSIGNED NOT NULL,
  subject     VARCHAR(190) NOT NULL COMMENT '外部唯一 ID：OIDC sub / 钉钉 unionId / 飞书 open_id / Slack sub / 企微 userid / CAS 用户 / LDAP 登录名（小写）',
  user_id     INT UNSIGNED NOT NULL COMMENT 'dbt_sql_users.id',
  create_time DATETIME     NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_provider_subject (provider_id, subject),
  KEY idx_user (user_id)
) ENGINE=InnoDB COMMENT='dbtool 外部身份绑定';
