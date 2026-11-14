-- ============================================================================
-- dbtool 元数据库基础表结构（001）。dbt-admin 会按文件摘要识别变更（改动后重跑是幂等的）。
--
-- 来源说明：表结构最初由一份纯表结构导出（mysqldump --no-data）清洗得到，之后直接在本文件里维护（没有生成脚本）。
-- 生成日期：2026-10-10
-- 清洗规则：去掉 DROP TABLE、mysqldump 的版本注释（字符集保存 / 恢复）语句、表选项 AUTO_INCREMENT=N；
--           CREATE TABLE 改为 CREATE TABLE IF NOT EXISTS；列名、列定义、索引名原样保留；
--           去掉所有 CHARACTER SET / CHARSET / COLLATE（表和字段都不指定字符集与校验规则，全部跟随数据库默认；
--           库必须是 utf8mb4，见 docs/charset-policy.md）；按外键依赖排序（被引用的表在前）。
-- 表名：新表名 = dbt_ + 源表名（本系统所有表统一 dbt_ 前缀）；仅有一个例外：
--           django_content_type -> dbt_content_type。
--           每张表前的注释写明它的源表名；REFERENCES 的目标表同步改写。
-- 外键约束名：新名 = dbt_ + 原名（InnoDB 的外键约束名在同一个库内必须唯一，不加前缀可能与同库里其它系统的同名约束冲突）；
--           超过 64 个字符时 = dbt_ + 原名前 55 个字符 + _ + 原名 SHA-1 的前 4 位十六进制（恰好 64 个字符）。
--           本文件共 43 个外键约束，其中 3 个因超长按后一种方式改写：
--             perf_collect_config_team_id_89e5d4f1_fk_resource_group_group_id -> dbt_perf_collect_config_team_id_89e5d4f1_fk_resource_group__0f7f
--             perf_team_resource_group_id_38137ae7_fk_resource_group_group_id -> dbt_perf_team_resource_group_id_38137ae7_fk_resource_group__6fe6
--             query_privileges_apply_instance_id_bc03347f_fk_sql_instance_id -> dbt_query_privileges_apply_instance_id_bc03347f_fk_sql_inst_2dfb
-- 范围：48 张表（源文件共 64 张，排除 16 张：7 张框架内部表与 9 张不再使用的表，见 docs/bootstrap-design.md）。
-- 排除：aliyun_rds_config、archive_config、archive_log、cloud_access_key、data_masking_columns、data_masking_rules、django_admin_log、django_cas_ng_proxygrantingticket、django_cas_ng_sessionticket、django_migrations、django_q_ormq、django_q_schedule、django_q_task、django_session、sql_config、sql_permission
--
-- 只用于【空库】初始化（推荐新建一个空库）；可重复执行（IF NOT EXISTS），但不会修改已存在的表。
-- 表名都带 dbt_ 前缀。
-- ============================================================================

SET NAMES utf8mb4;
SET FOREIGN_KEY_CHECKS=0;

