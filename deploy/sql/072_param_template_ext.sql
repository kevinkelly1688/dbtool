-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 072_param_template_ext.sql
-- @time: 2026/10/28

-- 参数模板的“推荐值”（dbtool 新增）。需要在元数据库里执行一次；脚本可重复执行。
-- 基础表 dbt_param_template 只有默认值与可修改值，不加列，所以推荐值另存一张旁路表，按 template_id 对应。
-- 表不存在时：参数配置照常使用，只是没有“推荐值 / 偏离推荐值”。

CREATE TABLE IF NOT EXISTS dbt_param_template_ext (
  template_id       INT UNSIGNED  NOT NULL COMMENT 'dbt_param_template.id',
  recommended_value VARCHAR(1024) NOT NULL DEFAULT '' COMMENT '推荐值；空=不提示偏离',
  updated_by        VARCHAR(30)   NOT NULL DEFAULT '',
  updated_at        DATETIME      NOT NULL,
  PRIMARY KEY (template_id)
) ENGINE=InnoDB COMMENT='dbtool 参数模板扩展：推荐值';
