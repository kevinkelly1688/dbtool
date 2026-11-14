-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 130_redis_slowlog.sql
-- @time: 2026/10/28

-- Redis 慢查询（dbtool 新增的表）。
-- 需要在元数据库里执行一次；脚本可重复执行。没有外键约束。
-- 采集：定时对 redis 实例执行 SLOWLOG GET 增量写入 dbt_redis_slowlog（与 MySQL 慢日志同一个调度，默认开启，SLOWLOG_COLLECT_ENABLE=0 整体关闭；实例开启“启用”标记才会采集）。
-- 汇总（按指纹的次数 / 总耗时 / 最大 / 平均 / 首末次时间）由明细表按 (instance_id, fp_md5, start_time) 索引聚合得到；
-- 明细只保留 93 天（与页面可查询的最长时间范围一致），由采集器每小时限量清理。

CREATE TABLE IF NOT EXISTS dbt_redis_slowlog (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  instance_id   INT UNSIGNED    NOT NULL COMMENT 'dbt_sql_instance.id',
  slow_id       BIGINT          NOT NULL COMMENT 'SLOWLOG 条目 id（实例重启后会从 0 重新计数）',
  start_time    DATETIME        NOT NULL COMMENT '命令开始执行的时间（采集端本地时间，秒级）',
  duration_us   BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '命令耗时（微秒）',
  command_name  VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '命令名，带子命令的写成 CONFIG GET',
  fingerprint   VARCHAR(255)    NOT NULL DEFAULT '' COMMENT '命令指纹：命令名 + 参数占位，如 HGETALL ?',
  fp_md5        CHAR(32)        NOT NULL DEFAULT '' COMMENT '指纹的 md5',
  command_text  TEXT            NULL COMMENT '完整命令（AUTH 等密码参数已抹掉，最多 4000 字符）',
  client_addr   VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '客户端地址 ip:port（Redis 4.0+）',
  client_name   VARCHAR(128)    NOT NULL DEFAULT '',
  collected_at  DATETIME        NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_inst_slow (instance_id, slow_id, start_time),
  KEY idx_inst_time (instance_id, start_time),
  KEY idx_inst_fp (instance_id, fp_md5, start_time),
  KEY idx_inst_cmd (instance_id, command_name, start_time),
  KEY idx_time (start_time)
) ENGINE=InnoDB COMMENT='Redis 慢查询明细（SLOWLOG GET 增量采集）';

-- 每个实例的采集游标与最近一次采集状态。
CREATE TABLE IF NOT EXISTS dbt_redis_slowlog_state (
  instance_id    INT UNSIGNED NOT NULL,
  last_slow_id   BIGINT       NOT NULL DEFAULT -1 COMMENT '已采集到的最大条目 id；-1 表示还没采集过',
  last_slow_time DATETIME     NULL COMMENT '该条目的开始时间；实例重启（id 回退）时按时间过滤',
  last_run_at    DATETIME     NULL,
  last_ok_at     DATETIME     NULL,
  status         VARCHAR(10)  NOT NULL DEFAULT '' COMMENT 'ok / error',
  last_error     VARCHAR(500) NOT NULL DEFAULT '',
  last_rows      INT          NOT NULL DEFAULT 0,
  total_rows     BIGINT       NOT NULL DEFAULT 0,
  duration_ms    BIGINT       NOT NULL DEFAULT 0,
  updated_at     DATETIME     NOT NULL,
  PRIMARY KEY (instance_id)
) ENGINE=InnoDB COMMENT='Redis 慢日志采集游标与状态';
