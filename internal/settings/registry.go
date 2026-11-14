// @author: kevinkelly
// @license: Apache Licence
// @file: registry.go
// @time: 2026/10/28

package settings

// 分组（页面左侧导航的顺序）。
const (
	GroupSecurity  = "security"
	GroupSMS       = "sms"
	GroupQuery     = "query"
	GroupFirewall  = "firewall"
	GroupReview    = "review"
	GroupNotify    = "notify"
	GroupGoInc     = "goinception"
	GroupExport    = "export"
	GroupBinlog    = "binlog"
	GroupPartition = "partition"
	GroupRetention = "retention"
	GroupPerf      = "perf"
	GroupInspect   = "inspect"
	GroupOptimizer = "optimizer"
)

// Groups 是分组的显示顺序。
var Groups = []Group{
	{GroupSecurity, "登录与安全", "登录锁定、密码与会话、两步验证、自助注册、页面水印"},
	{GroupSMS, "短信", "短信两步验证使用的短信服务商与密钥"},
	{GroupQuery, "在线查询与脱敏", "查询超时、行数 / 字节上限、结果脱敏"},
	{GroupFirewall, "SQL 防火墙", "总开关、全局模式与出错策略（规则在“SQL 防火墙”页面维护）"},
	{GroupReview, "SQL 上线与审核", "高危 DDL、自动审批与驳回、备份、批量工单并发"},
	{GroupNotify, "审批与通知", "审批约束与通知内容"},
	{GroupGoInc, "goInception", "审核引擎与备份库的连接"},
	{GroupExport, "数据导出", "导出文件的存放位置与上限"},
	{GroupBinlog, "日志解析", "Binlog 解析 / 回滚结果的条数与体积上限"},
	{GroupPartition, "分区维护", "分区表自动续建与过期删除"},
	{GroupRetention, "数据保留", "各类日志 / 记录 / 文件的保留与清理节奏"},
	{GroupPerf, "性能工单", "采集、严重度、验证与通知"},
	{GroupInspect, "性能巡检", "巡检报告保留与旧定时"},
	{GroupOptimizer, "SQL 优化工具", "SOAR 程序、配置文件与测试环境（仅超级用户可改，保存时会校验文件与测试库地址）"},
}

// offPageGroups 是在专门页面里维护的分组：登记与读写照常（命令行、各功能模块都能用），只是不出现在“系统配置”页。
var offPageGroups = map[string]bool{GroupGoInc: true}

// OffPage 报告分组是否不在“系统配置”页显示。
func OffPage(group string) bool { return offPageGroups[group] }

const (
	hrefSecurity = "/settings/security"
	hrefMasking  = "/settings/masking"
	hrefFirewall = "/settings/firewall"
	hrefFlow     = "/settings/audit-flow"
	hrefGI       = "/settings/goinception"
	hrefPerfCol  = "/perf/config/collect"
	hrefPerfSLA  = "/perf/config/sla"
)

const (
	riskDelete = "到期数据会被物理删除，不可恢复（开启“启用保留策略清理”后才会执行）"
)

// defs 是全部登记项，按页面显示顺序排列。
var defs = buildDefs()

