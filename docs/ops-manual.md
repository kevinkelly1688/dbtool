# dbtool 安装部署指南（全新安装）

> **适用版本**：以 `deploy/RELEASE_VERSION` / 发布包内 `VERSION` 为准。
> **读者**：把一套全新的 dbtool 部署上线的运维人员 / DBA。
> **文档类型**：部署与运维文档（安装步骤 + 配置说明 + 验证清单 + 命令速查）。
> **相关文档**：[one-click-deploy.md](one-click-deploy.md)（`install.sh` 全部子命令、升级 / 回滚 / 卸载 / 改配置）· [deployment.md](deployment.md)（运行期配置、日常运维、备份、排障）· [admin-cli.md](admin-cli.md)（`dbt-admin`）· [bootstrap-design.md](bootstrap-design.md)（基础表与基础数据）· [charset-policy.md](charset-policy.md)（字符集策略）· [settings-reference.md](settings-reference.md)（系统设置键表）· [../deploy/PACKAGING.md](../deploy/PACKAGING.md)（打包）。

**目标**：照着做能装起来、能访问、能登录，并完成上线必做的初始化。

**范围**：只讲全新部署（新主机 + 空的元数据库）。**不在本手册内**：升级 / 回滚 / 卸载（见 [one-click-deploy.md](one-click-deploy.md)）、日常运维 / 备份 / 排障（见 [deployment.md](deployment.md)）、各功能模块的使用（见 [user-guide.md](user-guide.md) 与各设计文档）。

> **注意**：本手册的文件名与 4.1、7.1、8.1、8.3、8.4、附录 D、附录 E 的编号被 `install.sh` 的输出、README 与其它文档引用，章节编号不会重排。

## 目录

