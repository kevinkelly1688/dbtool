// @author: kevinkelly
// @license: Apache Licence
// @file: keys.go
// @time: 2026/10/28

package settings

// 设置键：<领域>.<名称>。代码里一律用这些常量，不写裸字符串；完整说明、默认值与旧键名见 registry.go。
const (
	// ---- 登录与安全 ----
	KeySecurityLoginLockAttempts     = "security.login_lock_attempts"
	KeySecurityLoginLockSeconds      = "security.login_lock_seconds"
	KeySecurityRequire2FA            = "security.require_2fa"
	KeySecuritySelfSignup            = "security.self_signup"
	KeySecurityPasswordMinLength     = "security.password_min_length"
	KeySecurityPasswordComplexity    = "security.password_complexity"
	KeySecuritySessionMinutes        = "security.session_minutes"
	KeySecuritySessionCloseOnExit    = "security.session_close_on_exit"
	KeySecurityJWTHours              = "security.jwt_hours"
	KeySecurityAPIJWTRequireOTP      = "security.api_jwt_require_otp"
	KeySecurityAPIUserIDs            = "security.api_user_ids"
	KeySecurityLocalForm             = "security.local_login_form"
	KeySecurityExternalMatchBy       = "security.external_match_by"
	KeySecuritySuperuserLocalOnly    = "security.superuser_local_only"
	KeySecurityExternal2FA           = "security.external_2fa"
	KeySecurityAllowSelfBind         = "security.allow_self_bind"
	KeySecurityBackfillIDs           = "security.backfill_ids"
	KeySecurityDefaultAuthGroups     = "security.default_auth_groups"
	KeySecurityDefaultResourceGroups = "security.default_resource_groups"
	KeySecurityLocalLastLogin        = "security.local_last_login"
	KeySecurityWatermarkEnabled      = "security.watermark_enabled"
	KeySecurityWatermarkText         = "security.watermark_text"
	KeySecurityWatermarkExtra        = "security.watermark_extra"
	KeySecurityWatermarkOpacity      = "security.watermark_opacity"
	KeySecurityWatermarkFontSize     = "security.watermark_font_size"
	KeySecurityWatermarkAngle        = "security.watermark_angle"
	KeySecurityWatermarkGap          = "security.watermark_gap"
	KeySecurityWatermarkColor        = "security.watermark_color"

	// ---- 短信 ----
	KeySMSProvider         = "sms.provider"
	KeySMSAliyunKeyID      = "sms.aliyun_access_key_id"
	KeySMSAliyunKeySecret  = "sms.aliyun_access_key_secret"
	KeySMSAliyunSign       = "sms.aliyun_sign_name"
	KeySMSAliyunTemplate   = "sms.aliyun_template_code"
	KeySMSAliyunVar        = "sms.aliyun_variable_name"
	KeySMSTencentSecretID  = "sms.tencent_secret_id"
	KeySMSTencentSecretKey = "sms.tencent_secret_key"
	KeySMSTencentSign      = "sms.tencent_sign_name"
	KeySMSTencentTemplate  = "sms.tencent_template_id"
	KeySMSTencentAppID     = "sms.tencent_sdk_appid"

	// ---- 在线查询 ----
	KeyQueryMaskEnabled      = "query.mask_enabled"
	KeyQueryBlockOnMaskError = "query.block_on_mask_error"
	KeyQueryForbidSelectStar = "query.forbid_select_star"
	KeyQueryMaxExecSeconds   = "query.max_execution_seconds"
	KeyQueryAdminRowLimit    = "query.admin_row_limit"
	KeyQueryMaxCellBytes     = "query.max_cell_bytes"
	KeyQueryMaxResultBytes   = "query.max_result_bytes"

	// ---- SQL 防火墙 ----
	KeyFirewallEnabled          = "firewall.enabled"
	KeyFirewallMode             = "firewall.mode"
	KeyFirewallOnError          = "firewall.on_error"
	KeyFirewallExplainTimeoutMs = "firewall.explain_timeout_ms"
	KeyFirewallStoreSQL         = "firewall.store_sql"

	// ---- SQL 上线与审核 ----
	KeyReviewCriticalDDLRegex     = "review.critical_ddl_regex"
	KeyReviewAutoApproveEnabled   = "review.auto_approve_enabled"
	KeyReviewAutoApproveDBTypes   = "review.auto_approve_db_types"
	KeyReviewAutoApproveTags      = "review.auto_approve_tags"
	KeyReviewAutoApproveExclRegex = "review.auto_approve_exclude_regex"
	KeyReviewAutoApproveMaxRows   = "review.auto_approve_max_rows"
	KeyReviewAutoRejectLevel      = "review.auto_reject_level"
	KeyReviewDDLDMLSeparation     = "review.ddl_dml_separation"
	KeyReviewBackupEnabled        = "review.backup_enabled"
	KeyReviewExactRowCount        = "review.exact_row_count"
	KeyReviewMSSQLRules           = "review.mssql_rules"
	KeyReviewBatchWorkers         = "review.batch_parallel_workers"
	KeyReviewMSSQLPrintAsError    = "review.mssql_print_as_error"

	// ---- 审批与通知 ----
	KeyApprovalForbidSelf   = "approval.forbid_self_approve"
	KeyNotifyPhases         = "notify.phases"
	KeyNotifySQLPreview     = "notify.sql_preview"
	KeyNotifyDDLExtraGroups = "notify.ddl_extra_groups"

	// ---- goInception ----
	KeyGoInceptionHost           = "goinception.host"
	KeyGoInceptionPort           = "goinception.port"
	KeyGoInceptionUser           = "goinception.user"
	KeyGoInceptionPassword       = "goinception.password"
	KeyGoInceptionConfigFile     = "goinception.config_file"
	KeyGoInceptionBackupHost     = "goinception.backup_host"
	KeyGoInceptionBackupPort     = "goinception.backup_port"
	KeyGoInceptionBackupUser     = "goinception.backup_user"
	KeyGoInceptionBackupPassword = "goinception.backup_password"

	// ---- 数据导出 ----
	KeyExportStorageType = "export.storage_type"
	KeyExportLocalPath   = "export.local_path"
	KeyExportMaxRows     = "export.max_rows"
	KeyExportMaxSeconds  = "export.max_seconds"
	KeyExportMaskEnabled = "export.mask_enabled"

	// ---- 日志解析（Binlog 解析 / 回滚）----
	KeyBinlogMaxRows     = "binlog.max_rows"
	KeyBinlogMaxResultMB = "binlog.max_result_mb"

	// ---- 分区维护 ----
	KeyPartitionAutoRun             = "partition.auto_run"
	KeyPartitionAutoDrop            = "partition.auto_drop"
	KeyPartitionAutoIntervalMinutes = "partition.auto_interval_minutes"
	KeyPartitionNotifyOn            = "partition.notify_on"
	// 可用分区数量告警
	KeyPartitionAlertEnabled       = "partition.alert_enabled"
	KeyPartitionAlertMinAvailable  = "partition.alert_min_available"
	KeyPartitionAlertRepeatHours   = "partition.alert_repeat_hours"
	KeyPartitionAlertNotifyRecover = "partition.alert_notify_recover"

	// ---- 数据保留（各目标的天数键是 retention.<目标>_days，见 RetentionDaysKey）----
	KeyRetentionEnabled              = "retention.enabled"
	KeyRetentionWindow               = "retention.window"
	KeyRetentionIntervalMinutes      = "retention.interval_minutes"
	KeyRetentionMaxMinutes           = "retention.max_minutes"
	KeyRetentionMaxRows              = "retention.max_rows"
	KeyRetentionBatchSize            = "retention.batch_size"
	KeyRetentionBatchSleepMs         = "retention.batch_sleep_ms"
	KeyRetentionArchiveBackupEnabled = "retention.archive_backup_enabled"

	// ---- 性能工单 ----
	KeyPerfCollectWindowMinutes = "perf.collect_window_minutes"
	KeyPerfSeverityRules        = "perf.severity_rules"
	KeyPerfJobs                 = "perf.jobs"
	KeyPerfQuietHours           = "perf.quiet_hours"
	KeyPerfWeekendSilent        = "perf.weekend_silent"
	KeyPerfVerifyDays           = "perf.verify_days"
	KeyPerfVerifyImproveRatio   = "perf.verify_improve_ratio"
	KeyPerfAutoCloseAbsentDays  = "perf.auto_close_absent_days"
	KeyPerfDBAUsers             = "perf.dba_users"
	KeyPerfReopenMinScore       = "perf.reopen_min_score"
	KeyPerfAutoExplain          = "perf.auto_explain"
	KeyPerfExplainTimeoutMs     = "perf.explain_timeout_ms"
	KeyPerfSilentMode           = "perf.silent_mode"
	KeyPerfGroupNotify          = "perf.group_notify"

	// ---- 性能巡检 ----
	KeyInspectMySQLRetentionDays = "inspect.mysql_retention_days"
	KeyInspectMSSQLRetentionDays = "inspect.mssql_retention_days"
	KeyInspectRedisRetentionDays = "inspect.redis_retention_days"

	// ---- SQL 优化工具 ----
	KeyOptimizerSoarPath    = "optimizer.soar_path"
	KeyOptimizerSoarConfig  = "optimizer.soar_config"
	KeyOptimizerSoarTestDSN = "optimizer.soar_test_dsn"
)

// RetentionDaysKey 返回数据保留某个目标的保留天数键（target 如 "query_event"、"slow_logs"）。
func RetentionDaysKey(target string) string { return "retention." + target + "_days" }

// internalPrefix 开头的键是服务自己用的内部状态（导入标记等），不在页面显示，也不能通过 Set 修改。
const internalPrefix = "_internal."
