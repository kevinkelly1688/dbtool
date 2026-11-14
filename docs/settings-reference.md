# 系统设置参考

> 本文由 `internal/settings/registry.go` 生成，**不要手改**（改完登记表后运行 `UPDATE_DOCS=1 go test ./internal/settings -run TestReferenceDocUpToDate`）。
> 设计与规则见 [settings-design.md](settings-design.md)；页面入口：系统管理 › 系统配置（仅超级用户）；命令行：`dbt-admin settings`。

## 登录与安全（`security`）

登录锁定、密码与会话、两步验证、自助注册、页面水印

| 键 | 名称 | 类型 | 默认值 | 取值 | 说明 |
|---|---|---|---|---|---|
| `security.login_lock_attempts` | 连续失败锁定次数 | int | 5 | 1 ~ 1000 | 连续登录失败达到这个次数后锁定账号。 |
| `security.login_lock_seconds` | 锁定时长 | int | 300 | 1 ~ 2592000 秒 | 账号被锁定后多少秒内不能登录。 |
| `security.password_min_length` | 密码最短长度 | int | 9 | 6 ~ 64 位 | 新密码至少多少位（含修改、重置与新建用户）。 |
| `security.password_complexity` | 密码复杂度 | bool | false |  | 开启后新密码必须同时包含字母、数字与特殊字符。 |
| `security.session_minutes` | 会话有效期 | int | 300 | 5 ~ 43200 分钟 | 登录会话多久不活动后失效。 |
| `security.session_close_on_exit` | 关闭浏览器即退出 | bool | true |  | 会话 Cookie 不带过期时间，关闭浏览器后需要重新登录。 |
| `security.jwt_hours` | API 令牌有效期 | int | 4 | 1 ~ 720 小时 |  |
| `security.api_jwt_require_otp` | 申请 API 令牌需两步验证 | bool | false |  |  |
| `security.api_user_ids` | 允许使用 API 的用户 ID | list | （空） |  | 逗号分隔的用户 ID；为空表示不限制。 |
| `security.require_2fa` | 强制两步验证 | bool | false |  | 开启后用户登录必须完成两步验证。 |
| `security.external_2fa` | 外部登录也要两步验证 | bool | false |  | 通过 LDAP / OIDC 等外部登录方式进入的用户，是否仍要完成平台的两步验证。 |
| `security.self_signup` | 允许自助注册 | bool | false |  | 登录页是否显示注册入口。 |
| `security.default_auth_groups` | 新用户默认权限组 | list | （空） |  | 新用户默认加入的权限组名称，逗号分隔（登录方式自己配置了就用它的）。 |
| `security.default_resource_groups` | 新用户默认资源组 | list | （空） |  | 新用户默认加入的资源组名称，逗号分隔（登录方式自己配置了就用它的）。 |
| `security.local_login_form` | 本地账号表单 | enum | collapse | `always` `collapse` `hidden` | 配置了外部登录方式后，登录页上本地账号表单的显示方式。 |
| `security.external_match_by` | 外部账号匹配依据 | list | username,email |  | 外部账号首次登录时按哪些字段匹配已有用户，可选 username、email、third_id、phone，逗号分隔；为空则只认已绑定的外部身份。 |
| `security.superuser_local_only` | 超级用户只能本地登录 | bool | true |  | 开启后超级用户不能通过外部登录方式进入。 |
| `security.allow_self_bind` | 允许用户自行绑定外部账号 | bool | false |  | 允许用户在“外部账号绑定”页自行绑定 / 解绑。 |
| `security.backfill_ids` | 回填外部账号唯一 ID | bool | true |  | 按用户名等依据匹配成功后，记下外部唯一 ID，之后即使外部用户名变化也认这个绑定。 |
| `security.local_last_login` | 本地账号最近登录时间 | string | （空） |  | 内部状态，页面不显示。 |
| `security.watermark_enabled` | 启用页面水印 | bool | false |  | 在页面上叠加姓名、账号和时间，用于截图溯源。 |
| `security.watermark_text` | 水印内容 | string | {姓名}（{账号}）{时间} |  | 可用变量：{账号} {姓名} {部门} {IP} {日期} {时间}；必须包含 {账号} 或 {姓名}，最长 60 个字符。 |
| `security.watermark_extra` | 附加文字 | string | （空） |  | 附加在水印后面的固定文字，最长 40 个字符。 |
| `security.watermark_opacity` | 透明度 | int | 14 | 3 ~ 30 % |  |
| `security.watermark_font_size` | 字号 | int | 14 | 12 ~ 28 px |  |
| `security.watermark_angle` | 倾斜角度 | int | -22 | -60 ~ 0 度 |  |
| `security.watermark_gap` | 疏密 | enum | normal | `tight` `normal` `loose` |  |
| `security.watermark_color` | 颜色 | enum | auto | `auto` `gray` `red` `blue` |  |

