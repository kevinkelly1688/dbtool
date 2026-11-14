-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 170_redis_rules.sql
-- @time: 2026/10/28

-- Redis 工单上线的“管理员规则”（dbtool 新增的表）。需要在元数据库里执行一次；脚本可重复执行。
-- 规则只有超级用户能在「系统管理 → Redis 上线规则」里增删改；预置 0 条规则 = 默认全部拒绝。
-- 一条 Redis 命令必须同时命中某条“启用”规则的 命令名 + 键模式 + 适用范围 + 键数上限 才允许提交 / 执行。
-- 表不存在时：规则页提示先执行本脚本；Redis 工单上线按“没有任何规则”处理（全部拒绝），MySQL / SQL Server 工单不受影响。

CREATE TABLE IF NOT EXISTS dbt_redis_rule (
  id            INT UNSIGNED NOT NULL AUTO_INCREMENT,
  name          VARCHAR(50)  NOT NULL COMMENT '规则名称（唯一）',
  enabled       TINYINT(1)   NOT NULL DEFAULT 1,
  commands      VARCHAR(800) NOT NULL COMMENT '命令名，逗号分隔，大写，如 DEL,UNLINK',
  key_patterns  TEXT         NOT NULL COMMENT '键匹配模式（glob）的 JSON 数组，如 ["cache:*","session:*"]',
  instance_ids  VARCHAR(600) NOT NULL DEFAULT '' COMMENT '限定的实例 id，逗号分隔；空 = 全部实例',
  dbs           VARCHAR(600) NOT NULL DEFAULT '' COMMENT '限定的 db 编号，逗号分隔；空 = 全部库',
  max_keys      INT          NOT NULL DEFAULT 100 COMMENT '单条命令涉及的键数上限',
  record_before TINYINT(1)   NOT NULL DEFAULT 0 COMMENT '执行前记录被改键的原值（明文显示在工单详情里）',
  description   VARCHAR(500) NOT NULL DEFAULT '',
  remark        VARCHAR(500) NOT NULL DEFAULT '',
  created_by    VARCHAR(30)  NOT NULL DEFAULT '',
  updated_by    VARCHAR(30)  NOT NULL DEFAULT '',
  created_at    DATETIME     NOT NULL,
  updated_at    DATETIME     NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_name (name)
) ENGINE=InnoDB COMMENT='Redis 工单上线规则（管理员定义）';
