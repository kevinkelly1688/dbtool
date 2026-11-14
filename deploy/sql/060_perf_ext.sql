-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 060_perf_ext.sql
-- @time: 2026/10/28

-- 性能工单 / 优化工具扩展（dbtool 新增的表）。需要在元数据库里执行一次；脚本可重复执行。
-- 不执行也能用：下面三张表对应的功能（标记修复关联上线工单、路由规则的表名正则、SLA 升级通知对象矩阵）会自动降级为“没有这个功能”，
-- 其余功能不受影响；页面保存这些设置时会提示先执行本脚本。
-- dbt_perf_issue / dbt_perf_route 是基础表，dbtool 不改它们的结构，所以扩展信息放在下面的旁路表里。

-- dbt_perf_issue_ext：工单扩展。标记修复时可以填写关联的 SQL 上线工单号（dbt_sql_workflow.id），详情页据此提供跳转。
CREATE TABLE IF NOT EXISTS dbt_perf_issue_ext (
  issue_id    BIGINT UNSIGNED NOT NULL COMMENT 'dbt_perf_issue.id',
  workflow_id BIGINT UNSIGNED NULL     COMMENT '关联的 SQL 上线工单 dbt_sql_workflow.id',
  updated_at  DATETIME        NOT NULL,
  PRIMARY KEY (issue_id)
) ENGINE=InnoDB COMMENT='dbtool 性能工单扩展';

-- dbt_perf_route_ext：路由规则的表名正则（dbt_perf_route 只有库名正则）。没有记录或为空表示不限制表名，规则只看库名。
CREATE TABLE IF NOT EXISTS dbt_perf_route_ext (
  route_id   BIGINT UNSIGNED NOT NULL COMMENT 'dbt_perf_route.id',
  tbl_regex  VARCHAR(200)    NOT NULL DEFAULT '' COMMENT '表名正则（整体匹配；工单涉及的任一张表匹配即命中）',
  updated_at DATETIME        NOT NULL,
  PRIMARY KEY (route_id)
) ENGINE=InnoDB COMMENT='dbtool 性能工单路由规则扩展';

-- dbt_perf_setting：性能工单的键值设置。目前只有 escalation_matrix：SLA 升级（L1 催办 / L2 升级 / L3 升总监）要通知的对象 JSON。
-- 没有记录时使用默认：L1 负责人；L2 负责人 + 团队 TL + 跟进 DBA；L3 再加总监（与功能上线前写死的行为一致）。
CREATE TABLE IF NOT EXISTS dbt_perf_setting (
  skey       VARCHAR(64) NOT NULL,
  svalue     TEXT        NOT NULL,
  updated_by VARCHAR(64) NOT NULL DEFAULT '',
  updated_at DATETIME    NOT NULL,
  PRIMARY KEY (skey)
) ENGINE=InnoDB COMMENT='dbtool 性能工单设置';
