# DB Tool · 前端原型

[dbtool](../README.md) 的可交互高保真原型。**纯前端、零依赖、模拟数据**，不影响工程里任何现有代码；
结构与交互风格参考 `starrocks_ops` 原型（侧栏 + 按钮级权限 + 登录页 + 内置使用手册 + 三套主题）。

## 打开方式

- 直接双击 `index.html`（file:// 即可，手册已打包进 `js/help_content.js`）；或
- `python -m http.server 8765 --bind 127.0.0.1`（在本目录下）后访问 <http://127.0.0.1:8765/>。

先进入**登录页**（账号密码 / LDAP / 钉钉、飞书、企业微信、公司 SSO；部分账号需两步验证），页面底部「原型演示账号」下拉框可一键填充（密码 `Dba@2026`，验证码 `123456`）。
登录后顶栏右侧可**切换身份**（admin 超级用户 / dba_li DBA / tl_wang 组长 / rd_zhao 研发 / qa_sun 测试 / auditor_chen 审计），
对比不同权限组看到的菜单与按钮；右上角可切换 明亮 / 深空 / 护眼 三种主题。

## 与真实工程的对应关系

| 原型 | 真实工程 |
|---|---|
| `js/perm.js` 的 `PERM_TREE`（模块 → 页面 → 按钮码） | `internal/btnperm/registry.go` 的 `Tree`，码、显示名、高危 / 锁定标记一一对应 |
| `NAV_TREE`（侧栏分组与顺序） | `internal/server/menu.go` + 各模块 `AddMenu` |
| `GROUPS`（DBA / 组长 / 研发 / 测试 / 审计 / Default） | 权限组，同时是审批流里的“级” |
| 页面与交互 | `docs/user-guide.md`、站内帮助（`web/static/help/`）、真实页面（`web/templates/`） |

> 本原型是**侧栏式 SPA**，强调权限驱动的菜单 / 按钮、可切换身份、可多页联动（如工单 → 审批 → 通知），样式独立，便于直接用作新前端的设计稿。

## 目录

```
index.html            入口
css/style.css         样式（明亮 / 深空 / 护眼）
css/archive.css       数据归档页面专用样式（只用 style.css 的 CSS 变量，三套主题通用）
js/core.js            工具、图表、弹窗、路由、按钮动作分发
js/data.js            共享模拟数据：实例、资源组、库表结构、审计种子
js/perm.js            按钮权限树、侧栏菜单、权限组、用户、can() / B()
js/login.js           登录页、两步验证、SSO、个人菜单、主题
js/p_home.js          首页（我的待办、快捷入口、系统状态）
js/p_sql.js           SQL 上线（列表 / 提交 / 详情）、SQL 分析
js/p_query.js         在线查询、查慢日志、分区维护、数据导出、数据字典、权限申请、查询权限
js/p_opt.js           优化工具、慢查日志
js/p_redis.js         Redis 支持：巡检检查项 / 评分 / 报告详情、慢查询面板（无菜单页，由 p_inst.js / p_opt.js 调用）
js/p_redisq.js        Redis 在线查询：命令检查 / 限量改写 / 模拟键空间与执行 / 结果整形 / key 浏览 / 示例（window.RedisQuery，由 p_query.js / p_priv.js 调用）
js/p_engines.js       PostgreSQL / MongoDB / ClickHouse（第一期：在线查询 + 数据字典）：各引擎的语句检查（移植自 internal/engine 的拒绝 / 放行清单，原因文案原文）/ 模拟执行（小型 SQL 求值器、Mongo 过滤与聚合）/ 结果整形 / 库表树 / 数据字典详情 / 示例（window.EngineQuery，由 p_query.js / p_priv.js / p_inst.js 调用）
js/p_perf.js          性能工单（列表 / 详情 / 排行榜 / 采集 / 团队 / SLA）
js/p_inst.js          实例列表、会话、数据库、账号、参数配置、性能巡检
js/p_sys.js           审批流程、资源组、用户、权限组（按钮权限）
js/p_sys2.js          登录方式、安全设置、系统审计、goInception、通知渠道（钉钉 / 飞书 / Lark / Slack 机器人 + 邮件 SMTP：加密方式 / 证书校验 / 明文认证风险提示、@ 规则解析到用户邮箱并预览“将发送给谁”、没有邮箱的用户跳过、分阶段的测试发送模拟）
js/p_redisrule.js      Redis 上线规则页（仅超级用户）+ 规则引擎 window.RedisRules（SQL 上线的 Redis 命令模式调用）
js/p_archive.js       数据归档：任务 / 运行记录 / 备份存储 / 安全策略四个页签 + 六步向导 + 会走动的模拟运行引擎（见下文「数据归档」）
js/p_help.js          使用手册页；js/help_content.js 由 tools/build_help.py 自动生成
help/*.md             使用手册（唯一来源）
tools/build_help.py   生成权限码附录、help_content.js、docs/用户使用手册.md
```