- [快速上线路径（一页）](#快速上线路径一页)
- [0. 文档说明](#0-文档说明)
- [1. 产品与架构概览](#1-产品与架构概览)
- [2. 部署前准备](#2-部署前准备)
- [3. 元数据库准备](#3-元数据库准备)
- [4. 安装部署](#4-安装部署)
- [5. 配置参考](#5-配置参考)
- [6. 反向代理与 HTTPS](#6-反向代理与-https)
- [7. 外部依赖部署](#7-外部依赖部署)（goInception）
- [8. 首次上线流程](#8-首次上线流程)
- [9. 附录](#9-附录)（A 环境变量速查 · B `deploy/sql` 脚本表 · C 端口与网络矩阵 · D install.sh 速查 · E 目标库 GRANT 模板 · F 命令速查 · G 验证状态与已知限制）

---

## 快速上线路径（一页）

> 适用：**一台全新的主机 + 一个空的 MySQL**，从零部署 dbtool。每一步的细节和风险见后文对应章节。`dbt-admin`（初始化库 / 创建管理员 / 生成密钥）见 `docs/admin-cli.md`，基础表与基础数据的设计见 `docs/bootstrap-design.md`。
>
> **警告**：初始化库与一键安装只用内存假库 / 假驱动 / 假命令测过，**未在真实 MySQL 与 systemd 上验证**（附录 G），先在演练环境跑通。

| # | 做什么 | 命令 / 位置 | 验证 |
|---|---|---|---|
| 1 | 建空库（`dbt-admin` 不会建库）与两类账号：运行账号只要 DML，建表账号要 DDL | `CREATE DATABASE db_tool DEFAULT CHARACTER SET utf8mb4;`（3.2、3.3） | `SHOW CREATE DATABASE db_tool` |
| 2 | 打包、上传（tar.gz + .sha256；服务器上解压后运行 `install.sh`） | `make dist` → `dist/dbtool-<版本>-linux-amd64.tar.gz`（`sha256sum -c` 可先核对，4.4） | 校验通过；tar.gz 里有 `dbt-admin`、`bootstrap/001_base_schema.sql` |
| 3 | 演练（可选）：看将执行哪些文件 / 语句 | `DATABASE_URL=… ./dbt-admin bootstrap`（默认只演练，不写） | 输出目标库名 / 主机与文件清单 |
| 4 | 一条命令：创建用户 / 目录 + 生成密钥 + 初始化库 + 创建管理员 + systemd + 防火墙 + 启动 + 健康检查 | `read -rs DBT_ADMIN_PASSWORD; export DBT_ADMIN_PASSWORD` 后 `sudo --preserve-env=DBT_ADMIN_PASSWORD ./install.sh install --database-url 'mysql://…/db_tool' [--ddl-database-url 'mysql://…'] --gen-secrets --bootstrap --admin-user admin --public-url https://dba.example.com --require-goinception --open-firewall`（4.1；先加 `--dry-run` 看计划） | `./install.sh status` / `doctor`；`/healthz`；终端提示“已随机生成 SECRET_KEY …请备份” |
| 5 | **立刻备份** `/etc/dbtool/dbtool.env` 里的 `SECRET_KEY` 与 `MIRAGE_*` 到密码库 | 2.6 | 密码库里有记录；`MIRAGE_*` 一经使用不得再改 |
| 6 | 放 nginx + HTTPS → 部署 goInception → 初始化配置 → 抽样验证 | 第 6 章 / 7.1 / 8.3 / 8.4 | 浏览器登录页为 https；goInception “测试连接”成功；8.4 的抽样验证走通 |

建表账号与运行账号分开时（推荐）：另给 `--ddl-database-url`（或环境变量 `DDL_DATABASE_URL`），`--bootstrap` 与 `--apply-sql` 用它执行，运行账号只需 DML 权限（3.3）。

---

## 0. 文档说明

### 0.1 读者与用法

| 读者 | 建议阅读顺序 |
|---|---|
| 第一次部署的运维 | 快速上线路径 → 2 → 3 → 4 → 6 → 7 → 8 |
| 只想查某个配置项怎么写 | 5（配置参考）、附录 A / F |
| 接手的新人 | 0 → 1 → 5 |

### 0.2 约定

| 标记 | 含义 |
|---|---|
| `> **警告**：` | 可能造成数据丢失、停服或安全问题，操作前必须读完 |
| `> **注意**：` / `> **提示**：` | 容易踩的坑 / 补充说明 |
| **验证** | 执行后如何确认成功，预期结果是什么 |
| **回退** | 出问题时怎么退回 |
| 【代码】 | 来自代码或仓库文件的内置行为（文中给出文件路径，可自行核对） |
| 【建议】 | 推荐做法，**不是代码内置**（nginx、sudoers、账号拆分、备份策略等） |
| 【未验证】 | 写了但没有在真实环境验证过（真实 MySQL 路径、nginx、systemd 在目标发行版上的表现、Docker 镜像构建、外部 IdP 等） |
| 【待验证】 | 目标库权限等由代码推导、未在真实库上逐项确认的结论 |

- 示例域名一律 `dba.example.com`；示例密码一律 `<password>`；示例用户 / 账号名仅为占位；`<…>` 形式的是需要替换的占位符。
- **不要把真实密码、`SECRET_KEY`、`MIRAGE_SECRET_KEY` 写进本手册、工单、聊天或 Git。**
- 路径中的 `/opt/dbtool`、`/etc/dbtool` 是 `install.sh` 的默认值（可用 `--prefix` 改前者）。
- 本手册不给出 CPU / 内存 / 磁盘规格建议：仓库里没有压测数据，只写代码与脚本里有依据的要求（2.1）。

### 0.3 术语（精简）

| 术语 | 说明 |
|---|---|
| 元数据库 | dbtool 自己读写的 MySQL 库；本手册用 **`db_tool`** |
| `dbt_*` 表 | 本系统所有表的统一前缀：48 张基础表（`deploy/bootstrap/001_base_schema.sql`）+ 60 张扩展表（`deploy/sql/*.sql` 创建） |
| 扩展表脚本 | `deploy/sql/*.sql`（包内 `sql/*.sql`）：35 个脚本，创建 60 张扩展表，全部幂等 |
| 实例 | 被管理的目标库（MySQL 系 / SQL Server / Redis 等），存在 `dbt_sql_instance` 表，账号密码 mirage 加密 |
| mirage | 字段加密（`internal/crypto/mirage.go`），密钥来自 `MIRAGE_SECRET_KEY`（缺省取 `SECRET_KEY`） |
| goInception | SQL 审核 / 执行 / 备份回滚引擎，独立进程（本项目约定 Docker 部署） |
| 目标库 | 被 dbtool 管理的数据库（区别于 dbtool 自己的元数据库） |
| 超级用户 | `dbt_sql_users.is_superuser=1` 的用户；`dbt-admin admin create` 与 `install.sh --admin-user` 创建的就是这一类 |

### 0.4 如何确认你看的是最新版

1. 发布包命名为 `dbtool-<版本>-linux-<arch>.tar.gz`（解压后运行 `install.sh`，4.4），旁边的 `.sha256` 记录它的哈希；包内 `VERSION` 文件记录 `version / build_time / commit / arch`；`./install.sh version`、`dbtool -version`、`dbt-admin version` 都能打印当前版本。部署记录里写下版本号与 `sha256sum`。
2. 在仓库根目录核对手册里的几个数字是否仍与代码一致（手册写作时的取值：31 / 48 / 54）：

   ```bash
   ls deploy/sql/*.sql | wc -l
   ```

   ```bash
   grep -hE '^[[:space:]]*CREATE TABLE' deploy/bootstrap/001_base_schema.sql | wc -l
   ```

   ```bash
   grep -hE '^[[:space:]]*CREATE TABLE' deploy/sql/*.sql | wc -l
   ```

   三条分别是扩展表脚本数（35）、基础表数（48）、扩展表数（60）。

---

## 1. 产品与架构概览

### 1.1 一句话

dbtool 是数据库运维管理平台（SQL 审核上线、在线查询、巡检、归档等）：**单个 Go 二进制，服务端渲染，前端资源内嵌（`go:embed`），无需 Node，无需 Redis**（`internal/config/config.go`）。一个进程同时提供页面、JSON API 和全部后台任务。

### 1.2 组件图

```
                         ┌──────────────────────────────────────────────┐
  浏览器 / 脚本(JWT) ──► │ 反向代理 nginx (443, HTTPS)                  │
                         │  X-Forwarded-Proto / X-Forwarded-For         │
                         └───────────────┬──────────────────────────────┘
                                         │ HTTP :8000 (dbtool.toml [server])
                         ┌───────────────▼──────────────────────────────┐
                         │ dbtool  (单进程，systemd 管理)              │
                         │  · 页面 /  API /api/v1  静态 /static  /healthz│
                         │  · 后台任务(goroutine): 慢日志采集、性能工单、│
                         │    分区维护、定时巡检、数据归档、SQL 定时执行 │
                         │  · 进程内状态: 验证码/SSO state/2FA 计数/任务 │
                         └──┬──────────┬───────────┬───────────┬────────┘
                            │          │           │           │
        ┌───────────────────▼─┐  ┌─────▼─────┐  ┌──▼────────┐  ├──► 通知: 机器人 HTTPS / 邮件 SMTP (出站)
        │ 元数据库 db_tool   │  │goInception│  │ 目标库    │  ├──► IdP: LDAP/OIDC/CAS/钉钉/飞书/Slack/企微 (出站)
        │ (MySQL 5.7/8.0)     │  │ :4000     │  │ MySQL系 / │  ├──► 短信服务 (阿里云/腾讯云, 出站)
        │ 表名统一 dbt_ 前缀  │  │ (Docker)  │  │ SQL Server│  └──► 对象存储 (S3/MinIO/OSS 兼容端点, 归档备份)
        │                     │  │ 备份库 ───┼─►│ / Redis   │
        └─────────────────────┘  └───────────┘  └───────────┘
   磁盘: /etc/dbtool/{dbtool.toml,dbtool.env}  /opt/dbtool/{bin,sql}
         EXPORT_BASE_DIR(导出文件)  ARCHIVE_SPOOL_DIR(归档暂存)  goInception config.toml(+.dbtool.bak)
```

### 1.3 组件职责与依赖

| 组件 | 作用 | 必需？ | 备注 |
|---|---|---|---|
| dbtool 进程 | 全部业务 | 是 | 纯 Go、无 CGO；`make dist` 交叉编译 Linux 包 |
| 元数据库 `db_tool` | 用户、权限、实例、工单、配置、审计等全部数据（所有表带 `dbt_` 前缀） | 是 | MySQL 5.7 / 8.0；真库冒烟在 8.0 上做过（`docs/deployment.md` §2） |
| goInception | SQL 检查 / 执行 / 回滚 / 表级权限解析 / 脱敏语法树（脱敏语法树只用于 MySQL / PolarDB / ADB；PolarDB-X、SelectDB / Doris 按列名保守匹配） | 强烈建议 | 没有则降级；`REQUIRE_GOINCEPTION=true` 时直接拒绝而不降级（5.2） |
| 目标库 | 被管理的 MySQL 系 / SQL Server / Redis；PostgreSQL / MongoDB / ClickHouse（只有在线查询） | 按需 | 账号权限见 2.4 与附录 E（E.11–E.13） |
| 通知渠道 | 钉钉 / 企业微信 / 飞书 / Lark / Slack / Telegram 机器人；邮件（SMTP） | 否 | 机器人出站 HTTPS；邮件出站 SMTP（25 / 465 / 587）；无私信 |
| IdP | LDAP/AD、OIDC、CAS、钉钉、飞书/Lark、Slack、企业微信 | 否 | 本地账号始终可用 |
| 反向代理 | HTTPS 终止 | 生产必需（建议） | 必须透传 `X-Forwarded-Proto` |
| 对象存储 | 归档备份文件 | 否 | `local`/`nfs`/`s3`/`oss`（OSS 仅 S3 兼容端点） |

### 1.4 数据流（核心）

1. 用户 → nginx → dbtool：页面请求带会话 Cookie `dba_session`；脚本用 `Authorization: Bearer <JWT>`（`POST /api/v1/auth/login` 换取）。
2. dbtool → 元数据库：读写用户、权限、工单、审计等。DSN 固定带 `charset=utf8mb4&parseTime=True&loc=Local`（`config.go` `MySQLDSN`）——**时间用进程本地时区读写，与 MySQL 服务器时区无关**，所以进程 `TZ` 一经使用就不要再改（2.5）。
3. dbtool → 目标库：每个实例用 `dbt_sql_instance` 里**唯一一组账号**（`store.EngineConn`，mirage 解密）；各模块没有各自的专用账号。
4. dbtool → goInception：检查 / 执行 / 备份回滚 / 语法树；连接信息在系统设置（`goinception.*`），代码默认端口 4000（`internal/goinception/client.go`）。
5. dbtool → 通知 / IdP / 短信 / 对象存储：均为出站连接，需要放行（附录 C）。

### 1.5 端口清单（概要，完整见附录 C）

| 端口 | 方向 | 说明 |
|---|---|---|
| 443 | 用户 → nginx | HTTPS |
| 8000 | nginx → dbtool | 默认，改 `dbtool.toml` 的 `[server] port`；只给 nginx 用时建议 `host = "127.0.0.1"` |
| 3306 | dbtool → 元数据库 / 目标库 / goInception 备份库 | 以实际为准 |
| 4000 | dbtool → goInception | 代码默认值，可在页面改 |
| 6379 等 | dbtool → Redis 实例 | 以实例登记的端口为准 |

### 1.6 状态放在哪里（决定了备份、扩容、多实例怎么做）

| 位置 | 内容 | 影响 |
|---|---|---|
| **元数据库** | 用户 / 权限 / 实例（含加密密文）/ 工单 / 审批 / 审计 / 系统设置（`dbt_setting`）/ 巡检报告 / 慢日志 / 归档任务与运行记录（全部表都带 `dbt_` 前缀） | 全部持久数据都在这里；备份恢复不在本手册范围 |
| **进程内存** | 短信验证码、绑定中的 TOTP 密钥、已用 SSO state、TOTP 防重放窗口、2FA 失败计数、登录限流、SQL 分析任务（结果留 30 分钟）、Binlog 解析任务（结果留 30 分钟）、权限缓存（约 15 秒）、菜单 / 角标 / 设置缓存（几秒） | 重启即丢；**多实例需会话保持或单实例**（6.6） |
| **签名 Cookie** | 会话（HMAC-SHA256，密钥 = `SECRET_KEY`），无服务端存储 | 不能服务端强制踢人；改 `SECRET_KEY` 会使所有会话 / JWT 失效 |
| **磁盘** | `dbtool.toml` / `dbtool.env`；导出文件；归档暂存 `*.tmp`；goInception `config.toml` 及 `.dbtool.bak`；`local` 类型归档备份存储目录 | 需要单独备份 / 清理（4.5） |

---

## 2. 部署前准备

### 2.1 环境要求

下表只列代码、脚本或打包产物里有依据的要求。仓库里没有压测数据，**不给出 CPU / 内存 / 磁盘规格建议**；容量规划需要按实际负载自行评估。

| 项 | 要求 | 依据 |
|---|---|---|
| 操作系统 | 带 systemd 的 Linux。`install.sh doctor` 认得的发行版：RHEL / CentOS / Rocky / AlmaLinux / Fedora / Oracle Linux / Anolis / openEuler / Kylin；其它发行版只给警告 | 【代码】`deploy/install.sh` 的 `doctor_run`；脚本按 CentOS 7+ 编写（`useradd`、`systemctl`、`firewall-cmd`、`dnf` / `yum`） |
| 权限 | 写系统的子命令（`install` / `init-db` 等）需要 root；`status` / `doctor` / `version` 不需要 | 【代码】`require_root` |
| 脚本依赖命令 | 必需：`curl`、`sha256sum`、`install`、`awk`、`sed`、`grep`；建议：`flock`（util-linux，缺失时改用 `mkdir` 锁）；可选：`tar`、`firewall-cmd`（`--open-firewall`）、`journalctl`、`ss` / `netstat`（端口检查）、`restorecon`（SELinux） | 【代码】`doctor_run` |
| 磁盘 | 安装目录所在分区可用空间 ≥ 300 MB，且 ≥ 安装包目录大小的 2 倍 + 50 MB（不足为 FAIL）。默认保留最近 3 个版本目录 | 【代码】`doctor_run` 的磁盘检查、`--keep` |
| 元数据库 | MySQL 5.7 / 8.0；独立的新库（本手册用 `db_tool`），字符集 `utf8mb4`；真实库冒烟只在 8.0 上做过 | 【代码】`deploy/bootstrap`、`docs/charset-policy.md`；验证范围见 `docs/deployment.md` §8 |
| 目标库 | MySQL 系（MySQL / PolarDB / PolarDB-X / SelectDB / Doris / ADB）、SQL Server、Redis；PostgreSQL / MongoDB / ClickHouse（在线查询）；账号权限见 2.4 与附录 E | 【代码】`cmd/server/main.go` 注册的引擎 |
| goInception | 强烈建议；默认端口 4000 | 7.1 |
| Go 工具链 | **只在从源码构建时需要**（`make dist`、`install.sh --build`）：Go 1.25+；运行不需要 Go、Node、Redis | 【代码】`go.mod`（`go 1.25.0`）、`deploy/package-centos7.sh` |
| 端口 | dbtool 监听 TCP 8000（可改，1–65535）；`doctor` 检查端口是否已被其它进程占用 | 【代码】`internal/config`、`doctor_run` |
| 文件句柄 | 单元里设 `LimitNOFILE=65535` | 【代码】`render_unit` |

> **提示**：二进制是纯 Go、无 CGO 的静态可执行文件（`CGO_ENABLED=0`），不依赖主机上的 C 库；别的带 systemd 的 Linux 也能运行，但需要自行准备单元文件（4.2）。本手册所有 systemd / 防火墙行为均未在目标发行版上实测【未验证】。

磁盘与内存的使用特征（用于容量评估，不是规格）：

- 增长最快的元数据库表：`dbt_audit_log`、`dbt_query_log`、`dbt_slow_logs`、巡检报告、`dbt_redis_slowlog`（可用保留策略清理）。
- 进程内存：并发查询结果、Binlog 解析任务结果（保存 40 个任务 / 总体积 96 MB，超出淘汰最早结束的）、xlsx 导出（行数上限 `export.max_rows`，默认 10000 行）。
- 数据盘：导出文件与归档暂存都在 `<PREFIX>/data` 下；归档备份文件单个上限默认 256 MB（`PartMB`），磁盘写满会导致导出失败、归档暂停。

### 2.2 系统、网络、防火墙

| 项 | 要求 | 检查命令 |
|---|---|---|
| 时间同步 | chrony / ntpd 正常（SSO 的 state/令牌、TOTP 都依赖时间） | `chronyc tracking` |
| 时区名 | `TZ` 必须是合法的 IANA 时区名。二进制内嵌了时区库（`internal/partman/timeutil.go`、`internal/engine/clickhouse/shape.go` 引入 `time/tzdata`），不依赖主机的 `/usr/share/zoneinfo`；**名字写错时 Go 静默回落到 UTC**，`install.sh` 首次安装的 `--timezone` 不校验名字是否存在 | `ls /usr/share/zoneinfo/Asia/Shanghai`（主机上有该文件可作名字核对）；启动后看页面时间 |
| SELinux | 若开启，`install.sh` 会对安装目录执行 `restorecon`；其它目录（`EXPORT_BASE_DIR` 等）需要自己处理上下文【未验证】 | `getenforce` |
| 文件句柄 | 单元里已设 `LimitNOFILE=65535` | `systemctl show dbtool -p LimitNOFILE` |
| 出站 | 元数据库 3306、目标库端口、goInception 端口、通知机器人 / IdP / 短信 / 对象存储的 HTTPS 443、邮件渠道的 SMTP 端口（25 / 587 / 465） | 附录 C |
| 入站 | 仅 nginx 访问 dbtool 的监听端口；用户只访问 443 | 防火墙矩阵见附录 C |

### 2.3 账号与权限清单（总览）

| 账号 | 用途 | 最小权限 | 详见 |
|---|---|---|---|
| OS 用户 `dbtool` | 运行服务（`install.sh` 创建：`useradd --system --no-create-home --shell /sbin/nologin`） | 对 `EXPORT_BASE_DIR`、`ARCHIVE_SPOOL_DIR` 可写；读 `/etc/dbtool/dbtool.env`（组 `dbtool`，0640） | 4.5 |
| 元数据库运行账号 | `DATABASE_URL` 里的账号 | `db_tool.*` 上的 `SELECT, INSERT, UPDATE, DELETE` | 3.3 |
| 元数据库建表账号 | 执行 `bootstrap/*.sql`、`sql/*.sql` 扩展表脚本（`dbt-admin bootstrap`） | `db_tool.*` 上的 `CREATE, ALTER, INDEX, REFERENCES` + DML（`150_builtin_groups.sql` 要 `ALTER`）【待验证】 | 3.3 |
| 目标库实例账号 | `dbt_sql_instance` 里登记的账号 | 各功能不同，按需组合 | 2.4、附录 E |
| goInception 连接账号 | `goinception.user/password` | goInception 自己的账号（`config.toml` 里的 `[inc]` 设置），不是 MySQL 账号 | 7.1 |
| goInception 备份库账号 | `goinception.backup_*` | 对备份库有建库 / 建表 / 写入权限（页面“测试备份库”会用 `SHOW GRANTS` 判断写权限） | 7.1 |
| sudoers（可选） | 页面重启 goInception | 只放行 `dbtool` 执行一条 `docker restart <容器名>` | 7.1.4 |
| `mysql_config_editor` login-path | 手工执行扩展表脚本时免密码连接 | 用户家目录 `~/.mylogin.cnf`，权限 0600 | 3.4 |

### 2.4 目标库账号权限（要点；GRANT 模板见附录 E）

**关键事实**【代码】：dbtool 对每个实例只使用 `dbt_sql_instance` 里登记的**那一组**账号（`internal/store/store.go` 的 `EngineConn`，mirage 解密后直连）。在线查询、实例管理、巡检、慢日志、性能工单、分区维护、Binlog 解析、数据归档、对象信息 / 可编程对象、导出、SQL 优化、Redis 工单**共用这一个账号**，没有“归档专用账号”“Binlog 专用账号”“查询只读账号”之分。因此权限设计有两种取舍：

| 做法 | 说明 |
|---|---|
| 一个实例登记一个“并集权限”账号 | 简单，但账号权限偏大；所有功能的权限由**页面权限 + 资源组 + 工单审批**控制 |
| 同一物理库登记**多条实例记录**（不同账号、不同标签，如 `prod-ro` 带 `can_read`、`prod-dba` 带 `can_write`） | 【建议】用平台的标签与资源组做隔离：查询用只读实例记录，上线 / 归档 / 分区用高权限记录。代价是实例列表里出现同库多条 |

例外（不是实例账号）：goInception 连接与备份库账号（系统设置 `goinception.*`）、SOAR 的 `optimizer.soar_test_dsn`、SSH 隧道表 `dbt_ssh_tunnel`、归档备份存储的 AK/SK、Redis 的 ACL 账号即该 Redis 实例记录里的用户名 / 密码。

权限归类（【待验证】部分已在附录 E 标注）：

| 账号类型（按“用途”理解） | 主要权限 |
|---|---|
| 只读查询 / 对象信息 / 导出 | 对被查询 schema `SELECT`（`information_schema` 只显示有权限的对象）；`SHOW VIEW`（实例配了数据脱敏字段时需要：脱敏要读视图定义，读不到按脱敏失败处理；没配脱敏时可选） |
| 巡检 / 性能 / 慢日志 | 上面再加 `PROCESS`、`REPLICATION CLIENT`，`SELECT` on `performance_schema.*`、`sys.*`、`mysql.slow_log`、`mysql.user` |
| 实例管理（会话 / 账号 / 参数 / Binlog 清理） | `CREATE USER` + `GRANT OPTION`、`SYSTEM_VARIABLES_ADMIN`、`BINLOG_ADMIN`、`CONNECTION_ADMIN`（5.7 为 `SUPER`）、`SELECT` on `mysql.*` |
| Binlog 解析 | `REPLICATION SLAVE` + `REPLICATION CLIENT`（再加对库表的 `SELECT` 以取列名） |
| 分区维护 | 目标表上 `ALTER, DROP`（REORGANIZE 另需 `CREATE, INSERT`）+ `information_schema` 可读 |
| 数据归档 | 源：`SELECT, DELETE`；备份目标库：`SELECT, INSERT, CREATE`；可选 `REPLICATION CLIENT`（探测复制延迟） |
| SQL 上线（经 goInception / 原生执行） | 取决于工单语句（DDL/DML）；goInception 备份回滚需要 `REPLICATION SLAVE`+`REPLICATION CLIENT`（goInception 自己读 binlog）【待验证】 |
| Redis | ACL：只读元数据命令基线 + 按需的写命令（仅工单规则用到的） |

### 2.5 时区与字符集

| 项 | 要求 | 依据 |
|---|---|---|
| 进程时区 | 库里存的是**无时区的本地时间**，读写都按进程时区（`loc=Local`），所以**时区一经确定、库里有数据之后不要再改**（改了已有时间会整体偏移）。`install.sh` 首次安装把 `TZ="<--timezone，默认 Asia/Shanghai>"` 写进 `dbtool.env` | 【代码】`deploy/install.sh`；DSN `loc=Local` 见 `internal/config/config.go` |
| 定时巡检 / 归档窗口 / 分区边界 | 定时巡检、归档窗口按进程本地时区；分区边界按**分区配置自带的 `timezone` 字段**（留空 = 跟随目标库当前会话时区） | 【代码】巡检的计划时间按进程本地时区；分区边界按配置里的 `timezone` 字段 |
| MySQL 服务器时区 | 不影响 dbtool 对元数据库的读写，但会影响目标库侧 `NOW()` 语义（归档 `NOW() - INTERVAL n DAY`、慢日志）。归档引擎会读取目标服务器的当前偏移并固定会话 `time_zone` | 【代码】`internal/archive/engine/opener.go` |
| 字符集 | 元数据库 `utf8mb4`；连接固定 `charset=utf8mb4` | 【代码】`MySQLDSN` |
| 排序规则 | 建库时显式指定 utf8mb4 与排序规则（MySQL 8 推荐 `utf8mb4_0900_ai_ci`，5.7 用 `utf8mb4_general_ci`）。所有表（基础表与扩展表）都不指定字符集 / 校验规则，全部跟随库默认，保证各表一致；巡检列表用 `UNION ALL` 合并三种报告表，排序规则不一致会报 `Illegal mix of collations` | 【代码】`deploy/sql/131_redis_inspect.sql`、`docs/charset-policy.md` |

> **字符集策略（摘要，详见 `docs/charset-policy.md`）**：表与字段都不指定字符集和校验规则，完全跟随数据库默认；`dbt-admin bootstrap` 会在库默认字符集不是 utf8mb4 时拒绝执行。

**验证（建库后执行）**：

```sql
SELECT SCHEMA_NAME, DEFAULT_CHARACTER_SET_NAME, DEFAULT_COLLATION_NAME
  FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = 'db_tool';
SHOW VARIABLES LIKE 'collation_server';
```

### 2.6 密钥规划

| 密钥 | 用途 | 规则 | 丢失 / 改变的后果 |
|---|---|---|---|
| `SECRET_KEY` | ① 会话 Cookie 与 JWT 的 HMAC 签名（`internal/server/session.go`）② `MIRAGE_SECRET_KEY` 缺省值 | 随机；推荐 ≥ 50 字符（`dbt-admin gen-secrets` 生成 64 位字母数字）。程序对 `SECRET_KEY` 本身没有长度校验，但它同时是 `MIRAGE_SECRET_KEY` 的缺省值，而 mirage 要求 ≥ 32 字符；开发默认值是公开的，生产必须覆盖（`internal/config/config.go` 的 `defaultSecretKey`） | 所有会话 / JWT 失效（需重新登录）；若 `MIRAGE_SECRET_KEY` 未单独设置，则所有密文无法解密 |
| `MIRAGE_SECRET_KEY` | mirage 加密：实例账号密码、`dbt_setting` 里的敏感项（`encrypted=1`）、备份存储 AK/SK、goInception 配置版本内容等；归档文件加密密钥 = `sha256("dba-tool/archive/file-key/v1:" + MIRAGE_SECRET_KEY)` | ≥ 32 字符，建议单独设置（以后换 `SECRET_KEY` 不影响密文）；**一经使用不要直接改**，确需更换用 `dbt-admin reencrypt` 先重加密（`docs/admin-cli.md`） | 实例连不上（解密为空 / 乱码）；旧加密归档备份无法解密；goInception 配置历史版本无法读取 |
| `MIRAGE_CIPHER_MODE` / `MIRAGE_CIPHER_IV` | 加密模式与向量，缺省 `ECB` / `1234567890abcdef` | `--gen-secrets` 一并生成；一经使用不要改 | 同上 |

- 生成：`dbt-admin gen-secrets`，或安装时加 `--gen-secrets`（只在终端提示备份，不回显密钥值）。
- 存放：`/etc/dbtool/dbtool.env`（0640，属主 root、组 `dbtool`）；**立刻备份到密码库 / 密钥管理系统，与数据库备份分开保管**。`SECRET_KEY` / `MIRAGE_*` 丢失 = 库里实例账号等密文全部无法解密，没有补救办法。

> **警告**：不要把真实值贴进任何文档、聊天或工单。本手册示例统一用 `<secret-key>` 占位。

### 2.7 证书与域名

- 域名示例 `dba.example.com`，解析到 nginx。`PUBLIC_URL` 必须写成对外 HTTPS 地址（SSO 回调、通知里的工单链接都用它）。
- 证书：由公司 CA / ACME 签发，放在 nginx；dbtool 自身只监听 HTTP，不终止 TLS（【代码】`cmd/server/main.go` 使用 `ListenAndServe`）。
- 使用 SSO 登录方式时，IdP 登记的回调地址必须与 `PUBLIC_URL` 一致：`<PUBLIC_URL>/login/sso/<登录方式ID>/callback`。

### 2.8 上线前检查清单

在生产环境逐项打勾（任何一项为否，不要上线）：

**环境**
- [ ] 主机时间同步正常；`TZ` 是合法的 IANA 时区名（2.2）
- [ ] 防火墙：用户只能访问 443；dbtool 监听端口仅对 nginx 开放（附录 C）
- [ ] `dbtool` 用户存在，数据盘目录已建并 `chown dbtool`（4.5）

**数据库**
- [ ] 新库 `db_tool` 已建，字符集 utf8mb4（2.5）
- [ ] 运行账号 / 建表账号已分开创建（3.3）
- [ ] `dbt-admin bootstrap` 的演练输出已人工核对（目标库名 / 主机、文件清单，8.1）

**密钥与配置**
- [ ] `SECRET_KEY`、`MIRAGE_*`、`TZ` 已确定，且已存入密码库（2.5、2.6）
- [ ] `REQUIRE_GOINCEPTION=true`，goInception 已部署并通过“测试连接”（7.1）
- [ ] `PUBLIC_URL` 为 https 对外地址；nginx 透传 `X-Forwarded-Proto`（第 6 章）
- [ ] `EXPORT_BASE_DIR` / `ARCHIVE_SPOOL_DIR` 指向持久目录（手工写的 systemd 单元没有 `WorkingDirectory` 时，缺省的相对路径会落到 `/`，运行用户无权写，见 4.2、4.5；`install.sh` 已设置 `WorkingDirectory=<PREFIX>`，并默认把这两项指向 `<PREFIX>/data/{export,archive-spool}`）
- [ ] `DEBUG` 未开启

---

## 3. 元数据库准备

### 3.1 总体思路

- 【项目决定】元数据库用**独立的新库**，本手册用 `db_tool`，所有表带 `dbt_` 前缀。该库必须是**全新的空库**，不要与其它系统共库。
- 库里的表 = **48 张基础表**（`deploy/bootstrap/001_base_schema.sql`，另有 `002_base_data.sql` 基础数据：权限码、内置权限组等）+ **60 张扩展表**（`deploy/sql/*.sql`，35 个脚本，全部幂等）。设计见 `docs/bootstrap-design.md`。
- dbtool 不做 AutoMigrate，**程序启动时不会建任何表**；缺表时对应功能降级，或页面提示先执行某个 SQL 脚本；缺核心表（含 `dbt_setting`）时 `/readyz` 返回 503（6.4）。
- 全新空库用 `dbt-admin bootstrap`（或 `install.sh --bootstrap`）一步建好：基础表 + 基础数据 + 扩展表脚本（3.5、8.1）。

### 3.2 创建库

```sql
-- MySQL 8.0
CREATE DATABASE db_tool DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
-- MySQL 5.7
CREATE DATABASE db_tool DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
```

> **警告**：`DEFAULT CHARACTER SET utf8mb4` 不能省：`dbt-admin bootstrap` 会在库默认字符集不是 utf8mb4 时拒绝执行（`docs/charset-policy.md`）。MySQL 8 上建库不写 `COLLATE` 会得到 `utf8mb4_0900_ai_ci`，同一套库里各表保持一致即可（2.5）。

**验证**：`SHOW CREATE DATABASE db_tool;` 输出的字符集 / 排序规则符合预期。

**回退**：新库是空的，出错直接 `DROP DATABASE db_tool;` 重来（**只对新库**）。

### 3.3 账号与权限【建议 + 部分待验证】

```sql
-- 运行账号：dbtool 日常使用（DATABASE_URL 里的账号）
CREATE USER 'dbtool_rt'@'<dbtool 主机 IP>' IDENTIFIED BY '<password>';
GRANT SELECT, INSERT, UPDATE, DELETE ON db_tool.* TO 'dbtool_rt'@'<dbtool 主机 IP>';

-- 建表账号：只在初始化库（建表 / 执行扩展表脚本）时使用，用完可锁定
CREATE USER 'dbtool_ddl'@'<运维跳板机 IP>' IDENTIFIED BY '<password>';
GRANT CREATE, ALTER, INDEX, REFERENCES, SELECT, INSERT, UPDATE, DELETE ON db_tool.* TO 'dbtool_ddl'@'<运维跳板机 IP>';
```

| 账号 | 依据 | 备注 |
|---|---|---|
| 运行账号只要 DML | 【代码】程序不建表 / 不改表；`install.sh` 注释也写“运行账号不需要 DDL 权限”（`docs/deployment.md` §2） | `GET_LOCK()` 不需要额外权限（分区维护、归档用它做跨进程互斥）；是否还需要 `LOCK TABLES` / `CREATE TEMPORARY TABLES` 未发现使用，【待验证】 |
| 建表账号需要 `ALTER` + DML | 【代码】`150_builtin_groups.sql` 对 `dbt_auth_group_ext` 执行 `ALTER TABLE ... ADD COLUMN`，并有 `INSERT ... ON DUPLICATE KEY UPDATE`；基础数据要 `INSERT` | `REFERENCES` 在 8.0 不是必需，列出是为兼容 5.7【待验证】 |

> **注意**：运行账号的密码要 URL 编码后放进 `DATABASE_URL`（`mysql://用户:URL编码的密码@主机:3306/db_tool`）。`install.sh` 在解析时会 URL 解码（`parse_db_url`），但 dbtool 自己用 `net/url` 解析：`@ : / ? # %` 等必须编码。

### 3.4 执行 `deploy/sql`：顺序、方法、幂等性

**规则**：按文件名编号升序执行（`ls` 的字典序即正确顺序）；全部可重复执行；脚本只增不改（只 `CREATE TABLE IF NOT EXISTS`，唯一的 `ALTER` 是 `150`，且先查 `information_schema` 再动态执行）。**全新部署不需要手工执行**：`dbt-admin bootstrap` / `install.sh --bootstrap` 已经包含它们；下面的手工循环只用于单独补执行。

在有 `mysql` 客户端的机器上用建表账号执行。密码不上命令行：先建 login-path（交互输入密码，凭据存 `~/.mylogin.cnf`）：

```bash
mysql_config_editor set --login-path=dbt_ddl --host=<mysql-host> --port=3306 --user=dbtool_ddl --password
```

进入脚本目录（安装包解压后的 `sql/`，或仓库的 `deploy/sql/`）：

```bash
cd /opt/dbtool/sql
```

按文件名升序逐个执行，遇错即停：

```bash
for f in $(ls *.sql | sort); do echo "== $f"; mysql --login-path=dbt_ddl --default-character-set=utf8mb4 db_tool < "$f" || { echo "FAILED: $f"; break; }; done
```

> **注意**：`install.sh --apply-sql` 在没有 `--ddl-database-url` 时用 **`DATABASE_URL` 里的账号**建表（运行账号默认没有 DDL 权限，会失败并报“账号需要对库有 CREATE 权限”）。要么给 `--ddl-database-url`，要么改手工执行上面的循环。

> **注意**：`150_builtin_groups.sql` 依赖**基础数据**（`dbt_auth_group` 里已有 `DBA / TL / RD / QA / Default`）：手工执行时必须在 `002_base_data.sql` 之后；在没有这些组的库上执行会什么也标不到。`260_setting.sql` 创建 `dbt_setting`，是 `/readyz` 的必需表。

脚本清单（创建的表 / 用途 / 依赖 / 是否需要重启 / 不执行的后果）见**附录 B**。

**验证（执行后）**：

```sql
USE db_tool;
SELECT COUNT(*) AS dbt_tables FROM information_schema.tables
 WHERE table_schema = 'db_tool' AND table_type = 'BASE TABLE' AND table_name LIKE 'dbt\_%';   -- 预期 108 = 48 张基础表 + 60 张扩展表（dbt-admin 建过的库再多 1 张 dbt_bootstrap_applied）
SELECT COLUMN_NAME FROM information_schema.columns
 WHERE table_schema='db_tool' AND table_name='dbt_auth_group_ext' AND column_name='builtin';  -- 预期 1 行
SELECT group_id, builtin FROM dbt_auth_group_ext WHERE builtin = 1;   -- DBA/TL/RD/QA/Default 各一行
```

### 3.5 初始化库：`dbt-admin bootstrap`

全新空库用 `dbt-admin bootstrap` 一步完成：基础表（`bootstrap/001_base_schema.sql`）、基础数据（`bootstrap/002_base_data.sql`）与扩展表脚本（`sql/*.sql`）。默认只演练，加 `--apply --yes` 才写；遇错即停并报出文件名与语句序号；全部幂等，已应用的文件记在 `dbt_bootstrap_applied`。演练 / 执行的步骤、验证与创建第一个管理员见 **8.1**；一键安装时由 `install.sh --bootstrap` 代为执行（4.1）。选项以 `docs/admin-cli.md` 为准。

> **注意**：`dbt-admin bootstrap` 在 `dbt_sql_users` 已有数据时拒绝默认流程（本手册只讲空库，这种情况不应出现）。

---

## 4. 安装部署

### 4.1 一键部署脚本 `deploy/install.sh`

用法速查、设计与不变量见 `docs/one-click-deploy.md`；本节是首次部署用到的选项表（以 `deploy/install.sh` 的参数解析为准）。

> **警告**：行为测试用假命令跑过（`scripts/install-test/run.sh`），**未在真实 systemd / root / firewalld / MySQL 环境上验证**（附录 G）。
>
> **注意**：已安装的主机再次运行 `install` 等价于升级（配置 / 密钥 / 管理员 / 数据目录都不动，配置类选项被忽略）。升级 / 回滚 / 改配置 / 卸载不在本手册范围，见 `docs/one-click-deploy.md`。

**子命令**【代码】：写系统的子命令需要 root 与 systemd；`status` / `doctor` / `version` 不需要 root（`doctor` 非 root 时读不到 env 会提示）。

| 子命令 | 作用 |
|---|---|
| `install`（缺省，可省略） | 首次部署；**已有安装 / 配置时自动按升级处理** |
| `status` | 版本、服务、监听端口、`/healthz` `/readyz` |
| `doctor` | 只读预检与体检；有 FAIL 退出码 3 |
| `init-db` | 仅初始化元数据库，等价调用 `dbt-admin bootstrap`：库为空才建基础结构与数据，否则只执行幂等的 `sql/*.sql` 扩展表脚本 |
| `version` | 脚本 / 安装包 / 已安装版本（也可写 `-V` / `--version`） |
| `help`（或 `-h` / `--help`） | 打印脚本开头那段连续的 `#` 用法注释 |

此外还有 `upgrade`、`rollback`、`uninstall`、`reconfigure` 四个子命令，不属于全新部署，见 `docs/one-click-deploy.md`。

```bash
sudo ./install.sh [install] [选项]
```

```bash
./install.sh status
```

```bash
./install.sh doctor
```

```bash
sudo ./install.sh init-db [--admin-user NAME] [--ddl-database-url URL]
```

**选项逐项解释**（均已对照 `deploy/install.sh` 的参数解析 `case` 分支）

| 选项 | 含义 | 默认 / 行为 |
|---|---|---|
| `--database-url URL` | 元数据库 `mysql://用户:密码@主机:3306/库名`（密码特殊字符需 URL 编码）。也可用环境变量 `DBT_DATABASE_URL`（不出现在命令行上） | 无；非交互且未给则报错“缺少 --database-url” |
| `--secret-key KEY` | `SECRET_KEY`；也可用环境变量 `DBT_SECRET_KEY` | 无；非交互必须给（全新部署改用 `--gen-secrets`） |
| `--gen-secrets` | 没给 `--secret-key` 时随机生成 `SECRET_KEY` 与 `MIRAGE_SECRET_KEY` / `MIRAGE_CIPHER_IV` / `MIRAGE_CIPHER_MODE=ECB`（优先用包里的 `dbt-admin gen-secrets`，没有则 `/dev/urandom`），写进 `dbtool.env`（0640）；**只在终端提示备份，不回显密钥值**。给了 `--secret-key` 时不生成 `MIRAGE_*` | 不生成 |
| `--port PORT` | 监听端口，1–65535，写入 `dbtool.toml` 的 `[server] port` | 8000 |
| `--listen ADDR` | `主机:端口`（如 `127.0.0.1:9000`）；只给端口也行。与 `--port` 同给时端口以 `--port` 为准 | `0.0.0.0:8000` |
| `--public-url URL` | 写入 `PUBLIC_URL` | 空（运行时回落为 `http://127.0.0.1:端口`） |
| `--require-goinception` | 写入 `REQUIRE_GOINCEPTION=true`（对应的 `--no-require-goinception` 表示不写；代码缺省 `false`） | 不写 |
| `--timezone TZ` | 写入 `TZ=...`（2.5）；首次安装不校验名字是否存在于时区库 | `Asia/Shanghai` |
| `--bootstrap` | **初始化元数据库**，在服务启动前：`dbt-admin status` 判断 → 用户表不存在 / 为空则 `dbt-admin bootstrap --sql-root <release> --apply --yes`（基础表 + 基础数据 + `sql/*.sql`），否则**只执行幂等的 `sql/*.sql` 扩展表脚本**（`--only-sql`，不重复初始化基础数据）→ 没有启用的超级用户则 `dbt-admin admin create`。前置条件（连得上库、库已存在、管理员口令齐全）在改动系统**之前**检查；用 `--ddl-database-url` 的账号，没给用运行账号 | 不执行 |
| `--admin-user NAME` | `--bootstrap` / `init-db` 创建的超级用户名，不含空白、最长 30 个字符（`dbt-admin` 另有字符集规则，见 `docs/admin-cli.md` §5.1）。**非交互时必须给，且密码用环境变量 `DBT_ADMIN_PASSWORD` 提供**（不接受命令行密码）；交互时提示输入（缺省 `admin`），密码由 `dbt-admin` 在终端无回显询问。库里已有启用的超级用户时不创建 | 无 |
| `--ddl-database-url URL` | 建表账号连接串（也可用环境变量 `DDL_DATABASE_URL`），需要对库有 `CREATE` / `ALTER` 等权限；`--bootstrap`、`init-db` 的建表脚本用它执行，运行账号只需 DML 权限。连接串只经环境变量传给 `dbt-admin`，不出现在进程命令行 | 无（沿用运行账号） |
| `--apply-sql` | 首次安装时也执行 `sql/*.sql`（`dbt-admin bootstrap --only-sql --apply --yes`，记录到 `dbt_bootstrap_applied`）。包里没有 `dbt-admin` 的旧包才退回 `mysql` 客户端 + 运行账号。与 `--bootstrap` 同给时不重复执行 | 首次安装默认不执行，只提示 |
| `--open-firewall` | `firewalld` 运行时放行监听端口：规则已存在则不记为本脚本添加 | 不改；`firewalld` 未运行只警告 |
| `--build` | 用本机 Go（1.25+）从源码编译 `dbtool`、`dbt-admin`（`CGO_ENABLED=0 go build -trimpath`，注入版本号），要求在仓库的 `deploy/` 目录里运行 | 使用安装包里现成的二进制 |
| `--prefix DIR` | 安装目录（`releases/`、`current`、`data/`、`goinception/` 都在它下面）；只接受由字母、数字和 `. _ + - /` 组成的绝对路径，不含 `..`，并拒绝 `/etc`、`/usr`、`/opt`、`/var` 等系统目录本身 | `/opt/dbtool` |
| `--user NAME` | 运行服务的系统用户（须匹配 `^[a-z_][a-z0-9_-]{0,31}$`；不存在则创建为系统用户，`/sbin/nologin`，并记入状态文件） | `dbtool` |
| `--dry-run` | 对所有写子命令生效：只打印计划（只读连库做预检），不改文件 / 服务 / 库，也不写日志 | 否 |
| `-y`, `--yes` | 不询问，使用默认值 / 已给选项 | 交互 |
| `--health-timeout SEC` | 安装后等待 `/healthz` 与 `/readyz` 的秒数，须为正整数（环境变量 `DBT_HEALTH_TIMEOUT`） | 60 |
| `-h`, `--help` | 打印用法 | |

> **警告**：已有配置（`/etc/dbtool/dbtool.env` 已存在）时，配置类选项（`--database-url`、`--secret-key`、`--public-url`、`--require-goinception`、`--timezone`、`--port`、`--listen`、`--gen-secrets`）一律被忽略并提示改用 `reconfigure`（见 `docs/one-click-deploy.md`）。

**脚本做了什么**（`install`，终端显示 `[n/11]` 阶段进度与耗时；失败时报告已完成 / 失败于 / 未执行的阶段，各阶段幂等）【代码】

1. **预检**：加锁（`flock /var/lock/dbtool-install.lock`，拿不到退出码 4）→ 读状态文件与已有配置 → 定位安装包 → `doctor` 关键项（OS / systemd / 依赖命令 / 磁盘 / 配置完整性 / 包的 sha256 / 连库状态）→ `--bootstrap` 前置条件。**此阶段之前不改动任何东西**。
2. **备份当前配置**到 `/etc/dbtool/backup/<时间戳>/`（首次安装没有旧配置，跳过）。
3. **系统用户与目录**：运行用户不存在才创建；`<prefix>/releases`、`/etc/dbtool`、数据目录 `<prefix>/data/{export,archive-spool}` 与 goInception 配置目录 `<prefix>/goinception`（属主运行用户，0750）**只在缺失时创建**。
4. **安装新版本**：解到 `releases/.stage.<pid>`，用包内 `SHA256SUMS` 校验，通过后原子 `mv` 为 `releases/<版本>`；有 `restorecon` 则执行。
5. **写入配置**：写 `dbtool.env`（`umask 077`，先写 `.new` 再 `chown root:<user>` / `chmod 0640` / 原子 `mv`）与 `dbtool.toml`（0644，只含 `[server]` 的 `port` 与 `host`）；`dbtool.env` 里没有时补写 `EXPORT_BASE_DIR`、`ARCHIVE_SPOOL_DIR`（指向 `<prefix>/data/…`）与 `GOINCEPTION_CONFIG_FILE`（`<prefix>/goinception/config.toml`），并在该文件不存在时部署包里的 `goinception.config.toml`（属主运行用户，0640）。
6. **元数据库**：见 `--bootstrap` / `--apply-sql`。运行账号建表失败只警告（对应功能降级），给了建表账号而失败则中止（此时尚未切换版本）。
7. **部署 SOAR（SQL 优化工具；安装包里带 `plugins/soar` 才做）**：程序放到 `<prefix>/plugins/soar`（root:root 0755；与包内内容相同则不动，不同则先写同目录临时文件再 `mv`，正在运行的 SOAR 不受影响），配置放到 `<prefix>/plugins/soar.yaml`（已存在不覆盖，包内新版另存 `soar.yaml.dist`；`log-output` 改写为 `<prefix>/data/soar.log`），再用 `dbt-admin settings set` 把系统设置 `optimizer.soar_path` / `optimizer.soar_config` 指向它们（只在还没设置时写，管理员配置过的不改）。整步不致命：出错只警告，升级照常；每次 install / upgrade 都会执行，重复运行是安全的。测试库 `optimizer.soar_test_dsn` 要管理员自己填（必须是非生产库）。
8. **切换版本并重启**：原子切换 `current` → 写单元文件（`ExecStart=<prefix>/current/dbtool -config /etc/dbtool/dbtool.toml`）→ `daemon-reload`、`enable`、`restart`（**停止时会等待执行中的 SQL 工单，最长 10 分钟**，`TimeoutStopSec=660`）。
9. **防火墙**（`--open-firewall`）。
10. **健康检查**：`/healthz` 然后 `/readyz`，最长 `--health-timeout` 秒。`/readyz` 为 `degraded`（缺可选扩展表）只警告。首次安装健康检查失败时没有可回滚的版本（退出码 6）：脚本打印最近日志并保持当前状态，修正 `dbtool.env` 后 `systemctl restart dbtool`，或重新运行同一条命令。
11. **收尾**：写状态文件（附录 D.3）、输出访问地址与下一步清单（goInception、nginx / `PUBLIC_URL`、必配项）。健康检查通过后、写状态文件之后执行一次**权限迁移**（`dbt-admin perm migrate --apply --yes`，把权限组 / 用户的原权限码迁移成按钮授权并标记为已迁移，幂等、只增不减；撤销脚本存在配置备份目录里；输出实时显示，最多等 2 分钟，失败只警告；`--skip-perm-migrate` 可跳过）。

以上命令都在发布包解压出的 `dbtool/` 目录里运行（`tar xzf dbtool-<版本>-linux-amd64.tar.gz && cd dbtool`）；安装之后也可以直接用 `/opt/dbtool/current/install.sh`。

**示例 A（库已初始化，手工指定密钥）**

```bash
sudo ./install.sh install \
  --database-url 'mysql://dbtool_rt:<url-encoded-password>@<mysql-host>:3306/db_tool' \
  --secret-key '<secret-key>' \
  --public-url https://dba.example.com \
  --require-goinception \
  --timezone Asia/Shanghai \
  --listen 127.0.0.1:8000
```

> **提示**：先加 `--dry-run` 只看计划。监听 `127.0.0.1` 时不需要 `--open-firewall`；nginx 在别的机器上时才需要放行端口。

**示例 B（全新安装；库 `db_tool` 已建好、基础表还没有）：一条命令完成全部**

先在当前 shell 里输入管理员密码（不回显，不要写在命令行上）：

```bash
read -rs DBT_ADMIN_PASSWORD; export DBT_ADMIN_PASSWORD
```

再安装（`--preserve-env` 让 `sudo` 保留这个变量）：

```bash
sudo --preserve-env=DBT_ADMIN_PASSWORD ./install.sh install \
  --database-url 'mysql://dbtool_rt:<url-encoded-password>@<mysql-host>:3306/db_tool' \
  --ddl-database-url 'mysql://dbtool_ddl:<url-encoded-password>@<mysql-host>:3306/db_tool' \
  --gen-secrets --bootstrap --admin-user admin \
  --public-url https://dba.example.com --require-goinception --timezone Asia/Shanghai --open-firewall
```

终端会提示“已随机生成 SECRET_KEY 与 MIRAGE_*（不显示）”：立刻备份 `/etc/dbtool/dbtool.env` 里的这几项。

**预期输出**（示例 B；文案取自 `deploy/install.sh`，`<…>` 与数值随环境而变，检查项条数因主机而异）：

```
[1/11] 预检
dbtool 环境检查（preflight；只读，不改动任何东西）
  [ OK ] 操作系统: <发行版名称>
  [ OK ] 以 root 运行
  [ OK ] systemd 可用
  [ OK ] 磁盘空间：/opt/dbtool 所在分区可用 <N> MB
  [INFO] 还没有配置 /etc/dbtool/dbtool.env（全新主机；install 时会生成）
  [ OK ] 安装包 SHA256SUMS 校验通过
  [ OK ] 元数据库可连接：users_table=absent，启用的超级用户 0（<库名等信息>）
  [INFO] 库还是空的：用 install --bootstrap 或 init-db 初始化基础表与数据
结果：OK <n>，WARN <n>，FAIL 0
      完成（<N> 秒）
[2/11] 备份当前配置
    无需备份（首次安装，没有已有配置）
[3/11] 系统用户与目录
==> 创建系统用户 dbtool
[4/11] 安装新版本（解包 + sha256 校验）
==> sha256 校验通过（SHA256SUMS）
==> 已解包到 /opt/dbtool/releases/<版本>
[5/11] 写入配置
==> 已写入配置 /etc/dbtool/dbtool.env（权限 0640，仅 root 与 dbtool 可读）
警告: 已随机生成 SECRET_KEY 与 MIRAGE_* 并写入 /etc/dbtool/dbtool.env（不在终端显示）。请立即备份该文件中的 SECRET_KEY 与 MIRAGE_*：…
[6/11] 元数据库初始化 / 建表脚本
==> 初始化元数据库：基础表 + 基础数据 + 扩展表脚本（dbt-admin bootstrap）
    …（dbt-admin 的输出：目标库与主机、文件清单、逐文件“完成”、最后的“完成：执行 <n> 个文件、<n> 条语句；本次新建 <n> 张表 …”）
==> 创建初始管理员 admin
[7/11] 部署 SOAR（SQL 优化工具）
==> 已部署 SOAR：/opt/dbtool/plugins/soar（root:root 0755，sha256 <前 12 位>）
==> 已部署 SOAR 配置 /opt/dbtool/plugins/soar.yaml（日志写到 /opt/dbtool/data/soar.log）
==> 已设置系统设置 optimizer.soar_path = /opt/dbtool/plugins/soar（服务几秒内生效，不用重启）
==> 已设置系统设置 optimizer.soar_config = /opt/dbtool/plugins/soar.yaml（服务几秒内生效，不用重启）
    SOAR 还需要一个测试库（必须是非生产库，SOAR 会在其中建临时表）：到“平台设置 › 系统配置 › SQL 优化工具”填写“SOAR 测试环境 DSN”后，智能调优里的 SOAR 才可用
[8/11] 切换版本并重启服务
[9/11] 防火墙
[10/11] 健康检查
==> 等待服务就绪（/healthz 与 /readyz，最长 60 秒）
[11/11] 收尾

dbtool 已启动（版本 <版本>）
  访问地址   https://dba.example.com
  配置文件   /etc/dbtool/dbtool.toml（监听端口等；改动用 ./install.sh reconfigure）
  环境变量   /etc/dbtool/dbtool.env（数据库密码、SECRET_KEY 等敏感项）
  版本目录   /opt/dbtool/releases/*，当前 /opt/dbtool/current
  ……
下一步清单：
  0. 立刻备份 /etc/dbtool/dbtool.env 里的 SECRET_KEY 与 MIRAGE_*（密码库 + 离线保管）：…
  1. goInception：部署（Docker，见 docs/ops-manual.md 7.1），…
  2. nginx / HTTPS：…
  3. 必配项（docs/ops-manual.md 8.3）：…；上线前做 8.4 的抽样验证
  4. 数据目录 /opt/dbtool/data（导出文件、归档暂存）请纳入主机备份；…
```

任一阶段失败时，脚本退出前打印“已完成 / 失败于 / 未执行”三行和日志位置（`/var/log/dbtool-install.log`）；各阶段幂等，修复原因后重新运行同一条命令即可。退出码含义见附录 D.2。

**验证**：

```bash
sudo ./install.sh status
```

```bash
sudo ./install.sh doctor
```

```bash
curl -fsS http://127.0.0.1:8000/healthz
```

```bash
sudo journalctl -u dbtool -n 50 --no-pager
```

预期：`status` 列出版本、服务状态、监听端口，并打印 `/healthz` 与 `/readyz` 的 JSON；`doctor` 结尾为 `结果：OK <n>，WARN <n>，FAIL 0`；`/healthz` 返回 `{"status":"ok"}`；日志里有 `已加载配置文件`（带 `file=/etc/dbtool/dbtool.toml`）与 `dbtool 启动`（带 `listen=<主机:端口>`、`来源=…`，监听来源应是配置文件而不是环境变量 `LISTEN`）两条。

> **注意**：脚本没有 `--mirage-*` 选项。`--gen-secrets` 会生成 `MIRAGE_*` 写进 `dbtool.env`；用 `--secret-key` 手工指定时，`MIRAGE_*` 不设则缺省取 `SECRET_KEY`，需要单独的 `MIRAGE_SECRET_KEY` 时追加到 `dbtool.env` 后重启（一经使用不要再改，2.6）。

### 4.2 手工二进制 + systemd

适用于不用 `install.sh` 的场景（非 CentOS 系、统一配置管理）。手工部署的布局（`/opt/dbtool/bin/…`）与 `install.sh` 的 `releases/current` 布局不同，两种方式不要混用同一个 `--prefix`。

1. 创建运行用户：

   ```bash
   useradd --system --no-create-home --shell /sbin/nologin dbtool
   ```

2. 创建程序与配置目录：

   ```bash
   install -d -m 0755 /opt/dbtool/bin /opt/dbtool/sql /etc/dbtool
   ```

3. 安装二进制：

   ```bash
   install -m 0755 dbtool /opt/dbtool/bin/dbtool
   ```

4. 放置扩展表脚本（供 3.4 的手工循环补执行用；初始化库用包内的 `dbt-admin`，见 8.1）：

   ```bash
   cp sql/*.sql /opt/dbtool/sql/
   ```

5. 创建数据目录（属主运行用户，4.5）：

   ```bash
   install -d -m 0750 -o dbtool -g dbtool /var/lib/dbtool /var/lib/dbtool/export
   ```

6. 放置监听配置。示例文件里 `port = 8866`（不是默认值 8000），按需改端口 / 监听地址：

   ```bash
   install -m 0644 dbtool.example.toml /etc/dbtool/dbtool.toml
   ```

7. 创建环境变量文件并编辑（内容见 5.5）：

   ```bash
   install -m 0640 -o root -g dbtool /dev/null /etc/dbtool/dbtool.env
   ```

`/etc/systemd/system/dbtool.service`（沙箱与停止超时等参数与 `install.sh` 的 `render_unit` 生成的单元一致；`install.sh` 生成的单元用 `WorkingDirectory=<PREFIX>` 与 `ExecStart=<PREFIX>/current/dbtool -config /etc/dbtool/dbtool.toml`，下面按手工布局改写了这两项的路径）：

```ini
[Unit]
Description=dbtool 数据库运维管理平台
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=dbtool
Group=dbtool
EnvironmentFile=/etc/dbtool/dbtool.env
ExecStart=/opt/dbtool/bin/dbtool -config /etc/dbtool/dbtool.toml
Restart=on-failure
RestartSec=5
# 停止时会等待正在执行的 SQL 工单（main.go 最长等 10 分钟），所以给足时间
TimeoutStopSec=660
LimitNOFILE=65535
NoNewPrivileges=true
ProtectSystem=full
ProtectHome=true
PrivateTmp=true
# 工作目录：手工布局用 /var/lib/dbtool（install.sh 用 <PREFIX>）。缺它时相对路径会落到 /
WorkingDirectory=/var/lib/dbtool

[Install]
WantedBy=multi-user.target
```

写好单元文件后注册并启动：

```bash
systemctl daemon-reload && systemctl enable --now dbtool
```

**这些沙箱参数对功能的影响**（与 `docs/deployment.md` §6.3 一致，并按 `install.sh` 实际内容核对）

| 参数 | 影响 | 处理 |
|---|---|---|
| `ProtectSystem=full` | `/usr`、`/boot`、`/etc` 对服务只读 | 写 `/etc` 下的 goInception `config.toml` 需要 `ReadWritePaths=/etc/goinception`（drop-in，7.1.3）。`/var`、`/opt` 下的目录不受影响 |
| `ProtectHome=true` | `/home`、`/root`、`/run/user` 不可访问 | 不要把 `EXPORT_BASE_DIR` / `ARCHIVE_SPOOL_DIR` / 本地备份存储放在 `/home` |
| `PrivateTmp=true` | 服务看到的 `/tmp` 是私有的，重启即清空 | **不要**把归档暂存放 `/tmp`。`sqlopt` 外部工具用的临时目录用完即删，不受影响 |
| `NoNewPrivileges=true` | `sudo` 提权失效 | 页面“使用 sudo -n”重启 goInception 会失败，见 7.1.4 |
| 无 `WorkingDirectory` | 工作目录是 `/`；默认 `EXPORT_BASE_DIR=.`，导出目录会解析到 `/downloads/DataExportFile/`，归档暂存到 `/archive-spool`，运行用户无权写 | **必须设置 `EXPORT_BASE_DIR`（及 `ARCHIVE_SPOOL_DIR`）或 `WorkingDirectory`**（4.5）【代码+推断】 |

### 4.3 Docker

**按 `deploy/Dockerfile` 实际内容**【代码】：两阶段构建。构建阶段 `golang:1.25`，`CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/dbtool ./cmd/server`；运行阶段 `gcr.io/distroless/static-debian12:nonroot`，只拷入 `/dbtool`，`EXPOSE 8000`，`ENTRYPOINT ["/dbtool"]`。

**镜像里没有的东西**（如实列出）：
- 没有 `WORKDIR`、`VOLUME`、`HEALTHCHECK`，也没有显式的 `USER` 指令（`nonroot` 标签的基础镜像自带非 root 用户，通常 uid 65532【未验证】）；
- 没有 `deploy/sql/*.sql`（建表要在别处用 `mysql` 客户端执行）、没有 `mysql` 客户端、没有 shell、没有 `docker` 命令；
- 仓库里没有 `.dockerignore`（`COPY . .` 会把 `.git`、`.tmp`、`dist` 等整个送进构建上下文；构建阶段不进最终镜像，但**请先确认 `.tmp/` 里没有凭据**或自行加 `.dockerignore`）；
- 没有默认 `TZ` 变量；二进制内嵌了时区库（2.2），设置 `-e TZ=Asia/Shanghai` 即可生效，建议实测一次页面时间【未验证】。

```bash
docker build -f deploy/Dockerfile -t dbtool:<tag> .
```

运行示例【建议，未在真实环境验证】：

导出目录 / 归档暂存必须挂卷，并让容器用户可写（`nonroot`）：

```bash
mkdir -p /srv/dbtool/data && chown 65532:65532 /srv/dbtool/data
```

```bash
docker run -d --name dbtool --restart unless-stopped \
  -p 127.0.0.1:8000:8000 \
  --env-file /etc/dbtool/dbtool.env \
  -e TZ=Asia/Shanghai \
  -e EXPORT_BASE_DIR=/data \
  -e ARCHIVE_SPOOL_DIR=/data/archive-spool \
  -v /srv/dbtool/data:/data \
  --stop-timeout 660 \
  dbtool:<tag>
```

要点：
- `--stop-timeout 660` 对应 systemd 的 `TimeoutStopSec=660`（默认 10 秒会提前 `SIGKILL`，执行中的工单会卡在“执行中”）。
- 改端口：设环境变量 `LISTEN=:9000`，或挂载 toml（`-v ./dbtool.toml:/dbtool.toml -e DB_TOOL_CONFIG=/dbtool.toml`），并同步改 `-p` 映射。
- `--env-file` 的格式是 `KEY=VALUE`，**不会像 systemd 的 `EnvironmentFile` 那样处理引号**：值里不要加双引号。`install.sh` 生成的 `/etc/dbtool/dbtool.env` 的值是带双引号的，**不能原样**用于 `docker --env-file`，需要另存一份去掉引号的文件。
- **goInception 页面重启在此镜像里不可用**（无 `docker` 命令，也没挂 `docker.sock`）。**不要在“部署与重启”里填容器名**；配置文件只能“仅保存”，之后手工重启 goInception（7.1.5）。想要页面重启就得自己扩展镜像并挂载 `/var/run/docker.sock`。

> **警告**：挂载 `docker.sock` 等于把宿主机 Docker 的控制权交给该容器。
- 健康检查在容器外做：`curl -fsS http://127.0.0.1:8000/healthz`（镜像里没有 curl）。

### 4.4 构建与发布包 `make dist` / `deploy/package-centos7.sh`

`make dist` 只是调用 `deploy/package-centos7.sh`（检查 → 编译 → 复制 → 自检 → 打包 → 校验和 → 验收），两种方式产物一致；没有 `make` 的机器（Windows Git Bash）直接用脚本。完整说明见 `deploy/PACKAGING.md`。

有 `make` 时（产物 `dist/dbtool-<版本>-linux-amd64.tar.gz` 与 `.sha256`；`MAKE_RUN=1` 才额外生成 `.run`）：

```bash
make dist
```

arm64 服务器：

```bash
make dist ARCH=arm64
```

指定版本号（不指定时取 `deploy/RELEASE_VERSION`，再回退 `git describe --tags --always --dirty`，最后 `0.0.0-<日期>`）：

```bash
make dist VERSION=1.39
```

没有 `make` 时（如 Windows Git Bash）直接用脚本：

```bash
bash deploy/package-centos7.sh
```

打包前先跑 `go vet ./...` 与 `go test -count=1 ./...`，失败就不打包：

```bash
RUN_TESTS=1 bash deploy/package-centos7.sh
```

只改了文档 / 脚本时复用上次编译的二进制（改了 Go 代码、模板、静态文件不要用）：

```bash
SKIP_BUILD=1 bash deploy/package-centos7.sh
```

Makefile 目标【代码】：`build`（只编译 `bin/dbtool`，不含 `dbt-admin`）、`run`、`test`、`fmt`、`vet`、`dist`。`dist` 做的事：交叉编译 `linux/$(ARCH)`（`CGO_ENABLED=0`，`-ldflags "-s -w -X main.version=<版本>"`）的 `dbtool`、`dbt-admin` → 拷入 `install.sh`、`sql/*.sql`、`bootstrap/*.sql`（`deploy/bootstrap` 存在时）、`env.example`、`dbtool.example.toml`、`docs/{deployment,user-guide,ops-manual}.md`，以及存在时的 `docs/{bootstrap-design,admin-cli,one-click-deploy,charset-policy,release-notes,settings-design,settings-reference}.md` → 生成包内 `VERSION`（`version= / build_time= / commit= / arch=`）与 `SHA256SUMS`（覆盖包内每个文件）→ 打 tar.gz → 把 `deploy/run-header.sh`（占位符替换）与 tar.gz 拼成自解压 `.run` → 生成 `dbtool-<版本>-linux-<arch>.sha256`（记录 `.run` 与 tar.gz 的哈希）。

**包内容**：`dbtool/{dbtool, dbt-admin, install.sh, VERSION, SHA256SUMS, bootstrap/, sql/, env.example, dbtool.example.toml, goinception.config.toml, docs/, Dockerfile}`（`Dockerfile` 是发布包版，直接 `COPY` 包内二进制；CentOS 上用 `install.sh`，不需要它。）tar 里 `dbtool` / `dbt-admin` / `install.sh` 固定 0755，属主 0:0，其余文件 0644（Windows 上构建也一样）。脚本在打包前后各自检：必要文件齐全、脚本语法与 LF 换行、没有密钥 / 本机配置混进包、ELF 魔数与架构、解压后 `SHA256SUMS` 与 `.run --check`，任何一项失败都不会留下“看起来成功”的产物。包内目录布局与源码树一致（`bootstrap/`、`sql/` 同级），所以包里直接运行 `./dbt-admin bootstrap`（默认脚本根目录 = 可执行文件所在目录）即可；源码树里用 `--sql-root deploy`。

**可选：`.run` 自解压单文件**（默认不生成，`MAKE_RUN=1` 才有；bash 头 + `__ARCHIVE__` 标记行 + tar.gz 载荷）：

| 用法 | 作用 |
|---|---|
| `./dbtool-<版本>-linux-amd64.run --info` | 版本 / 架构 / 构建时间 / 提交 / 载荷 sha256 / 包内文件清单 |
| `./dbtool-<版本>-linux-amd64.run --check` | 校验载荷 sha256，并解到临时目录校验包内 `SHA256SUMS`；不安装 |
| `./dbtool-<版本>-linux-amd64.run --extract DIR` | 只解包到 `DIR/dbtool/` |
| `sudo ./install.sh <子命令> [选项]` | 先校验载荷 sha256，再解到临时目录，其余参数原样传给包内 `install.sh`，结束后清理临时目录 |

**发布前核对**：`cd dist && sha256sum -c dbtool-<版本>-linux-amd64.sha256`；`tar tzf dist/dbtool-<版本>-linux-amd64.tar.gz | head -30`；把 `.sha256` 的内容记在发布单里；`go test ./...`（`make test`）与 `bash scripts/install-test/run.sh` 在构建机跑一遍。

### 4.5 目录与文件布局

| 路径 | 属主 / 权限 | 用途 | 需备份？ |
|---|---|---|---|
| `/opt/dbtool/current` | root，符号链接 | 指向 `releases/<版本>`（相对链接，原子切换）；systemd 的 `ExecStart=/opt/dbtool/current/dbtool` | 否 |
| `/opt/dbtool/releases/<版本>/` | root:root 0755 | 一个版本一个目录：`dbtool`、`dbt-admin`、`sql/`、`bootstrap/`、`docs/`、`VERSION`、`SHA256SUMS`、`install.sh` 等 | 否（保留安装包即可） |
| `/opt/dbtool/current/dbt-admin`、`/opt/dbtool/current/bootstrap/*.sql` | root:root | 初始化 / 管理员命令行工具与基础表、基础数据脚本（`dbt-admin bootstrap --sql-root /opt/dbtool/current` 读取） | 否 |
| `/opt/dbtool/data/export`、`/opt/dbtool/data/archive-spool` | dbtool 0750 | `install.sh` 创建（只在缺失时），并在 `dbtool.env` 里未设置时写入 `EXPORT_BASE_DIR` / `ARCHIVE_SPOOL_DIR` 指向它们 | 见下面两行 |
| `/etc/dbtool/dbtool.toml` | root 0644 | 监听地址 / 端口（不含密钥） | **是** |
| `/etc/dbtool/dbtool.env` | root:dbtool 0640 | `DATABASE_URL`、`SECRET_KEY`、`MIRAGE_*`、`TZ`、开关等 | **是（最重要，务必备份到密码库）** |
| `/etc/dbtool/.install-state` | root 0640 | 安装状态（版本、`PREFIX`、运行用户、端口、是否由脚本创建用户 / 放行防火墙、单元 sha256；字段见附录 D.3）；不含机密 | 可重建 |
| `/etc/systemd/system/dbtool.service` | root 0644 | 单元文件 | 是（配置管理） |
| `/etc/systemd/system/dbtool.service.d/override.conf` | root | 【建议】drop-in：`ReadWritePaths=`、`NoNewPrivileges=false` 等；**脚本永不改动** | 是 |
| `/var/log/dbtool-install.log` | root 0640 | 安装日志（每步一行，不含机密） | 否 |
| `/var/lock/dbtool-install.lock` | root | 并发锁 | 否 |
| `${EXPORT_BASE_DIR}/downloads/DataExportFile/` | dbtool 0750 | 数据导出 zip（`export.local_path` 缺省值） | 按需 |
| `${ARCHIVE_SPOOL_DIR}`（缺省 `${EXPORT_BASE_DIR}/archive-spool`） | dbtool 0750 | 归档文件备份暂存；**必须持久目录**；强杀后残留 `*.tmp`（服务启动时扫描并提示，**不自动上传或删除**，需人工补传） | **是（含尚未上传的备份）** |
| 归档备份存储 `local`/`nfs` 的根目录 | dbtool | 最终归档备份文件 + `manifest.json` | **是** |
| goInception `config.toml` 所在目录 | 容器 / 部署决定 | 页面读写的配置；同目录有 `.dbtool.bak`（**含密码明文**，权限同原文件）、`.giconfig-*.tmp`（临时） | 是（页面也有版本历史，在 `dbt_gi_config_version`） |
| `/var/lib/dbtool`（手工部署的【建议】） | dbtool 0750 | 手工部署（4.2）时的 `WorkingDirectory` 与数据根目录；`install.sh` 不使用它（用 `<PREFIX>/data`，`WorkingDirectory=<PREFIX>`） | — |

> **警告**：`/opt/dbtool/data/archive-spool` 里可能有删源之后、上传之前的归档备份，不要随意清理。

---

## 5. 配置参考

### 5.1 配置来源与优先级

| 来源 | 内容 | 生效方式 |
|---|---|---|
| 命令行 `-config <路径>` | 指定 `dbtool.toml`（除 `-version` 打印版本并退出外，**唯一**的命令行参数） | 文件必须存在，否则启动失败 |
| 环境变量（systemd `EnvironmentFile=/etc/dbtool/dbtool.env`） | 绝大多数设置，含全部敏感项 | **仅启动时读取，改后 `systemctl restart dbtool`** |
| `dbtool.toml` | `[server]` 的 `host`、`port`，`[site]` 的 `version`、`icp`、`icp_url`（不含密钥） | 重启 |
| `dbt_setting` 表（系统配置页里改） | 业务开关与阈值 | 保存后本进程立即生效，其它进程最多 3 秒；见 5.6 |

**优先级**【代码】`internal/config/config.go`：
1. 监听地址：环境变量 `LISTEN` > `dbtool.toml` > 默认 `:8000`；两者都给且不一致时启动日志警告“环境变量 LISTEN 覆盖了配置文件”。
2. 配置文件查找：`-config` > 环境变量 `DB_TOOL_CONFIG` > `./dbtool.toml` > 可执行文件所在目录的 `dbtool.toml` > `/etc/dbtool/dbtool.toml`；前两种指定的文件必须存在；都没有则全用环境变量与默认值。
3. toml 用严格模式：**键名拼错 / 不认识的键 / 语法错误 / 端口不在 1–65535 都会启动失败**（报文件名和行号），不会悄悄用默认值。
4. 空字符串的环境变量等同于没设置；布尔量用 `strconv.ParseBool`（`1/t/true/0/f/false` 等），非法值回退为默认值**不报错**。（`SLOWLOG_COLLECT_ENABLE` 例外，见下表。）

### 5.2 环境变量全表

> “必填”指缺了会启动失败或功能不可用。“重启”：全部环境变量都只在启动时读取，改后都要重启，表中不再逐行重复。敏感项在日志里不会打印，但在 `dbtool.env`（0640）里是明文。

| 变量 | 默认 | 作用 | 必填 | 敏感 |
|---|---|---|---|---|
| `DATABASE_URL` | 无 | 元数据库，仅支持 `mysql://用户:密码@主机:3306/库名`；端口缺省 3306；DSN 固定 `charset=utf8mb4&parseTime=True&loc=Local`，URL 里的查询参数**被忽略**。本手册库名为 `db_tool` | **是**（缺则启动失败：`DATABASE_URL 未设置`） | 是 |
| `SECRET_KEY` | 公开的开发默认值（`config.go` 的 `defaultSecretKey`，也出现在 `configs/env.example`） | 会话 Cookie 与 JWT 的 HMAC 密钥；SSO/2FA 中间态 Cookie 的派生密钥；`MIRAGE_SECRET_KEY` 缺省值 | **是（生产）**：不设则用公开默认值，程序**不报错也无启动警告**，会话 / JWT 可被伪造 | 是 |
| `MIRAGE_SECRET_KEY` | 缺省取 `SECRET_KEY` | mirage 加密密钥；**至少 32 字符**，否则启动失败（`store.Open`）；归档文件加密密钥由它派生 | 否（建议单独设置；`--gen-secrets` 会生成，一经使用不要改） | 是 |
| `MIRAGE_CIPHER_MODE` | `ECB` | 只支持 `ECB` / `CBC` | 否 | 否 |
| `MIRAGE_CIPHER_IV` | `1234567890abcdef` | CBC 模式的向量，**必须正好 16 字节**（ECB 模式不使用，但 `--gen-secrets` 仍会生成一个随机值） | 否 | 视为敏感 |
| `PUBLIC_URL` | 由监听地址推出：`http://127.0.0.1:<端口>`（监听 `0.0.0.0` 或空主机时用 127.0.0.1）；末尾的 `/` 会被去掉 | SSO 回调、通知里的工单链接、性能 / 巡检 / 归档通知链接（邮件只在显式配置时才放链接） | **生产必填**（不设则 SSO 回调指向本机） | 否 |
| `DEFAULT_LANG` | `zh-CN` | 界面默认语言（`zh-CN` / `en`，也接受 `zh`、`zh_CN`、`en-US` 等写法）：只影响还没选过语言的访客，用户在页面顶栏切换后以自己的选择为准（记在 cookie `dbtool_lang`）。**其它取值会使启动失败**。 | 否 | 否 |
| `LISTEN` | 无（最终默认 `:8000`） | 监听 `主机:端口`；**优先于 toml**。一般不设，端口写 toml | 否 | 否 |
| `DB_TOOL_CONFIG` | 无 | 配置文件路径（`-config` 优先于它）；设了则文件必须存在 | 否 | 否 |
| `REQUIRE_GOINCEPTION` | `false` | `true` 时：未配置 goInception 就拒绝查询（表级权限校验 / 脱敏）、SQL 检查和原生执行；`false` 时降级（词法解析、原生检查 / 执行）。已配置但出错一律失败，不降级 | **生产建议 `true`** | 否 |
| `BATCH_DB_TYPES` | `mssql` | 允许批量上线 / 批量导出的实例类型，逗号分隔（如 `mssql,mysql`） | 否 | 否 |
| `EXPORT_BASE_DIR` | `.`（进程当前目录） | `export.local_path` 为相对路径时的基准目录；归档暂存缺省目录的基准 | **事实上必设**：手工单元无 `WorkingDirectory` 时缺省会落到 `/`（`install.sh` 已设 `WorkingDirectory` 并默认写入 `<PREFIX>/data/export`） | 否 |
| `ARCHIVE_SPOOL_DIR` | `$EXPORT_BASE_DIR/archive-spool` | 归档文件备份的本地暂存目录（删源在落盘之后、上传之前，必须持久目录） | 用文件备份时建议显式设 | 否 |
| `GOINCEPTION_CONFIG_FILE` | 无 | goInception `config.toml` 的**绝对路径**；设了页面该项只读，且优先于 `goinception.config_file` | 要在页面改配置才需要 | 否 |
| `SLOWLOG_COLLECT_ENABLE` | **开** | 只有 `0/false/no/off`（不区分大小写）才关闭整个慢日志采集，其它值（含不设）都是开；真正采哪些实例由各实例上的“慢日志采集”开关（`is_enable`）决定；**MySQL 与 Redis 慢日志共用此开关** | 否 | 否 |
| `SLOW_COLLECT_INTERVAL` | **10** 秒（代码），夹在 5–3600 | MySQL 慢日志采集间隔 | 否 | 否 |
| `SLOW_COLLECT_WORKERS` | 2，夹在 1–8 | 采集并发实例数 | 否 | 否 |
| `SLOWLOG_COLLECT_BACKFILL_DAYS` | 7，夹在 0–365 | 首次采集回溯天数；0 = 从日志表最开头 | 否 | 否 |
| `REDIS_SLOWLOG_COLLECT_INTERVAL` | 60 秒，夹在 10–3600 | 同一 Redis 实例两次采集的最小间隔 | 否 | 否 |
| `REDIS_INSPECT_BIGKEY_SAMPLE` | 0（关），上限 5000 | Redis 巡检大 key 抽样数（页面选项可覆盖） | 否 | 否 |
| `SSH_TUNNEL_LISTEN_HOST` | `127.0.0.1` | SSH 隧道本地转发器的监听地址。**保持默认**；只有 goInception 等在别的主机上、要连本服务转发的端口时才改，且必须用防火墙限制来源 | 否 | 否 |
| `SSH_TUNNEL_ADVERTISE_HOST` | `127.0.0.1` | 写进连接信息的转发器地址（与上一项配合，让 goInception 能连到本机转发端口） | 否 | 否 |
| `SSH_TUNNEL_IDLE_TIMEOUT` | `10m`（不得小于 `1m`） | 转发器无连接多久后关闭监听和 SSH 连接 | 否 | 否 |
| `DEBUG` | `false` | `true`：gin 调试模式（打印路由表）+ **GORM 记录每条 SQL（含参数）**；生产禁用 | 否 | 否 |
| `TZ` | 取系统时区（`/etc/localtime`），否则 UTC | Go 运行时读取；决定读写元数据库时间的时区（`loc=Local`），**一经使用不要改**（2.5）。`install.sh` 首次安装写入 | **是** | 否 |
| `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` | 无 | Go 标准行为：SSO 客户端、S3 归档存储、通知机器人都会遵守 | 出站要走代理时 | 视情况 |

**不是配置、不要设置的变量**：`ARCHIVE_IT_DSN`、`GICONFIG_HELPER`（仅测试用）；`cmd/demo` 的 `LISTEN`（演示程序）。

**只给命令行工具用的变量**（不是服务的配置）：

| 变量 | 使用者 | 作用 |
|---|---|---|
| `DBT_DATABASE_URL`、`DBT_SECRET_KEY`、`DDL_DATABASE_URL`、`DBT_ADMIN_PASSWORD`、`DBT_HEALTH_TIMEOUT` | `install.sh` | 见附录 D.1 |
| `DATABASE_URL`（`mysql://…`，可带 `?tls=`，取 `true` / `false` / `skip-verify` / `preferred`）、`DBT_ADMIN_PASSWORD`、`DBT_SETTING_VALUE`、`SECRET_KEY` / `MIRAGE_*`、`OLD_MIRAGE_SECRET_KEY` / `OLD_SECRET_KEY` / `OLD_MIRAGE_CIPHER_MODE` / `OLD_MIRAGE_CIPHER_IV` | `dbt-admin` | 见 `docs/admin-cli.md` §2、§6 |

外部工具（SOAR）的子进程**不继承**服务环境，只获得 `PATH`、`HOME`/`TMPDIR`（新建临时目录）、`LANG`/`LC_ALL=C.UTF-8`。

### 5.3 `dbtool.toml`

```toml
[server]
port = 8000            # 1–65535，默认 8000；<1024 需要 root 或 CAP_NET_BIND_SERVICE
# host = "0.0.0.0"     # 省略/留空 = 所有网卡；只给本机 nginx 用时写 "127.0.0.1"

[site]                 # 整段可省略
# version = "1.05"                        # 页面左下角显示的版本号；省略/留空 = 程序自带的版本；数字开头会自动补 v
# icp = "京ICP备12345678号-1"              # 页面底部显示的 ICP 备案号；省略/留空 = 不显示
# icp_url = "https://beian.miit.gov.cn/"  # 备案号链接；省略 = 工信部备案查询站；必须以 http:// 或 https:// 开头
```
当前有 `[server]` 的 `port`、`host` 与 `[site]` 的 `version`、`icp`、`icp_url`。`install.sh` 生成的 `dbtool.toml` 只含 `[server]`，`[site]` 需要手工加；之后升级与 `reconfigure` 保留它。改后 `systemctl restart dbtool`；改端口时同步改防火墙 / nginx 上游 / `PUBLIC_URL`。

### 5.4 示例文件逐项核对

两个示例文件都**不是生产配置**：`install.sh` 不读取它们（它自己生成 `dbtool.env` 与 `dbtool.toml`），只是随包附带供参考。逐项核对结果如下，**不要原样照抄**。

**`configs/dbtool.example.toml`**（包内 `dbtool.example.toml`）

| 项 | 示例里的写法 | 实际行为 / 注意 |
|---|---|---|
| `[server] port` | `port = 8866` | 代码默认是 **8000**（5.3）；示例写的是 8866，手工 `cp` 后端口会变成 8866，需要自行确认或修改 |
| `[server] host` | 注释掉的 `"0.0.0.0"` | 省略 = 所有网卡；只给本机 nginx 用时写 `"127.0.0.1"` |
| `[site]` | 三项都是注释掉的示例（`version`、`icp`、`icp_url`） | 都省略时：版本号显示程序自带版本、不显示备案号；要显示备案号就取消注释并填真实值，不要原样照抄示例号码 |
| 注释 | 写明查找顺序与“环境变量优先” | 与 5.1 一致 |

**`configs/env.example`**（包内 `env.example`）

| 行 | 内容 | 运维注意 |
|---|---|---|
| `DEBUG=true` | 示例里是**开启** | 照抄会让每条 SQL（含参数）进日志；生产必须删掉或设 `false` |
| `# LISTEN=:8000` | 注释 | 端口建议写 toml |
| `SECRET_KEY=<与代码 defaultSecretKey 相同的公开值>` | **就是公开的开发默认值** | 生产必须替换（`dbt-admin gen-secrets`） |
| `DATABASE_URL=mysql://dbtool_rt:password@127.0.0.1:3306/db_tool` | 占位示例 | 密码换成真实值（URL 编码） |
| `# MIRAGE_*` | 注释 | 见 2.6 |
| `# REQUIRE_GOINCEPTION=true`、`# PUBLIC_URL`、`# BATCH_DB_TYPES`、`# EXPORT_BASE_DIR`、`# GOINCEPTION_CONFIG_FILE`、`# DEFAULT_LANG` | 注释 | 对应 5.2 |
| `# SLOW_COLLECT_INTERVAL=60` | 注释里写 60 | 与代码默认 **10 秒**不一致；范围 5–3600 |
| 中部的“建表脚本编号列表” | 只列到 019 | 已过时，以 `deploy/sql` 实际为准（附录 B） |
| 末尾的“其余功能开关”说明 | 提到 `sql_config` 表、`perf_go_jobs`、`partman_auto_run`、`partman_auto_drop`、`partman_auto_interval`、`soar` / `sqladvisor` / `soar_test_dsn` / `soar_config` | 这些是旧键名；现行键为 `perf.jobs`、`partition.auto_run`、`partition.auto_drop`、`partition.auto_interval_minutes`、`optimizer.*`，存放在 `dbt_setting`，见 [settings-reference.md](settings-reference.md) |

> **注意**：上面几处示例文件与代码不一致的地方属于配置文件本身的注释 / 取值，本手册只做记录，不影响程序行为。

### 5.5 生产推荐配置样例

`/etc/dbtool/dbtool.toml`
```toml
[server]
port = 8000
host = "127.0.0.1"       # 只给本机 nginx；nginx 在别的机器上则不写 host 并用防火墙限制来源
```

`/etc/dbtool/dbtool.env`（权限 0640，root:dbtool；systemd 的 `EnvironmentFile` 语法，值可加双引号）

```ini
DATABASE_URL="mysql://dbtool_rt:<url-encoded-password>@<mysql-host>:3306/db_tool"
SECRET_KEY="<secret-key>"                 # dbt-admin gen-secrets 生成
MIRAGE_SECRET_KEY="<mirage-secret-key>"   # ≥ 32 字符；一经使用不得更改（--gen-secrets 会生成）
MIRAGE_CIPHER_MODE="ECB"
MIRAGE_CIPHER_IV="<16 bytes>"
TZ="Asia/Shanghai"                        # 一经使用不要改
PUBLIC_URL="https://dba.example.com"
REQUIRE_GOINCEPTION=true
# 下面两项：install.sh 安装时默认写 /opt/dbtool/data/export 与 /opt/dbtool/data/archive-spool；手工部署（4.2）用自己的持久目录
EXPORT_BASE_DIR="/var/lib/dbtool/export"
ARCHIVE_SPOOL_DIR="/var/lib/dbtool/archive-spool"
GOINCEPTION_CONFIG_FILE="/opt/dbtool/goinception/config.toml"
# BATCH_DB_TYPES="mssql"
# 后台任务（可选）：慢日志采集默认开启，不需要时整体关闭
# SLOWLOG_COLLECT_ENABLE=0                 # 默认开启（实例上的“慢日志采集”开关决定采哪些实例）；设成 0 整体关闭
# SLOW_COLLECT_INTERVAL=60                  # 代码默认 10 秒，对大量实例可调大
```
**验证**：`systemd-analyze verify /etc/systemd/system/dbtool.service`（无输出）；启动后 `journalctl -u dbtool -n 20`。

> **注意**：不要在共享终端执行 `systemctl show dbtool -p Environment`，它会显示环境变量，包括密钥。

### 5.6 在线配置项（系统设置）

业务开关与阈值（在线查询超时、导出行数、审批与安全策略、各后台任务开关……）不在环境变量里，存在元数据库 `dbt_setting`（键 `<领域>.<名称>`，只存显式设置过的项，没有行 = 用代码里登记的默认值；敏感项 mirage 加密）。超级用户在“平台设置 › 系统配置”里按分组修改，保存后本进程立即生效，其它进程最多 3 秒。**全部项目、默认值与取值范围见 [settings-reference.md](settings-reference.md)**（由登记表生成）；命令行 `dbt-admin settings list` / `get` / `set` / `reset` 见 `docs/admin-cli.md` §6.3（没有 `import`：旧版的设置导入在 1.31 已移除）；设计见 [settings-design.md](settings-design.md)。全新部署不需要预先设置，按 8.3 的清单在页面里配置即可。

---

## 6. 反向代理与 HTTPS

### 6.1 dbtool 对代理的要求（均来自代码）

| 要求 | 原因 / 依据 |
|---|---|
| 透传 `X-Forwarded-Proto: https` | 会话 Cookie `dba_session`、`dba_login_csrf`、SSO/2FA 中间态 Cookie 的 `Secure` 标记只在 `r.TLS != nil` 或 `X-Forwarded-Proto == "https"` 时设置（`internal/server/session.go`、`internal/authx/http.go`）。缺了它，Cookie 不带 `Secure` |
| `PUBLIC_URL=https://dba.example.com` | SSO 回调地址、通知里的工单链接都用它。**不设时被填成 `http://127.0.0.1:<端口>`**（`internal/config/config.go`），SSO 回调会指向本机而失败 |
| 用 `proxy_set_header X-Forwarded-For $remote_addr;`（覆盖，**不要**用 `$proxy_add_x_forwarded_for`） | 代码里没有调用 `SetTrustedProxies`，gin 默认信任所有来源并取 `X-Forwarded-For` 最左一项作客户端 IP（该项由客户端可控）。该 IP 用于：登录限流键（用户名 + IP 组合）、注册限流、审计表 `dbt_audit_ext` 的来源 IP、水印 `{IP}`、访问日志 |
| 监听端口只对 nginx 开放 | 同上：若监听端口可被直接访问，客户端可伪造 `X-Forwarded-For`，绕过按 IP 的限流并污染审计 IP。账号锁定按用户名，不受影响。做法：`dbtool.toml` 写 `host = "127.0.0.1"`，或防火墙只放行 nginx 的来源 |
| 不要改写/追加 CSP | dbtool 自己设置：`default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; worker-src 'self' blob:; frame-ancestors 'none'`，并设 `X-Frame-Options: DENY`、`X-Content-Type-Options: nosniff`、`Referrer-Policy: same-origin`。nginx 如再加 CSP 会出现两条取交集，可能使 ACE 编辑器的 blob worker 失效 |
| HSTS 由代理加 | 代码**不设** HSTS |
| 不需要 WebSocket / SSE | 代码和前端里未发现 WebSocket、EventSource；进度靠轮询 |

### 6.2 nginx 完整示例【建议，未在真实环境验证】

```nginx
# /etc/nginx/conf.d/dbtool.conf
upstream db_tool {
    server 127.0.0.1:8000;          # 与 dbtool.toml 的 [server] port 一致
    keepalive 16;
}

server {
    listen 80;
    server_name dba.example.com;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl http2;
    server_name dba.example.com;

    ssl_certificate     /etc/nginx/ssl/dba.example.com.crt;
    ssl_certificate_key /etc/nginx/ssl/dba.example.com.key;
    ssl_protocols       TLSv1.2 TLSv1.3;
    ssl_session_timeout 1d;
    add_header Strict-Transport-Security "max-age=31536000" always;   # HSTS：确认全站都走 https 再加 includeSubDomains

    # 请求体：程序自己的上限是 SQL 优化分析请求 12MB（其中上传文本 5MB，internal/sqlopt/types.go）、
    # 工单相关请求 8MB（internal/server/sql_handlers.go）；代理的上限不要比它们小
    client_max_body_size 16m;

    # 访问日志/错误日志（注意：SSO 回调 URL 的查询串含授权码/ticket；dbtool 自己的访问日志已对 /login/sso/ 打码，
    # nginx 默认 access_log 不会，建议为该路径关闭或自定义日志格式不记 $args）
    access_log /var/log/nginx/dbtool.access.log;
    error_log  /var/log/nginx/dbtool.error.log warn;

    location ^~ /login/sso/ {
        access_log off;                           # 避免把 code/ticket/state 写进代理日志
        proxy_pass http://db_tool;
        include /etc/nginx/dbtool-proxy.inc;
    }

    location ^~ /static/ {
        proxy_pass http://db_tool;               # dbtool 对静态资源返回 Cache-Control: no-cache
        include /etc/nginx/dbtool-proxy.inc;
        # 如需代理侧缓存：proxy_cache + proxy_cache_revalidate on; 升级后需清缓存（静态文件内嵌在二进制里）
    }

    location / {
        proxy_pass http://db_tool;
        include /etc/nginx/dbtool-proxy.inc;
        proxy_read_timeout 300s;                  # 数据导出、归档试运行等较慢的请求
        proxy_send_timeout 300s;
    }
}
```

```nginx
# /etc/nginx/dbtool-proxy.inc
proxy_http_version 1.1;
proxy_set_header Connection "";
proxy_set_header Host              $host;
proxy_set_header X-Real-IP         $remote_addr;
proxy_set_header X-Forwarded-For   $remote_addr;      # 覆盖而不是追加
proxy_set_header X-Forwarded-Proto $scheme;           # 必须
proxy_buffering on;
```

限流建议【建议】：dbtool 自带登录失败限流（同一 `用户名|IP` 10 分钟内 5 次，**进程内**，重启清零）和账号锁定（DB 内，阈值 `security.login_lock_attempts`/`security.login_lock_seconds`）。再在 nginx 对登录入口做一层：

```nginx
limit_req_zone $binary_remote_addr zone=dbt_login:10m rate=10r/m;
location = /login          { limit_req zone=dbt_login burst=10 nodelay; proxy_pass http://db_tool; include /etc/nginx/dbtool-proxy.inc; }
location = /api/v1/auth/login { limit_req zone=dbt_login burst=10 nodelay; proxy_pass http://db_tool; include /etc/nginx/dbtool-proxy.inc; }
```

**验证**：

```bash
nginx -t && systemctl reload nginx
```

```bash
curl -sI https://dba.example.com/healthz
```

```bash
curl -sI https://dba.example.com/login
```

预期：`/healthz` 返回 200（响应体 `{"status":"ok"}`），响应头里有 CSP、`X-Frame-Options`，以及 nginx 加的 HSTS；`/login` 的 `Set-Cookie: dba_login_csrf=…` 带 `Secure`。浏览器登录后在开发者工具里确认 `dba_session` 带 `Secure; HttpOnly; SameSite=Lax`。

### 6.3 CSP 与代理头注意事项

- 登录后页面不能出现 CSP 报错（控制台 `Refused to ...`）。常见原因：代理又加了 CSP、或通过非 `'self'` 域名（例如用 IP 访问另一域名）加载资源。
- 页面 `frame-ancestors 'none'`：不能被 iframe 嵌入（包括内网门户）。
- `Referrer-Policy: same-origin`：跳转到 IdP 时不会带 Referer。

### 6.4 `/healthz` 与监控探活

【代码】`GET /healthz` 固定返回 `200 {"status":"ok"}`，**不检查数据库**，不需要登录，也不写审计 IP；含义：只证明进程在监听，元数据库不通时仍返回 200。

【代码】`GET /readyz` 是**就绪探针**（不需要登录）：检查元数据库可达（`Ping`，2 秒超时）和 15 张核心表是否都在，再检查 24 张扩展表（清单以 `internal/server/ready.go` 的 `requiredTables` / `optionalTables` 为准）；缺扩展表只算**降级**，对应功能不可用，服务仍可用（环境字典表 `dbt_environment`，`270_environment.sql`，**不在**这份可选表清单里：缺它时 `/readyz` 不会降级，环境只有内置的生产 / 测试，见附录 B）。响应：

```json
{"status":"ok|degraded|fail","checks":[{"name":"metadb","ok":true},{"name":"required_tables","ok":true},{"name":"optional_tables","ok":true,"detail":"缺少可选扩展表…"}],"degraded":["dbt_redis_rule"]}
```

核心表：`dbt_sql_users`、`dbt_setting`、`dbt_sql_instance`、`dbt_resource_group`、`dbt_auth_group`、`dbt_workflow_audit`、`dbt_workflow_audit_setting`、`dbt_workflow_audit_detail`、`dbt_workflow_log`、`dbt_sql_workflow`、`dbt_sql_workflow_content`、`dbt_query_privileges_apply`、`dbt_query_privileges`、`dbt_query_log`、`dbt_audit_log`。

`ok` / `degraded` 返回 200，`fail`（元数据库不可达、超时，或缺核心表）返回 **503**。`detail` 只含固定描述和表名，不回显驱动错误（可能带主机 / 账号），完整错误写服务日志（`就绪探针：…`）；结果缓存 2 秒，免登录访问不会给元数据库加压。负载均衡 / 容器编排的就绪检查用 `/readyz`，存活检查仍用 `/healthz`。

### 6.5 HTTP 访问日志

【代码】`gin` 的访问日志写到标准输出（systemd 下进 journal），格式 `[GIN] 时间 | 状态码 | 耗时 | 客户端IP | 方法 路径`；`/login/sso/` 路径的查询串被替换为 `?…`（`internal/server/accesslog.go`）。

### 6.6 多实例 / 会话保持

【代码】会话是无状态签名 Cookie（密钥 = `SECRET_KEY`），**各实例 `SECRET_KEY` 相同则会话互通**，SSO 的 state / nonce / PKCE 也封在 Cookie 里（密钥由 `SECRET_KEY` 派生），因此 SSO 跨实例可用。以下状态仍在**进程内**，多实例需对同一用户做会话保持（`ip_hash` 或 Cookie 粘滞），或干脆单实例：

| 进程内状态 | 影响 |
|---|---|
| 短信验证码、绑定中的 TOTP 密钥（15 分钟）、2FA 失败计数、TOTP 防重放窗口、短信频控 | 换到另一实例后验证失败，需要重发 / 重新绑定 |
| 已用 SSO state 集合 | 仅影响回调重放防护的严格性 |
| 登录失败 IP 限流、自助改密尝试计数 | 各实例各算各的 |
| SQL 分析任务、Binlog 解析任务（结果留 30 分钟） | 轮询打到别的实例会 404 |
| 运行级“暂停 / 终止”归档运行 | 只能在**启动该运行的进程**上操作，其它进程返回 409。编辑 / 暂停 / 停用 / 删除**任务**不受此限：它们会看库里该任务租约未过期的运行记录，别的进程在跑也返回 409 |
| 权限、设置、菜单角标缓存（几秒到 15 秒） | 各实例短暂不一致 |

【建议】生产用**单实例 + systemd 自动重启**，高可用靠快速重建而不是多活。

---

## 7. 外部依赖部署

### 7.1 goInception（Docker）

#### 7.1.1 它是什么、没有它会怎样

SQL 检查 / 执行 / 备份回滚、在线查询的表级权限解析和脱敏语法树都依赖 goInception（默认端口 4000，`internal/goinception/client.go`）。`REQUIRE_GOINCEPTION=true` 时未配置 goInception 就**拒绝**查询 / 检查 / 执行；为 `false`（缺省）且未配置时静默降级为词法解析 / 原生检查与执行（较弱，仅开发用）。**已配置但出错时不会降级，请求直接失败**（见 `internal/query/auth.go`、`internal/sqlflow/check.go`）。

#### 7.1.2 容器部署示例【建议，未在真实环境验证；镜像、入口和配置路径以所用 goInception 版本的官方说明为准】

```bash
# 配置目录 /opt/dbtool/goinception 与初始 config.toml 已由 install.sh 创建 / 部署（属主 dbtool；页面保存 config.toml 是
# “临时文件 + rename”原子替换，需要运行用户对目录可写）。至少填写 [inc] 的 backup_host/backup_port/backup_user/backup_password（见 7.1.6）。

docker run -d --name goinception --restart unless-stopped \
  -p 127.0.0.1:4000:4000 \
  -v /opt/dbtool/goinception:/etc/goinception \
  <goinception-image>:<tag> \
  <按镜像入口，指定配置文件 /etc/goinception/config.toml>
```

> **警告**：**挂载目录，不要单文件 bind mount**。页面保存是“同目录临时文件 → fsync → rename”，单文件 bind mount 会让 rename 失败；目录只读 / 无写权限同样失败。

要点：
- 容器名（上面的 `goinception`）要与页面“部署与重启”里填的一致，只允许字母数字和 `_ . -`，以字母或数字开头，≤128 位（`BuildArgv` 校验）。
- 端口只对 dbtool 开放（`127.0.0.1:4000` 适用于同机部署；跨机就放行 dbtool 主机 IP）。
- goInception 的账号密码在 goInception 自己的配置里（不是 MySQL 账号），dbtool 用 `goinception.user/password` 连接。
- 初始 `config.toml` 的权限是 0640、属主为 dbtool 运行用户：容器里的进程要能读它（例如以 root 运行，或用 `--user` 指定同 uid / gid）。

#### 7.1.3 配置文件挂载与权限（dbtool 侧）

1. `install.sh` 安装时已自动完成：在 `dbtool.env` 写入 `GOINCEPTION_CONFIG_FILE=<安装目录>/goinception/config.toml`（默认 `/opt/dbtool/goinception/config.toml`；已有值不覆盖；环境变量优先于 `goinception.config_file`，设了环境变量页面上该项只读），并把包里的 `goinception.config.toml` 部署到该路径（文件不存在才部署，属主运行用户，0640；已有文件一个字都不改）。目录在 `/opt` 下，不受 `ProtectSystem=full` 影响，不需要 drop-in。
2. 只有把配置目录放在 `/etc` 下（`ProtectSystem=full` 让 `/etc` 只读）时才需要 drop-in：

```ini
# /etc/systemd/system/dbtool.service.d/override.conf
[Service]
ReadWritePaths=/etc/goinception      # 放在 /opt、/srv 或 /var 下则不需要此项（只有 /usr /boot /etc 被保护）
```
`systemctl daemon-reload && systemctl restart dbtool`。

3. 配置文件所在目录要授权给 `dbtool`（属主 / 属组；默认目录由 `install.sh` 创建，属主即运行用户，0750）。页面写文件时会尽力恢复原属主 / 属组（需 root，否则只恢复属组）；保留原权限位。
4. 覆盖前若内容不同，旧内容备份为 `<文件>.dbtool.bak`（**只留一份，含密码明文**）。

#### 7.1.4 页面重启：sudoers 与 `NoNewPrivileges` 的冲突

页面“重启 goInception”**只**有一种方式，命令固定为：

```
[sudo -n] docker restart <容器名>
```

不经过 shell，参数逐项 exec；勾选“使用 sudo”才加 `sudo -n`。dbtool 运行用户必须满足其一：

**方案 A（推荐）：sudo 白名单，只放行这一条命令**

先用 `which docker` 确认路径（常见 `/usr/bin/docker`），再写入白名单文件：

```bash
cat >/etc/sudoers.d/dbtool-goinception <<'EOF'
Cmnd_Alias DBT_GI_RESTART = /usr/bin/docker restart goinception
dbtool ALL=(root) NOPASSWD: DBT_GI_RESTART
Defaults:dbtool !requiretty
EOF
```

```bash
chmod 0440 /etc/sudoers.d/dbtool-goinception
```

```bash
visudo -cf /etc/sudoers.d/dbtool-goinception
```

> **警告**：与 systemd 的冲突——`install.sh` 生成的单元有 `NoNewPrivileges=true`，它会让 `sudo`（setuid 提权）**失效**，页面重启会失败。需要在 drop-in 里放宽：

```ini
[Service]
NoNewPrivileges=false
```

这等于放宽了整个服务的提权限制，所以 sudoers 必须**只**放行那一条固定命令（容器名固定、参数固定）。

**方案 B：把 `dbtool` 加入 `docker` 组，页面不勾选 sudo**。不需要改 `NoNewPrivileges`，但 `docker` 组成员**等价于宿主机 root**，风险更大。操作步骤：

```bash
usermod -aG docker dbtool
```

```bash
systemctl restart dbtool
```

```bash
systemctl show dbtool -p MainPID --value
```

> **注意**：进程只在启动时读取附加组，所以加入组之后**必须重启 dbtool 服务**才生效。上一条命令取主进程号，可再用 `grep Groups /proc/<主进程号>/status` 确认组号里含 `docker` 组的 GID（`getent group docker` 查看）。

加入后在页面“部署与重启”不要勾选“使用 sudo -n”，再点“仅重启”验证一次。**症状**：未加入时重启失败，提示 `permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock`。此时配置文件已经写入，容器没有被重启；平台会自动恢复旧配置并再重启一次，同样失败，流程记为失败并提示“请人工处理”。

**方案 C：不在页面重启**。“部署与重启”里容器名留空：保存并重启 / 仅重启 / 回滚都不可用（返回 409 `deploy_not_set`），配置文件只能“仅保存”，之后自行 `docker restart`；保存的版本会一直显示“待重启生效”。

**验证**：页面“部署与重启”→“检查权限与连通性”——它**只检查**配置文件可写、展示将执行的命令、goInception 是否可连，**不会执行 `docker restart`**。真正的重启只能用“仅重启”并在低峰验证一次（收敛为 docker 后**从未有过真实执行记录**）。

#### 7.1.5 dbtool 自己跑在容器里

`deploy/Dockerfile` 的 distroless 镜像没有 `docker` 命令，也没有 `docker.sock`：**不要填容器名**（4.3）。

#### 7.1.6 备份库设置

- dbtool 侧：页面“连接信息”里除了 goInception 连接，还要填**备份库**（`inception_remote_backup_host/port/user/password`，存系统设置 `goinception.backup_*`，敏感项 mirage 加密）。平台用它读取回滚信息（只读 `SELECT`，读 `<备份库>.$_$Inception_backup_information$_$` 及回滚表）。“测试备份库”会用 `SHOW GRANTS FOR CURRENT_USER()` 判断写权限，找不到 `INSERT`/`CREATE` 会警告。
- goInception 侧：`config.toml` 的备份库参数（参数目录里 `backup_host/backup_port/backup_user/backup_password`，具体键名与可用性随 goInception 版本，目录来自官方文档整理、**未对照真实版本逐项核对**）。goInception 自己用这个账号建库建表并写入，需要 `CREATE`、`INSERT`。

> **警告**：备份库是回滚 SQL 的唯一来源，要纳入备份与容量管理。

#### 7.1.7 健康检查

| 层面 | 方法 |
|---|---|
| dbtool 页面 | 平台设置 › 审核引擎 › 进程状态卡片（运行 / 无法连接 / 未知、版本、连接数、任务数） |
| 命令行 | `mysql -h127.0.0.1 -P4000 -u<inc-user> -p -e "inception get variables"`（goInception 讲 MySQL 协议；“测试连接”就是执行这条语句确认对方真是 goInception） |
| 容器 | `docker ps --filter name=goinception`；`docker logs --tail 100 goinception` |

---

## 8. 首次上线流程

全新部署只有一条路径：空库 → `dbt-admin bootstrap`（或 `install.sh --bootstrap`）→ 创建第一个管理员 → 完成必配项 → 抽样验证。

### 8.1 空库全新安装

基础表与基础数据由 `deploy/bootstrap/*.sql` 提供（`docs/bootstrap-design.md`），初始化 / 管理员由命令行工具 `dbt-admin` 完成（`docs/admin-cli.md`）。

> **警告**：这条路径只用内存假库与假驱动测过，**未在真实 MySQL 上验证**（附录 G）——先在演练库跑通。

**前提**：元数据库是**全新的空库**。`dbt-admin bootstrap` 在 `dbt_sql_users` 已有数据时拒绝；本手册只讲空库。

**一键（推荐）**：建好空库、备好账号后，一条命令完成下面第 2–6 步，以及运行用户、目录、配置、systemd 单元、防火墙、启动与健康检查（4.1）；反复运行是安全的（库里已有用户时只执行幂等的 `sql/*.sql` 扩展表脚本）：

```bash
read -rs DBT_ADMIN_PASSWORD; export DBT_ADMIN_PASSWORD
```

```bash
sudo --preserve-env=DBT_ADMIN_PASSWORD ./install.sh install \
  --database-url 'mysql://dbtool_rt:<url-encoded-password>@<mysql-host>:3306/db_tool' \
  --ddl-database-url 'mysql://dbtool_ddl:<url-encoded-password>@<mysql-host>:3306/db_tool' \
  --gen-secrets --bootstrap --admin-user admin --public-url https://dba.example.com --open-firewall
```

下面是分步的手工做法（用于演练或排错）。

1. **建空库**（`dbt-admin` 不会建库，库不存在会报错并提示语句）：

   ```sql
   CREATE DATABASE db_tool DEFAULT CHARACTER SET utf8mb4;
   ```
   并准备账号：建表账号（`CREATE`/`ALTER`/`INDEX`/`REFERENCES` + DML，用于 `bootstrap`）与运行账号（只需 DML；见 3.3）。建库时请显式指定 utf8mb4（MySQL 8 推荐 utf8mb4_0900_ai_ci，5.7 用 utf8mb4_general_ci）；`dbt-admin bootstrap` 会在库默认字符集不是 utf8mb4 时拒绝执行（`docs/charset-policy.md`）。
2. **演练**（默认只演练，不写任何东西）：

   ```bash
   export DATABASE_URL='mysql://dbtool_ddl:<url-encoded-password>@<mysql-host>:3306/db_tool'
   ./dbt-admin bootstrap                 # 包内；源码树里加 --sql-root deploy
   ```
   核对输出的**目标库名与主机**、文件清单与语句数。
3. **执行**：

   ```bash
   ./dbt-admin bootstrap --apply --yes   # 或不加 --yes，在终端里输入 yes 确认
   ```
   逐文件、逐语句执行，遇错即停并报出文件名与语句序号；全部幂等，修复后可直接重跑；已应用的文件记录在 `dbt_bootstrap_applied`。摘要里核对：新建表数、`dbt_auth_permission` 行数（基础数据已导入）。
4. **生成密钥并保管**：`./dbt-admin gen-secrets`（或安装时加 `--gen-secrets`）。生成的 `SECRET_KEY` 与 `MIRAGE_SECRET_KEY` 各 64 位字母数字（`MIRAGE_SECRET_KEY` 要求 ≥ 32 字符；建议单独设置，以后不动 `SECRET_KEY` 也不影响密文），`MIRAGE_CIPHER_MODE=ECB`。**立刻存入密码库**；`MIRAGE_*` 一经使用就不能再改（2.6）。
5. **创建第一个超级用户**：

   ```bash
   read -rs DBT_ADMIN_PASSWORD; export DBT_ADMIN_PASSWORD   # 或不设置，在终端里无回显输入两次
   ./dbt-admin admin create --username admin --display 管理员 --yes
   ```
   密码需满足密码策略（默认至少 9 位、不能全是数字、不能是常见弱口令、不能包含用户名）。可用 `--group DBA` 加入内置权限组（基础数据已创建 `DBA / TL / RD / QA / Default`）。
6. **安装**（4.1）：`install.sh` 把上面几步合成一条：`--gen-secrets --bootstrap --admin-user admin [--ddl-database-url …]`，见 4.1 的示例 B；`--bootstrap` 只在库为空时初始化，再次运行是安全的。进入 8.3。

**回退**：新库是空的，出错直接 `DROP DATABASE db_tool;` 重来（**只对新库**）。

### 8.2 初始管理员与内置权限组

- **管理员**：`install.sh --bootstrap --admin-user …` 或 `dbt-admin admin create` 建的 `dbt_sql_users.is_superuser=1` 的用户。登录只接受 `pbkdf2_sha256$迭代$盐$哈希` 格式的密码哈希；其它哈希器（bcrypt / argon2 / sha1）**登不进**（`internal/authx/login.go`）。命令行：`dbt-admin admin create / reset-password / unlock / activate / deactivate / list`（`docs/admin-cli.md`）；忘记密码 / 账号被锁：`dbt-admin admin reset-password --username …` / `admin unlock --username …`。
- 至少保留**两个**超级用户（平台禁止停用 / 降级“最后一个活跃的超级用户”和你自己）。
- `security.superuser_local_only`（缺省真）：超级用户只能用本地密码，不走 LDAP / SSO。
- **内置权限组**：名为 `DBA`、`TL`、`RD`、`QA`、`Default` 的 `dbt_auth_group` 记录（基础数据创建），由 `150_builtin_groups.sql` 在 `dbt_auth_group_ext.builtin` 上标记；内置组不能改名、不能删除，可以改权限与说明。UI 预置的权限集在 `internal/btnperm/presets.go`，权限码清单见 `web/static/help/permcodes.md`。

### 8.3 初始化后的必配项清单

按顺序做，每项后面是在哪里配、验证什么：

| # | 项 | 位置 | 验证 |
|---|---|---|---|
| 1 | **登录与安全设置**：账号锁定阈值（`security.login_lock_attempts` 缺省 5、`security.login_lock_seconds` 缺省 300 秒）、`security.require_2fa`、会话有效期（`security.session_minutes` 缺省 300）、JWT 有效期（`security.jwt_hours` 缺省 4）、密码策略（最小长度缺省 9）、`security.superuser_local_only`、水印、API 白名单（`security.api_user_ids`，**为空 = 不限制，所有用户都允许申请 API 令牌**） | 访问控制 › 安全策略 | 用测试账号走登录、锁定、改密流程 |
| 2 | **goInception**：连接、备份库、`config.toml`、容器名 | 平台设置 › 审核引擎 | “测试连接”“测试备份库”成功；`REQUIRE_GOINCEPTION=true` 下 SQL 检查正常 |
| 3 | 资源组（关联用户与实例） | 访问控制 › 资源组 | 页面不提示“无人可审” |
| 4 | 权限组与按钮权限（升级时已自动迁移；页面顶部有“还没迁移”提示时先在服务器上执行 `dbt-admin perm migrate`） | 访问控制 › 权限组 | “按用户查看有效权限”核对来源 |
| 5 | **审批流**：每个资源组按类型配置（查询权限申请、SQL 上线、数据归档） | 安全合规 › 审批流程 | 页面不提示“该级无人可审”；空审批流直接通过（SQL 上线），查询权限申请不允许空流程 |
| 6 | 实例：标签 `can_read`/`can_write`、资源组、慢日志采集开关（`is_enable`）、环境 / 停用（需 070） | 实例管理 | “测试连接”成功；写工单的实例有 `can_write` |
| 7 | 水印 / 安全头 / HSTS | 登录与安全设置 + nginx | 页面出现水印；6.2 的验证命令 |

其余功能（登录方式 SSO、通知渠道、在线查询与导出参数、性能工单、巡检、数据归档、Redis 规则、分区维护等）按需在页面里配置，使用说明见 `docs/user-guide.md` 与各功能的设计文档；系统设置项的默认值见 [settings-reference.md](settings-reference.md)。

### 8.4 抽样验证（上线必做）

1. 普通开发账号：**在线查询 → 申请权限 → 审批 → 查询**，确认审批通知、`dbt_query_log`、审计日志。
2. 提交 SQL 工单 → 检查（goInception）→ 审批 → 执行（在测试库）→ 回滚语句可见；通知到位。
3. 导出：提交导出工单 → 审批 → 执行 → 下载（需 `offline_download`，下载写 `dbt_audit_log`）。
4. 超级账号：系统管理各页能打开，审计页能看到刚才的操作。
5. 重启 `systemctl restart dbtool` 后会话仍有效（`SECRET_KEY` 不变，无状态 Cookie），后台任务状态正常。

---

## 9. 附录

### 附录 A　环境变量速查

完整说明（默认值、范围、敏感性）见 5.2。全部只在启动时读取，改后需重启。

| 变量 | 默认 | 变量 | 默认 |
|---|---|---|---|
| `DATABASE_URL` | 必填 | `SECRET_KEY` | 公开开发默认值（生产必须覆盖） |
| `MIRAGE_SECRET_KEY` | 取 `SECRET_KEY`（≥32 字符） | `MIRAGE_CIPHER_MODE` | `ECB` |
| `MIRAGE_CIPHER_IV` | `1234567890abcdef` | `PUBLIC_URL` | `http://127.0.0.1:<端口>` |
| `LISTEN` | 无（最终 `:8000`） | `DB_TOOL_CONFIG` | 无 |
| `REQUIRE_GOINCEPTION` | `false` | `DEBUG` | `false` |
| `BATCH_DB_TYPES` | `mssql` | `EXPORT_BASE_DIR` | `.` |
| `ARCHIVE_SPOOL_DIR` | `$EXPORT_BASE_DIR/archive-spool` | `GOINCEPTION_CONFIG_FILE` | 无 |
| `SLOWLOG_COLLECT_ENABLE` | 开（设 0 关闭） | `SLOW_COLLECT_INTERVAL` | 10 秒（5–3600） |
| `SLOW_COLLECT_WORKERS` | 2（1–8） | `SLOWLOG_COLLECT_BACKFILL_DAYS` | 7（0–365） |
| `REDIS_SLOWLOG_COLLECT_INTERVAL` | 60 秒（10–3600） | `REDIS_INSPECT_BIGKEY_SAMPLE` | 0（≤5000） |
| `SSH_TUNNEL_LISTEN_HOST` | `127.0.0.1` | `SSH_TUNNEL_ADVERTISE_HOST` | `127.0.0.1` |
| `SSH_TUNNEL_IDLE_TIMEOUT` | `10m`（≥ `1m`） | `DEFAULT_LANG` | `zh-CN` |
| `TZ`（Go 运行时） | 系统时区 | `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`（Go 标准） | 无 |

### 附录 B　`deploy/sql` 脚本表

执行顺序 = 文件名升序。全部可重复执行（`CREATE TABLE IF NOT EXISTS`；`150` 为条件 `ALTER` + `INSERT … ON DUPLICATE KEY UPDATE`；`180`、`270`、`290` 末尾各有条件 `ALTER`，都是先查 `information_schema` 再改）。“重启”列来自脚本头注释。

| 脚本 | 创建的表 | 用途 | 依赖 | 重启 | 不执行的后果 |
|---|---|---|---|---|---|
| `001_notify_channel.sql` | `dbt_notify_channel` | 机器人通知渠道 | — | — | 通知退回资源组表上的 webhook 列 |
| `002_sql_schedule.sql` | `dbt_sql_schedule` | SQL 工单定时执行计划 | — | — | 定时执行不可用 |
| `011_gi_config.sql` | `dbt_gi_config_version`、`dbt_gi_setting` | goInception 配置版本 / 部署设置 | — | — | goInception 配置页不可用 |
| `013_inspect.sql` | `dbt_inspect_plan`、`dbt_inspect_plan_instance`、`dbt_inspect_run`、`dbt_inspect_run_item` | 定时巡检 | — | — | 无定时巡检计划 |
| `015_slowlog.sql` | `dbt_slowlog_collect_state` | 慢日志采集状态 | — | — | 状态只在内存 |
| `017_perf.sql` | `dbt_perf_job_run` | 性能工单每日 / 每周任务抢占 | 基础表 `dbt_perf_*` | — | 周报不发 |
| `018_auth.sql` | `dbt_auth_provider`、`dbt_auth_identity` | 登录方式 / 身份绑定 | — | **是** | 登录走内置本地流程，不能配置外部登录方式 |
| `019_btnperm.sql` | `dbt_group_perm`、`dbt_user_perm`、`dbt_perm_setting` | 按钮级权限 | — | **是** | 未迁移、无按钮授权 |
| `040_query_apply_ext.sql` | `dbt_query_apply_ext` | 申请理由 | — | — | 不要求填理由 |
| `041_query_priv_ext.sql` | `dbt_query_priv_ext` | 授予来源 | — | — | 来源列为“—” |
| `050_query_reject.sql` | `dbt_query_reject`、`dbt_query_event` | 非成功查询记录 | — | **是** | 看不到非成功记录 |
| `060_perf_ext.sql` | `dbt_perf_issue_ext`、`dbt_perf_route_ext`、`dbt_perf_setting` | 性能工单扩展 | 017 | — | 功能降级 |
| `070_instance_ext.sql` | `dbt_instance_ext` | 实例停用 / 环境 | — | **是** | 无“停用 / 环境” |
| `071_account_reg.sql` | `dbt_account_reg` | 账号使用人 / 用途 / 到期日 | — | — | 仅备注 |
| `072_param_template_ext.sql` | `dbt_param_template_ext` | 参数推荐值 | — | — | 无推荐值 |
| `080_sysadmin_ext.sql` | `dbt_resource_group_ext`、`dbt_user_profile`、`dbt_auth_group_ext` | 资源组负责人 / 用户部门与强制改密 / 权限组说明 | — | **是** | 扩展字段不显示 |
| `090_audit_ext.sql` | `dbt_audit_ext` | 审计扩展 | — | — | 无结果 / IP / 类别列 |
| `091_notify_ext.sql` | `dbt_notify_channel_ext` | 多资源组 / 值班 / 细分事件 | 001 | — | 渠道只能关联一个资源组 |
| `092_notify_log.sql` | `dbt_notify_log` | 通知发送记录（30 天） | — | — | 无发送记录 |
| `130_redis_slowlog.sql` | `dbt_redis_slowlog`、`dbt_redis_slowlog_state` | Redis 慢查询 | — | — | Redis 慢日志不可用 |
| `131_redis_inspect.sql` | `dbt_inspect_redis_report` | Redis 巡检报告 | 013 | — | Redis 巡检不可用 |
| `150_builtin_groups.sql` | （无新表）给 `dbt_auth_group_ext` 加 `builtin` 列并标记内置组 | 内置权限组 | 080；`dbt_auth_group` 中已有 `DBA/TL/RD/QA/Default` | — | 所有组按普通组处理 |
| `170_redis_rules.sql` | `dbt_redis_rule` | Redis 工单上线规则 | — | — | Redis 工单全部拒绝 |
| `180_archive.sql` | `dbt_archive_task`、`_run`、`_event`、`_batch_stat`、`_store`、`_policy`、`_check` | 数据归档（末尾给已有的 `dbt_archive_task` 补 `time_unit` 列，支持按 10 位 / 13 位 Unix 时间戳列归档） | 基础表 `dbt_workflow_audit*` | 否 | 归档页提示先执行；升级程序后没执行这个脚本（缺 `time_unit` 列）也是同样的提示 |
| `205_retention.sql` | `dbt_retention_run`、`dbt_export_purged` | 数据清理每轮汇总 / 导出文件“已清理”标记 | — | 否 | 看不到每轮汇总；导出文件清理不执行 |
| `210_notify_email.sql` | `dbt_notify_email` | 邮件通知渠道的 SMTP 配置（密码在 `dbt_notify_channel.secret`） | 001 | — | 不能保存邮件渠道，其它渠道不受影响 |
| `230_masking_policy.sql` | `dbt_masking_policy` | 数据脱敏策略（“安全合规 › 数据脱敏”页保存的配置） | — | **是** | 没有脱敏策略，查询结果不脱敏；脱敏页提示先执行 |
| `240_firewall.sql` | `dbt_firewall_rule`、`dbt_firewall_exception`、`dbt_firewall_hit` | SQL 防火墙规则 / 例外 / 命中记录 | — | — | 在线查询不受影响（等同没有规则），管理页提示先执行 |
| `250_query_favorite.sql` | `dbt_query_favorite` | 在线查询的收藏 SQL（每人自己的） | — | 否 | 查询页“收藏”标签提示先执行 |
| `260_notify_template.sql` | `dbt_notify_template`、`dbt_notify_template_hist` | 通知消息模板及历史 | — | — | 通知按系统默认文案发送；模板页提示先执行 |
| `260_setting.sql` | `dbt_setting`、`dbt_setting_history` | 系统设置及变更记录（设计见 `docs/settings-design.md`） | — | — | **`/readyz` 返回 503（必需表）**，所有设置按默认值运行 |
| `270_environment.sql` | `dbt_environment`（并把 `dbt_instance_ext.env`、`dbt_firewall_rule.env` 放宽到 VARCHAR(16)） | 环境字典（平台设置 › 环境设置）；内置 `prod` / `test` 用 `INSERT IGNORE` 写入 | 070、240（脚本末尾对这两张表做 `ALTER`；`dbt-admin bootstrap` 遇到失败语句即停止） | — | 环境只有内置的生产 / 测试，环境设置页提示先执行；建表后无需重启，30 秒内生效 |
| `280_partman_alert.sql` | `dbt_partman_alert`、`dbt_partman_alert_log` | 分区维护的可用分区数量告警：每张表的阈值 / 静默 / 告警状态与推送记录 | — | 否 | 分区维护页提示先执行；列表照常，但不推送、不能给每张表设阈值 |
| `290_export_unmasked.sql` | `dbt_sql_export_ext`（末尾给已有的表补 `merge_files` 列） | 数据导出工单的扩展：超级用户提交时选择的“导出原值（不脱敏）”与理由；批量导出提交时选择的“合并成一个文件” | — | 否 | 导出申请页不出现“导出原值”选项；缺 `merge_files` 列（只执行过旧版脚本）时不出现“合并成一个文件”选项，提交带这个选项会被拒绝并提示重新执行本脚本；其它导出功能照常 |
| `300_duty.sql` | `dbt_duty_shift`、`dbt_duty_override` | DBA 值班表（平台设置 › 值班表）：班次与临时调班；顶栏时钟左边显示当前值班人 | — | 否 | 顶栏不显示值班信息，值班表页面提示先执行；不在 `/readyz` 的可选表清单里，缺表不会让它降级，其它功能照常；建表后约 30 秒内生效，无需重启 |

共 35 个脚本，`CREATE TABLE` 60 张（含上表）。

### 附录 C　端口与网络矩阵

| # | 源 → 目的 | 端口 / 协议 | 用途 | 必需 |
|---|---|---|---|---|
| 1 | 用户浏览器 / 脚本 → nginx | 443/TCP（80 跳转） | 访问 | 是 |
| 2 | nginx → dbtool | 8000/TCP（`[server] port`） | 反向代理 | 是 |
| 3 | dbtool → 元数据库 | 3306/TCP | 全部读写 | 是 |
| 4 | dbtool → goInception | 4000/TCP（可配） | 检查 / 执行 / 回滚 / 语法树 | 强烈建议 |
| 5 | dbtool → goInception 备份库 | 3306/TCP | 读取回滚信息 | 用回滚时 |
| 6 | goInception → 目标库 | 实例端口 | 审核 / 执行 / 备份读 binlog | 是 |
| 7 | goInception → 备份库 | 3306/TCP | 写回滚信息 | 是 |
| 8 | dbtool → 目标库（MySQL 系 / SQL Server 1433 / Redis 6379 等） | 实例登记的端口 | 查询、巡检、采集、归档、Binlog 拉取等 | 是 |
| 9 | dbtool → 机器人 | 443/TCP（`oapi.dingtalk.com`、`qyapi.weixin.qq.com`、`open.feishu.cn`、`open.larksuite.com`、`hooks.slack.com`、`api.telegram.org` 等，以 Webhook 为准；`api.telegram.org` 在国内网络通常访问不了，需要走出站代理 `HTTPS_PROXY`） | 通知 | 否 |
| 9a | dbtool → 邮件服务器（SMTP） | 587/TCP（STARTTLS）、465/TCP（SSL/TLS）、25/TCP（不加密），以渠道配置为准；直连，不走 `HTTP(S)_PROXY` | 邮件通知 | 否 |
| 10 | dbtool → IdP | 443（OIDC/CAS/钉钉/飞书/Slack/企微域名，视所用登录方式而定）；LDAP 389/636 | SSO / LDAP | 否 |
| 11 | dbtool → 短信 | 443：`dysmsapi.aliyuncs.com`、`sms.tencentcloudapi.com` | 2FA 短信 | 否 |
| 12 | dbtool → 对象存储 | 443 或自建 MinIO 端口 | 归档备份 | 否 |
| 13 | IdP / 浏览器 → `PUBLIC_URL` | 443 | 回调 | 用 SSO 时 |
| 14 | 运维跳板机 → 元数据库 | 3306 | 初始化库（建表 / 基础数据） | 是 |
| 15 | dbtool 主机 → Docker（本机） | unix socket / `sudo docker` | 页面重启 goInception | 否 |
| — | 以上均无 dbtool 入站的其它端口；无 Redis 依赖 | | | |

### 附录 D　install.sh 速查：退出码 / 状态文件 / 重复运行

> 代码：`deploy/install.sh`；行为测试：`scripts/install-test/run.sh`；设计与不变量：`docs/one-click-deploy.md`。

#### D.1 常用组合

选项逐项解释见 4.1。

| 目的 | 命令 |
|---|---|
| 首次部署（空库，一条命令） | `install --gen-secrets --bootstrap --admin-user admin --database-url … [--ddl-database-url …] --public-url … --open-firewall` |
| 看计划不改动 | 任意写子命令加 `--dry-run` |
| 体检 | `doctor`（只读；`--database-url` 可在全新主机上预检库） |
| 只初始化库 | `init-db [--admin-user NAME] [--ddl-database-url URL]` |

环境变量：`DBT_ADMIN_PASSWORD`（管理员密码）、`DBT_DATABASE_URL` / `DBT_SECRET_KEY` / `DDL_DATABASE_URL`（连接串与密钥，替代命令行选项）、`DBT_HEALTH_TIMEOUT`（健康检查秒数）。仅供测试用：`DBT_INSTALL_CONF_DIR`、`DBT_INSTALL_UNIT_DIR`、`DBT_INSTALL_LOCK_FILE`、`DBT_INSTALL_LOG_FILE`、`DBT_OS_RELEASE_FILE`、`DBT_INSTALL_TEST_TTY`、`DBT_DOCTOR_MIN_FREE_KB`，正常安装不要设置。

#### D.2 退出码

| 码 | 含义 |
|---|---|
| 0 | 成功（含 `doctor` 只有 WARN） |
| 1 | 一般错误（参数以外的失败，如 bootstrap / 建表 / 创建管理员失败、编译失败） |
| 2 | 用法 / 参数错误（未知选项、缺必填项、`--prefix` / `--user` 与已安装不一致、非交互缺 `--yes`） |
| 3 | 预检失败（`doctor` 有 FAIL、包 sha256 校验失败、新连接串连不上） |
| 4 | 已有另一个 `install.sh` 在运行（锁） |
| 5 | 升级 / 回滚 / 改配置失败，**已自动恢复到原状态且服务健康** |
| 6 | 失败且无法恢复（首次安装健康检查失败、没有可回滚版本、回滚后仍不健康）：需要人工处理 |
| 7 | 拒绝降级（加 `--allow-downgrade`） |

#### D.3 状态文件 `/etc/dbtool/.install-state`（key=value，0640，不含机密）

| 字段 | 含义 |
|---|---|
| `STATE_VERSION` | 状态文件格式版本（当前 1） |
| `INSTALLED` | `1` = 已安装；`0` = 已卸载（配置保留，重装时沿用） |
| `VERSION` | 当前版本目录名（`releases/<VERSION>`） |
| `PREVIOUS_VERSION` | 上一版本（升级后才有） |
| `INSTALLED_AT` | 首次安装时间（ISO 8601） |
| `UPDATED_AT` | 最近一次写状态文件的时间 |
| `PREFIX` | 安装目录 |
| `RUN_USER` | 运行服务的系统用户 |
| `USER_CREATED` | `1` = 运行用户由本脚本创建 |
| `PORT` | 监听端口 |
| `FIREWALL_OPENED` / `FIREWALL_PORT` | 本脚本是否放行了防火墙端口及端口号 |
| `UNIT_SHA256` | 脚本最近一次写入的单元文件 sha256（与磁盘不一致 = 被手工改过） |

#### D.4 重复运行与库保护（首次部署相关）

完整的重复运行安全矩阵（全新 / 已安装 / 库已有数据三种起始状态 × 全部子命令）见 `docs/one-click-deploy.md` 第 5 节，这里只列首次部署会遇到的几条：

| 情形 | 行为 |
|---|---|
| 首次部署中途失败后原样重跑 | 各阶段幂等，直接重跑同一条命令；已创建的用户 / 目录 / 配置不重复创建，已应用的 `dbt_bootstrap_applied` 文件幂等重跑 |
| 配置已存在后再次 `install` | 等价于升级：配置 / 密钥 / 管理员 / 数据目录不动，配置类选项被忽略并提示改用 `reconfigure` |
| `--bootstrap` 遇到库里已有用户 | 不重复初始化基础表与数据，只执行幂等的 `sql/*.sql`（`--only-sql`）；已有启用的超级用户则不创建管理员 |
| `init-db` | 库为空：基础表 + 基础数据 + 扩展表脚本，缺管理员再创建；否则只执行 `--only-sql` |
| 任意写子命令加 `--dry-run` | 只打印计划（只读连库做预检），不写文件、服务、库，也不写日志 |

**库保护**：脚本里没有任何 `DROP` / `TRUNCATE` / `DELETE`；基础结构与数据只在 `dbt-admin status` 判定用户表不存在或为空时才执行（`dbt-admin` 自己在用户表非空时也会拒绝默认流程）。

---

### 附录 E　目标库账号 GRANT 模板

> 前提（见 2.4）：每个实例只登记一组账号，模块共用。下面按“功能块”给模板，**按实际启用的功能叠加**。
> 权限由代码里执行的语句推导（语句位置见各块），**没有在真实服务器上逐项验证**；标【待验证】的是 MySQL 权限映射的推断。MySQL 8.0 语法；5.7 的差异在注释里。
> 所有 `<password>`、主机、库名均为占位。**不要对 `mysql.*`、`information_schema` 授写权限。**

#### E.1 只读查询账号（在线查询 / 对象信息 / 导出 / 导出行数统计 / EXPLAIN）

```sql
CREATE USER 'dbt_ro'@'<dbtool 主机 IP>' IDENTIFIED BY '<password>';
-- 被管理的业务库（逐库授权，不要 *.*）
GRANT SELECT, SHOW VIEW ON `<business_db>`.* TO 'dbt_ro'@'<dbtool 主机 IP>';
-- 若需要 SHOW DATABASES 列出全部库（否则只列有权限的库）：
-- GRANT SHOW DATABASES ON *.* TO 'dbt_ro'@'<dbtool 主机 IP>';
```

- 依据：用户 SQL 只允许 select/with/show/explain/desc，`mysql.*` 被拦截（`internal/query/service.go`）；元数据走 `SHOW TABLE STATUS`、`SHOW FULL COLUMNS`、`information_schema.COLUMNS`（`internal/query/meta.go`）；超时用 `KILL QUERY <自己的连接>`（无需额外权限）。“对象信息 / 可编程对象”读 `information_schema` 并 `SHOW CREATE TABLE/PROCEDURE/FUNCTION/EVENT`（`internal/dict/mysql.go`），例程 / 触发器 / 事件定义可能还要 `EXECUTE`/`TRIGGER`/`EVENT`，缺权限时该节显示“无权限”，不影响其它【待验证】。
- 实例上配了数据脱敏字段时，脱敏要读语句用到的视图的定义（`information_schema.VIEWS.VIEW_DEFINITION`，`internal/engine/mysql/views.go`）：模板里的 `SHOW VIEW` 不能省，没有它时视图定义是空的，按脱敏失败处理（系统配置 `query.block_on_mask_error` 开启时在线查询阻断，缺省放行并提示；导出工单中止）。

#### E.2 巡检 / 性能工单 / 慢日志采集账号

```sql
CREATE USER 'dbt_mon'@'<dbtool 主机 IP>' IDENTIFIED BY '<password>';
GRANT PROCESS, REPLICATION CLIENT ON *.* TO 'dbt_mon'@'<dbtool 主机 IP>';
GRANT SELECT ON performance_schema.* TO 'dbt_mon'@'<dbtool 主机 IP>';
GRANT SELECT ON sys.*              TO 'dbt_mon'@'<dbtool 主机 IP>';   -- 仅 sys.schema_table_lock_waits，best effort
GRANT SELECT ON mysql.slow_log     TO 'dbt_mon'@'<dbtool 主机 IP>';   -- 慢日志采集 / 性能工单的 slow_log 采集器；需 log_output 含 TABLE
GRANT SELECT ON mysql.user         TO 'dbt_mon'@'<dbtool 主机 IP>';   -- 可选：巡检“账号风险”步骤，缺则该步标“未采集”
GRANT SELECT ON `<business_db>`.*  TO 'dbt_mon'@'<dbtool 主机 IP>';   -- information_schema.TABLES/COLUMNS/STATISTICS 只显示有权限的对象
```

- 依据：`internal/inspect/mysql_collect.go`（`information_schema.innodb_trx/processlist`、`SHOW ENGINE INNODB STATUS`、`INNODB_METRICS`、`performance_schema.events_statements_summary_by_digest`、`table_io_waits_summary_by_index_usage`、`error_log`、`SHOW REPLICA STATUS` 回退 `SHOW SLAVE STATUS`、`SHOW BINARY LOGS`）；`internal/slowlog/source.go`（`mysql.slow_log`）；`internal/perf/collectors.go`（`events_statements_summary_by_digest`、`mysql.slow_log`、Doris `__internal_schema.audit_log`、PolarDB-X 摘要、自定义采集 SQL）。
- 冒烟记录：只有 `PROCESS` + 测试库权限时，巡检 3 步显示“未采集”（`mysql.user`、索引使用表、复制状态）——这三项是**可选**的。
- 性能工单 `perf.auto_explain` 打开时会对样例 SQL 执行 `EXPLAIN`（含 `UPDATE`/`DELETE` 的 EXPLAIN，需要对应表的 `UPDATE`/`DELETE` 权限）【待验证】；关掉 `perf.auto_explain` 可避免要求写权限。
- 【建议】性能工单 / 巡检 / 慢日志对应实例记录用本账号，查询用 E.1。

#### E.3 实例管理账号（会话 / 数据库 / 账号 / 参数 / Binlog 清理）— 高权限，慎用

```sql
CREATE USER 'dbt_admin'@'<dbtool 主机 IP>' IDENTIFIED BY '<password>';
GRANT PROCESS, REPLICATION CLIENT, CREATE, CREATE USER ON *.* TO 'dbt_admin'@'<dbtool 主机 IP>' WITH GRANT OPTION;
-- 8.0 动态权限：
GRANT SYSTEM_VARIABLES_ADMIN, BINLOG_ADMIN, CONNECTION_ADMIN ON *.* TO 'dbt_admin'@'<dbtool 主机 IP>';
GRANT SELECT ON mysql.*              TO 'dbt_admin'@'<dbtool 主机 IP>';   -- mysql.user、SHOW GRANTS FOR 他人
GRANT SELECT ON performance_schema.* TO 'dbt_admin'@'<dbtool 主机 IP>';   -- 锁 / 事务页签
-- 5.7：用 SUPER 代替 SYSTEM_VARIABLES_ADMIN/BINLOG_ADMIN/CONNECTION_ADMIN
```

- 依据：`internal/instmgr/{sessions,databases,accounts,params,instances,binlog}.go`。`CREATE USER`/`ALTER USER`/`DROP USER` 需全局 `CREATE USER`；`GRANT`/`REVOKE` 需 `GRANT OPTION` 加上要授予的每一项权限；`SET GLOBAL` 需 `SYSTEM_VARIABLES_ADMIN`（5.7 `SUPER`，**不使用** `SET PERSIST`）；`PURGE BINARY LOGS` 需 `BINLOG_ADMIN`（5.7 `SUPER`）；杀他人线程需 `CONNECTION_ADMIN`（5.7 `SUPER`）。以上权限映射【待验证】；账号管理、`SET GLOBAL`、`PURGE`、`KILL` 他人线程**从未在真实服务器上执行过**。
- 平台保护：平台连接实例所用的账号、`root@localhost` 等内置账号不允许被平台改密 / 锁定 / 删除 / 回收权限。
- 复制从库进度核对（清理 Binlog 前）：用**每个从库实例记录自己的账号**连从库执行 `SHOW REPLICA STATUS`，从库账号需 `REPLICATION CLIENT`。

> **警告**：这是最高风险的账号：只绑定给少数超级管理员 / DBA 资源组；对应页面按钮在 `btnperm` 里多为高危且有二次确认与审计。

#### E.4 Binlog 解析账号

```sql
CREATE USER 'dbt_binlog'@'<dbtool 主机 IP>' IDENTIFIED BY '<password>';
GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO 'dbt_binlog'@'<dbtool 主机 IP>';
GRANT SELECT ON `<business_db>`.* TO 'dbt_binlog'@'<dbtool 主机 IP>';  -- 取列名 / 主键 / 无符号信息（information_schema.COLUMNS）
```

- 依据：`internal/binlog/mysqlsource.go`：`SHOW GLOBAL VARIABLES`、`SHOW BINARY LOGS`（`REPLICATION CLIENT`）、`SHOW DATABASES`、`information_schema.COLUMNS`；拉取用 `COM_REGISTER_SLAVE` + `COM_BINLOG_DUMP`（`REPLICATION SLAVE`）。代码错误码 `no_privilege`（“实例账号缺 REPLICATION SLAVE”）。
- 只读：不会在目标库执行回滚语句，也不发写语句（e2e 测试确认）。

#### E.5 分区维护账号

```sql
CREATE USER 'dbt_part'@'<dbtool 主机 IP>' IDENTIFIED BY '<password>';
GRANT SELECT, ALTER, DROP ON `<business_db>`.`<partitioned_table>` TO 'dbt_part'@'<dbtool 主机 IP>';
-- REORGANIZE（含 MAXVALUE 分区的表，需人工执行）还需要 CREATE, INSERT
-- 初始化为分区表（整表重建的 ALTER ... PARTITION BY）建议同样授予 CREATE, INSERT【待验证】；“从最早数据开始”用到 SELECT
```

- 依据：`internal/partman/plan.go` 与 `internal/partman/plan_build.go` 生成 `ALTER TABLE ... ADD/DROP/REORGANIZE PARTITION`（初始化为一条 `ALTER TABLE ... PARTITION BY RANGE ...`）；`SET SESSION lock_wait_timeout=15`；读 `information_schema.PARTITIONS/TABLES/COLUMNS`，初始化另读 `STATISTICS` / `REFERENTIAL_CONSTRAINTS`，“从最早数据开始”对目标表执行 `SELECT MIN(分区字段)`（`internal/partman/target.go`）。支持 MySQL / PolarDB 与 SelectDB / Doris（后者用 `SHOW PARTITIONS` 与 `ALTER TABLE ... ADD / DROP PARTITION`，账号需要目标表的 ALTER / DROP 类权限与执行 `SHOW PARTITIONS` 的权限）；不支持 ADB / PolarDB-X。精确权限集（含初始化的 `PARTITION BY` 在各 MySQL 版本上要哪些权限、Doris 的权限名）【待验证】。

#### E.6 归档账号（源库 + 备份目标库）

```sql
-- 源库（被归档的表）：读 + 删
GRANT SELECT, DELETE ON `<business_db>`.`<table>` TO 'dbt_arch'@'<dbtool 主机 IP>';
-- 备份目标库（库表备份）：自动 CREATE TABLE ... LIKE，INSERT；不会创建数据库，库需预先存在
GRANT SELECT, INSERT, CREATE ON `<archive_db>`.* TO 'dbt_arch_dst'@'<dbtool 主机 IP>';
-- 可选：复制延迟探测
GRANT REPLICATION CLIENT ON *.* TO 'dbt_arch'@'<dbtool 主机 IP>';
-- 数据恢复（INSERT IGNORE 回灌源表）需要源表 INSERT：
-- GRANT INSERT ON `<business_db>`.`<table>` TO 'dbt_arch'@'<dbtool 主机 IP>';
```

- 依据：`internal/archive/engine/{mysqlsource,mysqldest,mysqlcommon,probe,opener}.go`：`SELECT ... WHERE ... LIMIT`、`DELETE ... WHERE pk IN (...)`、`SHOW CREATE TABLE`、`information_schema.*`、`SHOW GLOBAL STATUS LIKE 'Threads_running'`、`SHOW REPLICA STATUS`（失败回退 `SHOW REPLICAS`/`SHOW SLAVE HOSTS`）、`CREATE TABLE IF NOT EXISTS`、`INSERT`、恢复用 `INSERT IGNORE`。
- 会话参数（`lock_wait_timeout`、`innodb_lock_wait_timeout`、`time_zone`、`sql_mode`）都通过 DSN 设置，不需要 `SET GLOBAL`；要求会话 `sql_log_bin=1`（不是 1 就拒绝运行）。
- 源与目标都是 `dbt_sql_instance` 里登记的实例，各用各自实例记录里的账号；源和目标是同一实例时，该账号要同时具备两边权限。
- “从库连接”探针已接线（`internal/archive/hostinfo.go` 的 `slaveByHost`：按源库 `SHOW REPLICAS` 看到的连接来源在已登记的从库实例里找）；“磁盘余量”探针已接线（`Opener.Disk` 读该实例最近 48 小时内一次成功的 MySQL 巡检报告里的 `capacity.disk_free_pct`，或 `disk_free_bytes` / `disk_total_bytes`），**但目前的 MySQL 巡检不采集磁盘剩余，所以磁盘项在生产仍记“未检测”**，**不当作通过**。注释里写的是 `REPLICATION CLIENT`，而 MySQL 文档中 `SHOW REPLICAS` 需要 `REPLICATION SLAVE`，二者不一致，【待验证】。

#### E.7 SQL 上线执行账号（goInception / 原生执行）

- 取决于工单语句：DDL 需 `CREATE, ALTER, DROP, INDEX`，DML 需 `INSERT, UPDATE, DELETE`，再加检查阶段的 `SELECT`（元数据 / `EXPLAIN`）。
- goInception 开启备份时，goInception 自己读目标库 binlog，通常需要 `REPLICATION SLAVE, REPLICATION CLIENT`（goInception 文档）【待验证】；备份库账号（`inception_remote_backup_*`）需对备份库 `CREATE, INSERT`（页面“测试备份库”用 `SHOW GRANTS FOR CURRENT_USER()` 判断，找不到 `INSERT`/`CREATE` 会警告）。
- 原生执行（`REQUIRE_GOINCEPTION=false` 时的降级）先读 `@@global.read_only`，再 `USE` + 工单语句；**生产不建议降级**。

#### E.8 SQL 优化 / SQL 调优

- EXPLAIN：被分析表的 `SELECT`（`UPDATE`/`DELETE` 语句的 EXPLAIN 需相应写权限）。
- SQLTuning 会真实执行传入的 `SELECT`（`SET SESSION TRANSACTION READ ONLY`、`max_execution_time=60000`、`SET profiling=1` …）；只给 `SELECT`。
- SOAR：实例账号和密码作为**命令行参数**传给外部程序（本机进程列表可见）；SOAR 另需 `optimizer.soar_test_dsn`（**必须是非生产库**，账号要能在测试库建表）。

#### E.9 SQL Server

- 连接：`go-mssqldb`，`TrustServerCertificate=true`，默认端口 1433；实例的“SSL / 校验证书”开关对它**无效**（`internal/engine/mssql/mssql.go`）。
- 查询 / 上线：对象 `SELECT`、`VIEW DEFINITION`（实例配了数据脱敏字段时，脱敏读视图定义 `sys.sql_modules.definition` 也靠它，没有时定义为空，按脱敏失败处理）；`sys.dm_db_partition_stats` 需 `VIEW DATABASE STATE`。
- 巡检（27 项）：`VIEW SERVER STATE`、`VIEW ANY DEFINITION`；`msdb.dbo.backupset/sysjobs/sysjobhistory` 可读（如 `SQLAgentReaderRole` / `db_datareader` on `msdb`）；每库 `VIEW DATABASE STATE`。缺权限时对应项为“无权限”。均【待验证】，**未在真实 SQL Server 上验证**。

#### E.10 Redis ACL

- 实例记录里：用户名（空 = 只有密码）、密码、数字库号、SSL / 校验证书。只支持 RESP2，客户端不发 `HELLO`；不支持 Sentinel / Cluster 自动发现（每个节点单独登记）；SSH 隧道经本地转发器生效。
- 只读基线（巡检、慢日志、连通性、在线查询）：

```
ACL SETUSER dbt_redis on ><password> ~* &* \
  +ping +dbsize +role +time +lastsave +info +select +scan +type +ttl +pttl +strlen +llen +hlen +scard +zcard +xlen \
  +config|get +slowlog|get +slowlog|len +memory|usage +memory|stats +memory|doctor \
  +client|list +client|getname +client|info +latency|latest +latency|history +latency|doctor \
  +command|info +command|count +object|encoding +object|idletime +object|freq +cluster|info +cluster|nodes
```

  `+cmd|sub` 子命令写法需要 Redis 7+，6.x 要放开整个命令（如 `+config`，同时放宽了 `CONFIG SET`，请权衡）【待验证】。
- 在线查询的只读用户命令（`GET/HGETALL/LRANGE/...`，共 60 个）需另加：`+get +mget +hget +hgetall +lrange +smembers +zrange ...`；若用于只做巡检，不需要这些。
- Redis 工单（唯一写路径）：只给规则表里用到的命令（如 `+del +unlink +expire +set +hset ...`），另外工单执行前值快照用到 `+get +hmget`。`SLOWLOG RESET` 需要 `+slowlog|reset`，仅在使用“重置慢日志”按钮时授予。
- 云托管 Redis 常禁用 `CONFIG`：对应检查项标“未采集”，其余照常。

#### E.11 PostgreSQL（在线查询）

- 连接：`pgx/v5` 的 `pgconn` 层，文本格式结果、扩展查询协议；默认端口 5432；实例的“SSL”勾选 = 强制 TLS（“校验证书”决定是否校验，用系统根证书），不勾 = 先试 TLS（不校验）再回落明文（等同 `sslmode=prefer`）；不支持客户端证书 / 自定义 CA、Unix socket 之外的连接选项。要求 **PostgreSQL 10 及以上**。“库”= PostgreSQL database，页面库下拉用 `pg_database`（排除模板库和账号没有 `CONNECT` 权限的库）；表名是 `schema.table`。
- 引擎每次查询都在 `BEGIN READ ONLY … ROLLBACK` 里执行，并用启动参数固定 `default_transaction_read_only=on`、`statement_timeout`、`lock_timeout=5s`、`idle_in_transaction_session_timeout=60s` 等；**账号自身仍应最小权限**，代码里的只读防护是多层兜底，不替代账号权限。`temp_file_limit`、`work_mem` 等代码里无法限制，只能靠角色级设置。

```sql
-- 以超级用户执行
CREATE ROLE dbt_ro LOGIN PASSWORD '<password>' CONNECTION LIMIT 20;
ALTER ROLE dbt_ro SET default_transaction_read_only = on;
ALTER ROLE dbt_ro SET statement_timeout = '60s';
ALTER ROLE dbt_ro SET idle_in_transaction_session_timeout = '60s';
ALTER ROLE dbt_ro SET temp_file_limit = '2GB';

-- 逐库、逐 schema 授权（不要授 pg_read_all_data / pg_monitor / SUPERUSER）
GRANT CONNECT ON DATABASE <business_db> TO dbt_ro;
\c <business_db>
GRANT USAGE ON SCHEMA <schema> TO dbt_ro;
GRANT SELECT ON ALL TABLES IN SCHEMA <schema> TO dbt_ro;
ALTER DEFAULT PRIVILEGES IN SCHEMA <schema> GRANT SELECT ON TABLES TO dbt_ro;   -- 以后新建的表自动可读（按建表角色执行）
```

- 库表树 / 对象信息只列出账号有 `SELECT`（任一列）权限的表（`has_any_column_privilege`）；表注释、列注释、索引、`reltuples` 估算行数都来自 `pg_catalog`，普通账号可读。**没有权限的表不会出现在列表里**，页面的“锁”标记是平台的查询授权（`dbt_query_privileges`），与数据库账号权限是两层。实例上配了数据脱敏字段时，脱敏读视图 / 物化视图的定义用 `pg_catalog.pg_get_viewdef`（`internal/engine/postgres/views.go`），只读 `pg_catalog`，普通账号可读，不需要额外授权【待验证】。
- 引擎拒绝的敏感系统视图（`pg_authid`、`pg_shadow`、`pg_stat_activity`、`pg_stat_statements*`、`pg_hba_file_rules` …）和文件 / 大对象 / 睡眠 / 管理类函数；需要看会话 / 复制状态请用巡检 / 实例管理页面（第一期不支持）。
- **经 PgBouncer / pgpool 等代理连接**：引擎在启动包里发 `default_transaction_read_only`、`statement_timeout`、`lock_timeout`、`idle_in_transaction_session_timeout`、`extra_float_digits`、`bytea_output`、`IntervalStyle`、`DateStyle`、`application_name`、`client_encoding`、`standard_conforming_strings`，并在建连后用 ParameterStatus 二次确认 `standard_conforming_strings=on`、`client_encoding=UTF8`——代理不回报这两项就**拒绝建连（有意为之）**。PgBouncer 不认识的启动参数会报 `unsupported startup parameter`，需要：

```ini
; pgbouncer.ini
ignore_startup_parameters = default_transaction_read_only,statement_timeout,lock_timeout,idle_in_transaction_session_timeout,extra_float_digits,bytea_output
pool_mode = transaction        ; 引擎的每次查询在一个事务里完成，事务池可用
```

  这些参数被代理丢弃后，会话级的只读 / 超时就落到角色级设置（上面的 `ALTER ROLE … SET`）和引擎每次事务里的 `BEGIN READ ONLY`、`SET LOCAL statement_timeout`，所以**走代理时 `ALTER ROLE` 这几条务必配置**。【待验证】：没有在真实 PgBouncer / pgpool 上验证过。
- 全部 catalog 元数据 SQL、`DECLARE CURSOR` / `FETCH` 在扩展协议下的行为、各版本（9.6–17）兼容性都**只在假服务端（pgproto3）上测过，没有在真实 PostgreSQL 上验证**。

#### E.12 MongoDB（在线查询）

- 连接：`go.mongodb.org/mongo-driver/v2`；默认端口 27017；**认证库固定 `admin`**（所以账号要建在 `admin` 库里）；`SetDirect(true)`：只连实例记录里配置的这个节点，不做副本集拓扑发现，读偏好 `primaryPreferred`——副本集 / 分片集群请**每个节点单独登记**；“SSL”勾选 = TLS ≥ 1.2，“校验证书”不勾 = 跳过证书校验。用户名可以留空（不认证）。
- 只读账号（`read` 内置角色即可：`find`、`listCollections`、`listIndexes`、`collStats`、`dbStats` 都在其中）：

```javascript
use admin
db.createUser({
  user: "dbt_ro", pwd: "<password>",
  roles: [ { role: "read", db: "<business_db>" } ]        // 逐库授权，不要 readAnyDatabase / root / clusterAdmin
})
```

- 库列表用 `listDatabases` 的 `authorizedDatabases: true`（4.0.5+，不要求 `listDatabases` 权限，旧版本回落普通调用）；集合列表用 `listCollections` 的 `authorizedCollections: true`。`admin`、`local`、`config` 库和 `system.*` 集合不会出现，也不能查询。实例上配了数据脱敏字段时，脱敏读视图的定义（`viewOn` 与 `pipeline`）也走 `listCollections`（`read` 角色已包含），没有这个权限时每条脱敏的查询都按脱敏失败处理。
- 引擎只暴露固定的读操作（find / aggregate / count / distinct / listCollections / listIndexes / collStats / dbStats），没有“原始 runCommand”；aggregate 只放行白名单 stage（拒绝 `$out`、`$merge`、`$collStats`、`$currentOp` 等）和不含 JavaScript 的表达式（拒绝 `$where`、`$function`、`$accumulator`）；不开 `allowDiskUse`，排序 / 分组超内存时报错并提示加索引。
- 超时靠 context 截止时间推出的 `maxTimeMS`；**driver v2 的 maxTimeMS 行为、适配层整体（连接选项、`authSource=admin`、TLS、各 listXxx / stats 命令）都没有在真实 MongoDB 上验证**。字段信息是对集合前 100 个文档采样推断的，不是 schema。

#### E.13 ClickHouse（在线查询）

- 连接：**HTTP 接口**（标准库 `net/http`，不用 clickhouse-go），默认端口 8123，TLS 一般是 8443——勾选“SSL”**不会**自动改端口，需要手填；认证用 HTTP Basic（密码不进 URL 和日志）。要求较新的服务端（元数据列按约 22+ 版本编写，老版本缺列会报未知标识符）。
- 引擎每个请求都带 `readonly=1`（比 2 更严：服务端还会拒绝 `file / url / remote` 等表函数）以及 `query.max_execution_seconds`、`max_result_rows`、`max_rows_to_read`、`max_memory_usage` 等限额。**账号 profile 本身若是 `readonly=1`，服务端会拒绝请求里携带的任何设置（错误码 164 “Cannot modify … in readonly mode”），在线查询将无法使用**——账号 profile 需要用 `readonly=2`，并用 constraints 固化上限：

```sql
CREATE SETTINGS PROFILE IF NOT EXISTS dbt_ro_profile SETTINGS
  readonly = 2,
  max_execution_time = 60 MIN 1 MAX 600,
  max_memory_usage = 2000000000 MAX 16000000000,
  max_threads = 4 MAX 16,
  max_rows_to_read = 100000000 MAX 1000000000,
  max_result_rows = 1000 MAX 1000000;
CREATE USER IF NOT EXISTS dbt_ro IDENTIFIED WITH sha256_password BY '<password>' SETTINGS PROFILE 'dbt_ro_profile';
GRANT SELECT ON <business_db>.* TO dbt_ro;                  -- 逐库授权
-- 元数据与“表引擎核对”读 system.tables / columns / parts / data_skipping_indices / databases；默认每个用户都能读到自己有权限的对象，无需额外授权
```

- **引擎核对依赖账号能读 `system.tables`**：执行前会查语句涉及的表的存储引擎，`File / URL / S3 / MySQL / PostgreSQL / Kafka …` 等外部引擎表被拒绝；查不到引擎信息时按“无法确认”**默认拒绝**。`system` 库只放行摘要类表（`databases / tables / columns / parts / parts_columns / data_skipping_indices / data_type_families / table_engines / time_zones / one`），`users / grants / query_log / processes / clusters …` 一律拒绝。
- 实例上配了数据脱敏字段时，脱敏读视图的定义也查 `system.tables`（`as_select` 列，`internal/engine/clickhouse/views.go`）：账号看不到的视图这里没有，`as_select` 为空（没有权限看定义）按脱敏失败处理；服务端太旧、没有 `as_select` 列时不展开视图，不算失败。
- 静态检查是词法级的：**管理员配置的可执行 UDF、视图背后的表、Distributed / Merge 引擎背后的表不在检查能力范围内**，靠 `readonly` 和账号 ACL 兜底——不要把含敏感表的视图 / Distributed 表授给这个账号。`merge()`、`remote()`、`cluster()` 等无法静态确定对象的表函数被引擎拒绝。
- 行数上限由服务端 `max_result_rows`（`break`）加客户端按行截断实现，不改写用户的 `LIMIT`；全表排序 / 聚合这类不能流式提前结束的查询靠 `query.max_execution_seconds` / `max_rows_to_read` / `max_memory_usage` 兜底。**这些设置在真实服务端上的实际行为、`wait_end_of_query` 对错误响应的效果、64 位整数 / Decimal 的 JSON 输出形态都没有在真实 ClickHouse 上验证**。

### 附录 F　命令速查

每个代码块只有一条命令；`<…>` 为占位符。

#### F.1 服务

查看服务状态：

```bash
systemctl status dbtool
```

重启服务（会等待执行中的 SQL 工单，最长 10 分钟）：

```bash
systemctl restart dbtool
```

实时查看日志：

```bash
journalctl -u dbtool -f
```

只看最近一小时的错误日志：

```bash
journalctl -u dbtool --since "1 hour ago" -p err
```

安装脚本状态（版本、服务、监听端口、`/healthz`、`/readyz`）：

```bash
sudo ./install.sh status
```

存活探针（不查库）：

```bash
curl -fsS http://127.0.0.1:8000/healthz
```

就绪探针（查元数据库与核心表，失败返回 503；`degraded` = 缺可选扩展表）：

```bash
curl -fsS http://127.0.0.1:8000/readyz
```

确认监听端口：

```bash
ss -ltnp
```

记录当前版本指纹：

```bash
sha256sum /opt/dbtool/current/dbtool
```

#### F.2 构建 / 安装

打包（产物在 `dist/`；`ARCH=arm64`、`VERSION=<版本>`、`MAKE_RUN=1` 可选）：

```bash
make dist
```

没有 `make` 时：

```bash
bash deploy/package-centos7.sh
```

只读体检（全新主机或已安装主机均可）：

```bash
sudo ./install.sh doctor
```

先看计划、不改动任何东西：

```bash
sudo ./install.sh install --dry-run --gen-secrets --bootstrap --admin-user admin --database-url 'mysql://<用户>:<URL 编码的密码>@<主机>:3306/db_tool'
```

校验 `.run` 自解压包（仅 `MAKE_RUN=1` 生成的）：

```bash
./dbtool-<版本>-linux-amd64.run --check
```

#### F.3 元数据库

保存建表账号的 login-path（密码交互输入）：

```bash
mysql_config_editor set --login-path=dbt_ddl --host=<主机> --port=3306 --user=<用户> --password
```

演练初始化（默认只演练）：

```bash
./dbt-admin bootstrap
```

执行初始化：

```bash
./dbt-admin bootstrap --apply --yes
```

只补执行扩展表脚本：

```bash
./dbt-admin bootstrap --only-sql --apply --yes
```

查看库状态：

```bash
./dbt-admin status
```

核对表数量（预期 103；`dbt-admin` 建过的库再加 `dbt_bootstrap_applied` 共 104）：

```sql
SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='db_tool' AND table_type='BASE TABLE' AND table_name LIKE 'dbt\_%';
```

核对内置权限组（`DBA` / `TL` / `RD` / `QA` / `Default` 各一行）：

```sql
SELECT group_id, builtin FROM dbt_auth_group_ext WHERE builtin = 1;
```

#### F.4 goInception

查看容器：

```bash
docker ps --filter name=goinception
```

查看日志：

```bash
docker logs --tail 100 goinception
```

确认对方是 goInception（“测试连接”执行的就是这条语句）：

```bash
mysql -h127.0.0.1 -P4000 -u<inc-user> -p -e "inception get variables"
```

校验 sudoers 文件：

```bash
visudo -cf /etc/sudoers.d/dbtool-goinception
```

#### F.5 nginx / 证书

校验并重载配置：

```bash
nginx -t && systemctl reload nginx
```

查看证书到期日：

```bash
echo | openssl s_client -connect dba.example.com:443 2>/dev/null | openssl x509 -noout -enddate
```

查看登录页响应头：

```bash
curl -sI https://dba.example.com/login
```

### 附录 G　验证状态与已知限制

本手册里的步骤与结论按验证程度分三档。**没有写在“已验证”里的内容，不要当作已经在真实环境跑通。**

#### G.1 验证状态

| 项 | 状态 | 说明 |
|---|---|---|
| 配置加载：环境变量优先级、`dbtool.toml` 严格模式、`LISTEN` 覆盖提示 | 已验证 | `internal/config/config_test.go`（单元测试，不依赖外部环境） |
| 基础表 / 基础数据脚本 001 / 002 的语法与静态一致性（表名前缀、外键、字符集、权限码交叉检查） | 已验证（静态） | `internal/bootstrapsql`：语法树解析与交叉检查，**不连数据库** |
| `dbt-admin`（bootstrap / status / admin / gen-secrets / check-secrets / reencrypt / settings） | 仅假后端验证 | 内存假库 + 假 `database/sql` 驱动；`mysqlStore` 发出的 SQL 只核对了语句与参数 |
| `install.sh` 全流程 | 仅假命令验证 | `scripts/install-test/run.sh` 用假的 `systemctl` / `useradd` / `firewall-cmd` / `curl` / `dbt-admin` 等在临时目录里跑；真实 systemd、root 权限位与属主、firewalld、SELinux、`flock` 分支、终端无回显输入均未验证 |
| 001 / 002 / `deploy/sql/*.sql` 在真实 MySQL 上整体执行（含 3 个被截断的外键约束名、严格模式下的 `datetime … ON UPDATE` 列、两遍重跑行数不变） | 未验证 | `docs/bootstrap-design.md` 第 9 节、`docs/admin-cli.md` 第 10 节 |
| 库默认字符集预检与“表不指定字符集”在真实 MySQL（8.0 / 5.7 / MariaDB）上的效果 | 未验证 | `docs/charset-policy.md` 第 9 节 |
| 发布包（`make dist`）在真实 CentOS 上安装运行 | 未验证 | 打包脚本在 Windows Git Bash 上跑过并通过自检；`ARCH=arm64` 只验证了交叉编译与 ELF 架构检查 |
| 发布包版与源码版 Docker 镜像（`docker build`、运行、`TZ`、卷权限） | 未验证 | 4.3 |
| nginx 配置示例、限流示例 | 未验证 | 6.2 |
| goInception：页面重启（`docker restart`）、参数目录与真实版本的对应、备份库参数 | 未验证 | 收敛为 docker 后从未有过真实执行记录 |
| 目标库账号权限（附录 E 各块） | 待验证 | 由代码里执行的语句推导，未在真实服务器上逐项确认；实例管理的 `CREATE USER` / `SET GLOBAL` / `PURGE` / `KILL` 他人线程从未在真实服务器上执行过 |
| 外部系统：LDAP / OIDC / CAS / 钉钉 / 飞书 / Slack / 企业微信、阿里云 / 腾讯云短信、通知机器人、SMTP、对象存储 | 未验证 | `docs/deployment.md` 第 8 节 |
| SQL Server / PostgreSQL / MongoDB / ClickHouse / Redis 的真实实例 | 仅假后端或未验证 | 见本附录各引擎小节里的“没验证”说明 |
| 各功能设计文档里的细节（定时巡检 / 归档 / 分区维护的时区行为、归档探针、Binlog 只读保证、各引擎的权限与连接细节、goInception 参数目录等） | 未核对 | 本手册引用这些文档里的结论（附录 E 与 2.5 等处注明了出处），本次编写没有逐项重新核对其代码依据；以各文档为准 |
| 元数据库 MySQL 5.7 / 8.4 / MariaDB | 未验证 | 文档记载真库冒烟只在 8.0 上做过（`docs/deployment.md` 第 2 节，**本手册未重新核对**） |

#### G.2 已知限制

| 限制 | 影响 | 处置 |
|---|---|---|
| `SECRET_KEY` 缺省是公开值，且程序没有启动警告 | 忘记设置时会话 / JWT 可被伪造 | 安装时用 `--gen-secrets`；上线检查清单 2.8 |
| `MIRAGE_*` 一经使用不能改 | 改了实例账号等密文全部解不开 | 2.6；确需更换用 `dbt-admin reencrypt`（`docs/admin-cli.md` §6.2） |
| 进程时区一经使用不能改 | 已有时间整体偏移 | 2.5 |
| `TZ` 名字写错时静默回落到 UTC | 页面时间与预期差若干小时 | 安装后核对页面时间 |
| 进程内状态不跨进程（验证码、2FA 计数、SSO state、分析任务等） | 多实例需会话保持或单实例 | 6.6 |
| 会话是无状态签名 Cookie | 不能由服务端强制踢人；改 `SECRET_KEY` 使全部会话失效 | — |
| `dbt-admin` 在 macOS 等非 Linux / Windows 平台不支持交互输入密码 | 只能用 `DBT_ADMIN_PASSWORD` | `docs/admin-cli.md` §2 |
| Docker 镜像里没有 `docker` 命令 | goInception 页面重启不可用 | 4.3、7.1.5 |
| `install.sh` 布局（`releases/` + `current`）与手工布局（4.2）不同 | 混用同一 `--prefix` 会冲突 | 二选一 |
| `configs/env.example` / `configs/dbtool.example.toml` 里有与代码不一致的取值和旧键名 | 照抄会得到非预期的端口 / 调试日志 | 5.4 |