## 短信（`sms`）

短信两步验证使用的短信服务商与密钥

| 键 | 名称 | 类型 | 默认值 | 取值 | 说明 |
|---|---|---|---|---|---|
| `sms.provider` | 短信服务商 | enum | disabled | `disabled` `aliyun` `tencent` | 两步验证的短信通道；未启用时用户不能绑定短信验证。 |
| `sms.aliyun_access_key_id` | AccessKey ID | string | （空） |  |  |
| `sms.aliyun_access_key_secret` | AccessKey Secret | secret | — |  |  |
| `sms.aliyun_sign_name` | 短信签名 | string | （空） |  |  |
| `sms.aliyun_template_code` | 短信模板 Code | string | （空） |  |  |
| `sms.aliyun_variable_name` | 模板变量名 | string | code |  | 模板里放验证码的变量名。 |
| `sms.tencent_secret_id` | SecretId | string | （空） |  |  |
| `sms.tencent_secret_key` | SecretKey | secret | — |  |  |
| `sms.tencent_sign_name` | 短信签名 | string | （空） |  |  |
| `sms.tencent_template_id` | 短信模板 ID | string | （空） |  |  |
| `sms.tencent_sdk_appid` | SDKAppID | string | （空） |  |  |

## 在线查询与脱敏（`query`）

查询超时、行数 / 字节上限、结果脱敏

| 键 | 名称 | 类型 | 默认值 | 取值 | 说明 |
|---|---|---|---|---|---|
| `query.mask_enabled` | 查询结果脱敏总开关 | bool | false |  | 关闭后在线查询返回原值，脱敏规则暂不生效。 |
| `query.block_on_mask_error` | 脱敏出错时拦截结果 | bool | false |  | 开启：脱敏出错时不返回数据；关闭：放行未脱敏的结果并提示。 ⚠ 关闭后脱敏出错时，查询结果会以未脱敏状态返回给用户 |
| `export.mask_enabled` | 导出工单脱敏 | bool | true |  | SQL 导出工单是否按脱敏规则处理；关闭后导出原值。 ⚠ 关闭后导出文件里是未脱敏的原值 |
| `query.forbid_select_star` | 禁止 select * | bool | false |  | 开启后在线查询拒绝 select *。 |
| `query.max_execution_seconds` | 查询超时 | int | 60 | 1 ~ 3600 秒 | 超时后发 KILL QUERY 终止这条查询。 |
| `query.admin_row_limit` | 管理员查询行数上限 | int | 5000 | 1 ~ 1000000 行 | 超级用户 / 有全实例查询权限 / 资源组实例查询时的行数上限。 |
| `query.max_cell_bytes` | 单元格字节上限 | int | 4194304 | 1024 ~ 268435456 字节 | 超出的单元格被截断并提示；生成脚本类的查询一个格子就是几百 KB，别调太小。 |
| `query.max_result_bytes` | 整个结果集字节上限 | int | 33554432 | 1024 ~ 1073741824 字节 | 超出只返回前面的行并提示；结果全部读进内存，别调太大。 |

## SQL 防火墙（`firewall`）

总开关、全局模式与出错策略（规则在“SQL 防火墙”页面维护）