修改 `help/*.md` 或 `js/perm.js` 后运行 `python tools/build_help.py`。

## PostgreSQL / MongoDB / ClickHouse（在线查询 + 数据字典，`js/p_engines.js`）

第一期这三类实例（`type` 为 `PgSQL` / `Mongo` / `ClickHouse`，种子见 `data.js` 末尾：`pg-order-prod-m`、`pg-bi-prod-m`、`mongo-order-prod`、`mongo-user-prod`、`ch-order-olap`、`ch-dw-prod`，每种各一个在订单中心、一个在别的资源组）只支持**在线查询**和**数据字典**（含权限申请里的库表选择）；规则与文案以 `internal/engine/*` 的检查器为准。

- **可见性**：`perm.js` 的 `visibleInstances(withRedis)`：不传 = 只含 MySQL 系 / SQL Server；`true` = 再加 Redis（旧页面沿用）；`'ext'` = 再加这三类（数据字典用）；`'all'` = 全带（在线查询、权限申请、实例列表用）。其它页面（SQL 上线、巡检、会话 / 账号 / 参数、慢日志、分区、导出、归档、Binlog、性能采集…）都不会出现这三类；直接按 `INSTANCES` 排除式过滤的几处（`p_perf.js` 采集配置、`p_inst.js` 巡检计划按标签匹配）已改成白名单。
- **检查器**：`PG.check` / `CH.check` / `MG.check` 逐条移植自 Go（词法、语句类型、函数 / 表函数 / 敏感对象黑名单、涉及的表抽取、`FORMAT` 去除、限量改写），已用 `internal/engine/*/check_test.go` 里的放行 / 拒绝 / 涉及的表样例做过逐条对照。
- **模拟执行**：不连任何服务；每张表按行下标确定性地生成数据（最近 3000 行铺在最近 30 天），WHERE 里的主键 / 唯一键 / 索引列等值会合成命中的行，JOIN 对索引键做“按键取行”；`count` / `sum` 等聚合按样本放大到表的量级。不支持的写法（窗口函数、`ARRAY JOIN`、`LATERAL`、递归 CTE、`$facet` 等）提示“原型未模拟”。
- **库表授权**（`p_query.js` 里的 `qCheckExt`，对应 `internal/query/auth_multi.go`）：库级 + 表级；区分大小写；PostgreSQL 表名 `schema.table`（权限里不带点视为 `public`）；`pg_catalog` / `information_schema`、ClickHouse `system` 摘要表免表权限；`SHOW` / `DESCRIBE` / `EXISTS` 是结构查询；FROM 里的自定义函数 / `db.stats()` 要求库级权限；`read_view` 标签直接拒绝。
- **演示账号**：`rd_zhao` 对 `pg-order-prod-m / order_core` 只有 `public.orders`、`public.order_items` 表权限，对 `mongo-order-prod / order_mongo` 只有 `orders` 集合，对 `ch-order-olap / olap_order` 有库级权限（`order_report` 的权限已过期）；`qa_sun` 对 `mongo-user-prod / user_mongo` 有 100 行的库级权限；`tl_wang` 有 `pg-order-prod-m / order_core` 库级权限。示例下拉里有放行、限量、超时、服务端报错和被拦截的例子。
- 不做：列脱敏（这三类没有规则来源，结果页固定提示不做脱敏）、执行计划页签、每人 3 条并发的限制。