func buildDefs() []*Def {
	var ds []*Def
	add := func(d *Def) { ds = append(ds, d) }
	boolean := func(key, group, section, label, def string) *Def {
		return newDef(key, group, section, label, TypeBool, def)
	}
	integer := func(key, group, section, label, def string, lo, hi float64) *Def {
		return newDef(key, group, section, label, TypeInt, def).rng(lo, hi)
	}
	str := func(key, group, section, label, def string) *Def {
		return newDef(key, group, section, label, TypeString, def)
	}
	text := func(key, group, section, label, def string) *Def {
		return newDef(key, group, section, label, TypeText, def)
	}
	list := func(key, group, section, label, def string) *Def {
		return newDef(key, group, section, label, TypeList, def)
	}
	secret := func(key, group, section, label string) *Def {
		return newDef(key, group, section, label, TypeSecret, "")
	}
	enum := func(key, group, section, label, def string, pairs ...string) *Def {
		return newDef(key, group, section, label, TypeEnum, def).opts(pairs...)
	}

	// ================= 登录与安全 =================
	g := GroupSecurity
	add(integer(KeySecurityLoginLockAttempts, g, "登录锁定", "连续失败锁定次数", "5", 1, 1000).
		help("连续登录失败达到这个次数后锁定账号。"))
	add(integer(KeySecurityLoginLockSeconds, g, "登录锁定", "锁定时长", "300", 1, 86400*30).unit("秒").
		help("账号被锁定后多少秒内不能登录。"))
	add(integer(KeySecurityPasswordMinLength, g, "密码与会话", "密码最短长度", "9", 6, 64).unit("位").
		help("新密码至少多少位（含修改、重置与新建用户）。"))
	add(boolean(KeySecurityPasswordComplexity, g, "密码与会话", "密码复杂度", "false").
		help("开启后新密码必须同时包含字母、数字与特殊字符。"))
	add(integer(KeySecuritySessionMinutes, g, "密码与会话", "会话有效期", "300", 5, 60*24*30).unit("分钟").
		help("登录会话多久不活动后失效。"))
	add(boolean(KeySecuritySessionCloseOnExit, g, "密码与会话", "关闭浏览器即退出", "true").
		help("会话 Cookie 不带过期时间，关闭浏览器后需要重新登录。"))
	add(integer(KeySecurityJWTHours, g, "API", "API 令牌有效期", "4", 1, 24*30).unit("小时"))
	add(boolean(KeySecurityAPIJWTRequireOTP, g, "API", "申请 API 令牌需两步验证", "false"))
	add(list(KeySecurityAPIUserIDs, g, "API", "允许使用 API 的用户 ID", "").check(checkUintList).
		help("逗号分隔的用户 ID；为空表示不限制。").hint("例如 3,15"))
	add(boolean(KeySecurityRequire2FA, g, "两步验证", "强制两步验证", "false").
		help("开启后用户登录必须完成两步验证。"))
	add(boolean(KeySecurityExternal2FA, g, "两步验证", "外部登录也要两步验证", "false").
		help("通过 LDAP / OIDC 等外部登录方式进入的用户，是否仍要完成平台的两步验证。"))
	add(boolean(KeySecuritySelfSignup, g, "注册与默认权限", "允许自助注册", "false").
		help("登录页是否显示注册入口。"))
	add(list(KeySecurityDefaultAuthGroups, g, "注册与默认权限", "新用户默认权限组", "").
		help("新用户默认加入的权限组名称，逗号分隔（登录方式自己配置了就用它的）。"))
	add(list(KeySecurityDefaultResourceGroups, g, "注册与默认权限", "新用户默认资源组", "").
		help("新用户默认加入的资源组名称，逗号分隔（登录方式自己配置了就用它的）。"))
	add(enum(KeySecurityLocalForm, g, "登录页与外部账号", "本地账号表单", "collapse",
		"always=始终显示", "collapse=折叠", "hidden=隐藏（仅 /login?local=1）").
		help("配置了外部登录方式后，登录页上本地账号表单的显示方式。"))
	add(list(KeySecurityExternalMatchBy, g, "登录页与外部账号", "外部账号匹配依据", "username,email").
		check(checkCSVOf("username", "email", "third_id", "phone")).
		help("外部账号首次登录时按哪些字段匹配已有用户，可选 username、email、third_id、phone，逗号分隔；为空则只认已绑定的外部身份。"))
	add(boolean(KeySecuritySuperuserLocalOnly, g, "登录页与外部账号", "超级用户只能本地登录", "true").
		help("开启后超级用户不能通过外部登录方式进入。"))
	add(boolean(KeySecurityAllowSelfBind, g, "登录页与外部账号", "允许用户自行绑定外部账号", "false").
		help("允许用户在“外部账号绑定”页自行绑定 / 解绑。"))
	add(boolean(KeySecurityBackfillIDs, g, "登录页与外部账号", "回填外部账号唯一 ID", "true").
		help("按用户名等依据匹配成功后，记下外部唯一 ID，之后即使外部用户名变化也认这个绑定。"))
	add(str(KeySecurityLocalLastLogin, g, "内部状态", "本地账号最近登录时间", "").hide())
	add(boolean(KeySecurityWatermarkEnabled, g, "页面水印", "启用页面水印", "false").
		help("在页面上叠加姓名、账号和时间，用于截图溯源。"))
	add(str(KeySecurityWatermarkText, g, "页面水印", "水印内容", "{姓名}（{账号}）{时间}").check(checkWatermarkText). // i18n-ignore 默认值是带变量名的模板，不是界面文案
														help("可用变量：{账号} {姓名} {部门} {IP} {日期} {时间}；必须包含 {账号} 或 {姓名}，最长 60 个字符。"))
	add(str(KeySecurityWatermarkExtra, g, "页面水印", "附加文字", "").check(checkWatermarkExtra).
		help("附加在水印后面的固定文字，最长 40 个字符。"))
	add(integer(KeySecurityWatermarkOpacity, g, "页面水印", "透明度", "14", 3, 30).unit("%"))
	add(integer(KeySecurityWatermarkFontSize, g, "页面水印", "字号", "14", 12, 28).unit("px"))
	add(integer(KeySecurityWatermarkAngle, g, "页面水印", "倾斜角度", "-22", -60, 0).unit("度"))
	add(enum(KeySecurityWatermarkGap, g, "页面水印", "疏密", "normal", "tight=紧密", "normal=适中", "loose=稀疏"))
	add(enum(KeySecurityWatermarkColor, g, "页面水印", "颜色", "auto", "auto=自动", "gray=灰色", "red=红色", "blue=蓝色"))

	// ================= 短信 =================
	g = GroupSMS
	add(enum(KeySMSProvider, g, "服务商", "短信服务商", "disabled", "disabled=未启用", "aliyun=阿里云", "tencent=腾讯云").
		help("两步验证的短信通道；未启用时用户不能绑定短信验证。"))
	add(str(KeySMSAliyunKeyID, g, "阿里云", "AccessKey ID", ""))
	add(secret(KeySMSAliyunKeySecret, g, "阿里云", "AccessKey Secret"))
	add(str(KeySMSAliyunSign, g, "阿里云", "短信签名", ""))
	add(str(KeySMSAliyunTemplate, g, "阿里云", "短信模板 Code", ""))
	add(str(KeySMSAliyunVar, g, "阿里云", "模板变量名", "code").help("模板里放验证码的变量名。"))
	add(str(KeySMSTencentSecretID, g, "腾讯云", "SecretId", ""))
	add(secret(KeySMSTencentSecretKey, g, "腾讯云", "SecretKey"))
	add(str(KeySMSTencentSign, g, "腾讯云", "短信签名", ""))
	add(str(KeySMSTencentTemplate, g, "腾讯云", "短信模板 ID", ""))
	add(str(KeySMSTencentAppID, g, "腾讯云", "SDKAppID", ""))

	// ================= 在线查询与脱敏 =================
	g = GroupQuery
	add(boolean(KeyQueryMaskEnabled, g, "脱敏", "查询结果脱敏总开关", "false").
		help("关闭后在线查询返回原值，脱敏规则暂不生效。").link("数据脱敏", hrefMasking))
	add(boolean(KeyQueryBlockOnMaskError, g, "脱敏", "脱敏出错时拦截结果", "false").
		help("开启：脱敏出错时不返回数据；关闭：放行未脱敏的结果并提示。").
		risk("关闭后脱敏出错时，查询结果会以未脱敏状态返回给用户"))
	add(boolean(KeyExportMaskEnabled, g, "脱敏", "导出工单脱敏", "true").
		help("SQL 导出工单是否按脱敏规则处理；关闭后导出原值。").
		risk("关闭后导出文件里是未脱敏的原值").link("数据脱敏", hrefMasking))
	add(boolean(KeyQueryForbidSelectStar, g, "查询限制", "禁止 select *", "false").
		help("开启后在线查询拒绝 select *。"))
	add(integer(KeyQueryMaxExecSeconds, g, "查询限制", "查询超时", "60", 1, 3600).unit("秒").
		help("超时后发 KILL QUERY 终止这条查询。"))
	add(integer(KeyQueryAdminRowLimit, g, "查询限制", "管理员查询行数上限", "5000", 1, 1000000).unit("行").
		help("超级用户 / 有全实例查询权限 / 资源组实例查询时的行数上限。"))
	add(integer(KeyQueryMaxCellBytes, g, "SQL Server 结果上限", "单元格字节上限", "4194304", 1024, 268435456).unit("字节").
		help("超出的单元格被截断并提示；生成脚本类的查询一个格子就是几百 KB，别调太小。"))
	add(integer(KeyQueryMaxResultBytes, g, "SQL Server 结果上限", "整个结果集字节上限", "33554432", 1024, 1073741824).unit("字节").
		help("超出只返回前面的行并提示；结果全部读进内存，别调太大。"))

	// ================= SQL 防火墙 =================
	g = GroupFirewall
	add(boolean(KeyFirewallEnabled, g, "全局", "防火墙总开关", "true").
		help("关闭后所有规则暂停执行，也不再记录命中。").link("SQL 防火墙", hrefFirewall))
	add(enum(KeyFirewallMode, g, "全局", "全局模式", "observe", "observe=观察（只记录不拦截）", "enforce=拦截").
		help("观察模式下规则只记录命中，不拦截。").link("SQL 防火墙", hrefFirewall))
	add(enum(KeyFirewallOnError, g, "全局", "防火墙自身出错时", "allow", "allow=放行", "deny=拒绝").
		help("防火墙求值本身出错时放行还是拒绝。"))
	add(integer(KeyFirewallExplainTimeoutMs, g, "全局", "EXPLAIN 超时", "3000", 200, 30000).unit("毫秒").
		help("“预估扫描行数”条件取事实时的限时。"))
	add(enum(KeyFirewallStoreSQL, g, "全局", "命中记录里的语句", "masked", "masked=字面量替换为 ?", "raw=保存语句原文").
		risk("raw 会在命中记录里保存语句原文，可能包含敏感数据"))

	// ================= SQL 上线与审核 =================
	g = GroupReview
	add(str(KeyReviewCriticalDDLRegex, g, "高危语句", "高危 DDL 正则", "").check(checkRegex).
		help("命中的 DDL 在提交检查时直接按高危驳回。Go（RE2）正则语法。"))
	add(boolean(KeyReviewDDLDMLSeparation, g, "高危语句", "DDL / DML 分离", "false").
		help("开启后同一工单里不允许同时有 DDL 与 DML。"))
	add(boolean(KeyReviewAutoApproveEnabled, g, "自动审批", "启用自动审批", "false").
		help("符合下面全部条件的工单不经人工审批直接通过。").
		risk("开启后符合条件的工单不经人工审批直接通过"))
	add(list(KeyReviewAutoApproveDBTypes, g, "自动审批", "适用的实例类型", "").
		help("逗号分隔，如 mysql,redis。"))
	add(list(KeyReviewAutoApproveTags, g, "自动审批", "适用的实例标签", "").
		help("逗号分隔的实例标签代码。"))
	add(str(KeyReviewAutoApproveExclRegex, g, "自动审批", "不自动通过的语句正则", "").check(checkRegex).
		help("命中的语句不自动通过；为空用内置规则（alter / create / drop / truncate / rename / delete 等）。").
		risk("放宽这个正则会让更多语句不经人工审批通过"))
	add(integer(KeyReviewAutoApproveMaxRows, g, "自动审批", "最大影响行数", "50", 0, 100000000).unit("行").
		risk("调大会让更多大批量更新不经人工审批通过"))
	add(enum(KeyReviewAutoRejectLevel, g, "自动驳回", "出现问题时自动驳回", "error",
		"warning=出现警告或错误即驳回", "error=仅出现错误时驳回", "never=不自动驳回").
		help("检查结果有问题时是否由系统直接驳回。"))
	add(boolean(KeyReviewBackupEnabled, g, "goInception 检查", "生成回滚备份", "false").
		help("关闭时 goInception 检查的工单不生成备份；仅对 goInception 来源的检查有效。"))
	add(boolean(KeyReviewExactRowCount, g, "goInception 检查", "精确统计影响行数", "false").
		help("让 goInception 精确计算影响行数（real_row_count），而不是估算。"))
	add(text(KeyReviewMSSQLRules, g, "SQL Server", "审核规则", "").check(checkJSONObject).
		help("JSON：规则名 → 级别。非法值记日志并用默认规则。"))
	add(integer(KeyReviewBatchWorkers, g, "SQL Server", "批量工单并发实例数", "4", 1, 16).
		help("只有 SQL Server 实例并发执行，其余类型严格串行。"))
	add(boolean(KeyReviewMSSQLPrintAsError, g, "SQL Server", "PRINT 按失败处理", "false").
		help("关闭：PRINT 只作为信息显示在该语句的“信息”里；开启：收到 PRINT 就按失败处理并回滚该单元。"))

	// ================= 审批与通知 =================
	g = GroupNotify
	add(boolean(KeyApprovalForbidSelf, g, "审批", "禁止审批自己提交的工单", "true").
		help("开启后提交人不能审批自己的工单。").link("审批流程", hrefFlow))
	add(list(KeyNotifyPhases, g, "通知", "通知阶段", "").check(checkCSVOf("Pass", "Execute", "Cancel")).
		help("空 = 全部阶段都通知；否则只通知列出的阶段：Pass、Execute、Cancel（逗号分隔；新工单提交不受它控制）。"))
	add(enum(KeyNotifySQLPreview, g, "通知", "通知里的 SQL 预览", "head", "head=前 500 字", "mask=隐藏字符串常量", "none=不显示").
		help("没套消息模板时，默认通知文案里怎么显示 SQL。"))
	add(list(KeyNotifyDDLExtraGroups, g, "通知", "DDL 完成后额外通知的权限组", "").
		help("逗号分隔的权限组名称。"))

	// ================= goInception =================
	g = GroupGoInc
	add(str(KeyGoInceptionHost, g, "审核引擎", "地址", "").link("审核引擎", hrefGI))
	add(integer(KeyGoInceptionPort, g, "审核引擎", "端口", "4000", 1, 65535).link("审核引擎", hrefGI))
	add(str(KeyGoInceptionUser, g, "审核引擎", "账号", "").link("审核引擎", hrefGI))
	add(secret(KeyGoInceptionPassword, g, "审核引擎", "密码").link("审核引擎", hrefGI))
	add(str(KeyGoInceptionConfigFile, g, "审核引擎", "配置文件路径", "").
		help("“审核引擎”页读写的 config.toml。").link("审核引擎", hrefGI))
	add(str(KeyGoInceptionBackupHost, g, "备份库", "地址", "").link("审核引擎", hrefGI))
	add(integer(KeyGoInceptionBackupPort, g, "备份库", "端口", "3306", 1, 65535).link("审核引擎", hrefGI))
	add(str(KeyGoInceptionBackupUser, g, "备份库", "账号", "").link("审核引擎", hrefGI))
	add(secret(KeyGoInceptionBackupPassword, g, "备份库", "密码").link("审核引擎", hrefGI))

	// ================= 数据导出 =================
	g = GroupExport
	add(enum(KeyExportStorageType, g, "存储", "导出文件存储类型", "local", "local=本地目录").
		help("目前只支持本地目录。"))
	add(str(KeyExportLocalPath, g, "存储", "本地导出目录", "downloads/DataExportFile/").check(checkExportPath).
		help("相对路径时相对环境变量 EXPORT_BASE_DIR；不能含 ..；服务账号需要对它有写权限。"))
	add(integer(KeyExportMaxRows, g, "上限", "导出行数上限", "10000", 1, 10000000).unit("行").
		help("导出工单最多导出多少行；结果全部读进内存，别调太大。"))
	add(integer(KeyExportMaxSeconds, g, "上限", "导出最长执行时间", "60", 1, 86400).unit("秒"))

	// ================= 日志解析 =================
	g = GroupBinlog
	add(integer(KeyBinlogMaxRows, g, "上限", "最多条数的上限", "500000", 1, 10000000).unit("行").
		help("“日志解析”页里“最多条数”能填的最大值。结果全部放在服务内存里，条数很多时占用也很大，按需要调，别调太大。"))
	add(integer(KeyBinlogMaxResultMB, g, "上限", "结果体积的上限", "1024", 1, 8192).unit("MB").
		help("“日志解析”页里“结果体积上限”能填的最大值。结果全部放在服务内存里，下载时还要再占一份，别调太大。"))

	// ================= 分区维护 =================
	g = GroupPartition
	add(boolean(KeyPartitionAutoRun, g, "自动维护", "启用自动维护", "false").
		help("开启后到点自动为分区表续建分区（只续建；删除靠下面的开关；REORGANIZE 与初始化永不自动）。").
		risk("开启后服务会定时在目标库上执行分区 DDL"))
	add(boolean(KeyPartitionAutoDrop, g, "自动维护", "自动删除过期分区", "false").
		help("开启后自动删除超过保留期的分区。").
		risk("过期分区里的数据会被删除，不可恢复"))
	add(integer(KeyPartitionAutoIntervalMinutes, g, "自动维护", "自动维护间隔", "180", 5, 1440).unit("分钟").
		help("间隔在空转时同样生效：从关到开最长要等一个间隔。"))
	add(enum(KeyPartitionNotifyOn, g, "通知", "通知时机", "always", "always=执行了语句或失败就推", "on_failure=只在失败时推", "never=不推"))
	add(boolean(KeyPartitionAlertEnabled, g, "可用分区告警", "启用可用分区告警", "true").
		help("可用分区数（上界晚于当前时间的分区个数，含当前分区，不含 MAXVALUE 分区）小于阈值时，在“分区维护”列表里标出，并按检查间隔向“推送设置”里启用的通道推送。检查间隔同“自动维护间隔”，自动维护没开也会检查。"))
	add(integer(KeyPartitionAlertMinAvailable, g, "可用分区告警", "全局默认阈值", "3", 1, 100).unit("个分区").
		help("可用分区数小于它就告警；单张表可以在“分区维护”页里单独设置（填 0 = 这张表不告警）。别超过某张表的“预创建数 + 1”，否则续建之后也达不到，会一直告警。"))
	add(integer(KeyPartitionAlertRepeatHours, g, "可用分区告警", "重复提醒间隔", "24", 0, 720).unit("小时").
		help("持续不足时每隔多久再提醒一次；0 = 不重复，只在首次不足时推一次。"))
	add(boolean(KeyPartitionAlertNotifyRecover, g, "可用分区告警", "恢复后通知", "true").
		help("可用分区数回到阈值以上时，推送一条“已恢复”。"))

	// ================= 数据保留 =================
	g = GroupRetention
	add(boolean(KeyRetentionEnabled, g, "总开关与节奏", "启用保留策略清理", "false").
		help("总开关，缺省关闭；各目标的保留天数没设置（或为 0）时不清理。建议先只设天数、不开总开关，用预览确认数量。").
		risk("开启后到期的数据会被物理删除，不可恢复"))
	add(str(KeyRetentionWindow, g, "总开关与节奏", "清理时间窗口", "").check(checkWindow).
		help("形如 01:00-05:00（可跨零点）；空 = 不限制。").hint("01:00-05:00"))
	add(integer(KeyRetentionIntervalMinutes, g, "总开关与节奏", "清理间隔", "60", 10, 1440).unit("分钟"))
	add(integer(KeyRetentionMaxMinutes, g, "总开关与节奏", "单次最长运行", "120", 1, 1440).unit("分钟"))
	add(integer(KeyRetentionMaxRows, g, "总开关与节奏", "单次最多清理行数", "2000000", 1000, 50000000).unit("行").
		help("每个目标每轮最多删除的行数。"))
	add(integer(KeyRetentionBatchSize, g, "总开关与节奏", "每批行数", "1000", 100, 5000).unit("行"))
	add(integer(KeyRetentionBatchSleepMs, g, "总开关与节奏", "批间隔", "200", 0, 5000).unit("毫秒"))
	add(boolean(KeyRetentionArchiveBackupEnabled, g, "总开关与节奏", "归档备份文件清理开关", "false").
		help("开启才按备份存储的“保留天数”删除超期的备份对象（总开关之外的第二道开关）。").
		risk("超期的归档备份会被删除，不可恢复"))
	days := func(target, label, help string) {
		add(integer(RetentionDaysKey(target), g, "各目标保留天数", label, "", 0, 3650).unit("天").
			help(help).risk(riskDelete))
	}
	days("query_event", "在线查询事件", "0 / 未设置 = 不清理；下限 7 天，低于下限视为配置错误，该目标跳过。")
	days("query_reject", "查询拒绝记录", "0 / 未设置 = 不清理；下限 7 天。")
	days("query_log", "查询日志", "dbt_query_log（含完整查询语句）。0 / 未设置 = 不清理；下限 7 天。")
	days("slow_logs", "慢日志", "0 / 未设置 = 不清理；下限 7 天。")
	days("audit_log", "系统审计日志", "合规数据：先确认公司审计保留期。0 / 未设置 = 不清理；下限 30 天。")
	days("archive_run", "归档运行记录", "0 / 未设置 = 不清理；下限 7 天。")
	days("export_files", "导出文件", "删除已结束导出工单的 zip 文件，工单记录保留。0 / 未设置 = 不清理；下限 7 天。")
	days("archive_backup", "归档备份", "需要同时打开“归档备份文件清理开关”。0 / 未设置 = 不清理；下限 7 天。")

	// ================= 性能工单 =================
	g = GroupPerf
	add(integer(KeyPerfCollectWindowMinutes, g, "采集", "采集窗口", "", 1, 10080).unit("分钟").
		link("性能治理 › 采集配置", hrefPerfCol))
	add(str(KeyPerfJobs, g, "后台任务", "后台任务开关", "").check(checkCSVOf("collect", "sla", "verify", "report")).
		help("逗号分隔：collect、sla、verify、report；空 = 全部关闭。每分钟重读，无需重启。").
		risk("开启后本服务会自动采集 / 建单 / 通知"))
	add(text(KeyPerfSeverityRules, g, "严重度与 SLA", "严重度规则", "").check(checkJSONObject).
		help("JSON，覆盖内置的 P0 / P1 / P2 阈值。").link("性能治理 › SLA 策略", hrefPerfSLA))
	add(list(KeyPerfDBAUsers, g, "严重度与 SLA", "性能工单 DBA 用户", "").
		help("逗号分隔的用户名。"))
	add(integer(KeyPerfVerifyDays, g, "验证", "验证观察天数", "3", 1, 90).unit("天"))
	add(str(KeyPerfVerifyImproveRatio, g, "验证", "验证改善比例", "0.5").check(checkFloat(0, 1)).
		help("0 到 1 之间的小数。"))
	add(integer(KeyPerfAutoCloseAbsentDays, g, "验证", "消失多少天后自动关闭", "14", 0, 3650).unit("天").
		help("0 = 关闭自动关闭。"))
	add(str(KeyPerfReopenMinScore, g, "验证", "重新打开的最低影响分", "20").check(checkFloat(0, 1000000)))
	add(boolean(KeyPerfAutoExplain, g, "EXPLAIN", "自动 EXPLAIN", "true"))
	add(integer(KeyPerfExplainTimeoutMs, g, "EXPLAIN", "EXPLAIN 超时", "3000", 200, 60000).unit("毫秒"))
	add(str(KeyPerfQuietHours, g, "通知", "静默时段", "22-9").check(checkQuietHours).
		help("形如 22-9（22 点到次日 9 点不发非紧急通知）。"))
	add(boolean(KeyPerfWeekendSilent, g, "通知", "周末静默", "true").
		help("周末不发 P2 / P3 通知。"))
	add(boolean(KeyPerfSilentMode, g, "通知", "静默模式", "false").
		help("开启后不发任何性能工单通知。"))
	add(boolean(KeyPerfGroupNotify, g, "通知", "按分组汇总通知", "true"))

	// ================= 性能巡检 =================
	g = GroupInspect
	add(integer(KeyInspectMySQLRetentionDays, g, "报告保留", "MySQL 巡检报告保留天数", "", 1, 3650).unit("天").
		help("不属于任何计划的报告的保留天数；未设置 = 永不清理。").risk("超期的巡检报告会被删除"))
	add(integer(KeyInspectMSSQLRetentionDays, g, "报告保留", "SQL Server 巡检报告保留天数", "", 1, 3650).unit("天").
		help("不属于任何计划的报告的保留天数；未设置 = 永不清理。").risk("超期的巡检报告会被删除"))
	add(integer(KeyInspectRedisRetentionDays, g, "报告保留", "Redis 巡检报告保留天数", "", 1, 3650).unit("天").
		help("不属于任何计划的报告的保留天数；未设置 = 永不清理。").risk("超期的巡检报告会被删除"))

	// ================= SQL 优化工具 =================
	g = GroupOptimizer
	add(str(KeyOptimizerSoarPath, g, "SOAR", "SOAR 程序路径", "").check(checkSoarPath).hint("/usr/local/bin/soar").
		help("SOAR 可执行文件的绝对路径，文件名必须是 soar。未配置时优化页提示未配置（409）。").
		risk("该路径指向服务器上会被执行的程序"))
	add(str(KeyOptimizerSoarConfig, g, "SOAR", "SOAR 配置文件", "").check(checkAbsPath).hint("/etc/soar.yaml").
		help("可选：SOAR 的 -config 配置文件（绝对路径）。").
		risk("该文件由 SOAR 读取，决定它的行为"))
	add(secret(KeyOptimizerSoarTestDSN, g, "SOAR", "SOAR 测试环境 DSN").check(checkSoarDSN).hint("user:password@host:3306/db").
		help("格式 用户:密码@主机:端口/库名。必须是非生产库（SOAR 会在其中建临时表）；加密保存，保存时会校验它不是已登记的生产实例。").
		risk("SOAR 会在该库里建表、删表"))

	return ds
}