| 键 | 名称 | 类型 | 默认值 | 取值 | 说明 |
|---|---|---|---|---|---|
| `firewall.enabled` | 防火墙总开关 | bool | true |  | 关闭后所有规则暂停执行，也不再记录命中。 |
| `firewall.mode` | 全局模式 | enum | observe | `observe` `enforce` | 观察模式下规则只记录命中，不拦截。 |
| `firewall.on_error` | 防火墙自身出错时 | enum | allow | `allow` `deny` | 防火墙求值本身出错时放行还是拒绝。 |
| `firewall.explain_timeout_ms` | EXPLAIN 超时 | int | 3000 | 200 ~ 30000 毫秒 | “预估扫描行数”条件取事实时的限时。 |
| `firewall.store_sql` | 命中记录里的语句 | enum | masked | `masked` `raw` | ⚠ raw 会在命中记录里保存语句原文，可能包含敏感数据 |

## SQL 上线与审核（`review`）

高危 DDL、自动审批与驳回、备份、批量工单并发

| 键 | 名称 | 类型 | 默认值 | 取值 | 说明 |
|---|---|---|---|---|---|
| `review.critical_ddl_regex` | 高危 DDL 正则 | string | （空） |  | 命中的 DDL 在提交检查时直接按高危驳回。Go（RE2）正则语法。 |
| `review.ddl_dml_separation` | DDL / DML 分离 | bool | false |  | 开启后同一工单里不允许同时有 DDL 与 DML。 |
| `review.auto_approve_enabled` | 启用自动审批 | bool | false |  | 符合下面全部条件的工单不经人工审批直接通过。 ⚠ 开启后符合条件的工单不经人工审批直接通过 |
| `review.auto_approve_db_types` | 适用的实例类型 | list | （空） |  | 逗号分隔，如 mysql,redis。 |
| `review.auto_approve_tags` | 适用的实例标签 | list | （空） |  | 逗号分隔的实例标签代码。 |
| `review.auto_approve_exclude_regex` | 不自动通过的语句正则 | string | （空） |  | 命中的语句不自动通过；为空用内置规则（alter / create / drop / truncate / rename / delete 等）。 ⚠ 放宽这个正则会让更多语句不经人工审批通过 |
| `review.auto_approve_max_rows` | 最大影响行数 | int | 50 | 0 ~ 100000000 行 | ⚠ 调大会让更多大批量更新不经人工审批通过 |
| `review.auto_reject_level` | 出现问题时自动驳回 | enum | error | `warning` `error` `never` | 检查结果有问题时是否由系统直接驳回。 |
| `review.backup_enabled` | 生成回滚备份 | bool | false |  | 关闭时 goInception 检查的工单不生成备份；仅对 goInception 来源的检查有效。 |
| `review.exact_row_count` | 精确统计影响行数 | bool | false |  | 让 goInception 精确计算影响行数（real_row_count），而不是估算。 |
| `review.mssql_rules` | 审核规则 | text | （空） |  | JSON：规则名 → 级别。非法值记日志并用默认规则。 |
| `review.batch_parallel_workers` | 批量工单并发实例数 | int | 4 | 1 ~ 16 | 只有 SQL Server 实例并发执行，其余类型严格串行。 |
| `review.mssql_print_as_error` | PRINT 按失败处理 | bool | false |  | 关闭：PRINT 只作为信息显示在该语句的“信息”里；开启：收到 PRINT 就按失败处理并回滚该单元。 |

## 审批与通知（`notify`）

审批约束与通知内容

| 键 | 名称 | 类型 | 默认值 | 取值 | 说明 |
|---|---|---|---|---|---|
| `approval.forbid_self_approve` | 禁止审批自己提交的工单 | bool | true |  | 开启后提交人不能审批自己的工单。 |
| `notify.phases` | 通知阶段 | list | （空） |  | 空 = 全部阶段都通知；否则只通知列出的阶段：Pass、Execute、Cancel（逗号分隔；新工单提交不受它控制）。 |
| `notify.sql_preview` | 通知里的 SQL 预览 | enum | head | `head` `mask` `none` | 没套消息模板时，默认通知文案里怎么显示 SQL。 |
| `notify.ddl_extra_groups` | DDL 完成后额外通知的权限组 | list | （空） |  | 逗号分隔的权限组名称。 |