--
-- 表 `dbt_audit_log`（源表 `audit_log`）
--
CREATE TABLE IF NOT EXISTS `dbt_audit_log` (
  `id` int NOT NULL AUTO_INCREMENT,
  `user_id` int NOT NULL,
  `user_name` varchar(30) DEFAULT NULL,
  `user_display` varchar(50) DEFAULT NULL,
  `action` varchar(255) NOT NULL,
  `extra_info` longtext,
  `action_time` datetime(6) NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB;

--
-- 表 `dbt_auth_group`（源表 `auth_group`）
--
CREATE TABLE IF NOT EXISTS `dbt_auth_group` (
  `id` int NOT NULL AUTO_INCREMENT,
  `name` varchar(150) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `name` (`name`)
) ENGINE=InnoDB;

--
-- 表 `dbt_content_type`（源表 `django_content_type`）
--
CREATE TABLE IF NOT EXISTS `dbt_content_type` (
  `id` int NOT NULL AUTO_INCREMENT,
  `app_label` varchar(100) NOT NULL,
  `model` varchar(100) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `django_content_type_app_label_model_76bd3d3b_uniq` (`app_label`,`model`)
) ENGINE=InnoDB;

--
-- 表 `dbt_mysql_slow_query_review`（源表 `mysql_slow_query_review`）
--
CREATE TABLE IF NOT EXISTS `dbt_mysql_slow_query_review` (
  `checksum` char(32) NOT NULL,
  `fingerprint` longtext NOT NULL,
  `sample` longtext NOT NULL,
  `first_seen` datetime(6) DEFAULT NULL,
  `last_seen` datetime(6) DEFAULT NULL,
  `reviewed_by` varchar(20) DEFAULT NULL,
  `reviewed_on` datetime(6) DEFAULT NULL,
  `comments` longtext,
  `reviewed_status` varchar(24) DEFAULT NULL,
  PRIMARY KEY (`checksum`),
  KEY `idx_last_seen` (`last_seen`)
) ENGINE=InnoDB;

--
-- 表 `dbt_mysql_slow_query_review_history`（源表 `mysql_slow_query_review_history`）
--
CREATE TABLE IF NOT EXISTS `dbt_mysql_slow_query_review_history` (
  `id` int NOT NULL AUTO_INCREMENT,
  `hostname_max` varchar(64) NOT NULL,
  `client_max` varchar(64) DEFAULT NULL,
  `user_max` varchar(64) NOT NULL,
  `db_max` varchar(64) DEFAULT NULL,
  `checksum` char(32) NOT NULL,
  `sample` longtext NOT NULL,
  `ts_min` datetime(6) NOT NULL,
  `ts_max` datetime(6) NOT NULL,
  `ts_cnt` float DEFAULT NULL,
  `Query_time_sum` float DEFAULT NULL,
  `Query_time_min` float DEFAULT NULL,
  `Query_time_max` float DEFAULT NULL,
  `Query_time_pct_95` float DEFAULT NULL,
  `Query_time_stddev` float DEFAULT NULL,
  `Query_time_median` float DEFAULT NULL,
  `Lock_time_sum` float DEFAULT NULL,
  `Lock_time_min` float DEFAULT NULL,
  `Lock_time_max` float DEFAULT NULL,
  `Lock_time_pct_95` float DEFAULT NULL,
  `Lock_time_stddev` float DEFAULT NULL,
  `Lock_time_median` float DEFAULT NULL,
  `Rows_sent_sum` float DEFAULT NULL,
  `Rows_sent_min` float DEFAULT NULL,
  `Rows_sent_max` float DEFAULT NULL,
  `Rows_sent_pct_95` float DEFAULT NULL,
  `Rows_sent_stddev` float DEFAULT NULL,
  `Rows_sent_median` float DEFAULT NULL,
  `Rows_examined_sum` float DEFAULT NULL,
  `Rows_examined_min` float DEFAULT NULL,
  `Rows_examined_max` float DEFAULT NULL,
  `Rows_examined_pct_95` float DEFAULT NULL,
  `Rows_examined_stddev` float DEFAULT NULL,
  `Rows_examined_median` float DEFAULT NULL,
  `Rows_affected_sum` float DEFAULT NULL,
  `Rows_affected_min` float DEFAULT NULL,
  `Rows_affected_max` float DEFAULT NULL,
  `Rows_affected_pct_95` float DEFAULT NULL,
  `Rows_affected_stddev` float DEFAULT NULL,
  `Rows_affected_median` float DEFAULT NULL,
  `Rows_read_sum` float DEFAULT NULL,
  `Rows_read_min` float DEFAULT NULL,
  `Rows_read_max` float DEFAULT NULL,
  `Rows_read_pct_95` float DEFAULT NULL,
  `Rows_read_stddev` float DEFAULT NULL,
  `Rows_read_median` float DEFAULT NULL,
  `Merge_passes_sum` float DEFAULT NULL,
  `Merge_passes_min` float DEFAULT NULL,
  `Merge_passes_max` float DEFAULT NULL,
  `Merge_passes_pct_95` float DEFAULT NULL,
  `Merge_passes_stddev` float DEFAULT NULL,
  `Merge_passes_median` float DEFAULT NULL,
  `InnoDB_IO_r_ops_min` float DEFAULT NULL,
  `InnoDB_IO_r_ops_max` float DEFAULT NULL,
  `InnoDB_IO_r_ops_pct_95` float DEFAULT NULL,
  `InnoDB_IO_r_ops_stddev` float DEFAULT NULL,
  `InnoDB_IO_r_ops_median` float DEFAULT NULL,
  `InnoDB_IO_r_bytes_min` float DEFAULT NULL,
  `InnoDB_IO_r_bytes_max` float DEFAULT NULL,
  `InnoDB_IO_r_bytes_pct_95` float DEFAULT NULL,
  `InnoDB_IO_r_bytes_stddev` float DEFAULT NULL,
  `InnoDB_IO_r_bytes_median` float DEFAULT NULL,
  `InnoDB_IO_r_wait_min` float DEFAULT NULL,
  `InnoDB_IO_r_wait_max` float DEFAULT NULL,
  `InnoDB_IO_r_wait_pct_95` float DEFAULT NULL,
  `InnoDB_IO_r_wait_stddev` float DEFAULT NULL,
  `InnoDB_IO_r_wait_median` float DEFAULT NULL,
  `InnoDB_rec_lock_wait_min` float DEFAULT NULL,
  `InnoDB_rec_lock_wait_max` float DEFAULT NULL,
  `InnoDB_rec_lock_wait_pct_95` float DEFAULT NULL,
  `InnoDB_rec_lock_wait_stddev` float DEFAULT NULL,
  `InnoDB_rec_lock_wait_median` float DEFAULT NULL,
  `InnoDB_queue_wait_min` float DEFAULT NULL,
  `InnoDB_queue_wait_max` float DEFAULT NULL,
  `InnoDB_queue_wait_pct_95` float DEFAULT NULL,
  `InnoDB_queue_wait_stddev` float DEFAULT NULL,
  `InnoDB_queue_wait_median` float DEFAULT NULL,
  `InnoDB_pages_distinct_min` float DEFAULT NULL,
  `InnoDB_pages_distinct_max` float DEFAULT NULL,
  `InnoDB_pages_distinct_pct_95` float DEFAULT NULL,
  `InnoDB_pages_distinct_stddev` float DEFAULT NULL,
  `InnoDB_pages_distinct_median` float DEFAULT NULL,
  `QC_Hit_cnt` float DEFAULT NULL,
  `QC_Hit_sum` float DEFAULT NULL,
  `Full_scan_cnt` float DEFAULT NULL,
  `Full_scan_sum` float DEFAULT NULL,
  `Full_join_cnt` float DEFAULT NULL,
  `Full_join_sum` float DEFAULT NULL,
  `Tmp_table_cnt` float DEFAULT NULL,
  `Tmp_table_sum` float DEFAULT NULL,
  `Tmp_table_on_disk_cnt` float DEFAULT NULL,
  `Tmp_table_on_disk_sum` float DEFAULT NULL,
  `Filesort_cnt` float DEFAULT NULL,
  `Filesort_sum` float DEFAULT NULL,
  `Filesort_on_disk_cnt` float DEFAULT NULL,
  `Filesort_on_disk_sum` float DEFAULT NULL,
  `Bytes_sum` float DEFAULT NULL,
  `Bytes_min` float DEFAULT NULL,
  `Bytes_max` float DEFAULT NULL,
  `Bytes_pct_95` float DEFAULT NULL,
  `Bytes_stddev` float DEFAULT NULL,
  `Bytes_median` float DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `checksum` (`checksum`,`ts_min`,`ts_max`),
  KEY `idx_hostname_max_ts_min` (`hostname_max`,`ts_min`)
) ENGINE=InnoDB;

--
-- 表 `dbt_param_template`（源表 `param_template`）
--
CREATE TABLE IF NOT EXISTS `dbt_param_template` (
  `id` int NOT NULL AUTO_INCREMENT,
  `db_type` varchar(20) NOT NULL,
  `variable_name` varchar(64) NOT NULL,
  `default_value` varchar(1024) NOT NULL,
  `editable` tinyint(1) NOT NULL,
  `valid_values` varchar(1024) NOT NULL,
  `description` varchar(1024) NOT NULL,
  `create_time` datetime(6) NOT NULL,
  `sys_time` datetime(6) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `param_template_db_type_variable_name_139c76a6_uniq` (`db_type`,`variable_name`)
) ENGINE=InnoDB;

--
-- 表 `dbt_partman_config`（源表 `partman_config`）
--
CREATE TABLE IF NOT EXISTS `dbt_partman_config` (
  `id` bigint NOT NULL AUTO_INCREMENT COMMENT '主键无业务含义',
  `cnf_id` bigint NOT NULL COMMENT '配置信息ID',
  `instance_id` smallint NOT NULL COMMENT '数据库实例id',
  `dbname` varchar(64) NOT NULL COMMENT '库名',
  `tbname` varchar(64) NOT NULL COMMENT '表名',
  `column` varchar(64) NOT NULL COMMENT '分区字段',
  `col_type` varchar(50) NOT NULL COMMENT '字段类型[bigint|datatime]',
  `period` varchar(10) NOT NULL COMMENT '分区周期[minute/hour/day/week/month/year]',
  `interval` tinyint unsigned NOT NULL DEFAULT '0' COMMENT '周期跨度几天或月或几周为一个分区',
  `retain` int NOT NULL COMMENT '比如 10 表示保留10个历史分区',
  `preCreate` tinyint unsigned NOT NULL COMMENT '比如 3 表示创建未来3个周期的分区',
  `timezone` varchar(20) NOT NULL COMMENT '时区 比如 UTC 或 Asia/Shanghai',
  `isenable` tinyint NOT NULL COMMENT '本条配置的分区维护开关 1 启动 | 0 禁用',
  `create_time` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '配置创建时间',
  `lastest_time` datetime NOT NULL ON UPDATE CURRENT_TIMESTAMP COMMENT '配置更新时间',
  `env` varchar(100) NOT NULL COMMENT '环境说明 Prod|Dev|Sit',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_db_tb` (`instance_id`,`tbname`,`dbname`) USING BTREE,
  UNIQUE KEY `uniq_cnf_id` (`cnf_id`) USING BTREE
) ENGINE=InnoDB COMMENT='自动分区维护参数表';

--
-- 表 `dbt_partman_log`（源表 `partman_log`）
--
CREATE TABLE IF NOT EXISTS `dbt_partman_log` (
  `id` bigint NOT NULL AUTO_INCREMENT COMMENT '主键无业务含义',
  `cnf_id` bigint NOT NULL COMMENT '配置信息ID',
  `exec_result` text NOT NULL COMMENT '执行结果json',
  `create_time` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
  PRIMARY KEY (`id`)
) ENGINE=InnoDB COMMENT='分区维护日志';

--
-- 表 `dbt_partman_webhook`（源表 `partman_webhook`）
--
CREATE TABLE IF NOT EXISTS `dbt_partman_webhook` (
  `id` bigint NOT NULL AUTO_INCREMENT COMMENT '主键无业务含义',
  `webhook_url` varchar(500) NOT NULL COMMENT 'webhook_url地址',
  `type` varchar(64) NOT NULL COMMENT '推送平台类型slack|wecom|dingtalk|feishu',
  `isenabled` tinyint NOT NULL DEFAULT '0' COMMENT '消息推送开关 0 不推送|1 推送',
  `mention_on` varchar(20) NOT NULL DEFAULT 'on_failure' COMMENT 'always|on_failure(默认)|never',
  `mention` varchar(500) NOT NULL DEFAULT '' COMMENT '用户组/个人["<!subteam^S0ABUGATA>", "<@U999XXX>"]',
  `keyword` varchar(500) NOT NULL DEFAULT '' COMMENT '飞书 钉钉专用 若机器人开启“签名校验”则填密钥',
  `secret` varchar(500) NOT NULL DEFAULT '' COMMENT '飞书 钉钉专用 若用“自定义关键词”则填，会自动拼进消息',
  `create_time` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_webhook_url` (`webhook_url`)
) ENGINE=InnoDB COMMENT='推送配置';

--
-- 表 `dbt_perf_sla_policy`（源表 `perf_sla_policy`）
--
CREATE TABLE IF NOT EXISTS `dbt_perf_sla_policy` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `severity` varchar(2) NOT NULL,
  `accept_hours` int NOT NULL,
  `resolve_hours` int NOT NULL,
  `l3_after_hours` int NOT NULL,
  `remind_interval_hours` int NOT NULL,
  `max_extend_times` int NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `severity` (`severity`)
) ENGINE=InnoDB;

