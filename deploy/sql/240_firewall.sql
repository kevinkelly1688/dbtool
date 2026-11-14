-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 240_firewall.sql
-- @time: 2026/10/28

-- SQL 防火墙（规则式语句管控）的三张表（dbtool 新增）。需要在元数据库里执行一次；脚本可重复执行。
-- 表不存在时：在线查询不受影响（等同没有任何规则），管理页提示先执行本脚本。
-- 执行后默认没有任何规则，行为与执行前完全一致；规则（暂时）直接 INSERT dbt_firewall_rule，管理页随后提供。

-- 规则。一行 = 一条规则；命中条件放在 conditions（JSON 文本）里，规则是一个原子的版本单元。
CREATE TABLE IF NOT EXISTS dbt_firewall_rule (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  name          VARCHAR(100)    NOT NULL COMMENT '规则名称（唯一）',
  description   VARCHAR(500)    NOT NULL DEFAULT '' COMMENT '为什么要有这条规则',
  priority      INT             NOT NULL DEFAULT 100 COMMENT '数字小的先求值；最终决定按“最严”合并，与顺序无关',
  enabled       TINYINT(1)      NOT NULL DEFAULT 1 COMMENT '1=启用',
  mode          VARCHAR(8)      NOT NULL DEFAULT 'observe' COMMENT 'observe 只记录不拦截 / enforce 执行动作',
  observe_since DATETIME        NULL COMMENT '进入观察模式的时间',
  scenes        VARCHAR(64)     NOT NULL DEFAULT 'query' COMMENT '逗号分隔：query 在线查询 / workflow SQL 工单 / export 数据导出',
  env           VARCHAR(8)      NOT NULL DEFAULT 'all' COMMENT 'all / prod / test（按实例标签推断）',
  instance_ids  TEXT            NULL COMMENT '逗号分隔的实例 ID；空 = 不按实例限定',
  group_mode    VARCHAR(8)      NOT NULL DEFAULT 'all' COMMENT 'all / only 仅这些权限组 / except 排除这些权限组',
  group_ids     TEXT            NULL COMMENT '逗号分隔的权限组 ID（dbt_auth_group.id）',
  match_mode    VARCHAR(4)      NOT NULL DEFAULT 'all' COMMENT 'all 条件全部满足 / any 任一满足',
  conditions    TEXT            NOT NULL COMMENT '条件数组（JSON）：[{"t":"stmt","v":["DELETE"]},{"t":"feature","v":"no_where"}]',
  action        VARCHAR(16)     NOT NULL DEFAULT 'warn' COMMENT 'deny 拒绝 / reason 要求填写原因 / limit 限制返回行数 / mask 强制脱敏 / warn 仅告警',
  action_arg    INT             NOT NULL DEFAULT 0 COMMENT 'limit：最多返回行数',
  message       VARCHAR(500)    NOT NULL DEFAULT '' COMMENT '给用户看的说明',
  notify        TINYINT(1)      NOT NULL DEFAULT 0 COMMENT '1=命中后通知',
  updated_by    VARCHAR(30)     NOT NULL DEFAULT '',
  created_at    DATETIME        NOT NULL,
  updated_at    DATETIME        NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_name (name),
  KEY idx_enabled (enabled, priority)
) ENGINE=InnoDB COMMENT='dbtool SQL 防火墙规则';

-- 例外：豁免某条规则对某个主体的约束。必须写原因，必须有到期时间（到期自动失效，不依赖清理任务）。
CREATE TABLE IF NOT EXISTS dbt_firewall_exception (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  rule_id       BIGINT UNSIGNED NOT NULL COMMENT 'dbt_firewall_rule.id',
  subject_kind  VARCHAR(12)     NOT NULL COMMENT 'user 用户名 / group 权限组 ID / instance 实例 ID / fingerprint 语句指纹',
  subject_value VARCHAR(100)    NOT NULL,
  reason        VARCHAR(500)    NOT NULL COMMENT '必填：为什么这条规则对该主体不适用',
  expires_at    DATETIME        NOT NULL,
  created_by    VARCHAR(30)     NOT NULL DEFAULT '',
  created_at    DATETIME        NOT NULL,
  PRIMARY KEY (id),
  KEY idx_rule (rule_id, expires_at)
) ENGINE=InnoDB COMMENT='dbtool SQL 防火墙例外';

-- 命中记录。同一 (规则, 用户, 指纹, 结果) 在 10 分钟窗口内合并为一行（repeat_cnt 累加），避免循环里的语句刷表。
-- sql_text 默认只存把字符串 / 数字字面量换成 ? 之后的形态（系统配置 firewall_store_sql=raw 才存原文）。
CREATE TABLE IF NOT EXISTS dbt_firewall_hit (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  created_at    DATETIME        NOT NULL,
  last_at       DATETIME        NOT NULL COMMENT '最近一次合并进来的时间',
  rule_id       BIGINT UNSIGNED NOT NULL,
  rule_name     VARCHAR(100)    NOT NULL COMMENT '冗余存名字：规则改名 / 删除后记录仍可读',
  scene         VARCHAR(12)     NOT NULL COMMENT 'query / workflow / export',
  user_name     VARCHAR(30)     NOT NULL,
  instance_id   INT UNSIGNED    NOT NULL DEFAULT 0,
  instance_name VARCHAR(50)     NOT NULL DEFAULT '',
  db_name       VARCHAR(64)     NOT NULL DEFAULT '',
  effective     VARCHAR(16)     NOT NULL COMMENT '实际生效的动作：deny / reason / limit / mask / warn / observe',
  would_action  VARCHAR(16)     NOT NULL DEFAULT '' COMMENT 'observe 时记录“若生效会怎样”',
  fingerprint   VARCHAR(8)      NOT NULL DEFAULT '',
  sql_text      VARCHAR(2000)   NOT NULL DEFAULT '',
  reason_text   VARCHAR(500)    NOT NULL DEFAULT '' COMMENT 'reason 动作里用户填写的原因',
  matched       TEXT            NULL COMMENT '命中的条件说明（JSON）',
  repeat_cnt    INT UNSIGNED    NOT NULL DEFAULT 1,
  query_log_id  BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '成功执行的语句对应 dbt_query_log.id；0 = 没执行 / 不适用',
  PRIMARY KEY (id),
  KEY idx_created (created_at),
  KEY idx_rule (rule_id, created_at),
  KEY idx_user (user_name, created_at),
  KEY idx_fp (fingerprint)
) ENGINE=InnoDB COMMENT='dbtool SQL 防火墙命中记录';