## goInception（`goinception`）

审核引擎与备份库的连接

| 键 | 名称 | 类型 | 默认值 | 取值 | 说明 |
|---|---|---|---|---|---|
| `goinception.host` | 地址 | string | （空） |  |  |
| `goinception.port` | 端口 | int | 4000 | 1 ~ 65535 |  |
| `goinception.user` | 账号 | string | （空） |  |  |
| `goinception.password` | 密码 | secret | — |  |  |
| `goinception.config_file` | 配置文件路径 | string | （空） |  | “审核引擎”页读写的 config.toml。 |
| `goinception.backup_host` | 地址 | string | （空） |  |  |
| `goinception.backup_port` | 端口 | int | 3306 | 1 ~ 65535 |  |
| `goinception.backup_user` | 账号 | string | （空） |  |  |
| `goinception.backup_password` | 密码 | secret | — |  |  |

## 数据导出（`export`）

导出文件的存放位置与上限

| 键 | 名称 | 类型 | 默认值 | 取值 | 说明 |
|---|---|---|---|---|---|
| `export.storage_type` | 导出文件存储类型 | enum | local | `local` | 目前只支持本地目录。 |
| `export.local_path` | 本地导出目录 | string | downloads/DataExportFile/ |  | 相对路径时相对环境变量 EXPORT_BASE_DIR；不能含 ..；服务账号需要对它有写权限。 |
| `export.max_rows` | 导出行数上限 | int | 10000 | 1 ~ 10000000 行 | 导出工单最多导出多少行；结果全部读进内存，别调太大。 |
| `export.max_seconds` | 导出最长执行时间 | int | 60 | 1 ~ 86400 秒 |  |

## 日志解析（`binlog`）

Binlog 解析 / 回滚结果的条数与体积上限

| 键 | 名称 | 类型 | 默认值 | 取值 | 说明 |
|---|---|---|---|---|---|
| `binlog.max_rows` | 最多条数的上限 | int | 500000 | 1 ~ 10000000 行 | “日志解析”页里“最多条数”能填的最大值。结果全部放在服务内存里，条数很多时占用也很大，按需要调，别调太大。 |
| `binlog.max_result_mb` | 结果体积的上限 | int | 1024 | 1 ~ 8192 MB | “日志解析”页里“结果体积上限”能填的最大值。结果全部放在服务内存里，下载时还要再占一份，别调太大。 |

## 分区维护（`partition`）

分区表自动续建与过期删除

| 键 | 名称 | 类型 | 默认值 | 取值 | 说明 |
|---|---|---|---|---|---|
| `partition.auto_run` | 启用自动维护 | bool | false |  | 开启后到点自动为分区表续建分区（只续建；删除靠下面的开关；REORGANIZE 与初始化永不自动）。 ⚠ 开启后服务会定时在目标库上执行分区 DDL |
| `partition.auto_drop` | 自动删除过期分区 | bool | false |  | 开启后自动删除超过保留期的分区。 ⚠ 过期分区里的数据会被删除，不可恢复 |
| `partition.auto_interval_minutes` | 自动维护间隔 | int | 180 | 5 ~ 1440 分钟 | 间隔在空转时同样生效：从关到开最长要等一个间隔。 |
| `partition.notify_on` | 通知时机 | enum | always | `always` `on_failure` `never` |  |
| `partition.alert_enabled` | 启用可用分区告警 | bool | true |  | 可用分区数（上界晚于当前时间的分区个数，含当前分区，不含 MAXVALUE 分区）小于阈值时，在“分区维护”列表里标出，并按检查间隔向“推送设置”里启用的通道推送。检查间隔同“自动维护间隔”，自动维护没开也会检查。 |
| `partition.alert_min_available` | 全局默认阈值 | int | 3 | 1 ~ 100 个分区 | 可用分区数小于它就告警；单张表可以在“分区维护”页里单独设置（填 0 = 这张表不告警）。别超过某张表的“预创建数 + 1”，否则续建之后也达不到，会一直告警。 |
| `partition.alert_repeat_hours` | 重复提醒间隔 | int | 24 | 0 ~ 720 小时 | 持续不足时每隔多久再提醒一次；0 = 不重复，只在首次不足时推一次。 |
| `partition.alert_notify_recover` | 恢复后通知 | bool | true |  | 可用分区数回到阈值以上时，推送一条“已恢复”。 |

