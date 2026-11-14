# 一键部署：`install.sh`（发布包 tar.gz 解压后运行）

> **适用版本**：以 `deploy/RELEASE_VERSION` / 发布包内 `VERSION` 为准。
> **读者**：部署、升级、回滚、卸载 dbtool 的运维人员；维护 `deploy/install.sh` 的开发者。
> **文档类型**：部署与运维文档 + 脚本设计说明（子命令、流程、不变量、重复运行安全矩阵、排障）。
> **相关文档**：[ops-manual.md](ops-manual.md)（全新部署的完整步骤与选项表；退出码与状态文件在其附录 D）· [deployment.md](deployment.md)（运行期配置、备份、排障）· [admin-cli.md](admin-cli.md)（`dbt-admin`）· [release-notes.md](release-notes.md)（升级前必读）· [../deploy/PACKAGING.md](../deploy/PACKAGING.md)（打包）。

**目标**：部署、升级、回滚、卸载都是一条命令；反复运行、反复初始化不会影响已存在的系统（配置、密钥、管理员、数据目录、数据库里的数据）。

**代码**：`deploy/install.sh`（入口脚本）、`deploy/run-header.sh`（`.run` 的自解压头）、`Makefile` 的 `dist` 目标、`scripts/install-test/run.sh`（行为测试）。

> **警告**：行为测试用假的 `systemctl / id / useradd / firewall-cmd / curl / dbt-admin` 在临时目录里跑，**没有在真实的 systemd / root / firewalld / 真实 MySQL 环境上验证**（见第 9 节）。第一次先在演练主机和演练库上跑。

> **注意**：本文第 4 节、第 5 节、第 9 节的编号被测试脚本与其它文档引用，章节编号不会重排。

## 目录

