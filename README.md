# dbtool

> **适用版本**：以 `deploy/RELEASE_VERSION` / 发布包内 `VERSION` 为准
> **读者**：研发、实施与运维人员、首次接触本项目的人
> **文档类型**：项目总览
> **相关文档**：[用户手册](docs/user-guide.md)、[安装部署指南](docs/ops-manual.md)、[部署与运维指南](docs/deployment.md)、[发布说明](docs/release-notes.md)、[文档索引](docs/index.md)

## 目录

- [1. 产品定位](#1-产品定位)
- [2. 能力矩阵](#2-能力矩阵)
- [3. 数据库类型支持概览](#3-数据库类型支持概览)
- [4. 总体架构](#4-总体架构)
- [5. 目录结构](#5-目录结构)
- [6. 快速开始](#6-快速开始)
- [7. 配置概要](#7-配置概要)
- [8. 接口入口](#8-接口入口)
- [9. 文档地图](#9-文档地图)
- [10. 验证状态与已知限制](#10-验证状态与已知限制)
- [11. 注意事项](#11-注意事项)

## 1. 产品定位

dbtool 是数据库运维管理平台，面向 DBA 与研发团队：把在线查询、SQL 上线审核与审批、查询权限申请、数据导出与归档、慢日志与 SQL 优化、性能工单、实例与账号管理放在同一套按**按钮粒度**控制的权限体系和审批流之下。

- **服务端渲染**：Go `html/template` + 原生 JavaScript + ACE 编辑器，没有前后端分离，也没有 Node 构建；模板与静态资源用 `go:embed` 打进二进制；
- **独立的 MySQL 元数据库**：dbtool 自己的库，所有表带 `dbt_` 前缀；
- **界面外壳**：左侧导航栏 + 顶栏 + 首页（工作台），登录页为左右分栏；三套皮肤（深空 / 明亮 / 护眼）共用 `--dt-*` 变量；默认中文，顶栏可切换英文；
- **审核引擎**：SQL 检查与执行优先使用 goInception，未配置时降级为较弱的原生检查（生产环境建议设置 `REQUIRE_GOINCEPTION=true`）。

## 2. 能力矩阵

| 能力 | 菜单位置 | 支持的实例类型 |
|---|---|---|
| 在线查询（只读、库表级权限、脱敏、SET 脚本、收藏与历史、对象信息） | 数据服务 › 在线查询 | MySQL 协议系、SQL Server、Redis、PostgreSQL、MongoDB、ClickHouse |
| 查询权限申请与审批、权限管理 | 数据服务 › 权限申请 / 授权清单 | 同在线查询 |
| SQL 上线工单（检查、多级审批、立即 / 定时 / 手工执行、回滚、在线改表、批量） | 变更发布 › 变更工单 | MySQL 协议系、SQL Server、Redis（命令 + 管理员规则） |
| 数据导出工单 | 数据服务 › 数据导出 | MySQL 协议系、SQL Server |
| SQL 体检 / 智能调优 | 变更发布 › SQL 体检；性能优化 › 智能调优 | MySQL 协议系（外部工具仅 MySQL / PolarDB / PolarDB-X） |
| 慢日志（慢日志检索 / 慢 SQL 洞察） | 性能优化 › 慢日志检索；性能优化 › 慢 SQL 洞察 | MySQL / PolarDB / PolarDB-X / SelectDB / Doris；Redis SLOWLOG |
| 性能工单 | 性能治理 | MySQL 协议系 |
| 分区维护 | 数据服务 › 分区维护 | MySQL / PolarDB、SelectDB / Doris |
| 数据归档 | 数据服务 › 数据归档 | MySQL / MariaDB / PolarDB / PolarDB-X 主库（PolarDB-X 为兼容降级，未在真实实例验证） |
| 实例管理（列表、会话、数据库、账号、参数） | 实例运维 | MySQL / PolarDB / PolarDB-X（SelectDB / Doris / ADB 部分） |
| 性能巡检与定时巡检 | 实例运维 › 性能巡检 | MySQL 协议系、SQL Server、Redis |
| Binlog 解析 / 回滚 | 实例运维 › 日志解析 | MySQL |
| 数据脱敏 | 安全合规 › 数据脱敏 | 除 Redis 外的全部查询引擎 |
| SQL 防火墙 | 安全合规 › SQL 防火墙 | 在线查询：MySQL 协议系、SQL Server、PostgreSQL、ClickHouse；SQL 工单与数据导出：MySQL 协议系、SQL Server（预估扫描行数只有 MySQL / PolarDB / ADB 取得到） |
| 环境管理 | 平台设置 › 环境设置 | 与引擎无关（元数据库） |
| DBA 值班表（班次、临时调班，顶栏显示当前值班人） | 平台设置 › 值班表 | 与引擎无关（元数据库） |
| 管理员页面（审批流程、资源组、用户、权限组、审计日志、系统配置） | 访问控制 / 安全合规 / 平台设置 | — |
| 按钮级权限 | 权限组管理 › 按钮权限 | — |
| 登录方式与安全（LDAP / OIDC / 钉钉 / CAS / 飞书 / Lark / Slack / 企业微信、两步验证、水印） | 访问控制 › 登录认证 / 安全策略 | — |
| goInception 配置（连接、`config.toml` 在线编辑、重启） | 平台设置 › 审核引擎 | — |
| 通知渠道（钉钉 / 企业微信 / 飞书 / Lark / Slack / Telegram / 邮件）与消息模板 | 平台设置 › 通知渠道 | — |
| 数据保留清理 | 平台设置 › 系统配置 | — |

> 提示：数据字典功能已去掉；在线查询里只保留“对象信息”和“可编程对象”（`internal/dict`）。

## 3. 数据库类型支持概览

✔ 支持，部分 = 只支持其中一部分能力，✖ 不支持。逐项矩阵与“部分”的含义见 [用户手册第 2 章](docs/user-guide.md)。

| 实例类型 | 在线查询 | SQL 上线 | 数据导出 | 数据脱敏 | 性能巡检 | 实例管理 |
|---|---|---|---|---|---|---|
| MySQL / PolarDB | ✔ | ✔ | ✔ | ✔（goInception 语法树，精确） | ✔ | ✔ |
| PolarDB-X | ✔ | ✔ | ✔ | ✔（按列名保守匹配） | ✔ | ✔ |
| SelectDB / Doris | ✔ | ✔ | ✔ | ✔（按列名保守匹配） | ✔ | 部分 |
| ADB | ✔ | ✔ | ✔ | ✔（goInception 语法树） | ✔ | 部分 |
| SQL Server | ✔ | ✔（含批量） | ✔ | ✔ | ✔ | 仅实例列表 |
| Redis | ✔（只读命令） | ✔（命令 + 规则） | ✖ | ✖ | ✔ | 仅实例列表 |
| PostgreSQL / MongoDB / ClickHouse | ✔ | ✖ | ✖ | ✔ | ✖ | 仅实例列表 |

> 注意：PolarDB-X 的数据脱敏不使用 goInception 语法树，视同 SelectDB / Doris 完全按结果列名与表达式引用保守匹配（宁多勿漏）。查询权限的表级校验只对类型为 MySQL 的实例精确到表，其余类型按库校验。

## 4. 总体架构

```
 浏览器（服务端渲染页面 + 原生 JS + ACE）
        │  HTTPS（建议经 nginx 反向代理）
        ▼
 ┌───────────────────────────── dbtool 进程（cmd/server）────────────────────────────┐
 │  gin 路由：页面 / JSON 接口 / 静态资源（go:embed）                                  │
 │  认证：会话 Cookie + CSRF；JWT（脚本）；登录方式（LDAP / OIDC / …）与两步验证      │
 │  业务模块：query  sqlflow  privapply  audit  sysadmin  btnperm  authx  …           │
 │  后台任务（进程内）：工单定时调度、慢日志采集、性能工单任务、分区维护、              │
 │                      数据归档、定时巡检、数据保留清理                                │
 └───────┬──────────────────┬──────────────────┬────────────────────┬─────────────────┘
         │                  │                  │                    │
         ▼                  ▼                  ▼                    ▼
   元数据库（MySQL，     目标库（被管理的     goInception          通知渠道（钉钉 / 企业微信 /
   dbt_ 前缀的表）      数据库实例，经引擎    （Docker，SQL 审核    飞书 / Lark / Slack /
                         接口与可选 SSH 隧道）  与执行引擎）       Telegram / 邮件）
```

- **元数据库**：用户、权限、资源组、实例、工单、审计、系统设置（`dbt_setting`）都存在这里；表结构以 `deploy/bootstrap`（48 张基础表）与 `deploy/sql`（60 张扩展表，按编号执行、可重复执行）为准，不做 AutoMigrate；
- **目标库访问**：`internal/engine` 定义统一引擎接口，`engine/mysql`（含 PolarDB / PolarDB-X / SelectDB / Doris / ADB）、`mssql`、`redis`、`postgres`、`mongo`、`clickhouse` 各自实现；配置了 SSH 隧道的实例经本地转发器连接；
- **审批**：`internal/audit` 是通用多级审批引擎，查询权限申请、SQL 上线与数据导出、数据归档共用；
- **密钥**：实例账号密码、登录方式与通知渠道的密钥用 `SECRET_KEY` / `MIRAGE_*` 加密后入库，不回显。

## 5. 目录结构

```
cmd/server/          服务入口
cmd/dbt-admin/       初始化库、创建 / 重置管理员、生成密钥、系统设置命令行（见 docs/admin-cli.md）
cmd/demo/            预览服务：内存假数据，不连任何数据库，仅用于本地预览界面
cmd/dbt-i18n/        界面多语言开发辅助命令（不随产品发布）
internal/config/     环境变量与 dbtool.toml 配置
internal/crypto/     字段加密（mirage）、pbkdf2 密码校验
internal/model/      数据表映射（GORM，不做 AutoMigrate）
internal/store/      元数据库访问
internal/settings/   系统设置登记表、校验与存储（dbt_setting）
internal/sysconfig/  平台设置 › 系统配置页面
internal/engine/     数据库引擎接口与注册表；mysql / mssql / redis / postgres / mongo / clickhouse 各一个子包
internal/sqlx/       轻量 SQL 词法工具（切分语句、去注释、表提取、SELECT * 判断、LIMIT 改写）
internal/tsql/       T-SQL 解析、执行单元切分、参考 Bytebase 的上线规则
internal/query/      在线查询流水线（检查、授权、防火墙、脱敏、执行、审计）
internal/queryfav/   在线查询收藏
internal/masking/    脱敏规则与保守匹配；internal/maskadmin/ 脱敏管理页
internal/firewall/   SQL 防火墙内核；internal/firewalladmin/ 防火墙管理页
internal/audit/      通用审批引擎（多级审批、自动通过 / 驳回）与审计分类
internal/privapply/  查询权限申请、审批与授予
internal/sqlflow/    SQL 上线与导出工单（检查、审批、立即 / 定时 / 手工执行、回滚、在线改表、批量、取消执行）
internal/export/     导出文件生成（CSV / JSON / XML / XLSX / SQL，zip）
internal/redisrule/  Redis 工单的管理员规则与管理页
internal/review/     检查 / 执行结果的数据结构
internal/goinception/ goInception 客户端（检查、执行、备份回滚、OSC、语法树）
internal/giconfig/   goInception 配置页（连接信息、config.toml 在线编辑、重启）
internal/notify/     通知：钉钉 / 企业微信 / 飞书 / Lark / Slack / Telegram / 邮件，@ 规则、签名、消息模板
internal/server/     路由、会话 / CSRF / JWT、页面与 HTML 片段处理器、菜单与模块接入点（menu.go、ext.go）
internal/sysadmin/   系统管理：资源组、用户、权限组、审批流程配置、系统审计
internal/btnperm/    按钮级权限（登记表 + 解析器 + 一次性迁移）
internal/authx/      登录方式、登录安全、两步验证、页面水印
internal/instmgr/    实例管理：实例列表、会话、数据库、账号、参数；internal/instext/ 实例旁路扩展
internal/inspect/    性能巡检与定时巡检
internal/binlog/     Binlog 解析与回滚（原生解析，只读）
internal/sqlopt/ slowlog/ partman/ archive/ perf/   SQL 分析与优化、慢日志、分区维护、数据归档、性能工单
internal/retention/  数据保留清理
internal/dict/       库表对象信息（在线查询的“对象信息”“可编程对象”）
internal/sshtunnel/  SSH 隧道本地转发器
internal/i18n/ localize/   界面多语言目录与响应本地化
internal/dbtadmin/   dbt-admin 的实现；internal/bootstrapsql/ 基础脚本的测试
web/                 模板与静态资源（go:embed 打进二进制）；web/static/help 是站内使用文档章节
configs/ deploy/ docs/ scripts/ prototype/
```

## 6. 快速开始

需要 Go 1.25+。

**1. 先看界面（不需要数据库）**

```bash
go run ./cmd/demo          # http://127.0.0.1:8000（环境变量 LISTEN 可改），全是假数据
```

演示账号：`admin` / `admin123`（管理员）、`dev` / `dev123`（申请人）、`lead` / `lead123`（一级审批：组长）、`dba` / `dba123`（二级审批：DBA，也有执行权限）。

**2. 连接真实的元数据库**（先按 [安装部署指南](docs/ops-manual.md) 8.1 建好带 `dbt_` 前缀的结构；建议先用演练库）

```bash
cp configs/env.example .env   # DATABASE_URL 改成 dbtool 自己的库（如 db_tool）；SECRET_KEY 换成随机值（`dbt-admin gen-secrets` 生成）
export $(grep -v '^#' .env | xargs)
go run ./cmd/server                       # 默认监听 :8000
# 改端口：复制 configs/dbtool.example.toml 为 dbtool.toml，改 [server] port（环境变量 LISTEN 优先）；
# 也可显式指定：go run ./cmd/server -config /path/to/dbtool.toml
```

**3. 部署到 CentOS 服务器（一键部署脚本）**

```bash
make dist                                   # 生成 dist/dbtool-<版本>-linux-amd64.tar.gz 与 .sha256（版本号取 deploy/RELEASE_VERSION，成功后自动 +0.01）；arm64 用 make dist ARCH=arm64
scp dist/dbtool-*-linux-amd64.tar.gz dist/dbtool-*-linux-amd64.sha256 root@服务器:/root/ && ssh root@服务器
sha256sum -c dbtool-*-linux-amd64.sha256 && tar xzf dbtool-*-linux-amd64.tar.gz && cd dbtool
# 全新部署（空库）：用户 / 目录 / 密钥 / 配置 / 基础表与数据 / 管理员 / systemd / 防火墙 / 启动 / 健康检查，一条命令
read -rs DBT_ADMIN_PASSWORD; export DBT_ADMIN_PASSWORD
./install.sh install --gen-secrets --bootstrap --admin-user admin \
  --database-url 'mysql://用户:URL编码的密码@主机:3306/db_tool' --public-url https://dba.example.com --open-firewall
# 之后：upgrade（升级，失败自动回滚）/ rollback / uninstall [--purge] / status / doctor / init-db / reconfigure；--dry-run 只看计划
/opt/dbtool/current/install.sh --help     # 全部子命令与选项
```

**反复运行不会影响已存在的系统**：已安装主机再次运行 `install` 等价于升级，配置、密钥、管理员、数据目录与数据库里的数据都不动；`uninstall` 默认保留配置与数据目录且绝不触碰数据库。**升级前请先读 [发布说明](docs/release-notes.md)（含行为变化清单）**。用法速查、设计与不变量见 [docs/one-click-deploy.md](docs/one-click-deploy.md)，全新部署的完整选项见 [安装部署指南](docs/ops-manual.md) 4.1，升级 / 回滚 / 卸载见 [docs/one-click-deploy.md](docs/one-click-deploy.md)。

**4. 构建与测试**

```bash
make test     # 运行测试
```

```bash
make build    # 构建 bin/dbtool
```

> Windows 提示：若 `go` 报 `invalid GOTOOLCHAIN`，检查 `go env -w` 写入的 GOTOOLCHAIN 值（应为 `go1.25.x` 或 `local`），
> 临时可在命令前设置环境变量 `GOTOOLCHAIN=local`。

## 7. 配置概要

部署相关、不含密钥的设置放 `dbtool.toml`（查找顺序见 `configs/dbtool.example.toml`），密钥与连接串走环境变量（`configs/env.example`），业务开关在“平台设置 › 系统配置”（存在元数据库 `dbt_setting`，键与默认值见 [系统设置参考](docs/settings-reference.md)）。常用环境变量：

| 变量 | 作用 | 说明 |
|---|---|---|
| `DATABASE_URL` | 元数据库连接串 | `mysql://用户:密码@主机:3306/库名` |
| `SECRET_KEY` | 会话 / 令牌签名密钥 | 生产必须换成随机值，之后不要再改 |
| `MIRAGE_SECRET_KEY` / `MIRAGE_CIPHER_MODE` / `MIRAGE_CIPHER_IV` | 密文加密密钥与模式 | 建议单独设置，一经使用不要再改 |
| `LISTEN` | 监听地址 | 优先于配置文件 |
| `PUBLIC_URL` | 对外访问地址 | SSO 回调与通知里的工单链接 |
| `REQUIRE_GOINCEPTION` | 强制使用 goInception | 未配置时库表解析、检查与执行拒绝，不降级 |
| `BATCH_DB_TYPES` | 允许批量上线的实例类型 | 逗号分隔，默认只有 `mssql` |
| `EXPORT_BASE_DIR` | 导出文件基准目录 | `export.local_path` 为相对路径时相对于它 |
| `GOINCEPTION_CONFIG_FILE` | goInception `config.toml` 路径 | 页面读写它，优先于页面设置 |
| `SLOWLOG_COLLECT_ENABLE` | 慢日志采集总开关 | 默认开启，设为 `0` 整体关闭 |
| `DEFAULT_LANG` | 界面默认语言 | `zh-CN`（默认）或 `en` |

完整环境变量与示例见 [安装部署指南](docs/ops-manual.md) 第 5 章。

## 8. 接口入口

API 鉴权：浏览器会话（非 GET 需 `X-CSRF-Token`）或 `Authorization: Bearer <JWT>`；各接口还按权限码 / 按钮码校验。主要入口如下：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET / POST | `/login`、`POST /logout` | 页面登录（密码哈希 pbkdf2_sha256，Session Cookie + CSRF，失败限流与锁定） |
| POST | `/api/v1/auth/login` | 脚本换取 JWT |
| GET | `/healthz`、`/readyz` | 健康检查；就绪探针（元数据库可达 + 核心表存在，失败 503） |
| GET | `/query` | 在线查询工作台 |
| GET | `/ui/query/{databases,tree,policy}` | 局部刷新用的 HTML 片段 |
| GET | `/api/v1/query/instances[/:id/databases[/:db/tables[/:table/columns]]]` | 实例 / 库 / 表 / 字段 |
| POST | `/api/v1/query/execute` | 执行查询（命中防火墙“要求原因”时需带 `reason`） |
| GET | `/api/v1/query/history`、`/api/v1/query/history/users` | 执行历史（后者仅管理员） |
| GET | `/query/applies[/:id]`、`/query/privileges` | 权限申请与查询权限页面 |
| GET / POST | `/api/v1/query/privilege-applies[/:id[/audit` 或 `/abort]]` | 申请的 JSON 接口 |
| GET / PATCH / DELETE | `/api/v1/query/privileges[/:id]` | 已授予的权限 |
| GET | `/sqlworkflow`、`/sqlworkflow/new`、`/sqlworkflow/export/new`、`/sqlworkflow/:id` | 工单列表、提交页、导出提交页、详情 |
| GET / POST | `/api/v1/sqlworkflow[/:id]`、`/check`、`/options` | 列表、提交、检查预览、提交页选项 |
| POST | `/api/v1/sqlworkflow/:id/{pass,cancel,run-date,execute,schedule,osc}` | 审批通过、驳回 / 终止、改可执行时间、立即 / 手工执行、定时执行、在线改表控制 |
| GET | `/api/v1/sqlworkflow/:id/{rollback,download}` | 回滚语句、导出文件下载 |
| GET | `/help` | 站内使用文档（章节源文件 `web/static/help/*.md`） |

## 9. 文档地图

| 想做什么 | 文档 |
|---|---|
| 使用平台（按角色） | [docs/user-guide.md](docs/user-guide.md) |
| 全新部署、配置、首次上线 | [docs/ops-manual.md](docs/ops-manual.md) |
| 一键部署脚本用法、升级 / 回滚 / 卸载 | [docs/one-click-deploy.md](docs/one-click-deploy.md)、[docs/deployment.md](docs/deployment.md) |
| 初始化库与管理员命令行 | [docs/admin-cli.md](docs/admin-cli.md)、[docs/bootstrap-design.md](docs/bootstrap-design.md) |
| 升级前的行为变化 | [docs/release-notes.md](docs/release-notes.md) |
| 系统设置键与默认值 | [docs/settings-reference.md](docs/settings-reference.md)、[docs/settings-design.md](docs/settings-design.md) |
| 文档索引 | [docs/index.md](docs/index.md) |
| 字符集策略 | [docs/charset-policy.md](docs/charset-policy.md) |


| 点击浏览整个界面设计 | `prototype/README.md`（用浏览器打开 `prototype/index.html`） |
| 各功能的使用说明 | 站内帮助（登录后顶栏“使用文档”，源文件 `web/static/help/`） |

## 10. 验证状态与已知限制

各功能的验证状态见站内帮助各章末尾的“验证状态与已知限制”，以及[用户手册第 13 章](docs/user-guide.md)。概括如下：

| 范围 | 状态 |
|---|---|
| 在线查询、系统管理、按钮级权限、实例管理、慢日志、性能工单、性能巡检、分区维护的核心流程 | 在真实 MySQL 8.0 上冒烟验证过 |
| 真实 goInception | 未联调：检查、执行、语法树脱敏、备份读取、OSC 只用假实现验证 |
| SQL Server、PostgreSQL、MongoDB、ClickHouse、Redis | 只用假后端验证，未在真实实例上验证 |
| LDAP（LDAPS / StartTLS / AD）、钉钉 / 飞书 / Lark / Slack / 企业微信、短信 | 未联调 |
| SOAR | 未联调 |
| 分区维护的 SelectDB / Doris 方言与 webhook 推送 | 未联调 |
| 通知渠道里的企业微信、Telegram 机器人 | 只用本机的假机器人验证过，没有在真实群里验证（Telegram 还要服务器能访问 `api.telegram.org`，国内网络通常要配出站代理） |
| Binlog 解析、数据归档、邮件渠道 | 没有连接过真实 MySQL / SMTP 做端到端验证 |
| PolarDB-X | 没有在真实实例上验证；脱敏为保守匹配，SQL 防火墙的预估扫描行数不支持；数据归档对它做兼容降级（只用假 MySQL 协议服务端测试） |
| 安装部署脚本 | 只用假命令与临时目录验证，未在真实 systemd / firewalld / SELinux 上验证 |

已知限制：

- SSH 隧道不校验跳板机主机公钥；
- 数据导出文件只支持本地目录；
- 没有“免脱敏名单”，保守匹配没有表级精度，也认不出用列名列表改列名、`PIVOT` / `UNPIVOT`、`STRING_SPLIT` / `OPENJSON` 等把敏感字段的值换个列名输出的写法（见使用文档的已知限制）；
- 授权（License）尚未实现，不属于当前版本的功能。

## 11. 注意事项

- **`SECRET_KEY` / `MIRAGE_*` 一经设置不要随意更改**：加密保存的实例账号密码等依赖它们（见 [安装部署指南](docs/ops-manual.md) 2.6）；
- 表结构以 `deploy/bootstrap`（48 张基础表）与 `deploy/sql`（60 张扩展表）为准，表名统一带 `dbt_` 前缀；
- **后台任务默认关闭**的有：性能工单的采集 / SLA / 验证 / 周报（系统配置 `perf.jobs`）、分区维护自动执行（`partition.auto_run`）、数据保留清理（`retention.enabled`）；**慢日志采集默认开启**（只读，只采实例上“慢日志采集”开关打开的实例）；
- 分区维护：先在页面确认分区配置与推送通道，再打开 `partition.auto_run`；同一批目标表同一时间只让一个程序维护与推送，否则会重复改分区、同一事件重复通知；
- 权限只认按钮（权限组授权 + 用户单独授权 / 禁用）；早期的权限码由 `dbt-admin perm migrate` 一次性迁移（升级脚本自动执行）；
- 页面主题保存在 `dbtool_skin` Cookie 里（深空 / 明亮 / 护眼）。