--
-- 表 `dbt_query_log`（源表 `query_log`）
--
CREATE TABLE IF NOT EXISTS `dbt_query_log` (
  `id` int NOT NULL AUTO_INCREMENT,
  `instance_name` varchar(50) NOT NULL,
  `db_name` varchar(64) NOT NULL,
  `sqllog` longtext NOT NULL,
  `effect_row` bigint NOT NULL,
  `cost_time` varchar(10) NOT NULL,
  `username` varchar(30) NOT NULL,
  `user_display` varchar(50) NOT NULL,
  `priv_check` tinyint(1) NOT NULL,
  `hit_rule` tinyint(1) NOT NULL,
  `masking` tinyint(1) NOT NULL,
  `favorite` tinyint(1) NOT NULL,
  `alias` varchar(64) NOT NULL,
  `create_time` datetime(6) NOT NULL,
  `sys_time` datetime(6) NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB;

--
-- 表 `dbt_resource_group`（源表 `resource_group`）
--
CREATE TABLE IF NOT EXISTS `dbt_resource_group` (
  `group_id` int NOT NULL AUTO_INCREMENT,
  `group_name` varchar(100) NOT NULL,
  `group_parent_id` bigint NOT NULL,
  `group_sort` int NOT NULL,
  `group_level` int NOT NULL,
  `ding_webhook` varchar(255) NOT NULL,
  `feishu_webhook` varchar(255) NOT NULL,
  `slack_webhook` varchar(255) NOT NULL,
  `is_deleted` int NOT NULL,
  `create_time` datetime(6) NOT NULL,
  `sys_time` datetime(6) NOT NULL,
  PRIMARY KEY (`group_id`),
  UNIQUE KEY `group_name` (`group_name`)
) ENGINE=InnoDB;

--
-- 表 `dbt_sql_instance_tag`（源表 `sql_instance_tag`）
--
CREATE TABLE IF NOT EXISTS `dbt_sql_instance_tag` (
  `id` int NOT NULL AUTO_INCREMENT,
  `tag_code` varchar(20) NOT NULL,
  `tag_name` varchar(20) NOT NULL,
  `active` tinyint(1) NOT NULL,
  `create_time` datetime(6) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `tag_code` (`tag_code`),
  UNIQUE KEY `tag_name` (`tag_name`)
) ENGINE=InnoDB;

--
-- 表 `dbt_sql_mysql_inspection_report`（源表 `sql_mysql_inspection_report`）
--
CREATE TABLE IF NOT EXISTS `dbt_sql_mysql_inspection_report` (
  `id` int NOT NULL AUTO_INCREMENT,
  `instance_id` int NOT NULL,
  `status` varchar(20) NOT NULL DEFAULT 'running' COMMENT 'running / finish / failed / cancelled',
  `trigger_type` varchar(10) NOT NULL DEFAULT 'manual' COMMENT 'manual / schedule',
  `operator` varchar(30) NOT NULL DEFAULT '',
  `health_score` int NOT NULL DEFAULT '100',
  `alert_count` int NOT NULL DEFAULT '0',
  `attention_count` int NOT NULL DEFAULT '0',
  `failed_count` int NOT NULL DEFAULT '0',
  `total_items` int NOT NULL DEFAULT '0',
  `done_items` int NOT NULL DEFAULT '0',
  `result` json DEFAULT NULL COMMENT 'full_inspection() 完整结果',
  `error_message` longtext,
  `start_time` datetime(6) NOT NULL,
  `finish_time` datetime(6) DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_myinsp_instance_time` (`instance_id`,`start_time`)
) ENGINE=InnoDB COMMENT='MySQL性能巡检报告';

--
-- 表 `dbt_sql_users`（源表 `sql_users`）
--
CREATE TABLE IF NOT EXISTS `dbt_sql_users` (
  `id` int NOT NULL AUTO_INCREMENT,
  `password` varchar(128) NOT NULL,
  `last_login` datetime(6) DEFAULT NULL,
  `is_superuser` tinyint(1) NOT NULL,
  `username` varchar(150) NOT NULL,
  `first_name` varchar(150) NOT NULL,
  `last_name` varchar(150) NOT NULL,
  `email` varchar(254) NOT NULL,
  `is_staff` tinyint(1) NOT NULL,
  `is_active` tinyint(1) NOT NULL,
  `date_joined` datetime(6) NOT NULL,
  `display` varchar(50) NOT NULL,
  `ding_user_id` varchar(64) NOT NULL,
  `slack_id` varchar(50) NOT NULL,
  `feishu_open_id` varchar(64) NOT NULL,
  `failed_login_count` int NOT NULL,
  `last_login_failed_at` datetime(6) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `username` (`username`)
) ENGINE=InnoDB;

--
-- 表 `dbt_ssh_tunnel`（源表 `ssh_tunnel`）
--
CREATE TABLE IF NOT EXISTS `dbt_ssh_tunnel` (
  `id` int NOT NULL AUTO_INCREMENT,
  `tunnel_name` varchar(50) NOT NULL,
  `host` varchar(200) NOT NULL,
  `port` int NOT NULL,
  `user` varchar(200) DEFAULT NULL,
  `password` varchar(300) DEFAULT NULL,
  `pkey` longtext,
  `pkey_path` varchar(100) DEFAULT NULL,
  `pkey_password` varchar(300) DEFAULT NULL,
  `create_time` datetime(6) NOT NULL,
  `update_time` datetime(6) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `tunnel_name` (`tunnel_name`)
) ENGINE=InnoDB;

--
-- 表 `dbt_workflow_audit`（源表 `workflow_audit`）
--
CREATE TABLE IF NOT EXISTS `dbt_workflow_audit` (
  `audit_id` int NOT NULL AUTO_INCREMENT,
  `group_id` int NOT NULL,
  `group_name` varchar(100) NOT NULL,
  `workflow_id` bigint NOT NULL,
  `workflow_type` int NOT NULL,
  `workflow_title` varchar(50) NOT NULL,
  `workflow_remark` varchar(140) NOT NULL,
  `audit_auth_groups` varchar(255) NOT NULL,
  `current_audit` varchar(20) NOT NULL,
  `next_audit` varchar(20) NOT NULL,
  `current_status` int NOT NULL,
  `create_user` varchar(30) NOT NULL,
  `create_user_display` varchar(50) NOT NULL,
  `create_time` datetime(6) NOT NULL,
  `sys_time` datetime(6) NOT NULL,
  PRIMARY KEY (`audit_id`),
  UNIQUE KEY `workflow_audit_workflow_id_workflow_type_14044a22_uniq` (`workflow_id`,`workflow_type`)
) ENGINE=InnoDB;

--
-- 表 `dbt_workflow_audit_detail`（源表 `workflow_audit_detail`）
--
CREATE TABLE IF NOT EXISTS `dbt_workflow_audit_detail` (
  `audit_detail_id` int NOT NULL AUTO_INCREMENT,
  `audit_id` int NOT NULL,
  `audit_user` varchar(30) NOT NULL,
  `audit_time` datetime(6) NOT NULL,
  `audit_status` int NOT NULL,
  `remark` varchar(1000) NOT NULL,
  `sys_time` datetime(6) NOT NULL,
  PRIMARY KEY (`audit_detail_id`)
) ENGINE=InnoDB;

--
-- 表 `dbt_workflow_audit_setting`（源表 `workflow_audit_setting`）
--
CREATE TABLE IF NOT EXISTS `dbt_workflow_audit_setting` (
  `audit_setting_id` int NOT NULL AUTO_INCREMENT,
  `group_id` int NOT NULL,
  `group_name` varchar(100) NOT NULL,
  `workflow_type` int NOT NULL,
  `audit_auth_groups` varchar(255) NOT NULL,
  `create_time` datetime(6) NOT NULL,
  `sys_time` datetime(6) NOT NULL,
  PRIMARY KEY (`audit_setting_id`),
  UNIQUE KEY `workflow_audit_setting_group_id_workflow_type_5884053a_uniq` (`group_id`,`workflow_type`)
) ENGINE=InnoDB;

--
-- 表 `dbt_workflow_log`（源表 `workflow_log`）
--
CREATE TABLE IF NOT EXISTS `dbt_workflow_log` (
  `id` int NOT NULL AUTO_INCREMENT,
  `audit_id` int NOT NULL,
  `operation_type` smallint NOT NULL,
  `operation_type_desc` varchar(10) NOT NULL,
  `operation_info` varchar(1000) NOT NULL,
  `operator` varchar(30) NOT NULL,
  `operator_display` varchar(50) NOT NULL,
  `operation_time` datetime(6) NOT NULL,
  PRIMARY KEY (`id`),
  KEY `workflow_log_audit_id_71ad84b7` (`audit_id`)
) ENGINE=InnoDB;

--
-- 表 `dbt_2fa_config`（源表 `2fa_config`）
--
CREATE TABLE IF NOT EXISTS `dbt_2fa_config` (
  `id` int NOT NULL AUTO_INCREMENT,
  `username` varchar(200) NOT NULL,
  `auth_type` varchar(128) NOT NULL,
  `phone` varchar(64) DEFAULT NULL,
  `secret_key` varchar(256) DEFAULT NULL,
  `user_id` int NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `2fa_config_user_id_auth_type_303171fc_uniq` (`user_id`,`auth_type`),
  CONSTRAINT `dbt_2fa_config_user_id_a0d1d7c2_fk_sql_users_id` FOREIGN KEY (`user_id`) REFERENCES `dbt_sql_users` (`id`) ON DELETE RESTRICT ON UPDATE RESTRICT
) ENGINE=InnoDB;

--
-- 表 `dbt_auth_permission`（源表 `auth_permission`）
--
CREATE TABLE IF NOT EXISTS `dbt_auth_permission` (
  `id` int NOT NULL AUTO_INCREMENT,
  `name` varchar(255) NOT NULL,
  `content_type_id` int NOT NULL,
  `codename` varchar(100) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `auth_permission_content_type_id_codename_01ab375a_uniq` (`content_type_id`,`codename`),
  CONSTRAINT `dbt_auth_permission_content_type_id_2f476e4b_fk_django_co` FOREIGN KEY (`content_type_id`) REFERENCES `dbt_content_type` (`id`) ON DELETE RESTRICT ON UPDATE RESTRICT
) ENGINE=InnoDB;

--
-- 表 `dbt_perf_team`（源表 `perf_team`）
--
CREATE TABLE IF NOT EXISTS `dbt_perf_team` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `enabled` tinyint(1) NOT NULL,
  `remark` varchar(200) NOT NULL,
  `default_assignee_id` int DEFAULT NULL,
  `resource_group_id` int NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `resource_group_id` (`resource_group_id`),
  KEY `perf_team_default_assignee_id_38e04416_fk_sql_users_id` (`default_assignee_id`),
  CONSTRAINT `dbt_perf_team_default_assignee_id_38e04416_fk_sql_users_id` FOREIGN KEY (`default_assignee_id`) REFERENCES `dbt_sql_users` (`id`),
  CONSTRAINT `dbt_perf_team_resource_group_id_38137ae7_fk_resource_group__6fe6` FOREIGN KEY (`resource_group_id`) REFERENCES `dbt_resource_group` (`group_id`)
) ENGINE=InnoDB;

--
-- 表 `dbt_sql_instance`（源表 `sql_instance`）
--
CREATE TABLE IF NOT EXISTS `dbt_sql_instance` (
  `id` int NOT NULL AUTO_INCREMENT,
  `instance_name` varchar(50) NOT NULL,
  `type` varchar(6) NOT NULL,
  `db_type` varchar(20) NOT NULL,
  `mode` varchar(10) NOT NULL DEFAULT '',
  `host` varchar(200) NOT NULL,
  `port` int NOT NULL,
  `user` varchar(200) NOT NULL,
  `password` varchar(300) NOT NULL,
  `is_ssl` tinyint(1) NOT NULL DEFAULT '0',
  `is_enable` tinyint(1) NOT NULL DEFAULT '0',
  `verify_ssl` tinyint(1) NOT NULL DEFAULT '0',
  `db_name` varchar(64) DEFAULT '',
  `show_db_name_regex` varchar(1024) DEFAULT '',
  `denied_db_name_regex` varchar(1024) DEFAULT '',
  `charset` varchar(20) DEFAULT '',
  `service_name` varchar(50) DEFAULT NULL,
  `sid` varchar(50) DEFAULT NULL,
  `create_time` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `update_time` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `tunnel_id` int DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `instance_name` (`instance_name`),
  KEY `sql_instance_tunnel_id_99377638_fk_ssh_tunnel_id` (`tunnel_id`),
  CONSTRAINT `dbt_sql_instance_tunnel_id_99377638_fk_ssh_tunnel_id` FOREIGN KEY (`tunnel_id`) REFERENCES `dbt_ssh_tunnel` (`id`) ON DELETE RESTRICT ON UPDATE RESTRICT
) ENGINE=InnoDB;

--
-- 表 `dbt_sql_users_groups`（源表 `sql_users_groups`）
--
CREATE TABLE IF NOT EXISTS `dbt_sql_users_groups` (
  `id` int NOT NULL AUTO_INCREMENT,
  `users_id` int NOT NULL,
  `group_id` int NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `sql_users_groups_users_id_group_id_4540dddc_uniq` (`users_id`,`group_id`),
  KEY `sql_users_groups_group_id_d572a82e_fk_auth_group_id` (`group_id`),
  CONSTRAINT `dbt_sql_users_groups_group_id_d572a82e_fk_auth_group_id` FOREIGN KEY (`group_id`) REFERENCES `dbt_auth_group` (`id`) ON DELETE RESTRICT ON UPDATE RESTRICT,
  CONSTRAINT `dbt_sql_users_groups_users_id_d674bacf_fk_sql_users_id` FOREIGN KEY (`users_id`) REFERENCES `dbt_sql_users` (`id`) ON DELETE RESTRICT ON UPDATE RESTRICT
) ENGINE=InnoDB;

--
-- 表 `dbt_sql_users_resource_group`（源表 `sql_users_resource_group`）
--
CREATE TABLE IF NOT EXISTS `dbt_sql_users_resource_group` (
  `id` int NOT NULL AUTO_INCREMENT,
  `users_id` int NOT NULL,
  `resourcegroup_id` int NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `sql_users_resource_group_users_id_resourcegroup_id_304d8421_uniq` (`users_id`,`resourcegroup_id`),
  KEY `sql_users_resource_g_resourcegroup_id_0738ff9d_fk_resource_` (`resourcegroup_id`),
  CONSTRAINT `dbt_sql_users_resource_g_resourcegroup_id_0738ff9d_fk_resource_` FOREIGN KEY (`resourcegroup_id`) REFERENCES `dbt_resource_group` (`group_id`) ON DELETE RESTRICT ON UPDATE RESTRICT,
  CONSTRAINT `dbt_sql_users_resource_group_users_id_fe4a5def_fk_sql_users_id` FOREIGN KEY (`users_id`) REFERENCES `dbt_sql_users` (`id`) ON DELETE RESTRICT ON UPDATE RESTRICT
) ENGINE=InnoDB;

--
-- 表 `dbt_auth_group_permissions`（源表 `auth_group_permissions`）
--
CREATE TABLE IF NOT EXISTS `dbt_auth_group_permissions` (
  `id` int NOT NULL AUTO_INCREMENT,
  `group_id` int NOT NULL,
  `permission_id` int NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `auth_group_permissions_group_id_permission_id_0cd325b0_uniq` (`group_id`,`permission_id`),
  KEY `auth_group_permissio_permission_id_84c5c92e_fk_auth_perm` (`permission_id`),
  CONSTRAINT `dbt_auth_group_permissio_permission_id_84c5c92e_fk_auth_perm` FOREIGN KEY (`permission_id`) REFERENCES `dbt_auth_permission` (`id`) ON DELETE RESTRICT ON UPDATE RESTRICT,
  CONSTRAINT `dbt_auth_group_permissions_group_id_b120cbf9_fk_auth_group_id` FOREIGN KEY (`group_id`) REFERENCES `dbt_auth_group` (`id`) ON DELETE RESTRICT ON UPDATE RESTRICT
) ENGINE=InnoDB;

--
-- 表 `dbt_instance_account`（源表 `instance_account`）
--
CREATE TABLE IF NOT EXISTS `dbt_instance_account` (
  `id` int NOT NULL AUTO_INCREMENT,
  `user` varchar(128) NOT NULL,
  `host` varchar(64) NOT NULL,
  `db_name` varchar(128) NOT NULL,
  `password` varchar(128) NOT NULL,
  `remark` varchar(255) NOT NULL,
  `sys_time` datetime(6) NOT NULL,
  `instance_id` int NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `instance_account_instance_id_user_host_db_name_dcfbf9f6_uniq` (`instance_id`,`user`,`host`,`db_name`),
  CONSTRAINT `dbt_instance_account_instance_id_53a7a305_fk_sql_instance_id` FOREIGN KEY (`instance_id`) REFERENCES `dbt_sql_instance` (`id`) ON DELETE RESTRICT ON UPDATE RESTRICT
) ENGINE=InnoDB;

--
-- 表 `dbt_instance_database`（源表 `instance_database`）
--
CREATE TABLE IF NOT EXISTS `dbt_instance_database` (
  `id` int NOT NULL AUTO_INCREMENT,
  `db_name` varchar(128) NOT NULL,
  `owner` varchar(50) NOT NULL,
  `owner_display` varchar(50) NOT NULL,
  `remark` varchar(255) NOT NULL,
  `sys_time` datetime(6) NOT NULL,
  `instance_id` int NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `instance_database_instance_id_db_name_d8700935_uniq` (`instance_id`,`db_name`),
  CONSTRAINT `dbt_instance_database_instance_id_22c69243_fk_sql_instance_id` FOREIGN KEY (`instance_id`) REFERENCES `dbt_sql_instance` (`id`) ON DELETE RESTRICT ON UPDATE RESTRICT
) ENGINE=InnoDB;

--
-- 表 `dbt_param_history`（源表 `param_history`）
--
CREATE TABLE IF NOT EXISTS `dbt_param_history` (
  `id` int NOT NULL AUTO_INCREMENT,
  `variable_name` varchar(64) NOT NULL,
  `old_var` varchar(1024) NOT NULL,
  `new_var` varchar(1024) NOT NULL,
  `set_sql` varchar(1024) NOT NULL,
  `user_name` varchar(30) NOT NULL,
  `user_display` varchar(50) NOT NULL,
  `create_time` datetime(6) NOT NULL,
  `instance_id` int NOT NULL,
  PRIMARY KEY (`id`),
  KEY `param_history_instance_id_601e8d3f_fk_sql_instance_id` (`instance_id`),
  CONSTRAINT `dbt_param_history_instance_id_601e8d3f_fk_sql_instance_id` FOREIGN KEY (`instance_id`) REFERENCES `dbt_sql_instance` (`id`) ON DELETE RESTRICT ON UPDATE RESTRICT
) ENGINE=InnoDB;

--
-- 表 `dbt_perf_collect_config`（源表 `perf_collect_config`）
--
CREATE TABLE IF NOT EXISTS `dbt_perf_collect_config` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `product` varchar(20) NOT NULL,
  `collector` varchar(30) NOT NULL,
  `custom_sql` longtext NOT NULL,
  `min_avg_ms` int NOT NULL,
  `min_total_sec` int NOT NULL,
  `exclude_regex` varchar(500) NOT NULL,
  `exclude_dbs` varchar(500) NOT NULL,
  `enabled` tinyint(1) NOT NULL,
  `last_collect_time` datetime(6) DEFAULT NULL,
  `last_status` varchar(500) NOT NULL,
  `instance_id` int NOT NULL,
  `team_id` int DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `instance_id` (`instance_id`),
  KEY `perf_collect_config_team_id_89e5d4f1_fk_resource_group_group_id` (`team_id`),
  CONSTRAINT `dbt_perf_collect_config_instance_id_b5bb24b7_fk_sql_instance_id` FOREIGN KEY (`instance_id`) REFERENCES `dbt_sql_instance` (`id`),
  CONSTRAINT `dbt_perf_collect_config_team_id_89e5d4f1_fk_resource_group__0f7f` FOREIGN KEY (`team_id`) REFERENCES `dbt_resource_group` (`group_id`)
) ENGINE=InnoDB;

--
-- 表 `dbt_perf_digest_cursor`（源表 `perf_digest_cursor`）
--
CREATE TABLE IF NOT EXISTS `dbt_perf_digest_cursor` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `schema_name` varchar(128) NOT NULL,
  `digest` varchar(64) NOT NULL,
  `count_star` bigint NOT NULL,
  `sum_timer_wait` decimal(30,0) NOT NULL,
  `sum_rows_examined` bigint NOT NULL,
  `sum_rows_sent` bigint NOT NULL,
  `update_time` datetime(6) NOT NULL,
  `instance_id` int NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `perf_digest_cursor_instance_id_schema_name_digest_13e720a7_uniq` (`instance_id`,`schema_name`,`digest`),
  CONSTRAINT `dbt_perf_digest_cursor_instance_id_d164f9b1_fk_sql_instance_id` FOREIGN KEY (`instance_id`) REFERENCES `dbt_sql_instance` (`id`)
) ENGINE=InnoDB;

--
-- 表 `dbt_perf_issue`（源表 `perf_issue`）
--
CREATE TABLE IF NOT EXISTS `dbt_perf_issue` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `db_name` varchar(128) NOT NULL,
  `checksum` varchar(64) NOT NULL,
  `fingerprint` longtext NOT NULL,
  `sample_sql` longtext NOT NULL,
  `tables` varchar(500) NOT NULL,
  `source` varchar(30) NOT NULL,
  `first_seen` datetime(6) DEFAULT NULL,
  `last_seen` datetime(6) DEFAULT NULL,
  `exec_count_7d` bigint NOT NULL,
  `total_time_7d` double NOT NULL,
  `avg_time_ms` double NOT NULL,
  `max_time_ms` double NOT NULL,
  `rows_examined_avg` double NOT NULL,
  `rows_sent_avg` double NOT NULL,
  `load_share` double NOT NULL,
  `impact_score` double NOT NULL,
  `per_day_count` double NOT NULL,
  `category` varchar(30) NOT NULL,
  `owner_side` varchar(10) NOT NULL,
  `diagnosis` longtext NOT NULL,
  `suggestion` longtext NOT NULL,
  `suggested_ddl` longtext NOT NULL,
  `explain_result` longtext NOT NULL,
  `severity` varchar(2) NOT NULL,
  `severity_locked` tinyint(1) NOT NULL,
  `status` varchar(20) NOT NULL,
  `close_reason` varchar(20) NOT NULL,
  `plan_finish_date` date DEFAULT NULL,
  `fix_desc` longtext NOT NULL,
  `fixed_at` datetime(6) DEFAULT NULL,
  `accepted_at` datetime(6) DEFAULT NULL,
  `closed_at` datetime(6) DEFAULT NULL,
  `reopen_count` int NOT NULL,
  `baseline_avg_ms` double NOT NULL,
  `baseline_daily_sec` double NOT NULL,
  `pending_extend_date` date DEFAULT NULL,
  `pending_reason` varchar(1000) NOT NULL,
  `extend_times` int NOT NULL,
  `accept_deadline` datetime(6) DEFAULT NULL,
  `resolve_deadline` datetime(6) DEFAULT NULL,
  `escalation_level` int NOT NULL,
  `last_remind_at` datetime(6) DEFAULT NULL,
  `remind_count` int NOT NULL,
  `sla_breached` tinyint(1) NOT NULL,
  `pre_warned` tinyint(1) NOT NULL,
  `create_time` datetime(6) NOT NULL,
  `update_time` datetime(6) NOT NULL,
  `assignee_id` int DEFAULT NULL,
  `dba_id` int DEFAULT NULL,
  `instance_id` int NOT NULL,
  `team_id` int DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `perf_issue_instance_id_db_name_checksum_6a3a21ad_uniq` (`instance_id`,`db_name`,`checksum`),
  KEY `perf_issue_team_id_830381_idx` (`team_id`,`status`),
  KEY `perf_issue_assignee_id_3679e8ef_fk_sql_users_id` (`assignee_id`),
  KEY `perf_issue_dba_id_7d32bace_fk_sql_users_id` (`dba_id`),
  KEY `perf_issue_last_seen_ea9145e0` (`last_seen`),
  KEY `perf_issue_impact_score_57412091` (`impact_score`),
  KEY `perf_issue_severity_7d8bd44e` (`severity`),
  KEY `perf_issue_status_878e4380` (`status`),
  KEY `perf_issue_resolve_deadline_4620e90c` (`resolve_deadline`),
  CONSTRAINT `dbt_perf_issue_assignee_id_3679e8ef_fk_sql_users_id` FOREIGN KEY (`assignee_id`) REFERENCES `dbt_sql_users` (`id`),
  CONSTRAINT `dbt_perf_issue_dba_id_7d32bace_fk_sql_users_id` FOREIGN KEY (`dba_id`) REFERENCES `dbt_sql_users` (`id`),
  CONSTRAINT `dbt_perf_issue_instance_id_81ccf533_fk_sql_instance_id` FOREIGN KEY (`instance_id`) REFERENCES `dbt_sql_instance` (`id`),
  CONSTRAINT `dbt_perf_issue_team_id_e0e71c76_fk_resource_group_group_id` FOREIGN KEY (`team_id`) REFERENCES `dbt_resource_group` (`group_id`)
) ENGINE=InnoDB;

--
-- 表 `dbt_perf_route`（源表 `perf_route`）
--
CREATE TABLE IF NOT EXISTS `dbt_perf_route` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `db_regex` varchar(200) NOT NULL,
  `priority` int NOT NULL,
  `assignee_id` int DEFAULT NULL,
  `instance_id` int DEFAULT NULL,
  `team_id` int NOT NULL,
  PRIMARY KEY (`id`),
  KEY `perf_route_assignee_id_6560876f_fk_sql_users_id` (`assignee_id`),
  KEY `perf_route_instance_id_76043af9_fk_sql_instance_id` (`instance_id`),
  KEY `perf_route_team_id_29f6ba19_fk_resource_group_group_id` (`team_id`),
  CONSTRAINT `dbt_perf_route_assignee_id_6560876f_fk_sql_users_id` FOREIGN KEY (`assignee_id`) REFERENCES `dbt_sql_users` (`id`),
  CONSTRAINT `dbt_perf_route_instance_id_76043af9_fk_sql_instance_id` FOREIGN KEY (`instance_id`) REFERENCES `dbt_sql_instance` (`id`),
  CONSTRAINT `dbt_perf_route_team_id_29f6ba19_fk_resource_group_group_id` FOREIGN KEY (`team_id`) REFERENCES `dbt_resource_group` (`group_id`)
) ENGINE=InnoDB;

--
-- 表 `dbt_perf_team_directors`（源表 `perf_team_directors`）
--
CREATE TABLE IF NOT EXISTS `dbt_perf_team_directors` (
  `id` int NOT NULL AUTO_INCREMENT,
  `perfteam_id` bigint NOT NULL,
  `users_id` int NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `perf_team_directors_perfteam_id_users_id_31f3b261_uniq` (`perfteam_id`,`users_id`),
  KEY `perf_team_directors_users_id_7e0c029c_fk_sql_users_id` (`users_id`),
  CONSTRAINT `dbt_perf_team_directors_perfteam_id_6acda621_fk_perf_team_id` FOREIGN KEY (`perfteam_id`) REFERENCES `dbt_perf_team` (`id`),
  CONSTRAINT `dbt_perf_team_directors_users_id_7e0c029c_fk_sql_users_id` FOREIGN KEY (`users_id`) REFERENCES `dbt_sql_users` (`id`)
) ENGINE=InnoDB;

--
-- 表 `dbt_perf_team_leaders`（源表 `perf_team_leaders`）
--
CREATE TABLE IF NOT EXISTS `dbt_perf_team_leaders` (
  `id` int NOT NULL AUTO_INCREMENT,
  `perfteam_id` bigint NOT NULL,
  `users_id` int NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `perf_team_leaders_perfteam_id_users_id_751dd7ad_uniq` (`perfteam_id`,`users_id`),
  KEY `perf_team_leaders_users_id_7691b445_fk_sql_users_id` (`users_id`),
  CONSTRAINT `dbt_perf_team_leaders_perfteam_id_885434ee_fk_perf_team_id` FOREIGN KEY (`perfteam_id`) REFERENCES `dbt_perf_team` (`id`),
  CONSTRAINT `dbt_perf_team_leaders_users_id_7691b445_fk_sql_users_id` FOREIGN KEY (`users_id`) REFERENCES `dbt_sql_users` (`id`)
) ENGINE=InnoDB;

--
-- 表 `dbt_query_privileges`（源表 `query_privileges`）
--
CREATE TABLE IF NOT EXISTS `dbt_query_privileges` (
  `privilege_id` int NOT NULL AUTO_INCREMENT,
  `user_name` varchar(30) NOT NULL,
  `user_display` varchar(50) NOT NULL,
  `db_name` varchar(64) NOT NULL,
  `table_name` varchar(64) NOT NULL,
  `valid_date` date NOT NULL,
  `limit_num` int NOT NULL,
  `priv_type` int NOT NULL,
  `is_deleted` int NOT NULL,
  `create_time` datetime(6) NOT NULL,
  `sys_time` datetime(6) NOT NULL,
  `instance_id` int NOT NULL,
  PRIMARY KEY (`privilege_id`),
  KEY `query_privileges_instance_id_047fcde2_fk_sql_instance_id` (`instance_id`),
  KEY `query_privileges_user_name_instance_id_db__ed2ad8a3_idx` (`user_name`,`instance_id`,`db_name`,`valid_date`),
  CONSTRAINT `dbt_query_privileges_instance_id_047fcde2_fk_sql_instance_id` FOREIGN KEY (`instance_id`) REFERENCES `dbt_sql_instance` (`id`) ON DELETE RESTRICT ON UPDATE RESTRICT
) ENGINE=InnoDB;

--
-- 表 `dbt_query_privileges_apply`（源表 `query_privileges_apply`）
--
CREATE TABLE IF NOT EXISTS `dbt_query_privileges_apply` (
  `apply_id` int NOT NULL AUTO_INCREMENT,
  `group_id` int NOT NULL,
  `group_name` varchar(100) NOT NULL,
  `title` varchar(50) NOT NULL,
  `user_name` varchar(30) NOT NULL,
  `user_display` varchar(50) NOT NULL,
  `db_list` longtext NOT NULL,
  `table_list` longtext NOT NULL,
  `valid_date` date NOT NULL,
  `limit_num` int NOT NULL,
  `priv_type` int NOT NULL,
  `status` int NOT NULL,
  `audit_auth_groups` varchar(255) NOT NULL,
  `create_time` datetime(6) NOT NULL,
  `sys_time` datetime(6) NOT NULL,
  `instance_id` int NOT NULL,
  PRIMARY KEY (`apply_id`),
  KEY `query_privileges_apply_instance_id_bc03347f_fk_sql_instance_id` (`instance_id`),
  CONSTRAINT `dbt_query_privileges_apply_instance_id_bc03347f_fk_sql_inst_2dfb` FOREIGN KEY (`instance_id`) REFERENCES `dbt_sql_instance` (`id`) ON DELETE RESTRICT ON UPDATE RESTRICT
) ENGINE=InnoDB;

--
-- 表 `dbt_slow_logs`（源表 `slow_logs`）
--
CREATE TABLE IF NOT EXISTS `dbt_slow_logs` (
  `id` bigint NOT NULL AUTO_INCREMENT COMMENT 'ID',
  `instance_id` int NOT NULL,
  `updated_at_micro` bigint DEFAULT NULL COMMENT '游标(微秒)',
  `start_time` datetime DEFAULT NULL COMMENT '执行开始时间',
  `user_host` varchar(255) DEFAULT NULL COMMENT '用户@主机',
  `query_time_sec` float DEFAULT '0' COMMENT '执行时长(秒)',
  `lock_time_sec` float DEFAULT '0' COMMENT '锁定时长(秒)',
  `rows_sent` int DEFAULT '0' COMMENT '返回行数',
  `rows_examined` int DEFAULT '0' COMMENT '扫描行数',
  `db_name` varchar(100) DEFAULT NULL COMMENT '数据库',
  `sql_text` text COMMENT 'SQL文本',
  `sql_md5` varchar(64) DEFAULT NULL COMMENT 'SQL指纹',
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '采集时间',
  PRIMARY KEY (`id`),
  KEY `fk_slowlogs_instance` (`instance_id`),
  KEY `idx_start_time` (`start_time`),
  CONSTRAINT `dbt_fk_slowlogs_instance` FOREIGN KEY (`instance_id`) REFERENCES `dbt_sql_instance` (`id`) ON DELETE CASCADE ON UPDATE CASCADE
) ENGINE=InnoDB COMMENT='线上慢日志';

--
-- 表 `dbt_sql_instance_instance_tag`（源表 `sql_instance_instance_tag`）
--
CREATE TABLE IF NOT EXISTS `dbt_sql_instance_instance_tag` (
  `id` int NOT NULL AUTO_INCREMENT,
  `instance_id` int NOT NULL,
  `instancetag_id` int NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `sql_instance_instance_ta_instance_id_instancetag__2797cd96_uniq` (`instance_id`,`instancetag_id`),
  KEY `sql_instance_instanc_instancetag_id_d18b0067_fk_sql_insta` (`instancetag_id`),
  CONSTRAINT `dbt_sql_instance_instanc_instance_id_35a5a19d_fk_sql_insta` FOREIGN KEY (`instance_id`) REFERENCES `dbt_sql_instance` (`id`) ON DELETE RESTRICT ON UPDATE RESTRICT,
  CONSTRAINT `dbt_sql_instance_instanc_instancetag_id_d18b0067_fk_sql_insta` FOREIGN KEY (`instancetag_id`) REFERENCES `dbt_sql_instance_tag` (`id`) ON DELETE RESTRICT ON UPDATE RESTRICT
) ENGINE=InnoDB;

--
-- 表 `dbt_sql_instance_resource_group`（源表 `sql_instance_resource_group`）
--
CREATE TABLE IF NOT EXISTS `dbt_sql_instance_resource_group` (
  `id` int NOT NULL AUTO_INCREMENT,
  `instance_id` int NOT NULL,
  `resourcegroup_id` int NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `sql_instance_resource_gr_instance_id_resourcegrou_0dbfb149_uniq` (`instance_id`,`resourcegroup_id`),
  KEY `sql_instance_resourc_resourcegroup_id_1903be9b_fk_resource_` (`resourcegroup_id`),
  CONSTRAINT `dbt_sql_instance_resourc_instance_id_30cfcc32_fk_sql_insta` FOREIGN KEY (`instance_id`) REFERENCES `dbt_sql_instance` (`id`) ON DELETE RESTRICT ON UPDATE RESTRICT,
  CONSTRAINT `dbt_sql_instance_resourc_resourcegroup_id_1903be9b_fk_resource_` FOREIGN KEY (`resourcegroup_id`) REFERENCES `dbt_resource_group` (`group_id`) ON DELETE RESTRICT ON UPDATE RESTRICT
) ENGINE=InnoDB;

--
-- 表 `dbt_sql_mssql_inspection_report`（源表 `sql_mssql_inspection_report`）
--
CREATE TABLE IF NOT EXISTS `dbt_sql_mssql_inspection_report` (
  `id` int NOT NULL AUTO_INCREMENT,
  `instance_id` int NOT NULL,
  `status` varchar(20) NOT NULL DEFAULT 'running' COMMENT 'running / finish / failed',
  `trigger_type` varchar(10) NOT NULL DEFAULT 'manual' COMMENT 'manual / schedule',
  `operator` varchar(30) NOT NULL DEFAULT '',
  `server_start_time` datetime(6) DEFAULT NULL COMMENT '目标实例启动时间，相同才对累计计数器做差',
  `alert_count` int NOT NULL DEFAULT '0',
  `attention_count` int NOT NULL DEFAULT '0',
  `normal_count` int NOT NULL DEFAULT '0',
  `failed_count` int NOT NULL DEFAULT '0',
  `total_items` int NOT NULL DEFAULT '0',
  `done_items` int NOT NULL DEFAULT '0',
  `summary` json DEFAULT NULL COMMENT 'Top 问题、分区计数、实例概况',
  `error_message` longtext,
  `start_time` datetime(6) NOT NULL,
  `finish_time` datetime(6) DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_insp_instance_time` (`instance_id`,`start_time`),
  CONSTRAINT `dbt_fk_insp_report_instance` FOREIGN KEY (`instance_id`) REFERENCES `dbt_sql_instance` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB COMMENT='MSSQL性能巡检报告';

--
-- 表 `dbt_sql_users_user_permissions`（源表 `sql_users_user_permissions`）
--
CREATE TABLE IF NOT EXISTS `dbt_sql_users_user_permissions` (
  `id` int NOT NULL AUTO_INCREMENT,
  `users_id` int NOT NULL,
  `permission_id` int NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `sql_users_user_permissions_users_id_permission_id_5fffb2bb_uniq` (`users_id`,`permission_id`),
  KEY `sql_users_user_permi_permission_id_e990caab_fk_auth_perm` (`permission_id`),
  CONSTRAINT `dbt_sql_users_user_permi_permission_id_e990caab_fk_auth_perm` FOREIGN KEY (`permission_id`) REFERENCES `dbt_auth_permission` (`id`) ON DELETE RESTRICT ON UPDATE RESTRICT,
  CONSTRAINT `dbt_sql_users_user_permissions_users_id_efad14b0_fk_sql_users_id` FOREIGN KEY (`users_id`) REFERENCES `dbt_sql_users` (`id`) ON DELETE RESTRICT ON UPDATE RESTRICT
) ENGINE=InnoDB;

--
-- 表 `dbt_sql_workflow`（源表 `sql_workflow`）
--
CREATE TABLE IF NOT EXISTS `dbt_sql_workflow` (
  `id` int NOT NULL AUTO_INCREMENT,
  `workflow_name` varchar(50) NOT NULL,
  `demand_url` varchar(500) NOT NULL,
  `group_id` int NOT NULL,
  `group_name` varchar(100) NOT NULL,
  `db_name` varchar(64) NOT NULL,
  `syntax_type` int NOT NULL,
  `is_backup` tinyint(1) NOT NULL,
  `engineer` varchar(30) NOT NULL,
  `engineer_display` varchar(50) NOT NULL,
  `status` varchar(50) NOT NULL,
  `audit_auth_groups` varchar(255) NOT NULL,
  `run_date_start` datetime(6) DEFAULT NULL,
  `run_date_end` datetime(6) DEFAULT NULL,
  `create_time` datetime(6) NOT NULL,
  `finish_time` datetime(6) DEFAULT NULL,
  `is_manual` int NOT NULL,
  `is_offline_export` int NOT NULL,
  `export_format` varchar(10) DEFAULT NULL,
  `file_name` varchar(255) DEFAULT NULL,
  `instance_id` int NOT NULL,
  `batch_instance_ids` varchar(5000) DEFAULT NULL COMMENT '批量工单实例ID列表，逗号分隔；为空表示单实例工单',
  PRIMARY KEY (`id`),
  KEY `sql_workflow_instance_id_ad34809b_fk_sql_instance_id` (`instance_id`),
  CONSTRAINT `dbt_sql_workflow_instance_id_ad34809b_fk_sql_instance_id` FOREIGN KEY (`instance_id`) REFERENCES `dbt_sql_instance` (`id`) ON DELETE RESTRICT ON UPDATE RESTRICT
) ENGINE=InnoDB;

--
-- 表 `dbt_perf_issue_log`（源表 `perf_issue_log`）
--
CREATE TABLE IF NOT EXISTS `dbt_perf_issue_log` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `action` varchar(30) NOT NULL,
  `content` longtext NOT NULL,
  `operator` varchar(150) NOT NULL,
  `operator_display` varchar(50) NOT NULL,
  `create_time` datetime(6) NOT NULL,
  `issue_id` bigint NOT NULL,
  PRIMARY KEY (`id`),
  KEY `perf_issue_log_issue_id_8a6fdd42_fk_perf_issue_id` (`issue_id`),
  CONSTRAINT `dbt_perf_issue_log_issue_id_8a6fdd42_fk_perf_issue_id` FOREIGN KEY (`issue_id`) REFERENCES `dbt_perf_issue` (`id`)
) ENGINE=InnoDB;

--
-- 表 `dbt_perf_issue_stat`（源表 `perf_issue_stat`）
--
CREATE TABLE IF NOT EXISTS `dbt_perf_issue_stat` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `stat_date` date NOT NULL,
  `exec_count` bigint NOT NULL,
  `total_time` double NOT NULL,
  `max_time_ms` double NOT NULL,
  `rows_examined` double NOT NULL,
  `rows_sent` double NOT NULL,
  `issue_id` bigint NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `perf_issue_stat_issue_id_stat_date_b0e729fb_uniq` (`issue_id`,`stat_date`),
  CONSTRAINT `dbt_perf_issue_stat_issue_id_b07059d2_fk_perf_issue_id` FOREIGN KEY (`issue_id`) REFERENCES `dbt_perf_issue` (`id`)
) ENGINE=InnoDB;

