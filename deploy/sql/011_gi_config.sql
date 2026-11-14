-- @author: kevinkelly
-- @license: Apache Licence
-- @file: 011_gi_config.sql
-- @time: 2026/10/28

-- goInception 配置管理（系统管理 › goInception 配置，dbtool 新增的表）。
-- 需要在元数据库里执行一次；脚本可重复执行。

-- 配置文件（config.toml）的版本历史。
-- content 是完整文件，写入前用 SECRET_KEY 加密（库里不出现密码明文）；
-- content_sha256 是明文内容的 SHA-256，用来判断磁盘文件有没有被人绕过页面手工改过。
-- status：draft 已写入磁盘、尚未重启生效；applied 已重启并通过健康检查；
--         failed 重启失败；rolledback 重启失败，已自动恢复到上一个生效版本。
CREATE TABLE IF NOT EXISTS dbt_gi_config_version (
  id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  version        INT UNSIGNED    NOT NULL COMMENT '页面上显示的版本号（#1、#2…），递增且唯一',
  content        MEDIUMTEXT      NOT NULL COMMENT '完整配置文件（加密）',
  content_sha256 CHAR(64)        NOT NULL COMMENT '明文内容的 SHA-256',
  change_summary VARCHAR(255)    NOT NULL DEFAULT '' COMMENT '变更摘要',
  operator       VARCHAR(150)    NOT NULL DEFAULT '' COMMENT '操作人用户名',
  status         VARCHAR(12)     NOT NULL DEFAULT 'draft' COMMENT 'draft / applied / failed / rolledback',
  created_at     DATETIME        NOT NULL,
  applied_at     DATETIME        NULL COMMENT '重启并通过健康检查的时间',
  error          TEXT            NOT NULL COMMENT '失败原因 / 自动恢复说明',
  PRIMARY KEY (id),
  UNIQUE KEY uk_version (version),
  KEY idx_status_version (status, version)
) ENGINE=InnoDB COMMENT='dbtool goInception 配置文件版本历史';

-- 部署设置与运行状态（键值，一项一行）：
--   deploy        重启方式、命令模板、超时与回滚策略（JSON；SSH 密码与私钥已加密）
--   state_*       最近一次重启结果等小块状态
--   lock          “同一时刻只允许一个保存/重启流程”的操作锁（带过期时间，进程崩溃后自动失效）
CREATE TABLE IF NOT EXISTS dbt_gi_setting (
  item       VARCHAR(64) NOT NULL,
  value      MEDIUMTEXT  NOT NULL,
  updated_by VARCHAR(150) NOT NULL DEFAULT '',
  updated_at DATETIME    NOT NULL,
  PRIMARY KEY (item)
) ENGINE=InnoDB COMMENT='dbtool goInception 部署设置与状态';
