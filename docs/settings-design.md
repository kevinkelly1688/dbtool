# 系统设置（settings）设计

> **适用版本**：1.31 起（设置体系重建）；以 `deploy/RELEASE_VERSION` 为准。
> **读者**：研发（新增 / 修改设置项、模块接入）、实施与运维（理解设置的存储、生效与审计）。
> **文档类型**：设计说明 / 技术参考（概述、模型、接口、机制、安全、验证状态）。
> **相关文档**：[settings-reference.md](settings-reference.md)（全部设置键、默认值、取值范围；由登记表生成，不手改）· [admin-cli.md](admin-cli.md)（`dbt-admin settings`）· [ops-manual.md](ops-manual.md) 5.6（运维入口）· [release-notes.md](release-notes.md)（1.31 “系统设置重建”）。

**状态**：已实现。目标：一套统一的系统设置——统一登记、类型化、带默认值与校验、变更有记录、模块通过同一个服务读取。

## 目录

- [1. 概述与目标](#1-概述与目标)
- [2. 总体架构与处理流程](#2-总体架构与处理流程)
- [3. 数据模型](#3-数据模型)
- [4. 接口参考](#4-接口参考)
- [5. 关键机制](#5-关键机制)
- [6. 安全设计](#6-安全设计)
- [7. 配置项（领域分组）](#7-配置项领域分组)
- [8. 模块接入](#8-模块接入)
- [9. 可观测性](#9-可观测性)
- [10. 兼容性](#10-兼容性)
- [11. 验证状态与已知限制](#11-验证状态与已知限制)
- [12. 变更记录](#12-变更记录)

## 1. 概述与目标

- **统一登记**：每个设置在登记表里有键、类型、默认值、分组、中文名、说明与取值范围，默认值只写一次；
- **类型化读取**：模块通过同一个服务读取，按类型取值（字符串 / 布尔 / 整数 / 浮点 / 列表），带缓存；
- **带校验的修改**：类型 / 范围 / 枚举校验，敏感值加密，每次修改有变更记录与系统审计；
- **页面按分组展示**：显示默认值、是否已改、取值范围与说明；有专门页面的功能在设置中心给出跳转链接。

非目标：不存领域数据（规则、策略、渠道、登录方式等保存在各自的表里，见第 7 节末尾）；不是进程启动配置（监听端口、`DATABASE_URL`、密钥等在环境变量与 `dbtool.toml` 里，见 [ops-manual.md](ops-manual.md) 第 5 章）。

## 2. 总体架构与处理流程

```
  页面 /settings/config ──► sysconfig（接口，仅超级用户）──┐
  dbt-admin settings ───────────────────────────────────────┼──► settings.Service ──► dbt_setting / dbt_setting_history
  各业务模块（Lookup / Value / String / Bool / Int …）──────┘            （校验 · 加密 · 缓存 · 变更记录 · 审计）
```

### 2.1 登记（`internal/settings/registry.go`）

每个设置一个 `Def`：键、类型、默认值、分组、中文名、说明、取值范围 / 枚举 / 校验函数、是否敏感、是否高危、是否只读（程序路径类）、是否隐藏（运行状态）。**默认值只在这里写一次**，页面、模块读取、文档都以它为准；`settings-reference.md` 由它生成（`UPDATE_DOCS=1 go test ./internal/settings -run TestReferenceDocUpToDate`），测试会在文档与登记表不一致时失败。

键名规则：`<领域>.<名称>`，全小写下划线，如 `security.login_lock_attempts`、`query.max_execution_seconds`、`retention.enabled`。Go 里一律通过 `settings.KeyXxx` 常量引用，不出现裸字符串。分组 key 与键前缀并不总是一致：例如 `export.mask_enabled` 属于 `query` 分组，`approval.forbid_self_approve` 属于 `notify` 分组。以 `_internal.` 开头的键保留给服务内部状态，不能通过 `Set` 修改。

登记在启动时自检：键 / 分组 / 名称 / 类型不能为空，键不能重复，枚举必须有取值，**默认值必须通过自己的校验**；任何一项不满足都会在启动 / 测试时直接 panic。

### 2.2 服务（`internal/settings/service.go`）

| 方法 | 作用 |
|---|---|
| `Lookup(key) (string, bool)` | 显式值（敏感项已解密）；未设置 / 空白 / 密文解不开返回 `false`，即各模块的 `ConfigValue(item) (string, bool)` 约定 |
| `Value(key) (string, bool)` | 生效值：显式值，否则登记的默认值；两者都为空白返回 `false` |
| `String / Bool / Int / Float / List(key)` | 按类型取生效值 |
| `Items()` | 全部登记项（隐藏项除外）及当前值、是否修改、版本、最后修改人 |
| `Set(actor, key, value)` / `SetMany(actor, map)` / `Reset(actor, key)` | 修改：统一做类型 / 范围 / 枚举校验、敏感值加密，同一个事务写 `dbt_setting`、`dbt_setting_history` 与系统审计（`dbt_audit_log`）；值为空串 = 恢复默认（敏感项 = 清除） |
| `Prepare(by, map)` + `settings.WriteTx(tx, writes, hist)` | 供模块在**自己的事务**里写（同时写自己的审计）：`Prepare` 只校验并生成写入指令；提交后调用 `Invalidate()`。不写系统审计，也不检查 `Locked` / `Hidden` |
| `Open(db, cipher)` | 返回元数据库上的共享服务（同一个 `*gorm.DB` 只创建一个），各模块的 store 层一行接入 |
| `Ready()` / `UndecryptableKeys()` / `Invalidate()` | 表是否存在 / 密文解不开的键 / 立即让缓存失效 |

### 2.3 页面（平台设置 › 系统配置，`/settings/config`，仅超级用户）

- 左侧分组导航（带“已修改”计数）+ 搜索；右侧按分组、分区展示表单：开关 / 数字 / 下拉 / 文本 / 列表 / JSON，显示默认值、当前值是否被改过、取值范围、说明；
- 保存是“按分组一次保存所有改动”：先列出变更清单（旧 → 新），高危项要求键入项名确认；
- 敏感项只显示“已设置”，留空 = 不修改，提供“清除”；
- 每项有“恢复默认”和“变更记录”；顶部有全局“变更记录”；
- 有专门页面的功能（防火墙规则、登录方式等）保留原页面，设置中心里给出跳转链接，两边写同一份数据；**goInception 分组不在设置中心显示**（`settings.OffPage`），连接信息、备份库与配置文件只在“goInception 配置”页维护，`dbt-admin settings` 仍可读写 `goinception.*`。

## 3. 数据模型

DDL：`deploy/sql/260_setting.sql`（幂等）。

**`dbt_setting`**：只存**显式设置过**的值；没有行 = 用登记的默认值。

| 列 | 类型 | 含义 |
|---|---|---|
| `skey` | `VARCHAR(100)` 主键 | 设置键 |
| `svalue` | `MEDIUMTEXT` | 值；`encrypted=1` 时是 mirage 密文 |
| `encrypted` | `TINYINT(1)` | 敏感项加密保存 |
| `version` | `INT UNSIGNED` | 每修改一次加 1 |
| `updated_by` | `VARCHAR(64)` | 最后修改人；空 = 系统 |
| `update_time` | `DATETIME` | 最后修改时间 |

**`dbt_setting_history`**：每次修改一行；敏感项 `old_value` / `new_value` 为空，只记“改过”（`is_secret=1`）。

| 列 | 含义 |
|---|---|
| `id` | 自增主键 |
| `skey` | 设置键 |
| `old_value` / `new_value` | 旧值 / 新值（敏感项为空） |
| `is_secret` | 是否敏感项 |
| `action` | `set`（页面 / 接口修改）、`reset`（恢复默认）、`cli`（`dbt-admin settings` 写入时的来源）；列注释里还写着旧的 `import`，已不再产生 |
| `changed_by` / `change_time` | 修改人 / 时间 |

索引：`idx_key_time (skey, change_time)`、`idx_time (change_time)`。

不在 `dbt_setting` 里的：领域数据不是“系统参数”，保存在各自的表里——`dbt_gi_setting`（goInception 部署状态与操作锁）、`dbt_perf_setting`（升级矩阵 JSON，在性能工单页面编辑）、`dbt_perm_setting`（无权限按钮的显示方式，在按钮权限页面编辑；另有“是否已迁移”的标记）、各类规则 / 策略 / 渠道 / 登录方式表。

## 4. 接口参考

### 4.1 HTTP（`internal/sysconfig/http.go`）

全部仅超级用户（页面路由 `SuperuserPage`，接口 `SuperuserAPI`）。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/settings/config` | 页面 |
| GET | `/api/v1/settings/config` | 全部分组与设置项（敏感项只告诉“已设置”，不回显内容），外加 `ready`（`dbt_setting` 是否存在）、`security`（`public_secret_key`：`SECRET_KEY` 是公开缺省值；`separate_mirage_key`）、`undecryptable`（密文解不开的键） |
| PUT | `/api/v1/settings/config` | 批量修改，请求体 `{"changes": {"键": "值" \| null}}`；全部校验通过才写，一个事务；一次最多 200 项，请求体上限 512 KiB |
| PUT | `/api/v1/settings/config/:key` | 修改一项，请求体 `{"value": "…"}`（空串 / `null` = 恢复默认）；请求体上限 64 KiB |
| GET | `/api/v1/settings/config/history` | 变更记录（新的在前），参数 `key`、`limit`（缺省 50，最大 200）、`offset` |

修改接口的响应：`{"changes": […], "changed": true|false}`；值没变的项不写库、不记变更。

### 4.2 错误码

| HTTP | `code` | 含义 |
|---|---|---|
| 400 | `bad_request` | 请求体格式错误 / 一次修改项数为 0 |
| 400 | `bad_value` | 有项的值不合法；逐项原因在 `data.fields`（只有一项出错时 `message` 是“键：说明”） |
| 400 | `too_many` | 一次超过 200 项 |
| 404 | `not_found` | 没有这个设置项（含历史查询里不存在的键） |
| 409 | `table_missing` | `dbt_setting` 表不存在：先执行 `deploy/sql/260_setting.sql` |
| 500 | `internal` | 其它错误，细节在服务日志 |

### 4.3 命令行

`dbt-admin settings list | get | set | reset`（含内部状态项，需 `--force`，替代“直接改库”），详见 [admin-cli.md](admin-cli.md) §6.3。

## 5. 关键机制

- **缓存**：进程内快照，3 秒过期（`settings.DefaultTTL`），本进程写入后立即失效；多实例部署最多延迟 3 秒生效。
- **类型与校验**：值一律以字符串存储；`Normalize` 按类型（bool / int / float / string / text / enum / list / secret）、范围（`Min`~`Max`）、枚举取值与登记的专门校验函数（正则、JSON 对象、取值集合等）校验。登记的默认值必须通过自己的校验。
- **只读与隐藏**：`Locked`（页面里不能改、`dbt-admin settings set --force` 可改的机制；目前没有登记项使用它，SOAR 路径类曾使用，现改为页面可改加环境校验，见下）；`Hidden`（运行状态，如 `security.local_last_login`）不在页面显示、不能在页面修改，也不记变更记录。
- **高危项**：`Risk` 非空的项在页面保存时要求键入项名确认；`dbt-admin settings set / reset` 演练输出里会打印高危提示。
- **只写有变化的**：新值等于当前值（敏感项除外）时不写库、不记变更；恢复默认 = 删除行。
- **事务**：设置值、变更记录、系统审计在同一个事务里，要么都写要么都不写。

## 6. 安全设计

- **访问控制**：页面与接口仅超级用户；命令行靠 `DATABASE_URL` 与操作系统访问控制。
- **敏感项**：`secret` 类型的值用当前 mirage 密钥（`MIRAGE_SECRET_KEY`，缺省 `SECRET_KEY`）加密后存入，页面与接口只显示“已设置”，不回显；变更记录与审计里不记内容，只记“已更新 / 已清除”。命令行修改敏感项只能从环境变量 `DBT_SETTING_VALUE` 取值，不接受命令行参数；当前密钥是公开缺省值时拒绝写入敏感项。
- **密文解不开**：用另一把密钥保存的敏感项会被当成未设置；服务启动日志列出这些设置键，页面列出 `undecryptable`；用 `dbt-admin check-secrets` 核对，必要时 `reencrypt`。
- **指向服务器程序 / 文件 / 外部库的项**（`optimizer.soar_path` / `soar_config` / `soar_test_dsn`）页面可改，但加了四道约束，避免页面被滥用时让服务执行任意程序、在生产库里建表：① 写法：`Def.Check`——路径必须是绝对路径且不含 `..`，程序文件名必须是 `soar`，测试库 DSN 要能解析出主机端口；② 环境核对：`settings.RegisterGuard`（由 sqlopt 登记，页面 / 接口保存时执行，命令行 `--force` 跳过）——程序与配置文件必须存在、是普通文件（不是符号链接 / 目录）、程序有执行权限、两者都不能被“其他用户”改写（文件与所在目录），配置文件 ≤ 1MB；测试库地址不能是已登记的生产实例（含已停用的；主机名先解析成 IP 再比对），实例没标环境时也拒绝；③ 高危确认：三项都标了 `Risk`，保存时要键入确认文本；④ 审计：变更记录与系统审计照常记录（测试库 DSN 含密码，只记“已更新”）。只按地址比对，经代理 / 别名 / SSH 隧道连到生产库的认不出来，由超级用户把关。
- **输入限制**：批量修改最多 200 项、请求体 512 KiB；正则类设置按 Go（RE2）语法校验。

## 7. 配置项（领域分组）

完整清单、默认值与取值范围以 [settings-reference.md](settings-reference.md) 为准，这里只列分组：

| 领域前缀 | 内容 |
|---|---|
| `security.*` | 登录锁定、两步验证、注册、水印、密码策略、会话 / JWT、登录表单、外部账号匹配 |
| `sms.*` | 短信服务商与阿里云 / 腾讯云参数 |
| `query.*` | 在线查询：脱敏开关、超时、行数 / 字节上限（另含 `export.mask_enabled`） |
| `firewall.*` | SQL 防火墙总开关、模式、出错策略 |
| `review.*` | SQL 上线审核：高危 DDL、自动审批、备份、SQL Server 规则 |
| `approval.*` / `notify.*` | 审批与通知 |
| `goinception.*` | goInception 与备份库连接（页面“系统配置”不显示） |
| `export.*` | 数据导出 |
| `binlog.*` | 日志解析（Binlog 解析 / 回滚）：“最多条数”“结果体积上限”能填的最大值 |
| `partition.*` | 分区维护 |
| `retention.*` | 数据保留 |
| `perf.*` | 性能工单 |
| `inspect.*` | 性能巡检报告保留 |
| `optimizer.*` | SOAR 程序路径、配置文件与测试环境（只读） |

## 8. 模块接入

各模块经一个很小的接口读配置（`ConfigValue(item) (string, bool)`），实现为 `settings.Open(db, cipher).Lookup(键)` / `Value(键)`；键名一律用 `settings.KeyXxx` 常量。写入处（防火墙总开关、脱敏总开关、goInception 连接、登录与安全页、性能工单配置）使用 `Prepare` + `settings.WriteTx` 在自己的事务里写，并带各自的审计；提交后调用 `Invalidate()`。

## 9. 可观测性

| 位置 | 内容 |
|---|---|
| 系统审计（`dbt_audit_log`） | 动作 `修改系统设置`（页面 / 接口），正文含“键（名称）：旧 → 新”，敏感项只记“已更新 / 已清除（内容不记录）”，超长截断；命令行写入时动作为 `CLI-修改系统设置`，操作人 `dbt-admin(<系统用户>)`；模块在自己的事务里写时带各自的审计动作 |
| 变更记录（`dbt_setting_history`） | 每次修改一行，见第 3 节；页面“变更记录”与 `GET /api/v1/settings/config/history` 读它 |
| 启动日志 | 敏感项密文解不开时列出设置键（`dbt_setting` 里有敏感项的密文用当前 `SECRET_KEY` / `MIRAGE_*` 解不开） |
| `/readyz` | 必需表包含 `dbt_setting`；缺 `dbt_setting_history` 只算降级 |

## 10. 兼容性

- 键名是 `<领域>.<名称>`；旧版 `dbt_sql_config` / `dbt_auth_setting` 的键名全部改了，不再被读取，设置导入（含 `dbt-admin settings import`）已在 1.31 移除，升级说明见 [release-notes.md](release-notes.md) 的 1.31 条目（其中只给了几个“旧键 → 新键”的例子，没有完整对照表）。
- 外部脚本 / 手工 SQL 里按旧键名读写旧表的，改用 `dbt-admin settings`，或直接读写 `dbt_setting`。
- 升级：`install.sh upgrade` 默认执行 `sql/*.sql`（含建 `dbt_setting` 的 `260_setting.sql`）；缺表时所有设置按默认值运行，`/readyz` 返回 503。

## 11. 验证状态与已知限制

| 项 | 状态 | 说明 |
|---|---|---|
| 登记自检、校验、缓存、修改与变更记录、参考文档同步 | 已验证（单元测试） | `internal/settings`（含 `TestReferenceDocUpToDate`）、`internal/sysconfig`、`internal/dbtadmin` |
| 真实 MySQL 上的建表、`INSERT … ON DUPLICATE KEY` 写入与多进程缓存延迟 | 未验证 | 只在内存假库 / 假驱动与演示服务上验证过设置服务、页面与命令行（`docs/release-notes.md` 1.31 说明） |
| 页面交互（保存清单、高危确认、变更记录抽屉） | 未在本文逐项核对 | 描述取自原设计文档与前端脚本 `web/static/js/sysconfig/main.js` |

已知限制：

- `Prepare` 不检查 `Locked` / `Hidden`，只有 `SetMany`（页面 / 接口 / 命令行）检查；模块自己写入时由模块负责写哪些键。
- 多实例部署各进程最多相差 3 秒读到新值。
- 变更记录没有自动清理策略（`dbt_setting_history` 不在保留策略清理的目标里）。

## 12. 变更记录

只记录文档里已有的事实，不含日期。

| 版本 | 变更 |
|---|---|
| 1.29 | 新增只读的“平台设置 › 系统配置”页，列出旧配置表的全部项（`docs/release-notes.md`） |
| 1.30 | 系统配置页可以修改配置项（`docs/release-notes.md`） |
| 1.31 | 设置体系重建：新的 `dbt_setting`、登记表、设置中心与 `dbt-admin settings`；移除对旧表 `dbt_sql_config` / `dbt_auth_setting` 的导入 |
| 1.33 | “系统配置”页不再显示 goInception 分组；连接信息仍在“goInception 配置”页维护（`docs/release-notes.md`） |