## 开发约定（写新页面时）

- 页面 = `pages.<菜单id> = {render(sub) → HTML 字符串, init(sub)?, title?}`；路由 `#菜单id/子路径`，`sub` 是 `/` 后面的全部。
- 所有**需要权限的按钮**一律用 `B('权限码', '文本', {cls, data:{k:v}})` 渲染，点击后分发到 `ACTIONS['权限码'](dataset, el)`；
  高危按钮（kind = d）在 handler 里用 `risky(...)` 二次确认；操作结果调用 `audit(码, 描述)` 写入系统审计。
- 不需要权限的交互用 `data-on="名称"` → `ON.名称(dataset, el, event)`；表单控件用 `data-onchange` / `data-oninput`。
- 状态放在页面自己的模块级对象里（如 `const SW = {tab: 'list'}`），改完调用 `rerender()` 重绘。
- 写进 `innerHTML` 的动态字符串必须 `esc()`（原型也要养成习惯）。
- 模拟数据放在各 `p_*.js` 顶部；只有被多个页面共用的才放 `data.js`。


## 数据归档（SQL 查询 › 数据归档，`js/p_archive.js` + `css/archive.css`）

本页最初是在真实实现之前做的，现已按真实实现更新，**规则以真实实现为准**（`internal/archive` 的任务 / 审批 / 状态机 / 调度，`internal/archive/engine` 的参数范围 / 校验 / 试运行检查项 / 失败语义），手册章节 `help/archive.md`。菜单 id `archive`，页面权限 `archive.view`；共 11 个权限码（`archive.view / apply / dryrun / pause / run / stop / restore / delete / cfg_view`，以及两个锁定码 `store_edit`、`policy_edit`），与 `internal/btnperm/registry.go` 一致；默认授予见 `js/perm.js`：DBA 全部非锁定码，组长 view / apply / dryrun / pause，研发 view / apply / dryrun，测试与审计只读 view，超级用户全部。

| 页签 / 页面 | 内容 |
|---|---|
| 归档任务 | 列表（模式、备份去向、调度、状态、上次 / 下次运行、累计归档行数）、筛选与“待我审批”、运行中的实时进度；详情抽屉含配置、将执行的 SQL（读 / 写目标 / 删源）、审批进度、最近运行、变更历史 |
| 运行记录 | 每次运行一行（定时 / 手动 / 恢复 / 试运行），结果含“中断”“已跳过”；详情含批次吞吐曲线（内联 SVG）、限流事件、校验结果、备份位置与 sha256、日志；库表备份可“恢复数据”，文件备份可“下载清单”（模拟） |
| 备份存储 | 本地目录 / NFS / S3 兼容（含 MinIO）/ 阿里云 OSS（S3 兼容端点）；SK 不回显、留空不改；新建 / 编辑后“未测试”，测试连接（地址含 `10.255.` 或 `timeout` 超时，含 `refused` / `invalid` / `bad` / `403` 失败，其余成功）；被任务引用的存储不能删除、不能改类型 |
| 安全策略 | 最小保留天数、默认批量 / 休眠 / 阈值 / 等待上限 / 连续失败暂停、单次最大行数、每实例并发、受保护库表（“库”或“库.表”模式列表，内置系统库一直生效）、是否允许不备份清理；差异预览（含“放宽”风险提示）+ 二次确认 + 审计，改动立即影响向导校验 |
| `#archive/wizard` | 六步向导：来源 → 条件 → 模式与备份 → 稳定性参数 → 调度 → 试运行并提交（范围校验与引擎一致、WHERE 与 SQL 实时预览、未来 5 次调度预览、确定性的试运行结果）；新建和“草稿 / 已驳回”的任务可在最后一步“保存草稿”（不要求试运行、不建审批） |