## 数据保留（`retention`）

各类日志 / 记录 / 文件的保留与清理节奏

| 键 | 名称 | 类型 | 默认值 | 取值 | 说明 |
|---|---|---|---|---|---|
| `retention.enabled` | 启用保留策略清理 | bool | false |  | 总开关，缺省关闭；各目标的保留天数没设置（或为 0）时不清理。建议先只设天数、不开总开关，用预览确认数量。 ⚠ 开启后到期的数据会被物理删除，不可恢复 |
| `retention.window` | 清理时间窗口 | string | （空） |  | 形如 01:00-05:00（可跨零点）；空 = 不限制。 |
| `retention.interval_minutes` | 清理间隔 | int | 60 | 10 ~ 1440 分钟 |  |
| `retention.max_minutes` | 单次最长运行 | int | 120 | 1 ~ 1440 分钟 |  |
| `retention.max_rows` | 单次最多清理行数 | int | 2000000 | 1000 ~ 50000000 行 | 每个目标每轮最多删除的行数。 |
| `retention.batch_size` | 每批行数 | int | 1000 | 100 ~ 5000 行 |  |
| `retention.batch_sleep_ms` | 批间隔 | int | 200 | 0 ~ 5000 毫秒 |  |
| `retention.archive_backup_enabled` | 归档备份文件清理开关 | bool | false |  | 开启才按备份存储的“保留天数”删除超期的备份对象（总开关之外的第二道开关）。 ⚠ 超期的归档备份会被删除，不可恢复 |
| `retention.query_event_days` | 在线查询事件 | int | （空） | 0 ~ 3650 天 | 0 / 未设置 = 不清理；下限 7 天，低于下限视为配置错误，该目标跳过。 ⚠ 到期数据会被物理删除，不可恢复（开启“启用保留策略清理”后才会执行） |
| `retention.query_reject_days` | 查询拒绝记录 | int | （空） | 0 ~ 3650 天 | 0 / 未设置 = 不清理；下限 7 天。 ⚠ 到期数据会被物理删除，不可恢复（开启“启用保留策略清理”后才会执行） |
| `retention.query_log_days` | 查询日志 | int | （空） | 0 ~ 3650 天 | dbt_query_log（含完整查询语句）。0 / 未设置 = 不清理；下限 7 天。 ⚠ 到期数据会被物理删除，不可恢复（开启“启用保留策略清理”后才会执行） |
| `retention.slow_logs_days` | 慢日志 | int | （空） | 0 ~ 3650 天 | 0 / 未设置 = 不清理；下限 7 天。 ⚠ 到期数据会被物理删除，不可恢复（开启“启用保留策略清理”后才会执行） |
| `retention.audit_log_days` | 系统审计日志 | int | （空） | 0 ~ 3650 天 | 合规数据：先确认公司审计保留期。0 / 未设置 = 不清理；下限 30 天。 ⚠ 到期数据会被物理删除，不可恢复（开启“启用保留策略清理”后才会执行） |
| `retention.archive_run_days` | 归档运行记录 | int | （空） | 0 ~ 3650 天 | 0 / 未设置 = 不清理；下限 7 天。 ⚠ 到期数据会被物理删除，不可恢复（开启“启用保留策略清理”后才会执行） |
| `retention.export_files_days` | 导出文件 | int | （空） | 0 ~ 3650 天 | 删除已结束导出工单的 zip 文件，工单记录保留。0 / 未设置 = 不清理；下限 7 天。 ⚠ 到期数据会被物理删除，不可恢复（开启“启用保留策略清理”后才会执行） |
| `retention.archive_backup_days` | 归档备份 | int | （空） | 0 ~ 3650 天 | 需要同时打开“归档备份文件清理开关”。0 / 未设置 = 不清理；下限 7 天。 ⚠ 到期数据会被物理删除，不可恢复（开启“启用保留策略清理”后才会执行） |