--
-- 表 `dbt_sql_mssql_inspection_item`（源表 `sql_mssql_inspection_item`）
--
CREATE TABLE IF NOT EXISTS `dbt_sql_mssql_inspection_item` (
  `id` int NOT NULL AUTO_INCREMENT,
  `report_id` int NOT NULL,
  `section` varchar(20) NOT NULL,
  `item_key` varchar(50) NOT NULL,
  `item_name` varchar(100) NOT NULL,
  `db_name` varchar(128) NOT NULL DEFAULT '',
  `level` smallint NOT NULL DEFAULT '0' COMMENT '0正常 1关注 2告警 3未采集',
  `value` varchar(500) NOT NULL DEFAULT '',
  `threshold` varchar(200) NOT NULL DEFAULT '',
  `detail` json DEFAULT NULL COMMENT '{"columns": [...], "rows": [[...]]}',
  `raw_counter` json DEFAULT NULL COMMENT '累计型计数器原始值，供下次做差',
  `elapsed_ms` int NOT NULL DEFAULT '0',
  PRIMARY KEY (`id`),
  KEY `idx_insp_item_report` (`report_id`),
  KEY `idx_insp_item_key` (`item_key`,`db_name`),
  CONSTRAINT `dbt_fk_insp_item_report` FOREIGN KEY (`report_id`) REFERENCES `dbt_sql_mssql_inspection_report` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB COMMENT='MSSQL性能巡检项';

--
-- 表 `dbt_sql_workflow_content`（源表 `sql_workflow_content`）
--
CREATE TABLE IF NOT EXISTS `dbt_sql_workflow_content` (
  `id` int NOT NULL AUTO_INCREMENT,
  `sql_content` longtext NOT NULL,
  `review_content` longtext NOT NULL,
  `execute_result` longtext NOT NULL,
  `workflow_id` int NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `workflow_id` (`workflow_id`),
  CONSTRAINT `dbt_sql_workflow_content_workflow_id_3af79b62_fk_sql_workflow_id` FOREIGN KEY (`workflow_id`) REFERENCES `dbt_sql_workflow` (`id`) ON DELETE RESTRICT ON UPDATE RESTRICT
) ENGINE=InnoDB;

SET FOREIGN_KEY_CHECKS=1;