要点：

- **全是内存假数据**：9 个种子任务（已启用 ×3：归档并删除→同实例归档库 / 文件存储 OSS(gzip + 加密) / 仅清理；待审批 ×2；已暂停 ×2：连续失败自动暂停 / 结构变更暂停；已驳回 ×1；草稿 ×1，可继续编辑后提交）、34 条运行记录（含“已跳过”“中断”“已终止”“结构变更暂停”各一条）、5 个备份存储（MinIO 内网归档状态异常、MinIO 测试环境“未测试”）。刷新页面即还原。不连真实后端，调度到点自动触发运行、多进程部署也不模拟。
- **审批**：没有单独的按钮码，沿用「审批流程配置」里资源组的“数据归档申请”流（`window.approvalFlowOf(rg, 'archive')`）；审批人 = 该级权限组中属于该资源组的用户，**不能审批自己提的（超级用户也不能）**，驳回必填原因；侧栏角标 `NAV.archive.badge` = 等我审批的数量；也会出现在首页“我的待办”里。改关键字段 / 放宽参数 / 草稿 / 已驳回 / 已停用 / 结构变更暂停后提交要重新审批；只做“更保守”的调度 / 窗口 / 限速调整直接保存。
- **模拟运行**：`立即执行` 会启动一个每 400 ms 一跳的模拟（进度条、速度、预计剩余、当前批大小、`Threads_running` / 复制延迟仪表）；约 30% 进度处会自动出现一次负载尖峰（暂停等待并计入限流事件），批耗时超目标时批量减半 / 休眠加倍；详情里的“演示”按钮可手动模拟负载升高 / 持续高负载（等待超过最长等待后以“负载中断”结束）/ 从库延迟。离开页面时定时器被清理、模拟暂停推进，回来继续；切换身份时重新渲染并重建定时器。**源表结构指纹不一致**时，立即执行不会被直接拒绝：运行先启动，很快以“结构变更暂停”结束并把任务置为“已暂停（结构变更）”；连不上源库则以“失败”结束（不自动暂停）。成功跑完的“归档并删除 / 仅清理”任务断点清空（下次从头），“仅备份”保留断点。
- **模拟时钟**：与 Binlog 解析页同口径，“此刻”是 `2026-10-07 11:20`，之后随真实时间流逝；运行默认不在执行窗口内，立即执行时需勾选“忽略执行窗口”。
- **试运行**不是随机数：由所选表在 `data.js` 里的行数、保留天数、附加条件确定性地推出（预估命中行数、占比、批数、耗时、binlog、10 行样例、各检查项）；检查项的 key / 标题 / 级别 / 顺序与引擎 `check.go` 一致（`large_batch` 命中超过 30% 是黄色“需确认”，不是红色阻断），另加服务层的 `workflow` / `concurrency` / `flow`。例如时间列没有索引（`orders.updated_at`、`users.created_at`）、被外键子表引用（`users`、`payments`）、分区表（`order_log`、`login_log`）、占表 > 30% 都会触发相应的红 / 黄项。时间列“有没有索引”用 `data.js` 里的列键标记近似。
- **与其它页面的联动**：源表有“审核通过 / 定时执行 / 执行中”的 SQL 上线工单时运行被跳过（种子里 `order_log` 有工单 #1037，可观察到）；**通知没有归档专用订阅项**，归档事件复用已有事件名（开始执行 / 执行完成 / 执行有异常 / 待处理 / 审批通过 / 被驳回），通知渠道的“测试发送”里把“消息来源”选成“数据归档”可预览；系统审计里归档动作名是“数据归档-<动作>”，归入“查询与导出”类。
- 方便自测：`window.ARCHIVE_DEMO`（任务 / 运行 / 模拟 / 策略 / 存储，以及 `dryCompute`、`normalizeExtra`、`sqlPreview`、`schedTimes`、`startBlock` 等）。


