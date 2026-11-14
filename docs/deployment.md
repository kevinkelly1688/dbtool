# dbtool 部署与运维指南

> **适用版本**：以 `deploy/RELEASE_VERSION` / 发布包内 `VERSION` 为准。
> **读者**：负责部署、配置与日常运维 dbtool 的运维人员 / DBA。
> **文档类型**：部署与运维文档（环境要求、安装方式、运行期配置、验证清单、升级回滚备份、排障、命令速查）。
> **相关文档**：[ops-manual.md](ops-manual.md)（全新部署的完整步骤、配置键全表、nginx、goInception、目标库账号 GRANT 模板）· [one-click-deploy.md](one-click-deploy.md)（`install.sh` 全部子命令与不变量）· [admin-cli.md](admin-cli.md)（`dbt-admin`）· [settings-reference.md](settings-reference.md)（系统设置键表）· [release-notes.md](release-notes.md)（升级前必读）· [user-guide.md](user-guide.md)（使用方法）。

dbtool 是单个 Go 二进制（服务端渲染，前端资源已内嵌），使用**独立的 MySQL 元数据库**（推荐新库 `db_tool`，所有表带 `dbt_` 前缀），从全新的空库开始。本文说明已经装起来之后怎么配置、怎么开后台任务、怎么升级与排障；**从零安装的逐步说明在 [ops-manual.md](ops-manual.md)**。

> **提示**：推荐用一键部署脚本。部署、升级、回滚、卸载、体检、初始化库、改配置都由 `install.sh`（解压发布包 `dbtool-<版本>-linux-<arch>.tar.gz` 后在包目录里运行）一条命令完成，**反复运行不会影响已存在的系统**：已安装主机再次运行等价于升级，配置 / 密钥 / 管理员 / 数据目录与数据库里的数据都不动，升级失败自动回滚。本文与脚本冲突的地方以脚本为准。

## 目录

