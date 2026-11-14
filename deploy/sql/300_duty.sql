-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 300_duty.sql
-- @time: 2026/10/28

-- DBA 值班表（dbtool 新增）：页面在 平台设置 › 值班表，当前值班人显示在每个页面顶栏时钟的左边。
-- 需要在元数据库里执行一次（升级时 install.sh 会自动执行 sql/*.sql）；脚本可重复执行，不改基础表、不删数据。
-- 表不存在时：顶栏不显示值班信息，值班表页面提示先执行本脚本，其它功能不受影响。
-- 时间一律按服务器本地时区（与通知渠道的“上班时段”一致）。

-- 班次：每天（或每周几天）循环的值班时段，例如 白班 08:00–19:00、晚班 19:00–24:00、夜班 00:00–08:00。
-- 起止用“一天里的第几分钟”存（0–1439）：结束 ≤ 开始表示跨到次日（结束 0 = 次日 00:00，即当天 24:00；开始 = 结束表示 24 小时）。
-- 跨午夜的班属于“开始的那一天”：周五 22:00 开始、周六 06:00 结束的班算周五的班。
CREATE TABLE IF NOT EXISTS dbt_duty_shift (
  id          INT UNSIGNED      NOT NULL AUTO_INCREMENT,
  name        VARCHAR(40)       NOT NULL COMMENT '班次名称，如 白班 / 晚班 / 夜班',
  username    VARCHAR(150)      NOT NULL COMMENT '值班人（dbt_sql_users.username）',
  start_min   SMALLINT UNSIGNED NOT NULL COMMENT '开始时间：一天里的第几分钟，0–1439',
  end_min     SMALLINT UNSIGNED NOT NULL COMMENT '结束时间：同上；结束 ≤ 开始表示跨到次日',
  weekdays    TINYINT UNSIGNED  NOT NULL DEFAULT 127 COMMENT '适用星期的位图：bit0 周日、bit1 周一 … bit6 周六；127 = 每天；跨午夜的班按开始那天算',
  contact     VARCHAR(100)      NOT NULL DEFAULT '' COMMENT '联系方式（电话 / 分机，选填）；空则显示值班人的邮箱',
  remark      VARCHAR(200)      NOT NULL DEFAULT '',
  enabled     TINYINT(1)        NOT NULL DEFAULT 1 COMMENT '0 = 停用（不参与排班）',
  created_by  VARCHAR(150)      NOT NULL DEFAULT '' COMMENT '操作人登录名',
  created_at  DATETIME          NOT NULL,
  updated_by  VARCHAR(150)      NOT NULL DEFAULT '',
  updated_at  DATETIME          NOT NULL,
  PRIMARY KEY (id),
  KEY idx_username (username)
) ENGINE=InnoDB COMMENT='DBA 值班班次（每天 / 每周循环）';

-- 临时调班：请假、换班、节假日顶班。优先级高于班次：这段时间由这里指定的人值班，不改班次模板。
-- 撤销只打标记（canceled = 1），记录保留，方便回头看是谁顶过班。
CREATE TABLE IF NOT EXISTS dbt_duty_override (
  id          INT UNSIGNED NOT NULL AUTO_INCREMENT,
  username    VARCHAR(150) NOT NULL COMMENT '临时值班人（dbt_sql_users.username）',
  start_at    DATETIME     NOT NULL COMMENT '开始时间（含）',
  end_at      DATETIME     NOT NULL COMMENT '结束时间（不含）',
  reason      VARCHAR(200) NOT NULL DEFAULT '' COMMENT '原因，如 请假 / 换班',
  contact     VARCHAR(100) NOT NULL DEFAULT '' COMMENT '联系方式（选填）',
  canceled    TINYINT(1)   NOT NULL DEFAULT 0 COMMENT '1 = 已撤销',
  canceled_by VARCHAR(150) NOT NULL DEFAULT '',
  canceled_at DATETIME     NULL,
  created_by  VARCHAR(150) NOT NULL DEFAULT '' COMMENT '操作人登录名',
  created_at  DATETIME     NOT NULL,
  PRIMARY KEY (id),
  KEY idx_time (end_at, start_at)
) ENGINE=InnoDB COMMENT='DBA 临时调班（优先于班次）';
