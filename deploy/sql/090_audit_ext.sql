-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 090_audit_ext.sql
-- @time: 2026/10/28

-- 系统审计的扩展信息（dbtool 新增的表）。
-- 需要在元数据库里执行一次；脚本可重复执行。
-- 基础表 dbt_audit_log 不加列：结果 / 来源 IP / 类别 / 模块 / 权限码 / 高危标记放在这张旁路表，
-- 与 dbt_audit_log.id 一对一，由 dbtool 在写 dbt_audit_log 时统一写入（internal/audit/ext.go）。
-- 没有这张表时系统审计页照常工作，只是不显示这些列、不能按类别 / 结果 / 高危筛选；历史行（建表之前写入的）这些列显示为“—”。
CREATE TABLE IF NOT EXISTS dbt_audit_ext (
  audit_id BIGINT UNSIGNED NOT NULL COMMENT 'dbt_audit_log.id',
  result   VARCHAR(8)  NOT NULL DEFAULT '' COMMENT '成功 / 失败 / 拒绝',
  ip       VARCHAR(64) NOT NULL DEFAULT '' COMMENT '来源 IP；后台任务没有请求，为空',
  category VARCHAR(16) NOT NULL DEFAULT '' COMMENT 'login / sqlflow / query / inst / perf / opt / sys / other',
  module   VARCHAR(32) NOT NULL DEFAULT '' COMMENT '模块（按钮权限登记表里的页面名，或“登录认证”等）',
  code     VARCHAR(64) NOT NULL DEFAULT '' COMMENT '按钮权限码，如 session.kill；无法对应时为空',
  risky    TINYINT(1)  NOT NULL DEFAULT 0 COMMENT '高危：权限码登记为高危按钮，或“高危按钮操作”审计',
  PRIMARY KEY (audit_id),
  KEY idx_category (category),
  KEY idx_result (result),
  KEY idx_risky (risky)
) ENGINE=InnoDB COMMENT='dbtool 审计日志扩展信息（与 dbt_audit_log 一对一）';