- [1. 用法速查](#1-用法速查)
- [2. 目录与文件](#2-目录与文件)
- [3. 流程](#3-流程)
- [4. 不变量与对应的测试](#4-不变量与对应的测试)
- [5. 重复运行安全矩阵](#5-重复运行安全矩阵)
- [6. 可选：`.run` 自解压包（默认不生成）](#6-可选run-自解压包默认不生成)
- [7. 排障](#7-排障)
- [8. 与旧版脚本相比的行为变化](#8-与旧版脚本相比的行为变化)
- [9. 验证状态与已知限制](#9-验证状态与已知限制)
- [10. 命令速查](#10-命令速查)

## 1. 用法速查

各子命令的典型用法（每条命令的选项见下表与 `./install.sh --help`；命令速查见第 10 节）：

1. 取得发布包并校验、解压（只需要 `tar.gz` 与 `.sha256` 两个文件）：

   ```bash
   sha256sum -c dbtool-<版本>-linux-amd64.sha256 && tar xzf dbtool-<版本>-linux-amd64.tar.gz && cd dbtool
   ```

2. 首次部署（全新主机 + 空库）：一条命令完成 用户 / 目录 / 密钥 / 配置 / 基础表与数据 / 管理员 / systemd 单元 / 防火墙 / 启动 / 健康检查。管理员密码用环境变量或交互输入，不要写在命令行上：

   ```bash
   read -rs DBT_ADMIN_PASSWORD; export DBT_ADMIN_PASSWORD
   ```

   ```bash
   sudo --preserve-env=DBT_ADMIN_PASSWORD ./install.sh install --gen-secrets --bootstrap --admin-user admin --database-url 'mysql://dbtool_rt:<url-encoded-password>@<mysql-host>:3306/db_tool' --ddl-database-url 'mysql://dbtool_ddl:<url-encoded-password>@<mysql-host>:3306/db_tool' --public-url https://dba.example.com --open-firewall
   ```

3. 升级：解压新版 tar.gz 后同一个入口（已安装时 `install` 也等价于 `upgrade`）：

   ```bash
   sudo ./install.sh upgrade
   ```

4. 回滚到上一版本（或 `--to <版本>`）；不改数据库：

   ```bash
   sudo /opt/dbtool/current/install.sh rollback
   ```

5. 卸载（保留 `/etc/dbtool` 与 `<prefix>/data`；`--purge` 才删，要求键入 `PURGE`）；绝不触碰数据库：

   ```bash
   sudo /opt/dbtool/current/install.sh uninstall
   ```

6. 只读体检与状态：

   ```bash
   /opt/dbtool/current/install.sh doctor
   ```

   ```bash
   /opt/dbtool/current/install.sh status
   ```

7. 显式改配置（备份 → 原子替换；`--dry-run` 只看计划；`--restart` 改完重启并健康检查，失败自动恢复）：

   ```bash
   sudo /opt/dbtool/current/install.sh reconfigure --public-url https://dba.example.com --port 9000 --restart
   ```

8. 只初始化库（空库建基础表与数据，否则只执行幂等的 `sql/*.sql` 扩展表脚本）：

   ```bash
   sudo ./install.sh init-db --admin-user admin
   ```

| 子命令 | 作用 | 写系统？ |
|---|---|---|
| `install`（缺省） | 首次部署；**已有安装 / 配置时自动按升级处理** | 是 |
| `upgrade` | 升级到本包版本；没有已有安装则报错 | 是 |
| `rollback [--to 版本]` | 切回上一版本（或指定版本）并重启、健康检查；失败切回原版本 | 是（不碰库） |
| `uninstall [--purge]` | 停止并禁用服务、删单元与 `releases/`；`--purge` 才删配置与数据目录 | 是（不碰库） |
| `status` | 版本、保留的版本、服务、监听端口、`/healthz` `/readyz` | 否 |
| `doctor` | 只读预检与体检：系统 / systemd / 端口 / 磁盘 / 依赖命令 / 配置完整性 / 连库状态 / 服务健康；有 FAIL 退出码 3 | 否 |
| `init-db` | 仅初始化元数据库，等价调用 `dbt-admin bootstrap` | 写库（不写文件） |
| `reconfigure` | `--database-url --secret-key --public-url --port --listen --timezone --require-goinception`（或 `--no-require-goinception`） | 是 |
| `version` | 脚本 / 安装包 / 已安装版本（也可写 `-V` / `--version`） | 否 |
| `help` | 打印脚本开头的用法注释（也可写 `-h` / `--help`） | 否 |

通用选项：`--dry-run`（所有写子命令只打印计划）、`-y/--yes`、`--keep N`（保留版本数，默认 3）、`--health-timeout SEC`（默认 60）、`--allow-downgrade`、`--prefix`、`--user`、`--skip-preflight`、`--skip-sql`、`--skip-perm-migrate`、`--apply-sql`。全新部署用到的选项见 `docs/ops-manual.md` 4.1，其余见下表，`./install.sh --help` 打印脚本头部的说明。

**升级 / 回滚 / 改配置 / 卸载相关的选项**（全新部署用不到；“已有配置时”指 `/etc/dbtool/dbtool.env` 已存在）

| 选项 | 含义 | 默认 / 行为 | 已有配置时 |
|---|---|---|---|
| `--skip-sql` | 升级时不执行建表脚本（之后用 `init-db` 补） | 执行 | 是 |
| `--skip-perm-migrate` | 升级 / 安装时不执行权限迁移（`dbt-admin perm migrate --apply`，把原权限码迁移成按钮授权）；之后手工执行（迁移完成前普通用户没有任何按钮权限，超级用户不受影响） | 执行（健康检查通过之后，最多等 2 分钟，失败只警告） | 是 |
| `--purge` | 仅 `uninstall`：连 `<prefix>`（含 `data/`）与 `/etc/dbtool` 一起删；先打印将删除的绝对路径清单，要求键入 `PURGE`（非交互必须同时 `--yes`）；（由本脚本创建的运行用户不论是否 `--purge` 都会删除） | 保留配置与数据 | — |
| `--keep N` | 保留最近 N 个版本目录（当前版本与上一版本总是保留） | 3 | 是 |
| `--allow-downgrade` | 允许安装比当前版本低的包（数据库脚本只增不改，通常兼容但不保证） | 拒绝，退出码 7 | 是 |
| `--to VERSION` | `rollback`：回滚到指定版本 | 上一版本 | — |
| `--restart` / `--no-restart` | `reconfigure`：改完立即重启并做健康检查（失败自动恢复备份）/ 不重启 | 终端里询问；非交互不重启 | — |
| `--no-verify` | `reconfigure --database-url`：不用 `dbt-admin status` 验证新连接串 | 验证 | — |
| `--skip-preflight` | 跳过升级前的 `doctor` 预检（不推荐） | 预检 | 是 |

> **警告**：已有配置时，配置类选项（`--database-url`、`--secret-key`、`--public-url`、`--require-goinception`、`--timezone`、`--port`、`--listen`、`--gen-secrets`）一律被忽略，并明确提示改用 `reconfigure`；旧版“升级时 `--port` 可改端口”的行为已取消。新版本新增的配置项只补缺省（目前有 `EXPORT_BASE_DIR` / `ARCHIVE_SPOOL_DIR` / `GOINCEPTION_CONFIG_FILE`，且只补缺失的），绝不改已有值。

**卸载** `sudo ./install.sh uninstall [--purge]`：停止并禁用服务、删除单元（被手工改过则先备份为 `dbtool.service.bak`；drop-in 目录不动）、移除**本脚本添加的**防火墙规则、删除 `<prefix>/releases` 与 `current`；默认保留 `<prefix>/data`（导出文件与归档暂存，其中可能有尚未上传的备份）、`<prefix>/goinception`（goInception 配置，含备份库密码）与 `/etc/dbtool`（内含 `SECRET_KEY` 与库密码、配置备份、状态文件），重新 `install` 会检测到旧配置并沿用；`--purge` 先打印将删除的绝对路径清单并要求键入 `PURGE`，才连 `<prefix>`（含 `data/`）与 `/etc/dbtool` 一起删。由本脚本创建的运行用户不论是否 `--purge` 都会删除（预先存在的用户不动）：保留下来的 `data/` 与配置先改成 root 属主再删用户，避免无主 uid 被别的账号复用；状态文件记 `DATA_RESET=1`，重装时重新创建用户并把属主还原。**不删数据库里的任何东西**（`uninstall` 流程不调用 `dbt-admin` / `mysql`），`--purge` 也只删本机文件。

连接串与密钥除了 `--database-url` / `--secret-key` 外，也可用环境变量 `DBT_DATABASE_URL` / `DBT_SECRET_KEY` / `DDL_DATABASE_URL` 提供（这样它们不会出现在 `install.sh` 自己的命令行上）。

## 2. 目录与文件

```
/opt/dbtool/                         # --prefix，默认 /opt/dbtool
├── current -> releases/<版本>         # 符号链接，原子切换；systemd 的 ExecStart 指向 current/dbtool
├── releases/
│   ├── <版本>/                        # 每个版本一个目录（解包后用包内 SHA256SUMS 校验）
│   │   ├── dbtool  dbt-admin
│   │   ├── sql/  bootstrap/  docs/
│   │   ├── install.sh  env.example  dbtool.example.toml  goinception.config.toml
│   │   └── VERSION  SHA256SUMS  .installed_at
│   └── …                              # 默认保留最近 3 个（--keep N）；当前版本与上一版本总是保留
├── plugins/{soar,soar.yaml}           # SOAR（包里带才有；每次 install / upgrade 都部署：程序 root:root 0755，内容相同不动；soar.yaml 已有不覆盖，新版另存 soar.yaml.dist）
├── goinception/config.toml            # goInception 初始配置（首次安装部署，已有则不覆盖；属主运行用户 0640）
└── data/{export,archive-spool}        # 导出文件与归档暂存：升级 / 卸载默认都不动

/etc/dbtool/
├── dbtool.env                       # DATABASE_URL、SECRET_KEY、MIRAGE_* 等（0640 root:运行组）
├── dbtool.toml                      # 监听地址 / 端口（0644，不含密钥）
├── .install-state                     # 安装状态（key=value，0640，不含机密）
└── backup/<时间戳>/                   # 升级 / 回滚 / reconfigure 前的配置备份（env 0600，其余 0640），保留最近 3 份
/etc/systemd/system/dbtool.service   # 由脚本生成；dbtool.service.d/*.conf（drop-in）永不改动
/var/log/dbtool-install.log          # 安装日志（0640，不含机密）
/var/lock/dbtool-install.lock        # 并发锁（flock）
```

状态文件字段与退出码含义见 [ops-manual.md](ops-manual.md) 附录 D.2、D.3。

## 3. 流程

**install / upgrade**（11 个阶段，终端显示 `[n/11]` 与每阶段耗时；失败时报告“已完成 / 失败于 / 未执行”）：

1. **预检**：加锁（`flock`）→ 读状态文件与已有配置 → 定位安装包 → 降级判定 → `doctor` 关键项（磁盘、依赖命令、配置完整性、包 / 已装版本的 sha256、连库状态）→ `--bootstrap` 的前置条件（连得上库、管理员口令齐全）。**这一步之前没有改动任何东西**。
2. **备份当前配置**：`env / toml / unit / 状态文件` → `/etc/dbtool/backup/<时间戳>/`（只在确实有变化时备份）。
3. **系统用户与目录**：运行用户不存在才创建（并记入状态文件）；数据目录与 goInception 配置目录只在缺失时创建，已有目录的属主 / 权限 / 内容不动。
4. **安装新版本**：先解到 `releases/.stage.<pid>`，用包内 `SHA256SUMS` 校验通过后原子 `mv` 成 `releases/<版本>`。同一版本号但二进制不同（如 `-dirty` 构建）用 `<版本>-<sha8>` 区分，旧目录不动。
5. **写入配置**：首次安装生成 `env`（`umask 077`，先写临时文件再原子 `mv`）与 `toml`；已有配置**不覆盖**，只补缺省项 `EXPORT_BASE_DIR` / `ARCHIVE_SPOOL_DIR` / `GOINCEPTION_CONFIG_FILE`（只补缺失的）与缺失的 `toml`；`GOINCEPTION_CONFIG_FILE` 指向 `<prefix>/goinception/config.toml` 且该文件不存在时，部署包里的 `goinception.config.toml`。
6. **元数据库**：`--bootstrap` / `init-db`：库为空才执行基础结构与数据，否则只执行幂等的 `sql/*.sql` 扩展表脚本（`--only-sql`），缺管理员再创建；升级默认执行 `--only-sql`（库为空时跳过并提示；显式 `--apply-sql` 不跳过）。运行账号建表失败只警告（升级继续），给了建表账号而失败则中止（此时还没有切换版本）。
7. **部署 SOAR（SQL 优化工具；安装包里带 `plugins/soar` 才做）**：程序放到 `<prefix>/plugins/soar`（root:root 0755；与包内内容相同则不动，不同则先写同目录临时文件再 `mv`，正在运行的 SOAR 不受影响），配置放到 `<prefix>/plugins/soar.yaml`（已存在不覆盖，包内新版另存 `soar.yaml.dist`；`log-output` 改写为 `<prefix>/data/soar.log`），再用 `dbt-admin settings set` 把系统设置 `optimizer.soar_path` / `optimizer.soar_config` 指向它们（只在还没设置时写，管理员配置过的不改）。整步不致命：出错只警告，升级照常；每次 install / upgrade 都会执行，重复运行是安全的。测试库 `optimizer.soar_test_dsn` 要管理员自己填（必须是非生产库）。
8. **切换版本并重启**：原子切换 `current` → 单元文件有变化才重写（手工改过则先备份并提示合并）→ `systemctl restart`（会等待执行中的 SQL 工单，沿用 `TimeoutStopSec=660`）。没有任何变化且服务健康时不重启。
9. **防火墙**：`--open-firewall`：规则已存在则不记为本脚本添加；本脚本添加的才会在卸载时移除。
10. **健康检查**：先 `/healthz` 再 `/readyz`，最长 `--health-timeout` 秒。**失败时自动回滚**：切回上一版本、重启、再次健康检查；退出码 5（已恢复）或 6（无法恢复），并给出原因。
11. **收尾**：写状态文件（版本、上一版本、单元 sha256 …）、清理超出 `--keep` 的旧版本、打印访问地址与下一步清单。

**rollback**：预检（目标存在、sha256 通过）→ 备份配置 → 切换并重启 → 健康检查（失败切回原版本）→ 状态文件（`PREVIOUS_VERSION` 与 `VERSION` 互换，再 `rollback` 一次可回来）。数据库不回滚（脚本只增不改，旧版能运行在新表上）。

**doctor**（只读，不改动任何东西；有 FAIL 退出码 3）检查项：

| 类别 | 检查 | FAIL / WARN |
|---|---|---|
| 系统 | 发行版是否 RHEL / CentOS 系；是否 root；`systemctl` 与 `/run/systemd/system` | 没有 `systemctl` 为 FAIL；其余为 WARN |
| 依赖命令 | `curl`、`sha256sum`、`install`、`awk`、`sed`、`grep`；`flock`；`tar` / `firewall-cmd` / `journalctl` / `mysql` | 前六个缺失为 FAIL；`flock` 缺失 WARN；其余只提示 |
| 磁盘 | 安装目录所在分区可用空间 ≥ 300 MB 且 ≥ 包目录大小 ×2 + 50 MB（环境变量 `DBT_DOCTOR_MIN_FREE_KB` 可覆盖下限，仅测试用） | 不足 FAIL |
| 配置完整性 | `env` 里的 `DATABASE_URL`（须 `mysql://` 开头）、`SECRET_KEY`；`TZ`、`REQUIRE_GOINCEPTION`、`PUBLIC_URL`；文件权限；`LISTEN` 是否覆盖 toml；`EXPORT_BASE_DIR` / `ARCHIVE_SPOOL_DIR` 目录是否存在；监听端口合法性 | 缺必填项 / 端口非法 FAIL；其余 WARN / 提示 |
| 完整性 | 安装包 `SHA256SUMS`；已安装版本的文件与 `SHA256SUMS` 是否一致；`current` 是否完整；单元文件是否被手工改过 | 包校验失败 FAIL；已装版本不一致在体检里 FAIL、升级预检里只 WARN |
| 端口 | 监听端口是否被其它进程占用（服务未运行时） | 占用 FAIL |
| 元数据库 | `dbt-admin status` 能否连上、用户表状态、超级用户数 | 连不上 FAIL；库为空只提示 |
| 服务 | `systemctl is-active` / `is-enabled`、`/healthz`、`/readyz` | 未运行 / 无响应 / `degraded` 为 WARN |

**reconfigure**（需要已有配置；4 个阶段：预检 → 备份当前配置 → 应用修改 → 重启与健康检查）：

- 可改项：`--database-url`、`--secret-key`、`--public-url`（须 `http://` 或 `https://` 开头）、`--timezone`（只允许字母、数字和 `_ + / -`）、`--require-goinception` / `--no-require-goinception`、`--port`、`--listen`（1–65535）。一项都没给退出码 2；目标值与现值相同提示“没有变化”。
- `--database-url` 先用 `dbt-admin status` 验证新连接串（连不上退出码 3、不改配置），`--no-verify` 跳过。`--secret-key` 会提示：会话 / JWT 全部失效，且凭它加解密的数据无法解密（除非 `MIRAGE_SECRET_KEY` 已单独固定），并要求确认。
- 改端口 / 监听地址写 `dbtool.toml`，并把 `env` 里的 `LISTEN=` 行注释掉（先整体备份）；本脚本之前放行过旧端口时：加 `--open-firewall` 放行新端口并移除旧规则，否则只警告。
- `env` 只替换给出的键（值经 bash 内建处理，不进命令行），原子替换。是否重启：`--restart` 重启并做健康检查、`--no-restart` 不重启；都没给时终端里询问，非交互不重启。健康检查失败自动恢复备份的配置并再重启一次：已恢复退出码 5，仍不健康退出码 6。

**uninstall**：停止并禁用 → 删单元（被手工改过先备份为 `.bak`）→ 移除本脚本添加的防火墙规则 → 删 `releases/`、`current` → `--purge` 才删 `<prefix>`（含 `data/`）与 `/etc/dbtool` → **由本脚本创建的运行用户不论是否 `--purge` 都会删除**（预先存在的用户不动；保留下来的 `data/` 与配置先改成 root 属主，状态文件记 `DATA_RESET=1`，重装时重新创建用户并把属主还给它）→ 状态文件标记 `INSTALLED=0`（未 `--purge` 时保留，重装时沿用保留的配置）。

## 4. 不变量与对应的测试

测试在 `scripts/install-test/run.sh`（用例名里的 `inv<N>` 与下表编号一致）。

| # | 不变量 | 机制 | 测试 |
|---|---|---|---|
| 1 | 重复运行安全：已安装主机再次 `install` 等价于 `upgrade`；不重新生成 / 覆盖密钥，不覆盖 `env` / `toml` 已有值（新增项只补缺省，变更走 `reconfigure`），不重建管理员，不重复基础数据初始化，不动数据目录 | 有 `env` 就不写 `env`；`--database-url` 等被忽略并提示；管理员只在“库里没有启用的超级用户”时创建；目录只在缺失时创建 | `inv1_reinstall_is_upgrade_and_keeps_config`、`inv1_same_package_twice_is_noop`、`inv1_upgrade_only_backfills_missing_defaults`、`upgrade_gen_secrets_does_not_touch_keys`、`legacy_listen_kept_and_toml_generated` |
| 2 | 库保护：任何子命令都不 DROP / TRUNCATE；`init-db` / `--bootstrap` 只在判定库为空时执行基础结构 + 数据，否则只 `--only-sql`；`uninstall`（含 `--purge`）绝不触碰库 | 判定只用 `dbt-admin status --porcelain`；脚本里没有任何 DDL；卸载流程不调用 `dbt-admin` / `mysql` | `inv2_init_db_only_when_empty`、`inv2_no_drop_or_truncate_and_uninstall_never_touches_db`、`bootstrap_nonempty_runs_only_sql` |
| 3 | 升级可回滚：`releases/<版本>` + `current`；保留 N 个；加锁 → 预检 → 备份 → 解包校验 → `--only-sql` → 原子切换 → 重启 → 健康检查 → 失败自动回滚；`rollback`；降级需 `--allow-downgrade` | 见第 3 节 | `inv3_upgrade_flow_layout_and_order`、`inv3_keep_prunes_old_releases`、`inv3_auto_rollback_on_unhealthy`、`inv3_rollback_when_service_dies`、`inv3_manual_rollback_and_toggle`、`inv3_rollback_without_previous_fails`、`inv3_downgrade_refused_and_allowed`、`inv3_downgrade_by_build_time_for_non_semver`、`inv3_checksum_mismatch_aborts_before_switch`、`inv3_preflight_failure_changes_nothing`、`inv3_same_version_different_binary_gets_distinct_release`、`upgrade_skips_sql_when_db_is_empty`、`sql_failure_with_runtime_account_is_warning_on_upgrade` |
| 4 | 状态文件 `.install-state`（0640）：版本、安装时间、`PREFIX`、运行用户、端口、是否由本脚本开放防火墙、是否由本脚本创建用户、数据目录属主是否被重置（`DATA_RESET`）、单元文件 sha256 | 资源创建后立即落盘，卸载据此精确回收 | `inv4_state_file_fields`、`inv4_firewall_rule_not_ours_is_not_recorded` |
| 5 | 卸载：默认保留 `/etc/dbtool` 与 `<prefix>/data`；`--purge` 才删且要求键入 `PURGE`（`--yes --purge` 先打印将删除的绝对路径清单）；重装复用保留的配置 | 见第 3 节 | `inv5_uninstall_keeps_data_and_config_unless_purge`、`inv5_purge_requires_confirmation_and_lists_paths`、`inv5_purge_yes_prints_list_first`、`inv5_reinstall_after_uninstall_reuses_config`、`inv5_uninstall_on_clean_host_is_harmless` |
| 6 | 不丢用户修改：drop-in 永不改动；主单元被手工改过先备份并提示合并；`reconfigure` 之外不改配置文件 | 单元 sha256 与状态文件比对；drop-in 目录从不读写 | `inv6_unit_modification_backed_up_and_dropin_untouched`、`inv6_nothing_but_reconfigure_changes_config` |
| 7 | 幂等与并发：同一时刻一个实例（`flock`）；`--dry-run` 不写；失败报告已完成 / 未完成；各步骤可安全重跑 | 锁文件；每个写步骤有 dry-run 分支；`EXIT` trap 打印执行情况；先写临时目录再 `mv` | `inv7_concurrent_run_is_refused`、`inv7_dry_run_writes_nothing`、`inv7_dry_run_prints_plan`、`inv7_failure_reports_done_and_pending_steps_and_rerun_is_safe`、`inv7_interrupted_stage_dir_is_cleaned` |
| 8 | 机密卫生：密钥不进命令行参数、不回显、不写日志；连接串经环境变量前缀传给 `dbt-admin`；`env` 0640 root:运行组 | 配置值只经 bash 内建命令处理；`env` 先写 `.new` 再 `chown / chmod / mv` | `inv8_secrets_never_in_argv_logs_or_output`、`bootstrap_fresh_order_and_no_leak` |
| 9 | 首次部署一键完成，结束时输出访问地址与下一步清单；交互缺项逐项询问，非交互缺项明确报错 | 见第 3 节 | `inv9_one_command_full_deploy`、`inv9_noninteractive_missing_fields_errors`、`inv9_interactive_prompts_for_missing` |
| 10 | 日志：全部步骤写 `/var/log/dbtool-install.log`（0640，不含机密），终端显示阶段进度与耗时 | `log()` 只由脚本自己的固定文案调用，不传机密 | `inv10_install_log` |

另有：`doctor_*`（只读、FAIL 分类）、`reconfigure_*`（备份 / 原子替换 / 失败自动恢复 / 密钥变更需确认）、`run_header_info_check_extract_and_passthrough`（`.run` 的 `--info / --check / --extract` 与参数透传、载荷被篡改时拒绝）。

**元数据库判空的依据**：`dbt-admin status --porcelain` 输出里“用户表”那一行的值（`absent | empty | nonempty`）。脚本里只有一处常量 `STATUS_USERS_KEY`（取值 `users_table`；旧版本这个键叫 `sql_users`，嵌着用户表名，现在与表名无关，见 `docs/admin-cli.md` 7.1；测试里同样只有一个 `USERS_KEY`）。

## 5. 重复运行安全矩阵

三种起始状态：**全新**（主机上没有安装、没有 `/etc/dbtool`）、**已安装**（有状态文件 / `current`，库里已初始化）、**库已有数据**（目标库用户表非空；主机上可以是全新也可以是已安装）。

| 子命令 | 全新 | 已安装 | 库已有数据 |
|---|---|---|---|
| `install` | 完整部署（用户、目录、密钥、配置、`--bootstrap` 时初始化库与管理员、单元、防火墙、启动、健康检查） | **等价 `upgrade`**：配置 / 密钥 / 管理员 / 数据目录不动；配置类选项被忽略并提示；无变化时不备份、不重启 | `--bootstrap` 只执行幂等的 `sql/*.sql` 扩展表脚本（`--only-sql`），已有启用的超级用户则不创建管理员；`--gen-secrets` 对已有配置无效 |
| `upgrade` | 报错退出（退出码 2），什么都不改 | 加锁 → 预检 → 备份配置 → 解包 + sha256 校验 → `--only-sql` → 原子切换 → 重启 → 健康检查 → 失败自动回滚 | 同左（库为空时跳过建表脚本并提示） |
| `rollback` | 报错退出（没有 `current`） | 切到上一版本 / `--to`，备份配置，重启，健康检查（失败切回），**不碰库、不动配置** | 同左 |
| `uninstall` | 无操作（退出码 0） | 停止并禁用、删单元与 `releases/`；保留 `/etc/dbtool` 与 `data/`（属主改为 root）；只移除本脚本添加的防火墙规则；删除由本脚本创建的运行用户；`--purge` 才删配置与数据 | **不触碰数据库**（`--purge` 也只删本机文件） |
| `status` | 显示“未安装” | 版本、服务、端口、`/healthz`、`/readyz` | 同左（只读，不连库） |
| `doctor` | 预检（OS / systemd / 依赖 / 磁盘 / 端口；给 `--database-url` 则检查库），只读 | 全部体检项（含配置完整性、sha256、连库 `dbt-admin status`、服务健康），只读 | 同左，并显示用户表 / 超级用户状态 |
| `init-db` | 报错或需 `--database-url`（没有连接串） | 库为空：基础表 + 基础数据 + 扩展表脚本，缺管理员再创建；否则只 `--only-sql` | 只执行幂等的 `sql/*.sql` 扩展表脚本，**不重复基础数据**，已有超级用户不再创建 |
| `reconfigure` | 报错退出（没有已有配置） | 备份 → 原子替换 env / toml；`--restart` 失败自动恢复备份；`--database-url` 先用 `dbt-admin status` 验证；`--secret-key` 需确认 | 同左（只改配置文件，不写库） |
| `version` | 显示包版本 | 显示包版本、已装版本、上一版本 | 同左 |
| 任意写子命令 + `--dry-run` | 只打印计划（只读连库 / 只读体检），不写任何文件、服务、库，也不写日志 | 同左 | 同左 |

**库保护**：脚本里没有任何 `DROP` / `TRUNCATE` / `DELETE`；基础结构与数据只在 `dbt-admin status` 判定用户表不存在或为空时才执行（`dbt-admin` 自己在用户表非空时也会拒绝默认流程）；`uninstall` 与 `rollback` 不调用 `dbt-admin` / `mysql`。

## 6. 可选：`.run` 自解压包（默认不生成）

> 部署用的是 tar.gz 解压后的 `install.sh`；`.run` 只是同一份载荷的单文件封装，需要时才用 `MAKE_RUN=1 bash deploy/package-centos7.sh` 生成。

`make dist`（即 `bash deploy/package-centos7.sh [版本号]`，没有 make 时直接用脚本）产物：`dist/dbtool-<版本>-linux-<arch>.run` = `deploy/run-header.sh`（bash 头，占位符在打包时替换）+ `__ARCHIVE__` 标记行 + 原有的 `dist/dbtool-<版本>-linux-<arch>.tar.gz`。

显示版本 / 架构 / 构建时间 / 提交 / 载荷 sha256 / 包内文件：

```bash
./dbtool-<版本>-linux-amd64.run --info
```

校验载荷 sha256 与包内 `SHA256SUMS`，不安装：

```bash
./dbtool-<版本>-linux-amd64.run --check
```

只解包（得到 `/tmp/x/dbtool/{install.sh,dbtool,…}`）：

```bash
./dbtool-<版本>-linux-amd64.run --extract /tmp/x
```

其余参数原样传给包内 `install.sh`（先校验载荷 sha256，再解到临时目录运行，结束后清理）：

```bash
sudo ./dbtool-<版本>-linux-amd64.run install --gen-secrets --bootstrap --admin-user admin --database-url '<连接串>'
```

运行 `.run` 时环境变量 `DBT_RUN_SKIP_CHECK=1` 可跳过载荷 sha256 校验（不推荐）；`DBT_ADMIN_PASSWORD` 等环境变量原样传给包内 `install.sh`。

版本号不指定时取 `deploy/RELEASE_VERSION`（打包成功后自动加 0.01 写回），再回退 `git describe --tags --always --dirty`（没有提交 / 没有 git 时 `0.0.0-<日期>`），经 `-ldflags "-X main.version=…"` 注入 `dbtool` / `dbt-admin`（`dbtool -version`、`dbt-admin version` 可打印），并写入包内 `VERSION`。包内 `SHA256SUMS` 覆盖包里的每个文件；升级时解包到 `releases/.stage.*` 后逐文件校验，不一致就中止（退出码 3），不改动正在运行的版本。

## 7. 排障

| 现象 | 原因 / 处理 |
|---|---|
| 退出码 3，“预检失败” | 看上面的 `[FAIL]` 行：磁盘不足、缺命令、`env` 缺 `DATABASE_URL` / `SECRET_KEY`、连不上库、包 / 已装版本 sha256 不一致。修好后重跑；确需强制继续才用 `--skip-preflight` |
| 退出码 4 | 另一个 `install.sh` 在运行（`/var/lock/dbtool-install.lock`）；没有 `flock` 时用 `<锁文件>.d/` 目录锁，进程已不存在会自动接管 |
| 退出码 5 | 新版本没通过健康检查，**已自动回滚且服务正常**；失败版本保留在 `releases/` 供排查：`journalctl -u dbtool -n 100`，输出里有失败原因（`/healthz` 无响应 / 进程退出 / `/readyz` 未就绪） |
| 退出码 6 | 健康检查失败且没有可回滚的版本（首次安装），或回滚后也不健康：先看 `journalctl -u dbtool -n 50`（常见原因：配置错误、端口占用、连不上元数据库 / 缺表），修好后 `systemctl restart dbtool` 或重跑同一条命令 |
| 退出码 7 | 拒绝降级：确需降级加 `--allow-downgrade`；想回到上一版本用 `rollback` |
| 运行用户 / 目录不对 | 已安装后 `--prefix` / `--user` 不能改（会报错）；要换：`uninstall`（配置与数据保留）→ 手工迁移 → 新参数 `install` |
| 升级时给了 `--port` 等被忽略 | 这是设计：已有配置只能经 `reconfigure` 修改（会备份并原子替换） |
| `env` 里仍有 `LISTEN=` | 它优先于 `dbtool.toml`；`reconfigure --port N` 会把它注释掉（先备份） |
| 单元文件被手工改过 | 升级时备份为 `dbtool.service.bak` 与 `/etc/dbtool/backup/<时间戳>/` 后重写；把自定义项写成 `dbtool.service.d/*.conf` |
| `reconfigure` 退出码 3（“新的 DATABASE_URL 连不上元数据库”） | 新连接串验证失败，配置没有被改动；确认无误可加 `--no-verify` |
| `reconfigure --restart` 退出码 5 | 改动后服务不健康，已自动恢复备份的配置且服务正常；失败原因看 `journalctl -u dbtool -n 100` |
| 升级后某功能提示“请先执行 deploy/sql/NNN” | 运行账号没有建表权限，脚本只警告；用 `DDL_DATABASE_URL=… ./install.sh init-db`（或升级时 `--ddl-database-url`）补建，再 `systemctl restart dbtool` |

安装日志：`/var/log/dbtool-install.log`（每行 `时间 [pid] 级别 消息`，阶段行 `STEP [n/N] 名称`）。

## 8. 与旧版脚本相比的行为变化

> 完整的版本行为变化清单（含表名前缀、字符集等）见 [release-notes.md](release-notes.md) “一、升级前必读”；其中**已有配置时忽略配置类选项**与**卸载删除脚本创建的运行用户**两项在本节第一条与最后一条展开。

- 已有配置时 `--database-url / --secret-key / --public-url / --require-goinception / --timezone / --port / --listen` **一律被忽略并提示**（旧版 `--port` 在升级时会改端口）。改配置请用 `reconfigure`。
- 不再因为迁移 `LISTEN=` 而改 `env`：补生成 `toml` 时 `env` 里的 `LISTEN` 保持不动（它仍然优先，行为不变），`reconfigure --port` 才会注释它。
- 升级默认执行 `sql/*.sql` 扩展表脚本（`--only-sql`）；`--bootstrap` 在库里已有用户时不再“只提示”，而是执行 `--only-sql`。
- 二进制路径变为 `<prefix>/current/dbtool`（旧：`<prefix>/bin/dbtool`）；不再有 `dbtool.bak`，回滚用 `rollback`。
- `uninstall` 默认仍保留 `data/` 与 `/etc/dbtool`；新增：`--purge` 要求键入 `PURGE`（非交互必须同时 `--yes`，并先打印将删除的绝对路径清单）；只移除本脚本添加的防火墙规则；由本脚本创建的运行用户随卸载一起删除（预先存在的用户不会被删），保留的数据目录与配置先改为 root 属主，重装时还原给新建的用户。

## 9. 验证状态与已知限制

| 项 | 状态 | 说明 |
|---|---|---|
| 全部子命令的流程、不变量、退出码 | 仅假命令验证 | `scripts/install-test/run.sh` 共 82 个用例（`test_` 函数数；第 4 节引用的用例名均存在），用假命令在临时目录里跑 |
| 真实 systemd / root | 未验证 | 单元加载、`TimeoutStopSec` 等待在途工单、`ProtectSystem` 沙箱、SELinux（`restorecon`）、`useradd` / `userdel`、属主与权限位（测试里 `chown / chmod / install` 只记录调用） |
| 真实 firewalld | 未验证 | `--query-port / --add-port / --remove-port / --reload` 的实际行为（含 runtime 与 permanent 的差异） |
| 真实 MySQL | 未验证 | `dbt-admin bootstrap / status` 的真实行为（见 [admin-cli.md](admin-cli.md) 第 10 节）；`CREATE TABLE IF NOT EXISTS` 对无 `CREATE` 权限账号的表现（脚本按“可能失败”处理：运行账号失败仅警告） |
| `flock` | 仅代码审阅 | 测试主机（Windows Git Bash）没有 `flock`，走的是 `mkdir` 退路；真实 `flock` 分支只做了语法检查，未在 Linux 上跑 |
| 终端交互 | 仅模拟 | 用管道 + `DBT_INSTALL_TEST_TTY=1` 模拟了提示与 `PURGE` 确认；没有在真实终端（含 `read -s` 无回显）上验证 |
| `--build` | 仅代码审阅 | 没有在 Linux 上用 Go 工具链跑过 |
| 测试耗时 | 文档原有记录，本次未重新实测 | 在 Windows Git Bash 上顺序跑约 30–45 分钟（进程创建慢，每个用例 10–30 秒；`-j 8` 约 14 分钟）；Linux 上预期快得多，**未实测**。`deploy/PACKAGING.md` 旧稿写的“约 8 分钟”与此不一致，以实测为准 |

## 10. 命令速查

每个代码块只有一条命令。

体检（只读）：

```bash
sudo ./install.sh doctor
```

首次部署前先看计划：

```bash
sudo ./install.sh install --dry-run --gen-secrets --bootstrap --admin-user admin --database-url 'mysql://<用户>:<URL 编码的密码>@<主机>:3306/db_tool'
```

升级前先看计划：

```bash
sudo ./install.sh upgrade --dry-run
```

降级（默认拒绝，退出码 7）：

```bash
sudo ./install.sh upgrade --allow-downgrade
```

回滚到指定版本：

```bash
sudo /opt/dbtool/current/install.sh rollback --to <版本>
```

调整保留的版本数：

```bash
sudo ./install.sh upgrade --keep 5
```

升级时不执行建表脚本（之后用 `init-db` 补）：

```bash
sudo ./install.sh upgrade --skip-sql
```

用建表账号补执行建表脚本：

```bash
DDL_DATABASE_URL='mysql://<建表账号>:<URL 编码的密码>@<主机>:3306/db_tool' sudo --preserve-env=DDL_DATABASE_URL ./install.sh init-db
```

查看安装日志：

```bash
sudo tail -n 100 /var/log/dbtool-install.log
```

卸载并删除配置与数据（先打印清单，要求键入 `PURGE`）：

```bash
sudo /opt/dbtool/current/install.sh uninstall --purge
```