## 登录与安全设置（系统管理 › 登录与安全设置，`js/p_sys2.js` 的 `pages.sec`）

取值范围、单位、默认值、校验和文案**以真实实现为准**（`internal/authx/settings.go` 的 `DefaultSettings` / `validateSecurity`，页面 `web/templates/auth/security.html`、`web/static/js/auth/security.js`），手册章节 `help/sysadmin.md`、`help/login.md`。默认值在 `js/perm.js` 的 `SETTINGS`：

| 设置 | 范围 | 默认 | `SETTINGS` 字段 |
|---|---|---|---|
| 登录失败锁定次数 | 1–100 次 | 5 | `loginFailLock` |
| 锁定时间 | 10–86400 **秒** | 300 | `lockSeconds` |
| 密码最小长度 / 复杂度 | 6–64 位 / 字母 + 数字 + 特殊字符 | 9 / 关 | `pwdMinLen` / `pwdComplexity` |
| 会话有效期 | 5–43200 **分钟** | 300 | `sessionMinutes`（`sessionBrowserClose` 默认开） |
| JWT 有效期 | 1–720 小时 | 4 | `jwtHours`（`jwtRequireOtp` 默认关） |
| API 用户白名单 | 用户 ID 多选 | 空 = 不限制 | `apiWhitelist` |
| 强制 2FA / 自助注册 / 默认入组 / 短信 | — | 关 / 关 / 空 / 关闭 | `force2fa` / `selfRegister` / `defGroups`、`defRgs` / `smsProvider` |

要点：数字项在输入框下方提示"…必须是正整数"/"…应在 a 到 b 之间"；保存前是统一确认框（变更差异表 + 风险提示合并，按钮"我已了解风险，确认保存"）；保存后立即影响登录锁定（`login.js`）、新建用户 / 重置密码 / 自助改密的校验（`login.js` 的 `pwdCheck`，`p_sys.js` 调用）。登录**不**校验密码策略，所以演示密码 `Dba@2026`（8 位）不受最小长度 / 复杂度影响。登录失败（含被锁定、停用）一律提示"用户名或密码错误"，原因看系统审计。

## 分区维护（SQL 查询 › 分区维护，`js/p_query.js` 第五节）

已并入外部 partman 程序的能力，规则与文案以真实实现（`internal/partman`）为准（原型自己的字段命名见代码注释）：

- **SelectDB / Doris**：种子实例 `doris-dw-prod`（Doris）、`selectdb-log-prod`（SelectDB）与 `dw_ads` / `log_center` 库（`data.js` 末尾）；语句是 `ADD PARTITION p VALUES [("下界"), ("上界"))` / `DROP PARTITION`，含 MAXVALUE 拒绝（`max_partition`），未分区拒绝（`not_partitioned`），Range 读不到上界的分区不删除并给 warning；week 周期分区名取窗口最后一天。
- **初始化未分区的 MySQL 表**：`order_events` 库里的 7 张未分区表覆盖三种起点、`SELECT MIN` 超时、唯一键不含分区字段、外键、引擎不支持、列类型不一致；确认要键入 `库.表`，需要 `partman.run` + `partman.delete`，定时维护不会初始化。勾选弹窗里的“演示”可模拟预览之后表被改（409 `confirm_required`）。
- **时区**：输入框 + 常用项，空 = 跟随目标库会话时区，支持 IANA / `UTC+8` / `GMT+08:00` / `+0800` / `+8`，超过 20 个字符拒绝，有夏令时给 warning。
- **推送设置**：列表右上角（`partman.edit`），Slack / 企业微信 / 飞书 / 钉钉；地址脱敏、密钥只显示“已设置”、测试发送逐通道出结果；推送时机 `partman_notify_on` 只读。
- **运行输出**：维护日志里每条记录可展开原程序风格的步骤输出（级别过滤 / 搜索 / 复制 / 下载 .log / 超 200 行截断），预览弹窗里有“检查过程”，定时一轮有摘要（跳过原因）。