## 性能工单（`perf`）

采集、严重度、验证与通知

| 键 | 名称 | 类型 | 默认值 | 取值 | 说明 |
|---|---|---|---|---|---|
| `perf.collect_window_minutes` | 采集窗口 | int | （空） | 1 ~ 10080 分钟 |  |
| `perf.jobs` | 后台任务开关 | string | （空） |  | 逗号分隔：collect、sla、verify、report；空 = 全部关闭。每分钟重读，无需重启。 ⚠ 开启后本服务会自动采集 / 建单 / 通知 |
| `perf.severity_rules` | 严重度规则 | text | （空） |  | JSON，覆盖内置的 P0 / P1 / P2 阈值。 |
| `perf.dba_users` | 性能工单 DBA 用户 | list | （空） |  | 逗号分隔的用户名。 |
| `perf.verify_days` | 验证观察天数 | int | 3 | 1 ~ 90 天 |  |
| `perf.verify_improve_ratio` | 验证改善比例 | string | 0.5 |  | 0 到 1 之间的小数。 |
| `perf.auto_close_absent_days` | 消失多少天后自动关闭 | int | 14 | 0 ~ 3650 天 | 0 = 关闭自动关闭。 |
| `perf.reopen_min_score` | 重新打开的最低影响分 | string | 20 |  |  |
| `perf.auto_explain` | 自动 EXPLAIN | bool | true |  |  |
| `perf.explain_timeout_ms` | EXPLAIN 超时 | int | 3000 | 200 ~ 60000 毫秒 |  |
| `perf.quiet_hours` | 静默时段 | string | 22-9 |  | 形如 22-9（22 点到次日 9 点不发非紧急通知）。 |
| `perf.weekend_silent` | 周末静默 | bool | true |  | 周末不发 P2 / P3 通知。 |
| `perf.silent_mode` | 静默模式 | bool | false |  | 开启后不发任何性能工单通知。 |
| `perf.group_notify` | 按分组汇总通知 | bool | true |  |  |

## 性能巡检（`inspect`）

巡检报告保留与旧定时

| 键 | 名称 | 类型 | 默认值 | 取值 | 说明 |
|---|---|---|---|---|---|
| `inspect.mysql_retention_days` | MySQL 巡检报告保留天数 | int | （空） | 1 ~ 3650 天 | 不属于任何计划的报告的保留天数；未设置 = 永不清理。 ⚠ 超期的巡检报告会被删除 |
| `inspect.mssql_retention_days` | SQL Server 巡检报告保留天数 | int | （空） | 1 ~ 3650 天 | 不属于任何计划的报告的保留天数；未设置 = 永不清理。 ⚠ 超期的巡检报告会被删除 |
| `inspect.redis_retention_days` | Redis 巡检报告保留天数 | int | （空） | 1 ~ 3650 天 | 不属于任何计划的报告的保留天数；未设置 = 永不清理。 ⚠ 超期的巡检报告会被删除 |

## SQL 优化工具（`optimizer`）

SOAR 程序、配置文件与测试环境（仅超级用户可改，保存时会校验文件与测试库地址）

| 键 | 名称 | 类型 | 默认值 | 取值 | 说明 |
|---|---|---|---|---|---|
| `optimizer.soar_path` | SOAR 程序路径 | string | （空） |  | SOAR 可执行文件的绝对路径，文件名必须是 soar。未配置时优化页提示未配置（409）。 ⚠ 该路径指向服务器上会被执行的程序 |
| `optimizer.soar_config` | SOAR 配置文件 | string | （空） |  | 可选：SOAR 的 -config 配置文件（绝对路径）。 ⚠ 该文件由 SOAR 读取，决定它的行为 |
| `optimizer.soar_test_dsn` | SOAR 测试环境 DSN | secret | — |  | 格式 用户:密码@主机:端口/库名。必须是非生产库（SOAR 会在其中建临时表）；加密保存，保存时会校验它不是已登记的生产实例。 ⚠ SOAR 会在该库里建表、删表 |

