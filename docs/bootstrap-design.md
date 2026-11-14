# 元数据库基础脚本（bootstrap）设计

> **适用版本**：以 `deploy/RELEASE_VERSION` 为准；表 / 数据计数取自仓库里的 `deploy/bootstrap/*.sql`。
> **读者**：部署实施人员、维护基础表与基础数据的开发者。
> **文档类型**：设计说明 / 技术参考（概述、执行顺序、数据模型、校验、验证状态）。
> **相关文档**：[admin-cli.md](admin-cli.md)（`dbt-admin bootstrap` 的用法）· [charset-policy.md](charset-policy.md)（字符集策略）· [ops-manual.md](ops-manual.md)（全新部署步骤、`deploy/sql` 脚本表）。

dbtool 的元数据库基础表结构与必需的基础数据固化在仓库 `deploy/bootstrap/`，全新部署时由命令行工具 `dbt-admin` 读取执行。本系统的**所有表统一 `dbt_` 前缀**（命名规则见 3.1.1）。

> **警告**：脚本与测试已完成，但 **001 / 002 没有在真实 MySQL 上执行过**（见第 9 节）。

## 目录

- [1. 目的与边界](#1-目的与边界)
- [2. 产出文件](#2-产出文件)
- [3. 001 表结构：约定与表清单](#3-001-表结构约定与表清单)
- [4. 002 基础数据](#4-002-基础数据)
- [5. 初始化后的库里有什么](#5-初始化后的库里有什么)
- [6. 校验](#6-校验internalbootstrapsql不连任何库)
- [7. 维护 001 / 002](#7-维护-001--002)
- [8. 设计取舍备忘](#8-设计取舍备忘)
- [9. 验证状态与已知限制](#9-验证状态与已知限制)
- [10. 变更记录](#10-变更记录)

## 1. 目的与边界

| 项 | 说明 |
|---|---|
| 用途 | **只用于空库初始化**：新建一个空的 `db_tool` 库，依次执行 `001_base_schema.sql`、`002_base_data.sql`、`deploy/sql/*.sql`，得到可运行的元数据库 |
| 表名 | 表名与外键约束名都加 `dbt_` 前缀（3.1.1）；仍推荐使用独立的新库 |
| **绝不能做的事** | 不能在已经有其它来源的权限 / 权限组数据的库上执行 `002_base_data.sql`：里面的 ID 是固定值，与库里已有的权限、权限组 ID 不保证一致，会造成权限与权限组关系错位 |
| 不做的事 | 不创建用户 / 密码 / 密钥 / 实例；不写 `dbt_setting`、`dbt_workflow_audit_setting`、`dbt_resource_group` |

### 1.1 执行顺序

```
001_base_schema.sql  →  002_base_data.sql  →  deploy/sql/*.sql（按文件名升序）
```

- 002 必须在 `deploy/sql/150_builtin_groups.sql` 之前：150 按名称把 `DBA / TL / RD / QA / Default` 标成内置组，需要这些组已经存在。
  按上面的顺序执行不需要“数据导入后重跑 150”。
- 001 与 002 的所有语句都可重复执行（`CREATE TABLE IF NOT EXISTS`；INSERT 用 `ON DUPLICATE KEY UPDATE col = col` 空操作），
  重复执行不改变已有行。
- 两个文件开头都有 `SET NAMES utf8mb4`；001 用 `SET FOREIGN_KEY_CHECKS=0/1` 包裹（建表仍按外键依赖排好序，测试会校验）。
- 文件里没有 `DELIMITER`、没有存储过程 / 触发器，`dbt-admin` 的语句切分器与 mysql 客户端行为一致（测试里逐条对照过）。

## 2. 产出文件

| 文件 | 说明 |
|---|---|
| `deploy/bootstrap/001_base_schema.sql` | 48 张基础表（`dbt_` 前缀） |
| `deploy/bootstrap/002_base_data.sql` | 内容类型、权限、内置权限组及授权、实例标签 |
| `internal/bootstrapsql/*_test.go` | 校验两个文件的测试（不连数据库） |

两个文件直接维护，没有生成脚本；改动之后摘要会变，`dbt-admin` 的 `dbt_bootstrap_applied` 会据此识别“已应用的脚本被改动过”。

## 3. 001 表结构：约定与表清单

### 3.1 结构约定

1. 全部是 `CREATE TABLE IF NOT EXISTS`，不含 `DROP TABLE`、表选项 `AUTO_INCREMENT=N`、`DEFINER`、`/*!` 版本注释；
2. 表选项只有 `ENGINE=InnoDB`，可带 `COMMENT`；
3. 按外键依赖排序（被引用的表在前，同层按表名排序，输出稳定）；
4. **不指定任何字符集与校验规则**：列上的 `CHARACTER SET x` / `CHARSET x` / `COLLATE x`，表选项里的 `[DEFAULT] CHARSET=x` / `COLLATE=x` 一律不写。项目策略是所有表、所有字段都不指定，全部跟随数据库默认值（策略、理由与风险见 [charset-policy.md](charset-policy.md)）。

> 因为表不带字符集，001 建出来的表**完全取决于库的默认字符集 / 校验规则**：`dbt-admin bootstrap` 与 `status` 会先读 `information_schema.SCHEMATA`，库默认字符集不是 `utf8mb4` 时拒绝（见 [admin-cli.md](admin-cli.md) 第 4 节）。

### 3.1.1 命名规则：表名与外键约束名（R1 / R2）

本系统的所有表统一 `dbt_` 前缀，001 里写的就是最终名字；**列名、索引名不带前缀**：

| 对象 | 名字 | 说明 |
|---|---|---|
| 表名 | `dbt_` + 表名 | 如 `dbt_sql_users`、`dbt_2fa_config`、`dbt_audit_log`；`REFERENCES` 的目标表同样带前缀 |
| 内容类型表 | `dbt_content_type` | 命名规则里唯一的例外（测试里登记为例外表） |
| 外键约束名 | `dbt_` + 不带前缀的约束名 | InnoDB 的外键约束名在**同一个库内必须唯一**，加前缀避免与同库其它对象的同名约束冲突 |
| 外键约束名（超过 64 字符时） | `dbt_` + 不带前缀名的前 55 个字符 + `_` + 不带前缀名 SHA-1 的前 4 位十六进制（恰好 64） | 当前 43 个外键约束里有 3 个超长，001 文件头逐个列出了对应关系 |

规则在测试侧的镜像是 `internal/bootstrapsql/names_test.go`；`prefix_lint_test.go` 保证 001 / 002 / `deploy/sql/*.sql` 里不出现未加前缀的基础表名（见第 6 节）。`deploy/sql` 的扩展表本来就是 `dbt_*`，名字不加第二层前缀；测试断言基础表名与扩展表不重名、外键约束名与 `deploy/sql` 里的约束不冲突，名字全部唯一且不超过 64 个字符。

### 3.2 表清单（共 48 张）

“代码引用”指 `internal/**` 非测试代码里的 `TableName()`、裸 SQL 或表名常量；引用结果由 `internal/bootstrapsql/refs_test.go` 持续校验（001 里的每张表都至少被引用一次）。

| 类别 | 表 | 代码引用 |
|---|---|---|
| 身份与权限 | `dbt_sql_users`、`dbt_sql_users_groups`、`dbt_sql_users_resource_group`、`dbt_sql_users_user_permissions`、`dbt_2fa_config`、`dbt_auth_group`、`dbt_auth_group_permissions`、`dbt_auth_permission`、`dbt_content_type`、`dbt_resource_group`、`dbt_workflow_audit_setting` | authx / btnperm / store / sysadmin / server 等直接读写；权限逻辑按 `app_label + codename` 查 `dbt_auth_permission` ⨝ `dbt_content_type` |
| 实例与配置 | `dbt_sql_instance`、`dbt_sql_instance_tag`、`dbt_sql_instance_instance_tag`、`dbt_sql_instance_resource_group`、`dbt_ssh_tunnel`、`dbt_instance_account`、`dbt_instance_database`、`dbt_param_template`、`dbt_param_history` | instmgr / store / query / sshtunnel 等直接读写 |
| 查询权限 | `dbt_query_privileges`、`dbt_query_privileges_apply`、`dbt_query_log` | privapply / query / server / sysadmin / retention |
| 工单与审批 | `dbt_sql_workflow`、`dbt_sql_workflow_content`、`dbt_workflow_audit`、`dbt_workflow_audit_detail`、`dbt_workflow_log` | sqlflow / audit / review / server |
| 审计 | `dbt_audit_log` | 审计写入口，各模块都会引用 |
| 巡检 | `dbt_sql_mysql_inspection_report`、`dbt_sql_mssql_inspection_report`、`dbt_sql_mssql_inspection_item` | inspect / instmgr / archive |
| 慢日志 | `dbt_slow_logs`、`dbt_mysql_slow_query_review`、`dbt_mysql_slow_query_review_history` | slowlog / perf / retention（后两张是 pt-query-digest 推送表；不指定字符集、跟随库默认，它们的索引在 `utf8mb4` 下最长 256 字节，远小于 3072 的上限） |
| 性能工单 | `dbt_perf_issue`、`dbt_perf_issue_stat`、`dbt_perf_issue_log`、`dbt_perf_team`、`dbt_perf_team_leaders`、`dbt_perf_team_directors`、`dbt_perf_collect_config`、`dbt_perf_route`、`dbt_perf_sla_policy`、`dbt_perf_digest_cursor` | `internal/perf` |
| 分区维护 | `dbt_partman_config`、`dbt_partman_log`、`dbt_partman_webhook` | `dbt_partman_config` / `dbt_partman_log` 被 `internal/partman` 使用；`dbt_partman_webhook` 由 `internal/partman` 的推送通道管理读写 |

所有表在 001 里都只有结构，没有数据；`dbt_audit_log` / `dbt_query_log` / `dbt_workflow_log` / `dbt_partman_log` / `dbt_perf_issue_log` 等日志表同样只建空结构。

### 3.3 排除表（16 张）

001 的表结构最初由一份纯表结构导出（`mysqldump --no-data`，共 64 张表）清洗得到，之后直接在 001 里维护；其中 16 张**不进入 001**（分类取自 001 文件头与 `docs/release-notes.md` 1.31 的说明）。它们用的是未加前缀的旧名，在本系统里根本不建，所以没有对应的 `dbt_` 新名。清单由 `internal/bootstrapsql/schema_test.go` 的 `expectedExcluded` 持续校验；**改这份清单等于改取舍，必须同时改本节**。

| 类别 | 表（旧名） | 说明 |
|---|---|---|
| 框架内部表（7 张） | `django_admin_log`、`django_cas_ng_proxygrantingticket`、`django_cas_ng_sessionticket`、`django_migrations`、`django_q_ormq`、`django_q_task`、`django_session` | 属于原导出所来自的 Web 框架 / 任务队列 / 会话层，dbtool 不使用 |
| 不再使用的表（9 张） | `sql_config`、`sql_permission`、`data_masking_rules`、`data_masking_columns`、`archive_config`、`archive_log`、`cloud_access_key`、`aliyun_rds_config`、`django_q_schedule` | 对应功能已由 dbtool 自己的表取代或已取消：系统设置改用 `dbt_setting`（`deploy/sql/260_setting.sql`），脱敏改用 `dbt_masking_policy`（`230_masking_policy.sql`），数据归档改用 `dbt_archive_*`（`180_archive.sql`） |

> **注意**：`docs/release-notes.md` 在列同一批 9 张不再使用的表时，最后一项写作 `dbt_q_schedule`，与这里的 `django_q_schedule`（`schema_test.go`）不是同一个名字；以测试里的清单为准。已存在的库里若有这些旧表，保持原样，程序不再读写。

## 4. 002 基础数据

文件直接维护：内容类型 5 个、权限 57 个、内置权限组 5 个、组授权 116 行、实例标签 4 个，写入 `dbt_content_type`、`dbt_auth_permission`、`dbt_auth_group`、`dbt_auth_group_permissions`、`dbt_sql_instance_tag` 五张表。
权限码与组授权是**一次性迁移的输入**：运行时的权限只认按钮（`dbt_group_perm` / `dbt_user_perm`），空库装好后 `install.sh` 在健康检查通过之后执行 `dbt-admin perm migrate --apply --yes`，把这里的组授权按登记表展开成按钮授权。
全部是 `INSERT … ON DUPLICATE KEY UPDATE col = col`（空操作）。选择它而不是 `INSERT IGNORE`
是为了不吞掉真正的错误（外键不满足、数据过长会直接报错，而不是静默丢行）。内容类型里的 `app_label` / `model`、权限码、权限组名是**数据值**，不是表名，不随前缀改变。

### 4.1 内容类型与权限

- 只放迁移用得上的权限码（按钮登记表的 `Legacy`）：57 个，挂在 5 个内容类型下（每个内容类型一行，`app_label` + `model`）；
- `sql.permission` 上 47 个（`menu_*`、`sql_submit`、`query_submit`、`instance_account_manage` …，其中 2 个是 dbtool 定义的权限码，见 4.2）；
  `sql.instance` 3 个（`add_ / change_ / delete_instance`）、`sql.paramtemplate` 2 个（`add_ / change_paramtemplate`）、`sql.partmanconfig` 3 个（`add_ / change_ / delete_partmanconfig`）；
  `sql_perf.perfissue` 2 个（`menu_perf`、`perf_manage`）；
- 不再带 Django 的全套模型权限：admin / auth / contenttypes / sessions / django_q 的权限、其余各模型的 `add_ / change_ / delete_ / view_<model>` 四件套，
  以及 `menu_dashboard`、`menu_system`、`optimize_sqladvisor`、`query_download` 这类没有任何判断点的菜单码 / 功能码。原先的 291 个权限里有 233 个是这样的，授予了也不起作用，只会误导管理员，已去掉；
  1.81 又去掉了 `archive_review`（数据归档的审批不需要按钮，迁移时本来就丢弃）；
  `TestEveryPermissionRowHasAJudgmentPoint` 保证以后不再放进迁移用不上的权限码；
- 权限码按 `app_label + codename` 定位（`dbt_auth_permission` ⨝ `dbt_content_type`）。

**ID 固定**：内容类型与权限的 ID 都是 002 里写明的固定值；依赖 ID 数值的外部脚本不可用，dbtool 自己的代码一律按 `app_label + codename` 查询，不依赖 ID。

### 4.2 dbtool 定义的 2 个权限码

| codename | 名称 | 用途 |
|---|---|---|
| `inspection_plan_manage` | 管理定时巡检计划 | 定时巡检计划的管理权限；迁移时展开成 `inspect.plan_edit / plan_toggle / plan_run` |
| `binlog_del` | 清理实例Binlog | 迁移时展开成 Binlog 清理按钮 `instance.purge_binlog` |

两者都挂在 `sql.permission` 内容类型下，只授予 `DBA` 组。如果希望保持“只有超级用户能清理 binlog”，在 002 里删掉 `binlog_del` 的权限行与授权行，并在 `internal/bootstrapsql/data_test.go` 的 `knownAbsentLegacy` 里登记即可（测试会要求写明原因）。

### 4.3 内置权限组与授权

组名与 `deploy/sql/150_builtin_groups.sql`、`btnperm.PresetNames`（去掉非内置的“审计”）一致：`DBA`、`TL`、`RD`、`QA`、`Default`。
ID 固定：Default=1、RD=2、DBA=3、**TL=4**、QA=5。

每组授权 = **基础授权**（权限码列表，只取 `sql` / `sql_perf` 应用下唯一匹配的权限）
**+ 为迁移补充的 Legacy 权限码**（`registry.go` 的 `Legacy` 与 `presets.go` 的预设所需、但基础授权里没有的）：

| 组 | 基础授权 | 补充 | 合计 | 补充的权限码（来源按钮） |
|---|---|---|---|---|
| Default | 3 | 2 | 5 | `query_applypriv`（qpriv.list）、`query_submit`（query.run） |
| RD | 14 | 4 | 18 | `menu_partman_list`、`menu_perf`、`menu_sqlexportworkflow`、`sqlexport_submit` |
| DBA | 36 | 20 | 56 | `add_instance`、`change_instance`、`delete_instance`、`add_paramtemplate`、`change_paramtemplate`、`add_partmanconfig`、`change_partmanconfig`、`delete_partmanconfig`、`binlog_del`*、`inspection_plan_manage`*、`menu_mssqlinspection`、`menu_partman_list`、`menu_perf`、`perf_manage`、`menu_sqlexportworkflow`、`menu_sqlexportbatchworkflow`、`sqlexport_submit`、`sqlexportbatch_submit`、`sqlbatch_submit`、`offline_download` |
| TL | 17 | 7 | 24 | `archive_mgt`、`menu_instance_list`、`menu_partman_list`、`menu_perf`、`menu_sqlexportworkflow`、`query_resource_group_instance`、`sqlexport_submit` |
| QA | 7 | 6 | 13 | `menu_archive`、`menu_slowquery`、`menu_sqladvisor`、`menu_sqlexportworkflow`、`offline_download`、`sqlexport_submit` |

（* = 第 4.2 节的权限码。`menu_perf` / `perf_manage` 取 `sql_perf` 应用下的那一行。）

**迁移后的效果**（`TestGroupPermissionsCoverBtnpermPresets` 的输出）：

| 组 | 预设按钮 | 经权限码可达 | 仅能经按钮授权获得 | 缺口 | 超出预设的按钮（迁移后） |
|---|---|---|---|---|---|
| DBA | 101 | 98 | 3：`inspect.run`、`inspect.cancel`、`inspect.delete`（Super 且无 Legacy，只能在权限组的按钮授权里勾选） | 0 | 1：`sqlworkflow.execute_own`（基础授权的 `sql_execute` 与 `schedule` 等按钮同码） |
| TL | 33 | 33 | 0 | 0 | 13：`archive.run/stop/restore/delete/cfg_view`（`archive_mgt` 同码）、`sqlworkflow.execute_group/execute_own/schedule/manual_finish/osc/batch_op/cancel_run`、`optimize.soar`（基础授权的 `sql_execute*`、`optimize_soar`） |
| RD | 26 | 26 | 0 | 0 | 6：`archive.cfg_view`、`optimize.soar`、`sqlworkflow.schedule/manual_finish/osc/batch_op` |
| QA | 16 | 16 | 0 | 0 | 8：`archive.cfg_view`、`slowquery.view/advise`、`sqlworkflow.execute_own/schedule/manual_finish/osc/batch_op` |
| Default | 6 | 6 | 0 | 0 | 3：`sqlworkflow.view_own`（基础授权的 `menu_sqlworkflow`）、`cancel_own` / `rollback`（登录即可，无 `Legacy` 码） |

“超出预设”的原因只有两类：权限码比按钮粗（一个码对应多个按钮，迁移时一并展开），以及基础授权本来就比预设宽。
**需要精确到按钮时，装好之后在按钮权限页里调整**。其中 TL 的 `archive_mgt` 是为了让预设的 `archive.pause` 迁移后可用，
代价是同码的 `archive.run / stop / restore / delete` 也会展开出来；若不接受，从 002 里去掉它（`archive.pause` 就只能经按钮授权获得）。

“审计”预设不是内置组，002 不创建对应的权限组。

### 4.4 实例标签

| id | tag_code | tag_name | 依赖 |
|---|---|---|---|
| 1 | `can_write` | 可以写入 | 可写实例 |
| 2 | `can_read` | 支持查询 | `internal/privapply` 只列出带 `can_read` 的实例供申请权限 |
| 4 | `mysql_inspection` | MySQL巡检 | `internal/inspect` 定时巡检按标签圈定 MySQL 系实例 |
| 5 | `mssql_inspection` | MSSQL巡检 | `internal/inspect` 定时巡检按标签圈定 SQL Server 实例（名称是本方案取的） |

标签 ID 固定，3 号不使用（空缺），4、5 号保持不变。

### 4.5 逐处核对：代码读取却“假定已有某行”的地方

| 表 / 行 | 代码行为 | 决定 |
|---|---|---|
| `dbt_setting`（系统设置，由 `deploy/sql/260_setting.sql` 创建） | 只存显式设置过的值，缺行用登记表（`internal/settings/registry.go`）里的默认值；含连接信息 / 路径的项（goInception、SOAR 等）必须由部署方填写 | **不写**（含密钥 / 连接信息的项不能写；有默认值的不写） |
| `dbt_workflow_audit_setting` / `dbt_resource_group` | 由系统管理页面配置，没有“必须存在”的行 | 不预置 |
| `dbt_auth_group` 的 `Default` | 新用户默认入组可由登录方式或系统设置按名称配置，缺省不入组 | 作为内置组创建（见 4.3） |
| 扩展表（`sql/*.sql` 扩展表脚本建的 `dbt_*_ext` 等）里的默认行 | 各模块读取时有代码默认值（如 `btnperm.DefaultSetting()`）；由 `deploy/sql` 负责建表 | 不在本方案范围 |

## 5. 初始化后的库里有什么

依次执行 001、002、`deploy/sql/*.sql` 之后：

1. **ID**：内容类型、权限、内置权限组、实例标签的 ID 都是固定值（4.1、4.3、4.4）；
2. **权限码**：包含 dbtool 定义的 `inspection_plan_manage`、`binlog_del`（4.2）；
3. **内置权限组**：`Default`、`RD`、`DBA`、`TL`、`QA`；每组授权是“基础授权 ∪ 补充”（4.3）；
4. **实例标签**：`can_write`、`can_read`、`mysql_inspection`、`mssql_inspection`，3 号空缺（4.4）；
5. **`dbt_auth_group_permissions` 的行 ID**：002 不指定，由自增产生；
6. **不含**任何用户、系统设置项（`dbt_setting` 行）、资源组、审批流程；
7. **表名与外键约束名都带 `dbt_` 前缀**（3.1.1）：直接写这些表的外部程序（例如 `pt-query-digest` 推送脚本 `--review` / `--history` 的 `t=`）要使用带前缀的表名。

## 6. 校验（`internal/bootstrapsql`，不连任何库）

`go test -count=1 ./internal/bootstrapsql/...`：

- 001 / 002 的每条语句用 `github.com/bytebase/parser/mysql` 解析，必须无语法错误；用 `internal/dbtadmin.SplitSQL` 切分得到的语句序列与测试自己的切分器逐条一致；
- 001：全部 `CREATE TABLE IF NOT EXISTS`、无 `DROP` / `AUTO_INCREMENT=<数字>` / `DEFINER` / `/*!`；每个外键引用的表在文件中更早定义；
  外键约束名唯一、带 `dbt_` 前缀、不超过 64 个字符；表名都是 `dbt_` + 名字（或唯一的例外）、不超 64 字符、与 `deploy/sql` 的扩展表不重名；
  文件头声明的范围、例外、外键统计与被截断的外键约束名（含 SHA-1）与规则一致；
- 002：只写 5 张基础表（目标表与列都必须存在于 001）、全部幂等形式（`ON DUPLICATE KEY UPDATE col = col`）；内容类型 / 权限 ID 与 (app, model) / (内容类型, codename) 唯一，每个内容类型下至少有一个权限，权限只放 `sql` / `sql_perf` 应用下；
  组授权引用的权限 ID 与组 ID 存在、不重复、只授 `sql` / `sql_perf` 应用的权限；组名与 `150_builtin_groups.sql`、btnperm 预设一致；
  **交叉检查**：`registry.go` 的 57 个 `Legacy` 权限码全部在 `dbt_auth_permission` 数据里（应用与 `btnperm.LegacyApp` 一致），白名单为空；
  反方向：002 里的每个权限码都得被迁移用上（登记表的 `Legacy` 里有，或 `internal/**` 的代码里引用）；代码里校验的按钮码（`HasButton` / `RequireButtonAPI` / `ButtonOrFail` 的实参、`perm*` / `btn*` 常量、`*Buttons` 变量）必须都在按钮登记表里，判断点上写原权限码会被点名（原权限码只允许出现在 `internal/btnperm`）；
  每个内置组迁移后能拿到全部预设按钮（没有 `Legacy` 的按钮除外，列在日志里），且持有预设涉及页面的菜单码；
- 字符集 lint（`charset_lint_test.go`）：用 MySQL 语法树检查 001 与 `deploy/sql/*.sql` 的每条 `CREATE TABLE` / `ALTER TABLE`（含字符串里的预处理 DDL），出现任何字符集 / 校验规则即失败；
- 引用校验：扫描 `internal/**`、`cmd/**` 的 `TableName()` 与裸 SQL（约 520 个源文件，约 90 个名字），断言被引用的表 ⊆ 001 ∪ `deploy/sql` 创建的 54 张 `dbt_*` 表 ∪ 显式白名单；
  代码 SQL 里出现未加前缀的基础表名就是改名漏网，会被点名；代码里的 44 个权限码常量 / `HasPerm` 参数 / `menu_*` 字面量全部在 002 里。
- 前缀 lint（`prefix_lint_test.go`）：001 / 002 / `deploy/sql/*.sql` 里不得出现未加前缀的基础表名作为 `CREATE TABLE` / `ALTER TABLE` / `INSERT INTO` / `REFERENCES` / `FROM` / `JOIN` / `UPDATE` / `TRUNCATE` 的目标、`information_schema` 的 `table_name = '…'` 探测、被注释掉的示例语句，以及 COMMENT / 注释里的“表名.列”说明；
  检查器自己有正反例自检。数据值（notify 的 `kind` 标签、留存键、通知目标类型等恰好同形的字符串）不在检查范围。

扫描得到的“被引用而不在 001 / deploy/sql”的名字：**无**（白名单里只有 SQL 关键字、`dbt_bootstrap_applied`（由 `dbt-admin` 自己创建）、
`cmd/demo` 的演示表和导出模板占位名）。

## 7. 维护 001 / 002

1. 直接编辑 `deploy/bootstrap/*.sql`：保持 LF 行尾、幂等形式、表名与外键约束名带 `dbt_` 前缀、不指定字符集与校验规则；
2. 改了表结构后同步更新 001 文件头里声明的表数、外键统计与被截断的外键约束名（测试会校验文件头与正文一致）；
3. btnperm 登记表 / 预设变化后，按 `data_test.go` 的失败提示调整 002 里每组授权的补充权限码（并同步 4.3 的表）；
4. `go test -count=1 ./internal/bootstrapsql/...` 全绿；
5. 文件内容变化后摘要会变，`dbt-admin` 的 `dbt_bootstrap_applied` 会据此识别“已应用的脚本被改动过”。

## 8. 设计取舍备忘

- **基础授权 ∪ 补充，而不是只取预设需要的码**：内置组保留一套基础授权，再补上 dbtool 预设所需的权限码，保证预设可用。代价见 4.3 的“超出预设”。
- **002 不预置系统设置**：缺失都有代码默认值；写进去反而可能与部署方的真实配置冲突。
- **所有表统一 `dbt_` 前缀**：好处是表名统一、不与同库其它对象冲突；外键约束名同样加前缀，是因为它们在库内全局唯一，不加的话同库安装会冲突。代价是外部程序直接写这些表时（`pt-query-digest` 的 `t=`）要使用带前缀的名字。

## 9. 验证状态与已知限制

### 9.1 已验证 / 仅假后端验证 / 未验证

| 项 | 状态 | 说明 |
|---|---|---|
| 001 / 002 的语法（MySQL 语法树）、切分器一致性、表名 / 外键约束名规则、字符集 lint、前缀 lint、引用校验、权限码交叉检查 | 已验证（静态） | `go test -count=1 ./internal/bootstrapsql/...`（第 6 节），不连数据库 |
| `dbt-admin bootstrap` 的流程（前置检查、幂等重跑、遇错即停、`dbt_bootstrap_applied` 记录） | 仅假后端验证 | 内存假库与假 `database/sql` 驱动；仅验证了两边的语句切分结果一致 |
| 001 / 002 在真实 MySQL 8.0 上整体执行无报错（含 `datetime NOT NULL ON UPDATE CURRENT_TIMESTAMP` 无默认值的列在严格模式下的行为） | 未验证 | 001 不含 `utf8mb4_0900_ai_ci`，在 5.7 / MariaDB 上也不会因校验规则不存在而失败，但这同样**未在真实库验证** |
| 表按库默认建出：在 `utf8mb4` + `utf8mb4_general_ci` / `utf8mb4_0900_ai_ci` 的库上各跑一遍，核对 `information_schema.COLUMNS` 的字符集 / 校验规则 | 未验证 | — |
| 重复执行两遍不报错、行数不变 | 未验证 | — |
| `sql_mode` 含 `NO_BACKSLASH_ESCAPES` 时 002 里的转义 | 未验证 | 目前 002 的取值不含反斜杠 |
| 外键约束名加前缀后的 001：含 3 个截断到 64 字符的约束名能否整体建出 | 未验证 | 只做了静态断言 |
| 002 之后执行 `deploy/sql/*.sql`（含 150 标记内置组）的完整链路，以及用 002 的权限组登录后各页面的菜单可见性 | 未验证 | — |
| 单用户覆盖下的有效权限 | 未验证 | 本方案只覆盖迁移后权限组按钮的完备性（4.3） |
| `mssql_inspection` 标签的 `tag_name` | 未核对 | 是本方案取的名字，未与实际使用 SQL Server 巡检的部署对照 |
| `pt-query-digest` 的推送脚本、其它直接写 `dbt_mysql_slow_query_review*` 等表的外部程序 | 未验证 | 它们必须使用带 `dbt_` 前缀的表名（3.1.1） |

### 9.2 已知限制

- 002 里的 ID 是固定值，**不能**在已经有其它来源的权限 / 权限组数据的库上执行（第 1 节）。
- `binlog_del` 只授予 `DBA` 组；要保持“只有超级用户能清理 binlog”，需要在 002 里删掉它的权限行与授权行（4.2）。
- 内置组迁移后“超出预设的按钮”见 4.3：权限码比按钮粗，迁移时一个码展开成它对应的全部按钮；需要精确到按钮时在按钮权限页里调整。

## 10. 变更记录

只记录文档与 `docs/release-notes.md` 里已有的事实，不含日期。

| 版本 | 变更 |
|---|---|
| 1.20 | 去掉独立的“数据字典”页面；新建库的 `Default` 内置组不再补发 `menu_data_dictionary`（`docs/release-notes.md` “去掉独立的‘数据字典’页面（1.20）”） |
| 1.31 | 001 从 57 张表减到 **48 张**，去掉 9 张不再使用的旧表（3.3）；已存在的库不受影响。同版本去掉 `dbt-admin` 的 `bootstrap --schema-only` / `--same-database` 与旧表检测，`status --porcelain` 不再输出 `legacy_tables`（`docs/release-notes.md` “移除运行时兼容层”） |
| 1.34 | 去掉残留的“数据字典”权限码 `menu_data_dictionary` 与 `data_dictionary_export`：新装库的基础数据里不再创建；已有库里这两行与权限组授权保留但不再生效（`docs/release-notes.md` “实例列表批量设置环境（1.34）”） |
| 1.81 | 002 再去掉 `archive_review`（没有对应按钮，迁移时本来就丢弃）：权限 57 个、组授权 116 行；权限只认按钮，002 里的权限码授权只是 `dbt-admin perm migrate` 的输入（4 节开头）；`indirectPermRefs` 随之删除，判断点上出现原权限码会被 `TestPermissionCodesUsedByCodeExistInPermissionData` 点名 |
| 1.70 | 002 的权限只保留有判断点的 58 个（原 291 个，内容类型 57 → 5，组授权 149 → 118 行），权限 ID 不变；测试从“每个内容类型 4 个默认权限”改成“每个权限码都有判断点”（4.1、第 6 节；`docs/release-notes.md` “权限矩阵和基础数据里去掉没有任何判断点的权限码（1.70）”） |