- [0. 全新安装](#0-全新安装)
- [1. 先看这几条](#1-先看这几条)
- [2. 环境要求](#2-环境要求)
- [3. 安装](#3-安装)
- [4. 配置](#4-配置)
- [5. 建表脚本](#5-建表脚本)
- [6. 首次上线检查清单](#6-首次上线检查清单)
- [7. 后台任务的开关](#7-后台任务的开关)
- [8. 验证状态与已知限制（外部系统联调）](#8-验证状态与已知限制外部系统联调)
- [9. 升级、回滚、备份](#9-升级回滚备份)
- [10. 日常运维与排障](#10-日常运维与排障)
- [11. 多实例部署](#11-多实例部署)
- [12. 安全加固要点](#12-安全加固要点)
- [13. 命令速查](#13-命令速查)

## 0. 全新安装

从一个空库开始：基础表与基础数据由安装包里的 `bootstrap/*.sql` 提供，初始化与管理员由安装包里的 `dbt-admin` 完成（用法见 [admin-cli.md](admin-cli.md)，脚本设计见 [bootstrap-design.md](bootstrap-design.md)）。逐步说明、预期输出与回退见 [ops-manual.md](ops-manual.md) 第 3、4、8 章，这里只给最短路径。

1. 先建空库（`dbt-admin` 不会建库）：

   ```sql
   CREATE DATABASE db_tool DEFAULT CHARACTER SET utf8mb4;
   ```

2. 解压安装包：

   ```bash
   tar xzf dbtool-<版本>-linux-amd64.tar.gz && cd dbtool
   ```

3. 输入管理员密码（不要写在命令行上）：

   ```bash
   read -rs DBT_ADMIN_PASSWORD; export DBT_ADMIN_PASSWORD
   ```

4. 一条命令：随机生成密钥 + 初始化库（基础表 + 基础数据 + `sql/*.sql` 扩展表脚本）+ 创建管理员 + 启动服务：

   ```bash
   sudo --preserve-env=DBT_ADMIN_PASSWORD ./install.sh --database-url 'mysql://<用户>:<URL 编码的密码>@<主机>:3306/db_tool' --gen-secrets --bootstrap --admin-user admin --public-url https://dba.example.com
   ```

5. 终端会提示“已随机生成 SECRET_KEY 与 MIRAGE_*”（不显示值）：**立刻备份** `/etc/dbtool/dbtool.env` 里的 `SECRET_KEY` 与 `MIRAGE_*`，之后不能再改。

要点：

- 想先看会执行什么：`DATABASE_URL=… ./dbt-admin bootstrap`（默认只演练，不写任何东西；加 `--apply --yes` 才写）。
- 建表账号与运行账号分开：加 `--ddl-database-url 'mysql://…'`（或环境变量 `DDL_DATABASE_URL`），`--bootstrap` 用它执行，运行账号只需要读写权限。
- `--bootstrap` 只在 `dbt_sql_users` 为空时初始化；重跑 `install.sh`（升级）不会重复初始化，也不会重复创建管理员。
- 忘记管理员密码 / 账号被锁：`dbt-admin admin reset-password --username …` / `admin unlock --username …`（见 [admin-cli.md](admin-cli.md)）。
- 推荐使用独立的新库；库里有其它系统的表时，回退只能清空 / 删除 `dbt_` 表，不能 `DROP DATABASE`。

> **警告**：这条路径只用内存假库 / 假命令测过，**未在真实 MySQL 与 systemd 上验证**（见第 8 节），请先在演练库跑通。

## 1. 先看这几条

1. **`SECRET_KEY` 与 `MIRAGE_*` 要妥善备份，不要随意更改**（`--gen-secrets` 生成后立刻备份），否则无法解密实例账号密码等加密保存的数据；时区（`--timezone` / `TZ`，默认 `Asia/Shanghai`）一经使用也不要改（库里存的是无时区的本地时间）。
2. **表结构要先建好，且全部带 `dbt_` 前缀**：全新安装由 `dbt-admin bootstrap` 一步完成（48 张基础表 + 60 张扩展表）。扩展表脚本（`deploy/sql/*.sql`，可重复执行）每次升级带来新的 sql 文件都要再执行一遍（`install.sh upgrade` 默认会执行）。
3. **部分后台任务默认是关的**：性能工单的采集 / SLA / 验证 / 周报、分区维护自动执行（慢日志采集默认开启，只采实例上“慢日志采集”开关打开的实例）。开启方式见第 7 节。
4. **生产环境请加 `REQUIRE_GOINCEPTION=true`**，并在“平台设置 › 审核引擎”里配好连接，否则库表权限校验和 SQL 检查会降级为较弱的本地解析 / 原生执行。
5. 前面放 nginx / HTTPS，设置 `PUBLIC_URL`，并让代理透传 `X-Forwarded-Proto`（Cookie 的 Secure 标记、SSO 回调地址都依赖它）。
6. 进程内保存的少量状态（短信验证码、SSO state 防重放、2FA 失败计数）**不跨进程**，生产建议单实例部署，或在负载均衡上对同一用户做会话保持（第 11 节）。

## 2. 环境要求

| 项 | 要求 |
|---|---|
| 操作系统 | CentOS 7/8/9、Rocky、AlmaLinux、RHEL 同系（systemd）；二进制纯 Go、无 CGO，别的 Linux 也能跑，只是安装脚本按 CentOS 系写 |
| 元数据库 | MySQL 5.7 / 8.0（真库冒烟在 8.0 上做过），**独立的库**（推荐新库 `db_tool`），字符集 `utf8mb4`（见 [charset-policy.md](charset-policy.md)） |
| goInception | 强烈建议（SQL 检查、执行、回滚、表级权限解析）。没有则降级，见第 1 节 |
| 其它 | 不需要 Redis、不需要 Node；构建才需要 Go 1.25+ |
| 目标库 | MySQL 系（mysql / polardb / polardb-x / selectdb / doris / adb）为主；SQL Server 支持工单、批量上线、巡检；其它引擎见各设计文档的“没做 / 做不了” |
| 网络 | dbtool → 元数据库、各目标库、goInception、通知渠道 / IdP（按需）；用户 → dbtool（经 nginx） |

元数据库账号：日常运行需要对该库的 `SELECT / INSERT / UPDATE / DELETE`；执行 `deploy/sql` 里的建表脚本需要 `CREATE`（可用另一个管理账号执行，运行账号不需要 DDL 权限）。

脚本依赖命令、磁盘空间、端口等细节见 [ops-manual.md](ops-manual.md) 2.1；本手册不给出 CPU / 内存规格建议（仓库里没有压测数据）。

## 3. 安装

### 3.1 一键脚本（推荐）

在有 Go 的机器上打包（Windows / macOS 也可交叉编译），产物是 `dist/dbtool-<版本>-linux-amd64.tar.gz` 与 `.sha256`；arm64 用 `make dist ARCH=arm64`：

```bash
make dist
```

没有 `make`（Windows Git Bash）时用脚本，产物相同（需要自解压 `.run` 时加 `MAKE_RUN=1`），说明见 [../deploy/PACKAGING.md](../deploy/PACKAGING.md)：

```bash
bash deploy/package-centos7.sh
```

在服务器上解压后在包目录里运行 `install.sh`。全新安装见第 0 节（`--gen-secrets --bootstrap` 一条命令）；下面是库已初始化、手工指定密钥的写法：

```bash
sudo ./install.sh --secret-key '<secret-key>' --database-url 'mysql://<用户>:<URL 编码的密码>@<主机>:3306/db_tool' --public-url https://dba.example.com --require-goinception --open-firewall
```

不带参数则交互询问；`./install.sh --help` 查看全部选项。

脚本会：创建系统用户 `dbtool` → 安装到 `/opt/dbtool` → 写 `/etc/dbtool/dbtool.toml`（监听端口等，0644）与 `/etc/dbtool/dbtool.env`（密码、密钥，0640）→ 注册并启动 systemd 服务 → 按需执行 `sql/*.sql` 扩展表脚本 → 放行防火墙 → 健康检查。再次运行即升级（配置不变；版本放在 `/opt/dbtool/releases/<版本>`，`current` 原子切换，健康检查失败自动回滚；已有配置时 `--port` 等配置类选项被忽略，改配置用 `reconfigure`）。`./install.sh status` 看状态，`./install.sh doctor` 体检，`./install.sh rollback` 回滚，`./install.sh uninstall [--purge]` 卸载。

脚本还会：创建数据目录 `/opt/dbtool/data/{export,archive-spool}`（运行用户可写，0750）并在 `dbtool.env` 里**未设置时**写入 `EXPORT_BASE_DIR` / `ARCHIVE_SPOOL_DIR` 指向它们；单元文件带 `WorkingDirectory=/opt/dbtool`（配置文件仍由 `-config /etc/dbtool/dbtool.toml` 指定）。全新安装用的 `--bootstrap`、`--gen-secrets`、`--admin-user`、`--ddl-database-url` 见第 0 节与 [ops-manual.md](ops-manual.md) 4.1。

服务单元里的几个参数值得知道：`TimeoutStopSec=660`（停止时最多等 10 分钟让执行中的工单跑完）、`NoNewPrivileges=true`、`ProtectSystem=full`、`ProtectHome=true`、`PrivateTmp=true`。它们对下面几个功能有影响，见第 6.3 节。

### 3.2 手工安装

适用于前台试运行或自行用别的方式托管进程。环境变量设好后直接启动（自动读取当前目录的 `dbtool.toml`）：

```bash
DATABASE_URL='mysql://<用户>:<URL 编码的密码>@<主机>:3306/db_tool' SECRET_KEY='<secret-key>' PUBLIC_URL=https://dba.example.com ./dbtool
```

或显式指定配置文件：

```bash
./dbtool -config /path/to/dbtool.toml
```

`dbtool.example.toml` 里的 `port = 8866` 不是默认值（默认 8000），复制后按需修改；生产请交给 systemd。手工 systemd 单元与目录布局见 [ops-manual.md](ops-manual.md) 4.2。

### 3.3 Docker

`deploy/Dockerfile` 生成基于 distroless 的镜像（只含二进制，默认监听 8000）：

```bash
docker build -f deploy/Dockerfile -t dbtool .
```

```bash
docker run -d --name dbtool -p 8000:8000 --env-file dbtool.env dbtool
```

要改端口：挂载配置文件并用 `DB_TOOL_CONFIG` 指向它（`-v ./dbtool.toml:/dbtool.toml -e DB_TOOL_CONFIG=/dbtool.toml`），或直接设环境变量 `LISTEN=:9000`，同时调整 `-p` 映射。`--env-file` 不处理引号，值里不要加双引号（`install.sh` 生成的 `dbtool.env` 带双引号，需要另存一份去掉引号的文件）。

容器里没有 shell 和 mysql 客户端：建表脚本要在别处执行；导出文件目录要挂卷（`EXPORT_BASE_DIR`）。停止超时要与单元一致（`--stop-timeout 660`），否则执行中的工单会被提前终止。

**goInception 配置页的“重启”在这个镜像里不可用**：页面重启只支持 `docker restart <容器名>`（命令不经 shell，直接 exec），而 distroless 镜像里既没有 `docker` 命令，也没有挂载 `docker.sock`。这种部署下**不要在“部署与重启”里填容器名**——页面会提示“尚未设置 goInception 的 Docker 容器名”，保存并重启 / 仅重启 / 回滚都不可用，配置文件只能“仅保存”（要挂载 `GOINCEPTION_CONFIG_FILE` 所在目录并保证可写），保存后自行重启 goInception（保存的版本会一直显示“待重启生效”）。如果填了容器名，执行 `docker restart` 会失败，流程记为失败（会自动恢复旧配置并再试一次，同样失败）。

> **警告**：确实需要在页面里重启时，要自行扩展镜像（安装 docker 客户端）并挂载 `-v /var/run/docker.sock:/var/run/docker.sock`。这等于把宿主机的 Docker 控制权交给 dbtool 容器，请评估风险；容器内的运行用户还要有访问 `docker.sock` 的权限。

## 4. 配置

### 4.0 配置文件（`dbtool.toml`，监听端口在这里改）

不含密钥的部署设置放在 TOML 配置文件里，示例见 `configs/dbtool.example.toml`（安装包里是 `dbtool.example.toml`）：

```toml
[server]
port = 8000          # 监听端口，1–65535，默认 8000
host = "0.0.0.0"     # 监听地址；省略 = 所有网卡，只给本机 nginx 用时写 "127.0.0.1"

[site]
version = "1.05"                        # 页面左下角（侧栏底部）显示的版本号；省略 = 显示程序自带的版本
icp = "京ICP备12345678号-1"              # 页面底部（登录页与登录后的页面）显示的 ICP 备案号；省略 = 不显示
icp_url = "https://beian.miit.gov.cn/"  # 备案号的链接；省略 = 指向工信部备案查询站
```

- **查找顺序**：命令行 `-config 路径` → 环境变量 `DB_TOOL_CONFIG` → 当前目录 `dbtool.toml` → 可执行文件所在目录 `dbtool.toml` → `/etc/dbtool/dbtool.toml`（`install.sh` 写在这里）。前两种方式指定的文件必须存在，否则启动失败；都没有则全部使用默认值与环境变量。
- **优先级**：环境变量 `LISTEN` > 配置文件 > 默认 `:8000`。两边都给且不一致时，启动日志会警告“环境变量 LISTEN 覆盖了配置文件”。
- **校验**：端口不是 1–65535 的整数、键名拼错、TOML 语法错误，都会在启动时报错（带文件名与行号），不会悄悄用默认端口。当前有 `[server]` 的 `port`、`host` 与 `[site]` 的 `version`、`icp`、`icp_url` 五项。
- **版本号与备案号**（`[site]`，改后重启生效）：`version` 以数字开头时页面上自动补 `v`（`1.05` 显示为 `v1.05`），省略或留空显示程序自带的版本（`-version` 打印的那个，`make dist` 打包时注入）；`icp` 省略或留空则页面底部不显示备案号；`icp_url` 必须以 `http://` 或 `https://` 开头，否则启动失败。`install.sh` 只在全新安装或缺少文件时生成 `dbtool.toml`（只含 `[server]`），`[site]` 需要手工加到 `/etc/dbtool/dbtool.toml`；之后升级与 `reconfigure` 不会改动它。
- **改端口**：编辑 `/etc/dbtool/dbtool.toml` 的 `port`，`systemctl restart dbtool`，并放行新端口（`firewall-cmd --permanent --add-port=9000/tcp && firewall-cmd --reload`）；也可以 `./install.sh reconfigure --port 9000 --open-firewall --restart` 一步完成（先备份、原子替换，重启后健康检查，失败自动恢复）。反向代理（nginx）的上游端口、`PUBLIC_URL` 也要跟着改。
- **从旧版升级**：旧版把端口写在 `dbtool.env` 的 `LISTEN=` 里。升级时 `install.sh` 在没有 `dbtool.toml` 的情况下会按它生成等价的 `dbtool.toml`，env 里的 `LISTEN` 行保持不动（它仍然优先，行为不变）并给出提示；`./install.sh reconfigure --port N` 才会先备份、再把 `LISTEN` 行注释掉。手工部署的旧环境，删掉 `LISTEN` 变量再写配置文件即可。
- 数据库密码、`SECRET_KEY` 等敏感项**不要**写进这个文件，仍然放环境变量（见下）。

### 4.1 环境变量（`/etc/dbtool/dbtool.env`，示例见 `configs/env.example`）

全部环境变量只在启动时读取，改后 `systemctl restart dbtool`。下表是运维中最常碰到的；**全部变量（含 `DEFAULT_LANG`、`SSH_TUNNEL_*`、`REDIS_*`、`MIRAGE_CIPHER_*`）、取值范围与敏感性见 [ops-manual.md](ops-manual.md) 5.2**。

| 变量 | 说明 | 默认 |
|---|---|---|
| `DATABASE_URL` | 元数据库 `mysql://用户:密码@主机:3306/库名`（密码含特殊字符需 URL 编码；URL 里的查询参数被忽略） | 必填 |
| `SECRET_KEY` | 会话 / JWT 签名；未单独设置 `MIRAGE_SECRET_KEY` 时也是实例账号密码等加密字段的密钥；一经设置不要随意更改 | 开发默认值（公开值，生产必须改成随机值，可用 `dbt-admin gen-secrets` 生成） |
| `MIRAGE_SECRET_KEY` / `MIRAGE_CIPHER_MODE` / `MIRAGE_CIPHER_IV` | 字段加密（mirage）单独使用的密钥（≥ 32 字符）/ 模式（`ECB` 或 `CBC`）/ IV（CBC 用，16 字节）；不设置时密钥取 `SECRET_KEY` | 模式 `ECB`、IV 取内置缺省值 |
| `LISTEN` | 监听地址 `主机:端口`，**优先于配置文件**；一般不需要设，端口请写在 `dbtool.toml` | 取配置文件，再默认 `:8000` |
| `DB_TOOL_CONFIG` | 配置文件路径（命令行 `-config` 优先于它） | 按 4.0 的顺序查找 |
| `PUBLIC_URL` | 对外地址，SSO 回调、通知里的工单链接用它 | 空（回落到 `http://127.0.0.1:端口`，生产必须配） |
| `REQUIRE_GOINCEPTION` | 开启后检查 / 执行 / 库表权限必须用 goInception，未配置则拒绝 | `false`（生产建议 `true`） |
| `BATCH_DB_TYPES` | 允许批量上线 / 导出的实例类型，逗号分隔（如 `mssql,mysql`；批量导出对 MySQL 系已有测试覆盖） | `mssql` |
| `EXPORT_BASE_DIR` | 导出文件基准目录 | 当前目录（`install.sh` 写入 `<PREFIX>/data/export`） |
| `ARCHIVE_SPOOL_DIR` | 归档文件备份的本地暂存目录（必须持久目录） | `$EXPORT_BASE_DIR/archive-spool`（`install.sh` 写入 `<PREFIX>/data/archive-spool`） |
| `GOINCEPTION_CONFIG_FILE` | goInception `config.toml` 的绝对路径（goInception 配置页读写它；也可用系统设置 `goinception.config_file`，环境变量优先） | 空（`install.sh` 写入 `<PREFIX>/goinception/config.toml`） |
| `SLOWLOG_COLLECT_ENABLE` | 设 `0` / `false` / `no` / `off` 才整体关闭慢日志采集（MySQL 与 Redis 共用）；采哪些实例由实例上的“慢日志采集”开关决定 | 开 |
| `SLOW_COLLECT_INTERVAL` / `SLOW_COLLECT_WORKERS` / `SLOWLOG_COLLECT_BACKFILL_DAYS` | 采集间隔秒（5–3600）/ 并发（1–8）/ 首次回溯天数（0–365） | 10 / 2 / 7 |
| `DEBUG` | `true` 时打印 SQL 日志（含参数），生产不要开；`configs/env.example` 里写的是 `DEBUG=true`，照抄会开启 | `false` |

### 4.2 页面里配置的项（不用改文件、即时生效）

- **平台设置 › 审核引擎**：goInception 连接信息、备份库、`config.toml`、Docker 容器名（页面重启只支持 `docker restart`）。
- **访问控制 › 登录认证 / 登录与安全设置**：LDAP / OIDC / 钉钉 / CAS / 飞书 / Lark / Slack / 企业微信，锁定策略、2FA、密码策略、会话与 JWT 有效期、API 白名单、水印、短信服务。
- **平台设置 › 通知渠道**：钉钉 / 企业微信 / 飞书 / Lark / Slack / Telegram 机器人、邮件（SMTP）。
- **访问控制 › 权限组**：按钮级权限（权限组授权、无权限按钮的显示方式）。

### 4.3 系统设置（`dbt_setting`；页面：平台设置 › 系统配置；命令行：`dbt-admin settings`）

本系统的设置体系：键名 `<领域>.<名称>`，每项有类型、默认值与取值范围，只存显式设置过的项。完整清单见 [settings-reference.md](settings-reference.md)（由登记表生成），规则见 [settings-design.md](settings-design.md)。下面是常用项：

| 设置键 | 作用 | 默认 |
|---|---|---|
| `goinception.host` / `port` / `user` / `password`、`goinception.backup_*` | goInception 连接（建议在“goInception 配置”页里改，那里有连接测试；“系统配置”页不显示这一组） | 端口 4000、备份库端口 3306，其余空 |
| `perf.jobs` | 性能工单的后台任务，逗号分隔 `collect,sla,verify,report` | 空（全关） |
| `perf.*` 其余项 | 静默时段、验证天数、严重度规则等 | — |
| `partition.auto_run` / `partition.auto_drop` / `partition.auto_interval_minutes` | 分区维护自动执行 / 允许自动删除过期分区 / 间隔分钟 | 关 / 关 / 180 |
| `partition.alert_enabled` / `partition.alert_min_available` / `partition.alert_repeat_hours` / `partition.alert_notify_recover` | 分区维护的可用分区数量告警：总开关 / 全局默认阈值（可用分区数小于它就告警，每张表可在页面里单独设）/ 持续不足时的重复提醒间隔（小时，0 = 不重复）/ 回到阈值以上时推“已恢复”；需要先执行 `280_partman_alert.sql`，检查间隔同 `partition.auto_interval_minutes`，自动维护没开也检查 | 开 / 3 / 24 / 开 |
| `partition.notify_on` | 分区维护的 webhook 推送时机：`always`（执行了语句或失败就推）/ `on_failure`（只在失败时推）/ `never`；推送通道在页面“分区维护 › 推送设置”里配 | `always` |
| `optimizer.soar_path`、`optimizer.soar_test_dsn`、`optimizer.soar_config`（超级用户在“系统配置 › SQL 优化工具”里设置，保存时会核对文件和测试库） | SQL 优化外部工具；没配的工具在页面上提示“未配置”，EXPLAIN 与内置规则不依赖它们 | 空 |
| `export.max_rows`、`export.max_seconds`、`export.local_path`、`export.storage_type` | 数据导出的行数 / 时间上限、本地目录（仅支持本地存储） | 10000 / 60 / `downloads/DataExportFile/` / `local` |
| `security.login_lock_attempts` / `security.login_lock_seconds` / `security.require_2fa` / `security.self_signup` 等 | 登录安全项（也可在“登录与安全设置”页改） | 5 / 300 秒 / 关 / 关 |
| `retention.*`、`inspect.*_retention_days` | 数据保留与清理（总开关 `retention.enabled` 默认关闭，开启后到期数据被物理删除） | 关 |

> **注意**：`inspection_plan_manage` 是权限码，不是设置项：定时巡检计划的管理权限，可选，超级用户始终可管理。

## 5. 建表脚本

`deploy/sql/*.sql`（包内 `sql/*.sql`）共 34 个脚本、创建 58 张扩展表，全部幂等（`CREATE TABLE IF NOT EXISTS`；会改已有表的 `150_builtin_groups.sql`、`180_archive.sql`（给 `dbt_archive_task` 补 `time_unit` 列）、`290_export_unmasked.sql`（给 `dbt_sql_export_ext` 补 `merge_files` 列）都先查 `information_schema` 再动态执行），按文件名升序执行即可。**每个脚本创建的表、用途、依赖、是否需要重启与“不执行的后果”见 [ops-manual.md](ops-manual.md) 附录 B**，这里不再重复。

执行方式（三选一）：

- 全新空库：`dbt-admin bootstrap`（或 `install.sh --bootstrap`），已包含它们。
- 升级后补执行：`install.sh upgrade` 默认执行；也可 `install.sh init-db`（库为空才建基础结构，否则只执行 `sql/*.sql`），或：

  ```bash
  dbt-admin bootstrap --only-sql --apply --yes
  ```

- 没有 `dbt-admin` 时用 `mysql` 客户端按编号顺序逐个执行，见 [ops-manual.md](ops-manual.md) 3.4。

编号 003–010、012、014、016 没有使用（对应模块直接用 `deploy/bootstrap` 里的基础表）。`260_setting.sql` 创建 `dbt_setting`，是 `/readyz` 的必需表。018 / 019 / 050 / 070 / 080 / 230 等脚本执行后需要重启服务才生效。

**权限码（基础数据已内置，可选授权）**：

- `inspection_plan_manage`（管理定时巡检计划）：要让非超级用户管理定时巡检计划，在“权限组管理”里把它授给对应权限组；超级用户始终可管理。
- `binlog_del`（实例列表里的 Binlog 清理）：同样在“权限组管理”里授权后非超级用户才能使用。
- 性能工单的 `menu_perf`、`perf_manage` 在 `sql_perf` 应用下，已内置。

## 6. 首次上线检查清单

按顺序做（每项的命令与预期见 [ops-manual.md](ops-manual.md) 8.3、8.4）：

1. **启动并登录**：用 `dbt-admin admin create`（或 `install.sh --admin-user`）建的超级用户登录。看 `journalctl -u dbtool`：没有“登录方式 / 安全设置功能未启用”之类提示（有就是 018 没执行）。
2. **登录与安全设置**：确认账号锁定阈值、是否强制 2FA、会话有效期、新用户默认权限组 / 资源组；需要 SSO 的先到“登录方式”添加并**测试通过后再启用**。本地账号永远保留给超级用户（`/login?local=1`），SSO 配错了也不会把自己锁在外面。
3. **goInception 配置**：填连接信息并“测试连接”；如果要在页面里改 `config.toml`：用 `install.sh` 安装时已自动设好 `GOINCEPTION_CONFIG_FILE=<安装目录>/goinception/config.toml` 并部署初始文件（默认 `/opt/dbtool/goinception/config.toml`，已有则不覆盖）；手工部署的才需要自己设。然后在“部署与重启”里填好 goInception 的 Docker 容器名（页面重启只支持 `docker restart`，不填则只能“仅保存”；第 6.3 节）。
4. **资源组 / 权限组 / 审批流程**：资源组关联用户与实例；权限组按角色勾选权限码；每个资源组按工单类型（查询权限申请、SQL 上线、数据归档）配置审批流，页面会提示“该级无人可审”。
5. **通知渠道**：按资源组配机器人并“测试发送”。
6. **实例**：实例列表里检查实例、标签（`can_read` / `can_write`）、慢日志采集开关。
7. **抽样验证**：用一个普通用户走一遍“在线查询 → 申请权限 → 审批 → 查询”和“提交 SQL 工单 → 审批 → 执行”，确认通知、审计日志、导出下载正常。
8. **按钮级权限**：权限只认按钮这一套。`install.sh install / upgrade` 在健康检查通过之后自动执行 `dbt-admin perm migrate --apply`，把权限组 / 用户的原权限码一次性迁移成按钮授权并标记为已迁移（幂等、只增不减，撤销脚本存在配置备份目录里）；建议升级前先用新包里的 `./dbt-admin perm migrate`（不加 `--apply`，只读）预览。

### 6.1 反向代理（nginx）

完整的 nginx 配置（含 HTTPS、SSO 路径关日志、限流示例）、对代理的全部要求与验证命令见 [ops-manual.md](ops-manual.md) 第 6 章。最小要求：

```nginx
server {
    listen 443 ssl;
    server_name dba.example.com;
    # ssl_certificate ...
    client_max_body_size 16m;                # 程序自己的请求体上限：SQL 优化分析 12MB、工单相关 8MB
    location / {
        proxy_pass http://127.0.0.1:8000;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_set_header X-Forwarded-Proto $scheme;   # 必须，Cookie Secure 与 SSO 回调依赖
        proxy_read_timeout 300s;             # 导出下载等较慢的请求
    }
}
```

### 6.2 文件与目录权限

- `/etc/dbtool/dbtool.env` 里有数据库密码和 `SECRET_KEY`，保持 `0640`、属主 root、组 `dbtool`；`dbtool.toml` 不含密钥，`0644` 即可。
- 数据导出目录（`EXPORT_BASE_DIR` 下的 `downloads/DataExportFile/`）要对运行用户可写，并定期清理（或用保留策略清理导出文件）。
- 访问日志不会记录 SSO 回调的查询串（授权码、ticket、state）。

### 6.3 安装脚本生成的服务单元对哪些功能有影响

| 功能 | 需要 | 处理 |
|---|---|---|
| goInception 配置页**写它的 `config.toml`** | 运行用户对文件**所在目录**可写（原子替换） | `install.sh` 默认把配置放在 `/opt/dbtool/goinception/config.toml`（属主 `dbtool`，目录 0750 / 文件 0640），不受 `ProtectSystem=full` 影响。**只有**放在 `/etc` 下才需要 drop-in：`/etc/systemd/system/dbtool.service.d/override.conf` 写入 `[Service]` 与 `ReadWritePaths=/etc/goinception`（目录按实际填），再 `systemctl daemon-reload && systemctl restart dbtool`；并把目录授权给 `dbtool` |
| goInception 配置页重启：`docker restart <容器名>`（页面唯一的重启方式；勾选“使用 sudo -n”时为 `sudo -n docker restart <容器名>`） | dbtool 运行用户能执行 `docker`：要么在 `docker` 组里（能访问 `docker.sock`），要么走 `sudo -n` + sudoers 白名单 | 勾选 sudo 时：`NoNewPrivileges=true` 会让 sudo 失效，要在 drop-in 里加 `NoNewPrivileges=false`，并在 sudoers 里**只放行** `dbtool` 执行 `docker restart <容器名>` 这一条。不想放宽提权：把 `dbtool` 加进 `docker` 组（它等价于对宿主机的 root 权限，请评估）并且不勾 sudo。不填容器名则页面不执行任何重启，配置文件只能“仅保存”，之后自行重启 goInception |
| 数据导出写文件 | `EXPORT_BASE_DIR` 可写 | 若设在 `/home` 下会被 `ProtectHome` 挡住，放到 `/var/lib/dbtool` 一类位置，必要时同样用 `ReadWritePaths` |
| 归档文件暂存 | `ARCHIVE_SPOOL_DIR` 是持久目录 | 不要放 `/tmp`（`PrivateTmp=true` 下重启即清空） |

drop-in 目录里的内容 `install.sh` 升级和卸载都不会改动；对主单元文件的手工修改会在升级时被备份为 `.bak` 后重写，所以自定义项请写成 drop-in。

## 7. 后台任务的开关

| 任务 | 开关 | 开启步骤 |
|---|---|---|
| 慢日志采集 | 实例上的“慢日志采集”开关（采集器默认开启） | ① 在实例管理里打开要采集的实例的开关 ② 在“慢查日志”页底部看采集状态。注意首次只回溯 7 天（`SLOWLOG_COLLECT_BACKFILL_DAYS`）；采集写 `dbt_slow_logs` |
| 性能工单（采集 / SLA 巡检 / 每日验证 / 周报） | `perf.jobs`（默认全关） | ① 在“平台设置 › 系统配置”里设 `perf.jobs`（如 `collect,sla,verify,report`，每分钟重读，无需重启）② 看“采集配置”页的上次采集结果 |
| 分区维护自动执行 | `partition.auto_run`（默认关） | ① 在“分区维护”页确认分区配置，在“推送设置”里确认推送通道并“测试发送”（推送地址只允许平台域名 + https，指向自建转发的发不出去）② 再打开 `partition.auto_run`。近期日志保护窗口只用于多实例 / 手动与定时撞车。只自动“续建”，删除需另开 `partition.auto_drop`，REORGANIZE 与初始化未分区表永不自动执行，推送时机见 `partition.notify_on` |
| 定时巡检 | 页面里建计划 | 在“性能巡检”页建计划并启用 |
| SQL 工单定时执行 | 自动 | 计划存在 `dbt_sql_schedule`，由进程内调度器执行 |
| 数据保留清理 | `retention.enabled`（默认关） | 先只设各目标的保留天数、不开总开关，用预览确认数量，再开总开关 |

**权限**：权限只认按钮（权限组授权 + 用户单独授权 / 禁用）；从早期版本升级时，原来的权限码由 `dbt-admin perm migrate` 一次性迁移成按钮授权（升级脚本自动执行）。权限变更最长约 15 秒后在各页面生效（进程内缓存）。

## 8. 验证状态与已知限制（外部系统联调）

以下功能代码和单元测试已完成，但**没有对真实外部系统联调过**，上线前请按各设计文档末尾的清单验证。表分三档：**未联调** = 只有单元测试，没有真实外部系统；**仅假后端** = 用假目标库 / 假 webhook / 假驱动测过；**已知限制** = 有意为之或做不了。

| 范围 | 状态 | 说明 |
|---|---|---|
| 登录：LDAP / AD（LDAPS、StartTLS、证书）、钉钉、飞书 / Lark、Slack、企业微信、真实 OIDC / CAS 服务；阿里云 / 腾讯云短信 | 未联调 | — |
| goInception：真实版本的参数目录、`processlist` 列、`docker restart` 重启（`docker.sock` 权限 / sudoers 白名单 / 容器不存在时的失败提示） | 未联调 | 页面重启只支持 docker，未做过真实验证 |
| SQL 优化：SOAR 的实际参数与输出 | 未联调 | — |
| SQL Server：工单 / 批量 / 导出 / 巡检 | 未联调 | 未在真实 SQL Server 上验证 |
| MySQL 5.7 / 8.4 / PolarDB / Doris：采集与 EXPLAIN 语句 | 只在 8.0 上验证 | — |
| 分区维护：SelectDB / Doris 方言、MySQL 的“初始化为分区表”、四类 webhook（Slack / 企业微信 / 飞书 / 钉钉）推送 | 仅假后端 | 未在真实环境验证 |
| 通知机器人真实发送 | 未联调 | — |
| SSH 隧道 | 未联调 | 已接入公共连接层（`store.EngineConn` 对配置了隧道的实例返回本机转发器地址）；未验证真实跳板机，且**不校验跳板机主机公钥**（已知限制） |
| 全新部署路径（`dbt-admin` / `install.sh`） | 仅假后端 | 见 [ops-manual.md](ops-manual.md) 附录 G |

## 9. 升级、回滚、备份

升级前先读 [release-notes.md](release-notes.md) 的“一、升级前必读”（行为变化清单）。

**升级**：`sudo ./install.sh upgrade`（或解压新包后同一入口；先加 `--dry-run` 看计划）：加锁 → 预检 → 备份配置 → 解包并校验 sha256 → 执行新增的 `sql/*.sql`（`--only-sql`，建表账号用 `--ddl-database-url`）→ 原子切换 `current` → 重启 → 健康检查，失败自动回滚 → 健康检查通过后执行一次权限迁移（`dbt-admin perm migrate --apply`，把原权限码迁移成按钮授权，幂等；输出实时显示，最多等 2 分钟，失败只警告；`--skip-perm-migrate` 可跳过）。配置保持不变。停止服务时会等执行中的工单最多 10 分钟，仍未结束的工单会停在“执行中”，需到目标库确认实际结果。

**回滚**：`sudo /opt/dbtool/current/install.sh rollback`（回到上一版本，或 `--to <版本>`；默认保留最近 3 个版本）。数据库不回滚，新增的扩展表无需删除。

**备份**（建议；仓库里没有备份恢复脚本）：

| 对象 | 说明 |
|---|---|
| 元数据库 | 所有持久数据都在这个库里（所有表都带 `dbt_` 前缀）；用 MySQL 通用的逻辑 / 物理备份方式备份【建议】 |
| `/etc/dbtool/dbtool.env` | 含 `DATABASE_URL`、`SECRET_KEY`、`MIRAGE_*`；**丢失后库里的实例账号密码等密文无法解密**，与数据库备份分开保管 |
| `/etc/dbtool/dbtool.toml` | 监听地址 / 端口 |
| `/etc/dbtool/backup/<时间戳>/` | 升级 / 回滚 / `reconfigure` 前脚本自动备份的配置，保留最近 3 份 |
| SOAR（`<PREFIX>/plugins/soar`、`soar.yaml`） | 随安装包部署（每次 install / upgrade 重新部署程序；`soar.yaml` 已存在不覆盖），不含密钥，不需要备份；测试库 DSN 存在系统设置里（加密） |
| goInception 的 `config.toml` | 默认 `/opt/dbtool/goinception/config.toml`（含备份库密码，注意权限）；页面里也有版本历史（`dbt_gi_config_version`） |
| `<PREFIX>/data/export`、`<PREFIX>/data/archive-spool` | 导出文件按需备份；归档暂存里可能有删源之后、尚未上传的备份，**不要随意清理** |
| 归档备份存储（`local` / `nfs` 目录、S3 兼容对象存储） | 最终归档备份文件 |

> **注意**：`uninstall` 默认保留 `/etc/dbtool` 与 `<PREFIX>/data`，绝不触碰数据库；`--purge` 才删除本机文件并要求键入 `PURGE`。细节见 [one-click-deploy.md](one-click-deploy.md)。

## 10. 日常运维与排障

日志：`journalctl -u dbtool -f`；存活检查：`/healthz`（只证明进程在监听，不查库）；就绪检查：`/readyz`（查元数据库与核心表，失败 503，`degraded` = 缺可选扩展表）。

| 现象 | 可能原因 | 处理 |
|---|---|---|
| goInception 配置页“保存并重启”失败，提示 `permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock` | dbtool 运行用户不在 `docker` 组，连不上 `docker.sock`（`root:docker`，0660）。配置文件已写入，容器没有被重启；平台会自动恢复旧配置并再次重启，同样失败，最终记为失败 | 方案一：`usermod -aG docker dbtool`，再 `systemctl restart dbtool`（服务重启后才获得新组），页面点“仅重启”；`docker` 组等价于宿主机 root，请评估。方案二：改用 sudo 白名单，见第 6.3 节与 [ops-manual.md](ops-manual.md) 7.1.4。方案三：容器名留空，只“仅保存”，自行 `docker restart` |
| 启动失败，日志“加载配置失败” | `DATABASE_URL 未设置`；配置文件有不认识的键 / 语法错误 / 端口不合法（报文件名与行号）；`DEFAULT_LANG` 不是 `zh-CN` / `en`；`DB_TOOL_CONFIG` / `-config` 指向的文件不存在 | 按日志提示修正后 `systemctl restart dbtool` |
| 启动失败，日志“初始化存储失败” | 元数据库连不上、库不存在、账号密码不对；`MIRAGE_SECRET_KEY`（缺省取 `SECRET_KEY`）短于 32 个字符；`MIRAGE_CIPHER_MODE` 不是 `ECB` / `CBC` | 核对 `dbtool.env`；`install.sh doctor` 会用 `dbt-admin status` 检查连库 |
| 端口被占用 | 监听端口已被其它进程使用 | `install.sh doctor` 的端口检查；换端口见 4.0 |
| `/readyz` 返回 503 | 元数据库不可达 / 超过 2 秒，或缺核心表（含 `dbt_setting`） | 看服务日志里的“就绪探针：…”；缺表执行 `install.sh init-db` |
| `/readyz` 返回 `degraded` | 缺可选扩展表，对应功能降级 | `install.sh init-db`（或 `dbt-admin bootstrap --only-sql --apply --yes`）补建，再 `systemctl restart dbtool` |
| 实例连不上 / 账号解密失败；启动日志提示“密文解不开” | `SECRET_KEY`（或 `MIRAGE_*`）与加密这些数据时使用的值不一致 | 运行 `dbt-admin check-secrets` 核对，必要时 `dbt-admin reencrypt`（见 [admin-cli.md](admin-cli.md)） |
| 页面里时间差 8 小时 | `--timezone` / `TZ` 与写入这些时间时使用的时区不一致；`TZ` 名字写错时回落到 UTC | 核对 `dbtool.env` 的 `TZ`；已有数据后不要改时区 |
| 分区维护页提示“可用分区告警的扩展表还没有建”，或收不到可用分区告警 | 没执行 `deploy/sql/280_partman_alert.sql`；总开关 `partition.alert_enabled` 关着；“推送设置”里没有启用的通道；该表在静默期或阈值为 0 | 执行脚本后刷新页面（升级时安装脚本会自动执行）；推送情况见页面里的“告警记录” |
| 页面空白或 403 | 没有对应权限码或按钮权限 | “访问控制 › 权限组 › 按用户查看有效权限”看来源；权限组页面顶部有“还没迁移”提示时先执行 `dbt-admin perm migrate` |
| 登录后 Cookie 不带 `Secure` / SSO 回调报错 | 代理没有透传 `X-Forwarded-Proto`；`PUBLIC_URL` 与 IdP 登记的回调地址不一致 | 见 [ops-manual.md](ops-manual.md) 6.1；回调地址是 `<PUBLIC_URL>/login/sso/<登录方式ID>/callback` |
| 慢日志 / 性能工单没有数据 | 后台任务没打开 | 慢日志：实例上的“慢日志采集”开关与 `SLOWLOG_COLLECT_ENABLE`；性能工单：`perf.jobs`；见第 7 节 |
| 导出失败 / 找不到导出文件 | `EXPORT_BASE_DIR` 不存在或运行用户不可写；设在 `/home` 下被 `ProtectHome` 挡住 | 见第 6.3 节 |
| goInception 配置页保存失败 | 运行用户对配置文件所在目录不可写（目录只读 / 单文件挂载 / `ProtectSystem` 下的 `/etc`） | 见第 6.3 节与 [ops-manual.md](ops-manual.md) 7.1.3 |
| goInception 页面重启失败 | `NoNewPrivileges=true` 使 `sudo` 失效；镜像里没有 `docker`；容器名不合法 | 见第 6.3 节与 [ops-manual.md](ops-manual.md) 7.1.4 |
| 升级后某功能提示“请先执行 deploy/sql/NNN” | 运行账号没有建表权限，脚本只警告 | `DDL_DATABASE_URL=… ./install.sh init-db`（或升级时 `--ddl-database-url`）补建，再 `systemctl restart dbtool` |
| 停止 / 重启服务很慢 | 正在等执行中的 SQL 工单（`TimeoutStopSec=660`，最多 10 分钟） | 属预期；超时仍未结束的工单停在“执行中”，到目标库确认 |
| 归档暂存目录里有遗留 `*.tmp` | 进程被强杀时尚未上传的备份 | 服务启动日志会提示；**不会自动上传或删除**，需人工核对补传 |
| 非超级用户打开页面慢 | 每个页面要判断几十个权限；权限已按用户做了 15 秒缓存，元数据库延迟高时首次打开会慢 | 元数据库放近一点 |

**生产不要开 `DEBUG=true`**（会把带参数的 SQL 打进日志）。

## 11. 多实例部署

支持多个 dbtool 进程指向同一个库：定时巡检、性能工单任务、SQL 工单调度、分区维护都用数据库行 / 条件更新做了防重。但下列状态仍在进程内，**需要会话保持或单实例**：短信验证码、已用 SSO state、TOTP 防重放窗口、2FA 失败计数、SQL 优化分析任务（结果保留 30 分钟）。会话本身是无状态签名 Cookie，不能由服务端强制踢出（停用用户后，其管理类接口立即失效，其它接口在会话到期前仍可用）。完整清单见 [ops-manual.md](ops-manual.md) 6.6。

## 12. 安全加固要点

只列代码行为已经保证、或配置项能直接落实的做法；各项依据见括号里的位置。

| 要点 | 做法 | 依据 |
|---|---|---|
| 密钥 | 生产必须覆盖 `SECRET_KEY`（缺省是公开值且没有启动警告）；`MIRAGE_*` 单独设置并备份；`dbtool.env` 保持 `0640 root:dbtool` | [ops-manual.md](ops-manual.md) 2.6、5.2 |
| 监听 | 只给 nginx 用时 `host = "127.0.0.1"`；直接暴露监听端口会让客户端伪造 `X-Forwarded-For`，绕过按 IP 的登录限流并污染审计来源 IP | [ops-manual.md](ops-manual.md) 6.1 |
| 代理头 | `X-Forwarded-For` 用 `$remote_addr` 覆盖（不要追加）；透传 `X-Forwarded-Proto`；HSTS 由代理加（程序不设）；不要再叠加 CSP | [ops-manual.md](ops-manual.md) 6.1 |
| 调试 | 不要设 `DEBUG=true`（SQL 含参数进日志）；`configs/env.example` 里的 `DEBUG=true` 与公开 `SECRET_KEY` 不能照抄 | [ops-manual.md](ops-manual.md) 5.4 |
| 账号 | 至少保留两个超级用户；`security.superuser_local_only` 缺省开启（超级用户只能用本地密码）；锁定阈值 `security.login_lock_attempts` 缺省 5 次 / `security.login_lock_seconds` 缺省 300 秒 | [settings-reference.md](settings-reference.md) |
| API | `security.api_user_ids` 为空 = 不限制用户申请 API 令牌，按需填写白名单 | [settings-reference.md](settings-reference.md) |
| 提权 | 页面重启 goInception 需要放宽 `NoNewPrivileges=false` 时，sudoers 只放行 `docker restart <容器名>` 这一条固定命令；把 `dbtool` 加进 `docker` 组等价于授予宿主机 root | 第 6.3 节、[ops-manual.md](ops-manual.md) 7.1.4 |
| 文件 | `<PREFIX>/data/archive-spool` 与 goInception 配置目录（含 `.dbtool.bak` 密码明文）限定运行用户访问，纳入备份 | [ops-manual.md](ops-manual.md) 4.5 |
| 目标库账号 | 每个实例只登记一组账号、各模块共用：按需叠加最小权限，必要时同库登记多条实例记录做隔离 | [ops-manual.md](ops-manual.md) 2.4、附录 E |
| 数据保留 | 清理策略总开关默认关闭；审计日志属合规数据，先确认公司保留期再设天数 | [settings-reference.md](settings-reference.md) 的 `retention.*` |

## 13. 命令速查

安装脚本体检（只读）：

```bash
sudo ./install.sh doctor
```

安装脚本状态（版本、服务、端口、`/healthz`、`/readyz`）：

```bash
sudo ./install.sh status
```

升级（先 `--dry-run` 看计划）：

```bash
sudo ./install.sh upgrade
```

回滚到上一版本：

```bash
sudo /opt/dbtool/current/install.sh rollback
```

改配置（备份 → 原子替换 → 重启并健康检查，失败自动恢复）：

```bash
sudo /opt/dbtool/current/install.sh reconfigure --public-url https://dba.example.com --restart
```

补建缺失的扩展表：

```bash
sudo ./install.sh init-db
```

实时日志：

```bash
journalctl -u dbtool -f
```

存活与就绪探针：

```bash
curl -fsS http://127.0.0.1:8000/healthz
```

```bash
curl -fsS http://127.0.0.1:8000/readyz
```

核对密文用的是哪把密钥（先加载与服务相同的 `SECRET_KEY` / `MIRAGE_*` / `DATABASE_URL`；只读）：

```bash
set -a; . /etc/dbtool/dbtool.env; set +a; /opt/dbtool/current/dbt-admin check-secrets
```

重置管理员密码（`DATABASE_URL` 取自上面加载的环境；新密码用环境变量 `DBT_ADMIN_PASSWORD` 提供，或在终端里无回显输入）：

```bash
set -a; . /etc/dbtool/dbtool.env; set +a; /opt/dbtool/current/dbt-admin admin reset-password --username <用户名> --yes
```

> **注意**：用 shell 的 `.` 加载 `dbtool.env` 时，值里的 `$`、反引号会被 shell 展开，而 systemd 的 `EnvironmentFile` 不会。`install.sh --gen-secrets` 与 `dbt-admin gen-secrets` 生成的密钥只含字母数字，不受影响；手工写入含这些字符的值（例如数据库密码）时，请改用逐项 `export`。
