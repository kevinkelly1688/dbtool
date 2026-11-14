-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 270_environment.sql
-- @time: 2026/10/28

-- 环境字典（dbtool 新增）：实例的“环境”不再写死“生产 / 测试”，在 系统管理 › 环境管理 里维护。
-- 需要在元数据库里执行一次；脚本可重复执行，不改基础表、不删数据。
-- 表不存在时：环境仍然只有内置的“生产（prod）/ 测试（test）”两个，行为与引入本表之前完全一致，环境管理页提示先执行本脚本。

CREATE TABLE IF NOT EXISTS dbt_environment (
  code        VARCHAR(16)  NOT NULL COMMENT '环境编码：小写字母开头，只含小写字母 / 数字 / _ / -；创建后不可改',
  name        VARCHAR(40)  NOT NULL COMMENT '显示名称',
  is_prod     TINYINT(1)   NOT NULL DEFAULT 0 COMMENT '1=按生产对待（红色徽标、工单详情的生产确认、防火墙“仅生产”类提示）',
  sort_order  INT          NOT NULL DEFAULT 100 COMMENT '排序，小的在前',
  created_by  VARCHAR(30)  NOT NULL DEFAULT '',
  updated_by  VARCHAR(30)  NOT NULL DEFAULT '',
  created_at  DATETIME     NOT NULL,
  updated_at  DATETIME     NOT NULL,
  PRIMARY KEY (code),
  UNIQUE KEY uk_name (name)
) ENGINE=InnoDB COMMENT='dbtool 环境字典';

-- 内置环境：prod / test 不可删除。已存在时保持你改过的名称与排序。
INSERT IGNORE INTO dbt_environment (code, name, is_prod, sort_order, created_by, updated_by, created_at, updated_at) VALUES
  ('prod', '生产', 1, 10, 'system', 'system', NOW(), NOW()),
  ('test', '测试', 0, 20, 'system', 'system', NOW(), NOW());

-- 环境编码从最长 8 位放宽到 16 位（实例的环境存在 dbt_instance_ext.env，防火墙规则的实例环境范围存在 dbt_firewall_rule.env）。
-- 两张表不存在时下面对应的语句会报错，可忽略（那一项功能本来就没启用）。
ALTER TABLE dbt_instance_ext MODIFY COLUMN env VARCHAR(16) NOT NULL DEFAULT '' COMMENT '环境编码（dbt_environment.code），空 未设置';
ALTER TABLE dbt_firewall_rule MODIFY COLUMN env VARCHAR(16) NOT NULL DEFAULT 'all' COMMENT 'all / 环境编码（dbt_environment.code）';
