# `dbt-admin`：管理命令行工具参考

> **适用版本**：以 `deploy/RELEASE_VERSION` / 发布包内 `VERSION` 为准；`dbt-admin version` 打印当前版本。
> **读者**：部署实施人员、DBA（初始化库、创建 / 重置管理员、生成与核对密钥、命令行改系统设置）。
> **文档类型**：命令行参考（命令、选项、环境变量、退出码、安全设计、排障）。
> **相关文档**：[bootstrap-design.md](bootstrap-design.md)（`bootstrap/*.sql` 的设计）· [charset-policy.md](charset-policy.md)（字符集预检）· [ops-manual.md](ops-manual.md) 8.1（全新部署流程）· [one-click-deploy.md](one-click-deploy.md)（`install.sh` 如何调用它）· [settings-reference.md](settings-reference.md)（`settings` 子命令的键名）。

**代码**：`cmd/dbt-admin/`（入口）、`internal/dbtadmin/`（实现）。**目标**：完成全新部署——初始化元数据库、创建 / 重置管理员、生成密钥。

> **警告**：用内存假库 + 假 `database/sql` 驱动测过，**未在真实 MySQL 上验证**（见 §10）。第一次先在演练库上跑。

## 目录

- [1. 它做什么、不做什么](#1-它做什么不做什么)
- [2. 安全设计](#2-安全设计)
- [3. 通用约定](#3-通用约定)
- [4. `bootstrap`](#4-bootstrap)
- [5. `admin`](#5-admin)
- [6. 密钥与系统设置命令](#6-密钥与系统设置命令)
- [7. `status`](#7-status)
- [8. 场景对照](#8-场景对照)
- [9. 与 `install.sh` 的集成](#9-与-installsh-的集成)
- [10. 验证状态与已知限制](#10-验证状态与已知限制)
- [11. 常见问题与排障](#11-常见问题与排障)
- [12. 命令速查](#12-命令速查)

## 1. 它做什么、不做什么

| 命令 | 作用 |
|---|---|
| `dbt-admin bootstrap` | 初始化元数据库：基础表 + 基础数据（`bootstrap/*.sql`），再执行 `sql/*.sql` 扩展表脚本。**默认只演练**，加 `--apply` 才写 |
| `dbt-admin status` | 只读：目标库状态（`dbt_sql_users` 是否为空、启用的超级用户数、已应用的脚本数、库默认字符集） |
| `dbt-admin admin create` | 创建**超级用户**（`is_superuser=1, is_staff=1, is_active=1`），可加入权限组 |
| `dbt-admin admin reset-password` | 重置密码，并清零失败计数 / 解除锁定；可要求下次登录改密 |
| `dbt-admin admin unlock` | 解锁因登录失败被锁定的账号 |
| `dbt-admin admin activate` / `deactivate` | 启用 / 停用用户；**不允许停用最后一个启用的超级用户** |
| `dbt-admin admin list` | 列出用户（用户名、姓名、超级用户、启用、锁定状态、最近登录） |
| `dbt-admin gen-secrets` | 生成并**打印**一组随机密钥（`SECRET_KEY`、`MIRAGE_*`） |
| `dbt-admin encrypt-url` | 把环境变量 `DATABASE_URL` 里的明文密码加密成 `ENC(密文)` 后**打印**（密钥用 `MIRAGE_SECRET_KEY`，缺省 `SECRET_KEY`；公开缺省密钥拒绝）。结果整串写回 env 的 `DATABASE_URL`，`dbtool` 与 `dbt-admin` 都认；换密钥要重新加密 |
| `dbt-admin check-secrets` | **只读**自检：库里的加密值（系统设置里的敏感项、实例账号 / 密码）用哪把密钥加密的、当前密钥解不解得开；不输出任何密钥 / 密文（§6.1） |
| `dbt-admin reencrypt` | 在库里**原地**把 5 张表的加密列从旧密钥换成当前密钥；默认只演练；解不开的原样保留；不输出任何密钥 / 密文 / 明文（§6.2） |
| `dbt-admin settings list` / `get` / `set` / `reset` | 命令行读写系统设置（`dbt_setting`），含页面里不显示的内部状态项（`--force`）；默认只演练（§6.3） |
| `dbt-admin perm migrate` | 权限体系一次性迁移：原权限码 → 按钮授权并标记为已迁移；默认只演练，`--apply` 写入并先存撤销脚本（§6.4） |
| `dbt-admin version` | 打印版本（也可写 `--version`） |

不做：不建库（库不存在时报错并给出建库语句）；不修改、不删除已有的表；不管理普通用户资料（在页面里操作）。

> **表名**：本系统所有表统一带 `dbt_` 前缀（如 `dbt_sql_users`、`dbt_auth_group`、`dbt_audit_log`）。本文里出现的表名都是实际表名。

## 2. 安全设计

- **连接串只从环境变量 `DATABASE_URL` 读取**（`mysql://用户:URL编码的密码@主机:3306/库名`），**不接受命令行传密码 / 连接串**：`--password`、`--database-url`、位置参数一律拒绝（并且不回显参数值）。
- **新密码**来自环境变量 `DBT_ADMIN_PASSWORD`，或在终端里**无回显**输入两次；非终端环境又没有该变量时报错（退出码 2）。
- **连接串的写法**：`mysql://用户:URL编码的密码@主机:3306/库名`，密码里的 `@ : / ? # %` 必须 URL 编码；端口缺省 3306；库名只允许字母、数字、下划线与 `$`；可带查询参数 `?tls=`，取 `true` / `false` / `skip-verify` / `preferred`（只有 `dbt-admin` 认这个参数；`dbtool` 服务的 `DATABASE_URL` 会忽略查询参数）。
- **输出和日志里不出现密码、密钥、哈希**：错误信息里 MySQL 返回的单引号内容（如 `Duplicate entry '…'`）被抹掉；执行失败时回显的语句会把字符串字面量换成 `'***'` 并截断到 200 个字符；审计里不含密码 / 哈希。
- **默认只演练、写入前确认**：`bootstrap` 加 `--apply` 才写；所有写操作（含 `admin` 子命令）需要 `--yes`，或在终端里输入 `yes` 确认；确认前显示**目标库名与主机**（`库 db_tool @ 10.0.0.5:3306（用户 dbtool）`）。
- **审计**：`dbt_audit_log` 存在时，每个写操作写一条审计，操作人为 `dbt-admin(<系统用户名>)`，动作形如 `CLI-创建超级用户`、`CLI-重置密码`、`CLI-解锁账号`、`CLI-启用用户`、`CLI-停用用户`、`CLI-初始化基础表与数据`、`CLI-执行建表脚本`。表不存在则跳过并提示。`dbt_audit_ext` 存在时同步补一行（类别 / 结果由 `internal/audit` 的 `Classify` 推断；来源 IP 为空）。
- **事务**：创建用户 + 权限组 + 审计在同一个事务里，任何一步失败整体回滚；`FOR UPDATE` 行锁避免并发创建同名用户。
- **最后一个超级用户**：停用时在同一事务里统计“除它之外启用的超级用户数”，为 0 则拒绝。

> 局限：终端无回显读密码用的是标准库 `syscall`（Linux 的 `TCGETS/TCSETS`、Windows 的 `SetConsoleMode`），因为项目没有引入 `golang.org/x/term`；其他平台（如 macOS）不支持交互输入密码，请用 `DBT_ADMIN_PASSWORD`。

## 3. 通用约定

- 连接信息：`DATABASE_URL`；时区：运行时的 `TZ` 应与 dbtool 服务一致（库里的时间是无时区的本地时间；`install.sh` 调用时已传入）。
- 环境变量（`dbt-admin` 不接受命令行传密码 / 连接串，所有输入都用 `--选项` 或下面的环境变量）：

  | 变量 | 用于 | 说明 |
  |---|---|---|
  | `DATABASE_URL` | 所有连库命令 | 元数据库连接串；缺失退出码 2 |
  | `DBT_ADMIN_PASSWORD` | `admin create` / `reset-password` | 新密码；不设置则在终端里无回显输入两次 |
  | `DBT_SETTING_VALUE` | `settings set` | 敏感项的新值（也可用于普通项）；敏感项不接受 `--value` |
  | `SECRET_KEY` / `MIRAGE_SECRET_KEY` / `MIRAGE_CIPHER_MODE` / `MIRAGE_CIPHER_IV` | `check-secrets` / `reencrypt` / `settings`（敏感项）/ `admin`（读设置） | 与 dbtool 服务取法一致：`MIRAGE_SECRET_KEY`，缺省 `SECRET_KEY`，都没设用公开缺省值 |
  | `OLD_MIRAGE_SECRET_KEY`（缺省 `OLD_SECRET_KEY`）/ `OLD_MIRAGE_CIPHER_MODE` / `OLD_MIRAGE_CIPHER_IV` | `reencrypt` | 显式给出的旧密钥（§6.2） |
  | `TZ` | 所有命令 | 与服务一致 |
- **脚本根目录**：`<root>/bootstrap/*.sql`、`<root>/sql/*.sql`，各自按文件名升序执行。默认 root 是**可执行文件所在目录**（安装包解压后目录布局与源码树一致）；`--sql-root DIR` 可改。在源码树里用 `--sql-root deploy`；安装到 `/opt/dbtool` 后用 `--sql-root /opt/dbtool`（`install.sh` 就是这样调用的）。
- 退出码：`0` 成功；`1` 失败（连不上库、库不存在、用户已存在 / 不存在、语句执行失败、被保护规则拒绝等）；`2` 参数错误（缺 `--username`、用户名不合规、密码不满足策略、缺 `--yes` 且不是终端、缺环境变量等）。
- `--help`：`dbt-admin --help`、`dbt-admin <命令> --help`、`dbt-admin admin <子命令> --help`。

## 4. `bootstrap`

1. 设置连接串（连接串只能来自环境变量）：

   ```bash
   export DATABASE_URL='mysql://<用户>:<URL 编码的密码>@127.0.0.1:3306/db_tool'
   ```

2. 演练（默认）：打印目标库、将执行的文件与语句数，不写任何东西：

   ```bash
   dbt-admin bootstrap --sql-root /opt/dbtool
   ```

3. 确认后执行：

   ```bash
   dbt-admin bootstrap --sql-root /opt/dbtool --apply --yes
   ```

4. 升级（出现了新的 `sql/*.sql` 扩展表脚本）：只执行扩展表脚本（替代 `install.sh --apply-sql` 里用运行账号建表的做法）：

   ```bash
   dbt-admin bootstrap --sql-root /opt/dbtool --only-sql --apply --yes
   ```

选项：`--sql-root DIR`、`--apply`、`--only-sql`、`--yes`（`-y`）、`--skip-charset-check`（需同时加 `--yes`）。

| 选项 | 说明 |
|---|---|
| `--sql-root DIR` | 脚本根目录（含 `bootstrap/` 与 `sql/`） |
| `--apply` | 真正写入；不加只演练 |
| `--yes` / `-y` | 写入前不再交互确认 |
| `--only-sql` | 只执行 `sql/*.sql` 扩展表脚本（升级：基础表已存在）；不检查用户表 |
| `--skip-charset-check` | 跳过字符集预检；须同时加 `--yes`，见 1a |

流程：

1. 连接目标库。**库不存在就报错**，并提示 `CREATE DATABASE `库名` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;`（MySQL 8；5.7 / MariaDB 用 `utf8mb4_general_ci`）；工具不会自动建库。
1a. **库默认字符集预检**（`--only-sql` 同样执行）：所有表、所有字段都不指定字符集与校验规则（见 [charset-policy.md](charset-policy.md)），建出来的表完全取决于库默认值，所以先读 `information_schema.SCHEMATA` 里该库的 `DEFAULT_CHARACTER_SET_NAME` / `DEFAULT_COLLATION_NAME`，并在演练输出里显示：
   - 字符集**不是 `utf8mb4`**（`utf8mb3`、`latin1`、`gbk` 等）：**拒绝**（退出码 1），不写任何东西，并给出修复命令 `ALTER DATABASE `库名` CHARACTER SET utf8mb4 COLLATE <建议校验规则>;`（MySQL 8 建议 `utf8mb4_0900_ai_ci`，其余 `utf8mb4_general_ci`；该语句只改库的默认值，只影响之后新建的表）；
   - 是 `utf8mb4`，但校验规则不在 `utf8mb4_general_ci` / `utf8mb4_0900_ai_ci` / `utf8mb4_unicode_ci` 之内（例如 `_bin`、`_as_cs`）：**警告**并说明大小写 / 重音敏感性的影响，不阻止；
   - 读不到（账号没有 `information_schema.SCHEMATA` 可见性）：同样拒绝；
   - `--skip-charset-check`：跳过上述预检，**必须同时加 `--yes`**（否则退出码 2），并打印风险（表按库默认建出，非 utf8mb4 的库会得到非 utf8mb4 的表，emoji 等 4 字节字符写入失败 / 被截断，事后统一需要重建表）。
   演练输出还会提示：**已存在表的字符集 / 校验规则不会被本工具修改**（只用 `CREATE TABLE IF NOT EXISTS` 补缺失的表；对已有表从不执行 `ALTER`）。
2. 读入**所有**脚本并切分语句（任何一个文件有问题——引号没闭合、含 `DELIMITER`——都在写入之前报出来）。
3. 前置检查（默认流程）：`dbt_sql_users` **不存在或为空**才允许；已有用户则**拒绝**（退出码 1），提示用 `--only-sql`。`--only-sql` 不做这项检查（它不写基础数据，只补缺失的表）。
4. 打印计划（目标库、主机、MySQL 版本、每个文件的语句数；文件内容与上次记录不同会标注）。没有 `--apply` 到此结束。
5. 确认（`--yes` 或终端确认）后：先 `CREATE TABLE IF NOT EXISTS dbt_bootstrap_applied`，再**逐文件、逐语句**执行；**遇错即停**，报出文件名、语句序号（第几条 / 共几条）和所在行，已执行的内容保持原样。
6. 每个文件成功后记录到 `dbt_bootstrap_applied`（`file, sha256, applied_at, tool_version`）；文件名带目录前缀（如 `bootstrap/001_base_schema.sql`）。
7. 写一条审计，打印摘要：执行了几个文件 / 几条语句、新建多少张表、`dbt_auth_permission` 行数，以及下一步（创建管理员、配置密钥、启动服务）。

**幂等与重跑**：全部脚本幂等（`IF NOT EXISTS` / `INSERT IGNORE` / `ON DUPLICATE KEY`），中途失败修复后直接重跑即可。记录里的 sha256 与当前文件不同（升级到新版本）时只提示，仍会重跑。

**SQL 切分器**（`internal/dbtadmin/split.go`）：处理 `'…'` / `"…"`（`\` 转义与重复引号）、反引号标识符、`-- `、`#`、`/* */` 注释，`/*! … */` 与 `/*+ … */` 原样保留；只含注释的“语句”丢弃；最后一条可无分号；**遇到 `DELIMITER` 报错**（含存储过程 / 触发器的脚本请用 `mysql` 客户端）；单条语句上限 8 MiB。同一个连接贯穿全程，所以脚本里的 `SET NAMES`、`SET FOREIGN_KEY_CHECKS=0` 对后续语句有效。

**基础数据只写进准备初始化的库**：库里已有表时，完整的 `bootstrap` 会提示确认这个库确实是准备初始化的新库。

## 5. `admin`

以下命令都需要先设置 `DATABASE_URL`（§3）。

创建超级用户（非交互：密码用环境变量）：

```bash
DBT_ADMIN_PASSWORD='<强密码>' dbt-admin admin create --username ops --display 运维 --email ops@example.com --group DBA --yes
```

交互方式（不设置 `DBT_ADMIN_PASSWORD`，在终端里无回显输入两次，再输入 `yes` 确认）：

```bash
dbt-admin admin create --username ops
```

只检查、不写：

```bash
dbt-admin admin create --username ops --dry-run
```

管理员锁死：重置密码并清零失败计数，要求其下次登录改密：

```bash
DBT_ADMIN_PASSWORD='<临时强密码>' dbt-admin admin reset-password --username ops --require-change --yes
```

只解锁（失败计数清零）：

```bash
dbt-admin admin unlock --username ops --yes
```

停用 / 启用用户：

```bash
dbt-admin admin deactivate --username leaver --yes
```

```bash
dbt-admin admin activate --username leaver --yes
```

列出用户：

```bash
dbt-admin admin list
```

所有 `admin` 子命令先确认 `dbt_sql_users` 存在；不存在时报错并提示先执行 `dbt-admin bootstrap --apply`。

### 5.1 `create`

| 选项 | 说明 |
|---|---|
| `--username U` | 必填。1–30 个字符，只能含字母、数字与 `@ . + - _`（同页面“新建用户”的规则） |
| `--display D` | 姓名，1–10 个字符；缺省取用户名前 10 个字符 |
| `--email E` | 可选，格式同页面 |
| `--group G` | 权限组名称，可重复或逗号分隔；权限组必须已存在（否则报错，不创建用户） |
| `--yes` / `--dry-run` | 不确认直接写 / 只检查不写 |

- 写入 `pbkdf2_sha256` 哈希：随机 22 位字母数字盐，迭代次数 390000（与页面里创建用户一致；测试断言它与 `sysadmin.NewService` 的取值相同），可被 `internal/crypto` 的密码校验通过。
- `dbt_sql_users` 里只写本库实际存在的列。
- 用户名已存在报错（退出码 1），不覆盖。

### 5.2 密码策略

按系统设置 `dbt_setting` 里的 `security.password_min_length` / `security.password_complexity` 取值（表不存在则用默认值），与页面一致：**至少 9 位**（可配置）、不能全是数字、不能是常见弱口令、不能包含用户名 / 邮箱前缀 / 姓名、可选“字母 + 数字 + 特殊字符”。校验实现在 `internal/dbtadmin/password.go`：是 `internal/sysadmin/password.go` 的副本——不直接 import `sysadmin` 是因为它依赖整个 HTTP 服务，会让 `dbt-admin` 从约 6 MB 涨到约 55 MB；`password_test.go` 逐项对照两边行为并从 `sysadmin` 源码里解析弱口令表做集合相等校验，改策略时两处一起改。

交互输入时校验失败最多重试 3 次；环境变量给出的密码不满足策略则直接报错（退出码 2）。

### 5.3 `reset-password` / `unlock` / `activate` / `deactivate`

- `reset-password`：密码来源与策略同 `create`；同时 `failed_login_count = 0, last_login_failed_at = NULL`（解除锁定）。`--require-change` 在 `dbt_user_profile.must_change_password` 置 1（表不存在则整体回滚并报错）；不加该选项且用户之前有遗留标记时清掉（同页面的“重置密码”）。**LDAP 认证的用户拒绝重置**（密码由外部系统管理，规则同页面）。重置密码不会撤销已签发的登录令牌（与页面行为一致）。
- `unlock`：失败计数清零；没有失败记录时提示“无需解锁”，退出码 0，不写审计。
- `activate` / `deactivate`：已经是目标状态时提示无变化，退出码 0，不写审计。`deactivate` 的保护规则见 §2。
- `list`：锁定状态按系统设置 `security.login_lock_attempts`（默认 5）/ `security.login_lock_seconds`（默认 300 秒）计算（读 `dbt_setting`）。

## 6. 密钥与系统设置命令

本节包含 `gen-secrets`（生成密钥）、`check-secrets`（6.1）、`reencrypt`（6.2）、`settings`（6.3）与 `perm`（6.4）。

**`encrypt-url`**

```bash
DATABASE_URL='mysql://dbtool:p%40ss@127.0.0.1:3306/dbtool' MIRAGE_SECRET_KEY=... dbt-admin encrypt-url
# mysql://dbtool:ENC(<密文>)@127.0.0.1:3306/dbtool
```

- 把连接串里的密码（URL 解码后）用 Mirage 加密，替换成 `ENC(密文)`；其余部分（主机、库、`?tls=` 等）不变。已经是 `ENC(...)` 的原样输出。
- 标准输出只有这一行；错误在标准错误，不回显连接串。输出前自检：同一把密钥能还原成同一个密码。
- 当前生效的密钥是公开缺省值时拒绝（等于没加密）。**换了 `SECRET_KEY` / `MIRAGE_SECRET_KEY`（或 `MIRAGE_CIPHER_MODE` / `MIRAGE_CIPHER_IV`）后，env 里的密文就解不开，要用明文连接串重新运行本命令**；解不开时 `dbtool` / `dbt-admin` 报“加密密码用当前密钥解不开”，不会拿密文去连库。
- 它只能防“配置被截图 / 随手贴出去”；密文和密钥在同一个 env 文件里，能读该文件的人仍能还原。安装脚本：`./install.sh encrypt-db-url`（已有环境）；新装默认加密。

**`gen-secrets`**

```bash
dbt-admin gen-secrets
# SECRET_KEY=<64 位字母数字>
# MIRAGE_SECRET_KEY=<64 位字母数字>
# MIRAGE_CIPHER_MODE=ECB
# MIRAGE_CIPHER_IV=<16 位字母数字>
```

- 用 `crypto/rand`，拒绝采样保证字符均匀；只含字母数字，复制进 env 文件 / shell 无需转义。`SECRET_KEY` ≥ 50 字符，`MIRAGE_SECRET_KEY` ≥ 32 字符，`MIRAGE_CIPHER_IV` 16 字节，`MIRAGE_CIPHER_MODE=ECB`。
- **标准输出只有 4 行 `KEY=VALUE`**（便于脚本捕获）；提示信息在标准错误：立即存入密码库；`MIRAGE_*` 一旦使用就不能再改（改了所有密文都解不开），`SECRET_KEY` 同理。
- 不写任何文件。**生成后立即存入密码库**；`MIRAGE_*` 一经使用不得再改。

### 6.1 `check-secrets`

只读自检：判断库里“看起来是密文”的值能不能用**当前密钥**解开；解不开时，再用 `SECRET_KEY` 和公开缺省密钥各试一遍，指出是哪把加密的。
典型场景：实例账号 / 密码是用另一把密钥（例如公开缺省 `SECRET_KEY`）加密的，而部署时生成了另一把密钥——**用户能正常登录**（密码哈希是 PBKDF2，不依赖这把密钥），但配置项读出来是乱码、实例连不上目标库。

先取与服务相同的 `SECRET_KEY` / `MIRAGE_*` / `DATABASE_URL`：

```bash
set -a; . /etc/dbtool/dbtool.env; set +a
```

> **注意**：用 shell 的 `.` 加载时，值里的 `$`、反引号会被 shell 展开，而 systemd 的 `EnvironmentFile` 不会。生成的密钥只含字母数字，不受影响；手工写入含这些字符的值（例如数据库密码）时请改用逐项 `export`。

然后执行检查：

```bash
dbt-admin check-secrets
```

- 检查对象：`dbt_setting.svalue`（仅 `encrypted=1` 的行）、`dbt_sql_instance.user`、`dbt_sql_instance.password`；空值不计，不像密文的按明文计（读取时兼容明文）。
- 密钥规则与服务一致：`MIRAGE_SECRET_KEY`，没设用 `SECRET_KEY`，都没设用缺省值；模式 / IV 取 `MIRAGE_CIPHER_MODE` / `MIRAGE_CIPHER_IV`。
- 输出分类计数，以及“需要关注”的**配置项名 / 实例名**（不是秘密）；**不输出任何密钥、密文或明文值**。
- 退出码：`0` 全部解得开；`1` 有解不开的值；`2` 参数 / 环境错误。
- 判定时要求解出来是合法 UTF-8 且无控制字符：mirage 在密钥错误时约 0.4% 的概率碰巧通过填充校验，不能只看“没报错”。
- 修法：先确认是哪把密钥加密的（核对 `MIRAGE_SECRET_KEY` / `SECRET_KEY` 是否是加密这些值时用的那一把），改回正确的密钥后重启；确属无法恢复的设置项，在“平台设置 › 系统配置”里重新填写；批量处理用 6.2 的 `reencrypt`。

### 6.2 `reencrypt`

在目标库里**原地**把加密列从旧密钥换成 dbtool 当前生效的密钥（不需要源库，可重复运行）。用在“密文是用另一把密钥加密的、读出来是乱码”的库上；转换规则见 `internal/crypto` 的 `Rekeyer`。

1. 加载与服务相同的密钥（当前密钥 = 服务在用的密钥；`DATABASE_URL` 指向要处理的库）：

   ```bash
   set -a; . /etc/dbtool/dbtool.env; set +a
   ```

2. 旧密钥单独配置过时，另给（缺省会依次尝试 `SECRET_KEY` 与公开缺省密钥）：

   ```bash
   export OLD_MIRAGE_SECRET_KEY='<旧密钥>' OLD_MIRAGE_CIPHER_MODE=ECB OLD_MIRAGE_CIPHER_IV='<旧 IV>'
   ```

3. 演练：只统计，不写：

   ```bash
   dbt-admin reencrypt
   ```

4. 写入前先备份这 5 张表：

   ```bash
   mysqldump -h <主机> -P <端口> -u <账号> -p <库名> dbt_sql_instance dbt_ssh_tunnel dbt_instance_account dbt_setting dbt_2fa_config > secrets-backup.sql
   ```

5. 执行：

   ```bash
   dbt-admin reencrypt --apply --yes
   ```

6. 复核：

   ```bash
   dbt-admin check-secrets
   ```

- **处理对象**：`dbt_sql_instance`（user / password）、`dbt_ssh_tunnel`（user / password / pkey / pkey_password）、`dbt_instance_account`（user / password）、`dbt_setting`（svalue，仅 `encrypted=1`）、`dbt_2fa_config`（username / auth_type / phone / secret_key）；只改这些列。
- **新密钥** = 服务当前生效的密钥（`MIRAGE_SECRET_KEY`，缺省 `SECRET_KEY`）。**旧密钥**依次尝试：`OLD_MIRAGE_SECRET_KEY`（缺省 `OLD_SECRET_KEY`；模式 / IV 取 `OLD_MIRAGE_CIPHER_MODE` / `OLD_MIRAGE_CIPHER_IV`，缺省 `ECB` / `1234567890abcdef`）、`SECRET_KEY`、公开缺省密钥。
- **每个值独立判断**：旧密钥解得开 → 重新加密；明文 → 用当前密钥加密；已经是当前密钥的 → 不动（混合状态也安全，重复运行结果不变）；谁都解不开 → 原样保留，只列配置项名 / 实例名 / 隧道名；加密后超出列宽的保留明文。
- **安全**：默认只演练；写入需要 `--apply`，并要 `--yes` 或终端里输入 yes；所有改动在**一个事务**里，条件更新（值被别人改过则不动，提示再运行）；写一条系统审计“密文重加密”（只有数量）。
- **拒绝的情形**：当前生效的密钥无效（太短等，不会退而求其次拿别的密钥写库）；当前生效的密钥就是公开缺省值（演练照常给统计并警告，`--apply` 拒绝：用公开密钥加密等于没加密，请先 `gen-secrets` 并配置自己的 `MIRAGE_SECRET_KEY`）。
- 退出码：`0` 成功（含“没有需要改的值”）；`1` 失败（写入出错已整体回滚 / 复查仍有值没换完）；`2` 参数 / 环境错误。
- 服务不需要重启：配置与实例凭据都是用到时读库解密（登录与安全设置有 10 秒缓存）。

### 6.3 `settings`

在命令行读写系统设置（`dbt_setting`；页面入口：平台设置 › 系统配置）。键名与取值范围见 [settings-reference.md](settings-reference.md)。子命令只有 `list`、`get`、`set`、`reset`（旧版的 `import` 在 1.31 已移除）。`--group` 的取值：`security` / `sms` / `query` / `firewall` / `review` / `notify` / `goinception` / `export` / `partition` / `retention` / `perf` / `inspect` / `optimizer`。

列出设置（敏感项只显示“已设置”；`--modified` 只列与默认值不同的，`--group` 只列一个分组）：

```bash
dbt-admin settings list --group query --modified
```

一项的当前值、默认值、说明、最后修改：

```bash
dbt-admin settings get --key query.max_execution_seconds
```

修改：默认只演练，打印“当前 → 改为”：

```bash
dbt-admin settings set --key query.max_execution_seconds --value 120
```

确认后写入（需要 `--apply`，并加 `--yes` 或在终端里确认）：

```bash
dbt-admin settings set --key query.max_execution_seconds --value 120 --apply --yes
```

恢复默认（敏感项 = 清除）：

```bash
dbt-admin settings reset --key query.max_execution_seconds --apply --yes
```

敏感项（密钥 / 密码）的值只能从环境变量取，不接受命令行参数（会留在命令历史里）：

```bash
DBT_SETTING_VALUE='<值>' dbt-admin settings set --key goinception.password --apply --yes
```

内部状态项（页面里不显示，如 `security.local_last_login`）需要 `--force`：

```bash
dbt-admin settings set --key security.local_last_login --value "2026-01-01 10:00:00" --force --apply --yes
```

SOAR 的程序路径、配置文件与测试库（`optimizer.soar_*`）页面和命令行都能改：写法要合格（路径必须是绝对路径、文件名 `soar`；测试库 DSN 形如 `user:password@host:3306/db`）；命令行不检查文件是否存在、测试库是否生产库（这两项只在页面保存时核对，加 `--force` 的命令行同样不核对）。

- 写入走与页面相同的规则：按类型 / 范围 / 取值集合校验，敏感项用当前密钥（`MIRAGE_SECRET_KEY`，缺省 `SECRET_KEY`）加密；当前密钥是公开缺省值时拒绝写入敏感项。
- 一次写入一个事务：设置值、变更记录（`dbt_setting_history`，敏感项不带值）、系统审计（动作 “CLI-修改系统设置”，操作人 `dbt-admin(<系统用户>)`）要么都成功要么都不写。
- 服务进程最多 3 秒内读到新值，不需要重启。

### 6.4 `perm migrate`

权限体系的一次性迁移：把权限组、用户**单独**持有的原权限码，按登记表展开成按钮授权（`dbt_group_perm` / `dbt_user_perm`），再在 `dbt_perm_setting` 里记下 `mode=strict`（“已迁移”的标记）。迁移前后每个用户能用的按钮完全相同；没有对应按钮的早期遗留码丢弃。只增不减，可重复执行；`install.sh install / upgrade` 会在健康检查通过之后自动执行一次。

```bash
dbt-admin perm migrate                                    # 演练：列出每个权限组 / 用户新增的按钮、丢弃的权限码，不写库
dbt-admin perm migrate --apply                            # 写入（终端里确认；先把撤销脚本存成 ./perm-migrate-rollback-<时间>.sql）
dbt-admin perm migrate --apply --yes --rollback-file /root/perm-rollback.sql   # 脚本里用
```

- 需要 `dbt_group_perm` / `dbt_user_perm` / `dbt_perm_setting`（`deploy/sql/019_btnperm.sql`），没有时提示先执行 `dbt-admin bootstrap --only-sql --apply`；
- 撤销脚本只含这次新增的行的 `DELETE`（已有的权限组按钮、单用户授权 / 禁止不在其中），最后一条删除 `mode` 设置（回到“未迁移”）；原权限码相关的表不会被修改，所以回滚到旧版本不受影响；
- 写入在一个事务里，失败整体回滚；写一条系统审计“迁移原权限到按钮权限（全部）”；服务最多 15 秒内生效，不需要重启；
- 整条命令有 2 分钟的期限（超时报错退出，不会无限等待）。

## 7. `status`

```bash
dbt-admin status --porcelain
```

输出（`key=value`，一行一个）：

```
db=db_tool
host=10.0.0.5:3306
users_table=absent
users=0
superusers=0
bootstrap_applied=0
charset=utf8mb4
collation=utf8mb4_0900_ai_ci
charset_check=ok
```

不带 `--porcelain` 时输出面向人读的几行：目标库与 MySQL 版本、库默认字符集 / 校验规则及预检提示、`dbt_sql_users` 的状态（不存在 / 为空 / 有数据，含用户数与启用的超级用户数）、已记录应用的脚本文件数，以及“结论”（空库可以执行 `bootstrap --apply`；没有启用的超级用户可用 `admin create`；库里已有数据则 `bootstrap` 只能加 `--only-sql`）。

### 7.1 `--porcelain` 输出键

输出是 `key=value` 行（一行一个键，供脚本解析）。键名与表名无关：

| 键 | 取值 | 含义 |
|---|---|---|
| `db` | 库名 | 连接串里的库 |
| `host` | `主机:端口` | |
| `users_table` | `absent` / `empty` / `nonempty` | 用户表 `dbt_sql_users` 不存在 / 存在但为空 / 有数据 |
| `users` | 整数 | 用户表里的用户数（表不存在为 0） |
| `superusers` | 整数 | 启用的超级用户数 |
| `bootstrap_applied` | 整数 | `dbt_bootstrap_applied` 里记录的文件数 |
| `charset` / `collation` | 字符串 | 库默认字符集 / 校验规则（读取失败为空） |
| `charset_check` | `ok` / `warn` / `fail` / `unknown` | 默认字符集预检结论（`fail` 时退出码为 1） |

> 键名说明：用户表状态的键是 `users_table=absent|empty|nonempty`（与表名无关）。`install.sh` 只解析这个键与 `superusers=`。

库默认字符集不是 `utf8mb4` 时（`charset_check=fail`）：输出照常完整打印，**退出码为 1**，修复命令（`ALTER DATABASE …`）写在标准错误；加 `--skip-charset-check --yes` 可让退出码保持 0（状态里仍如实显示 `fail`）。读取失败（`unknown`）不影响退出码。判断逻辑与 `bootstrap` 的预检相同，见第 4 节。


`install.sh --bootstrap` 用它判断“是否需要初始化、是否需要创建管理员”。

## 8. 场景对照

| 场景 | 用什么 |
|---|---|
| **全新安装** | `dbt-admin bootstrap`（基础表 + 基础数据 + 扩展表脚本）→ `dbt-admin admin create` → `gen-secrets`。用一个空库 |
| 已有库升级，出现新的 `sql/*.sql` 扩展表脚本 | `dbt-admin bootstrap --only-sql --apply --yes`（或 `install.sh --apply-sql`） |
| 管理员锁死 / 忘记密码 | `dbt-admin admin reset-password` / `unlock` |

`dbt-admin bootstrap --only-sql` 执行 `deploy/sql` 里的扩展表脚本（都幂等），不需要 `mysql` 客户端，并且会记录已应用的文件；字符集策略见 [charset-policy.md](charset-policy.md)、`docs/bootstrap-design.md`。

## 9. 与 `install.sh` 的集成

`deploy/install.sh` 里与它集成的选项：

| 选项 | 作用 |
|---|---|
| `--bootstrap` | 启动服务前：`dbt-admin status` 判断 → 空库则 `bootstrap --apply --yes` → 没有超级用户则 `admin create`。**只在 `dbt_sql_users` 为空 / 不存在时初始化**（status 的 `users_table`）；已有用户（升级）只提示；已有启用的超级用户不再创建管理员 |
| `--admin-user NAME` | 初始管理员用户名；非交互时必填，密码用环境变量 `DBT_ADMIN_PASSWORD`（不允许放命令行） |
| `--gen-secrets` | 没给 `--secret-key` 时随机生成 `SECRET_KEY` 与 `MIRAGE_*` 写进 env 文件（0640），**只在终端提示备份，不回显值** |
| `--ddl-database-url URL` | 建表账号（或环境变量 `DDL_DATABASE_URL`）；给了就用它执行 `--bootstrap` 与 `--apply-sql`，运行账号只需 DML |

`--apply-sql`：包里有 `dbt-admin`（现行发布包都有）时走 `dbt-admin bootstrap --only-sql --apply --yes`，用 `--ddl-database-url` 给的建表账号，没给则用运行账号（运行账号通常只有 DML 权限，建表会失败：初始化时是致命错误，升级时只警告）；只有包里没有 `dbt-admin` 的旧包才退回 `mysql` 客户端 + 运行账号。完整选项见 `docs/ops-manual.md` 4.1。

权限迁移：`install.sh install / upgrade` 在**健康检查通过之后**执行 `dbt-admin perm migrate --apply --yes --rollback-file <配置备份目录>/perm-migrate-rollback-<时间>.sql`（需要上面的元数据库步骤执行过）；输出实时显示，命令本身最多 2 分钟，超时报错退出；失败只警告，升级照常完成，但迁移完成前普通用户没有任何按钮权限（超级用户不受影响），修好后尽快手工执行同一条命令。不想在升级时执行就加 `--skip-perm-migrate`。新版本没起来而自动回滚时不会执行，库里的权限保持迁移前的样子。

**迁移卡住怎么办**（1.79 有这个问题，1.80 已修复）：写入在一个事务里，卡在读取阶段时什么都没写，Ctrl+C 是安全的；残留的进程用 `pkill -f 'dbt-admin perm migrate'` 结束；服务保持新版本，迁移完成前普通用户没有任何按钮权限（超级用户不受影响）；换成 1.80 以上的包再升级，或先 `./dbt-admin perm migrate` 预览。

## 10. 验证状态与已知限制

### 10.1 已覆盖的测试

`internal/dbtadmin/*_test.go`，全部用内存假库 `memStore` 与假 `database/sql` 驱动，不连任何真实库：语句切分器（字符串 / 注释 / 反引号 / 分号 / 空语句 / 超长 / `DELIMITER` / 行号）、bootstrap 前置检查（空库 / 已有用户拒绝 / `--only-sql`）、幂等重跑与 sha 变化提示、遇错即停与错误报告（不泄漏值）、`dbt_bootstrap_applied` 记录、先全部校验再写入、管理员创建（哈希可验证、弱口令、重名、保护最后一个超级用户、LDAP 拒绝）、审计行、输出不含密码 / 哈希 / 密钥、`gen-secrets` 的熵与格式、dry-run 不写、缺 `--yes`、环境变量缺失、`mysqlStore` 发出的 SQL（列适配、参数化、事务提交 / 回滚）。`install.sh` 的行为测试见 `scripts/install-test/run.sh`。

### 10.2 验证状态

| 项 | 状态 | 说明 |
|---|---|---|
| 命令流程、参数校验、退出码、输出不泄密 | 仅假后端验证 | 见 10.1 |
| 库默认字符集预检与“表不指定字符集”在真实 MySQL 上的实际效果 | 未验证 | `DatabaseCharset` 只用假驱动核对过语句；在 `utf8mb4` + `general_ci` / `0900_ai_ci` 的库、以及 `latin1` / `utf8mb3` 的库上的拒绝与建表结果需要在演练库上实测 |
| 真实 MySQL 上 `mysqlStore` 的 SQL | 未验证 | `information_schema` 查询、`FOR UPDATE`、`INSERT … ON DUPLICATE KEY`、`parseTime/loc` 与 `SET FOREIGN_KEY_CHECKS` 在同一连接上的行为；真实的 `deploy/bootstrap/*.sql` 只验证了“能被切分器完整切分、在假库上走通”，没有在真实 MySQL 上执行过 |
| 终端无回显输入 | 仅编译检查 | Linux / Windows 的实现只做了编译检查（`GOOS=linux` / `windows` 的 `go vet`），没有在真实终端上交互验证 |
| 真实 systemd / root 环境下的 `install.sh --bootstrap` | 未验证 | 属主、权限位、SELinux、`WorkingDirectory`；`scripts/install-test` 用假 `systemctl / install / chown` 验证的是调用顺序与参数 |

### 10.3 已知限制

- 终端无回显读密码用的是标准库 `syscall`（Linux 的 `TCGETS/TCSETS`、Windows 的 `SetConsoleMode`），没有引入 `golang.org/x/term`；其他平台（如 macOS）不支持交互输入密码，请用 `DBT_ADMIN_PASSWORD`。
- 密码策略的实现是 `internal/sysadmin/password.go` 的副本（不直接 import 是因为它依赖整个 HTTP 服务，会让 `dbt-admin` 从约 6 MB 涨到约 55 MB，数字取自 `internal/dbtadmin/password.go` 的注释）；改策略时两处要一起改。
- `admin` 子命令不管理普通用户资料、不建权限组（权限组必须已存在）；不建库。
- 修改 `SECRET_KEY` / `MIRAGE_*` 之后只有 `reencrypt` 能处理“已经用旧密钥加密的值”；解不开的值只列名字，需要在页面里重新填写。

## 11. 常见问题与排障

| 现象 | 可能原因 | 处理 |
|---|---|---|
| 退出码 2，“必须设置环境变量 DATABASE_URL” | 没有设置 `DATABASE_URL`，或把连接串 / 密码写在了命令行上（一律被拒绝） | `export DATABASE_URL='mysql://…'` 后重试 |
| “目标库 … 不存在（本工具不会自动建库）” | 库没建 | 用有建库权限的账号 `CREATE DATABASE <库> DEFAULT CHARACTER SET utf8mb4;` |
| `bootstrap` 退出码 1：库默认字符集不是 utf8mb4 | 库的默认字符集是 `utf8mb3` / `latin1` / `gbk` 等 | 按提示 `ALTER DATABASE … CHARACTER SET utf8mb4 COLLATE …`（只影响之后新建的表）；确需跳过加 `--skip-charset-check --yes`（风险见 §4） |
| `bootstrap` 退出码 1：“已有 N 个用户，拒绝执行基础表 / 基础数据脚本” | 库不是空库 | 升级场景只补扩展表脚本：加 `--only-sql` |
| `bootstrap` 中途失败 | 某条语句报错（输出里有文件名、语句序号、所在行） | 修复原因后原样重跑（全部幂等，已应用的文件幂等重跑） |
| `admin` 子命令提示先执行 `bootstrap` | 库里没有 `dbt_sql_users` | `dbt-admin bootstrap --apply` |
| `admin create`：权限组不存在 | `--group` 的名称在库里没有 | 先确认基础数据已导入（`DBA / TL / RD / QA / Default`） |
| `admin create` / `reset-password`：密码不满足策略（退出码 2） | 长度不足、全数字、常见弱口令、包含用户名 / 邮箱前缀 / 姓名；或开启了复杂度要求 | 换一个密码；策略取自 `security.password_min_length` / `security.password_complexity` |
| `admin reset-password`：拒绝 | 该用户使用 LDAP 认证，密码由外部系统管理 | 在外部系统里改密 |
| `admin deactivate`：拒绝 | 它是最后一个启用的超级用户 | 先让另一个超级用户启用 / 创建 |
| `check-secrets` 退出码 1 | 库里有当前密钥解不开的值（多半是当初用另一把密钥加密的） | 核对 `MIRAGE_SECRET_KEY` / `SECRET_KEY`；批量处理用 `reencrypt`（§6.2） |
| `reencrypt --apply` 被拒绝 | 当前生效的密钥无效（太短），或就是公开缺省值 | 先 `gen-secrets` 并配置自己的 `MIRAGE_SECRET_KEY` |
| `settings` 提示“库里没有 dbt_setting 表” | 没有执行 `260_setting.sql` | `dbt-admin bootstrap --only-sql --apply --yes` |
| `settings set` 提示“这是内部状态项，不能修改” | 内部状态项（页面里不显示） | 确认后加 `--force` |

## 12. 命令速查

下面的命令都需要先设置 `DATABASE_URL`（见 §3）；每个代码块只有一条命令。

演练初始化（默认只演练，不写任何东西）：

```bash
dbt-admin bootstrap
```

执行初始化：

```bash
dbt-admin bootstrap --apply --yes
```

升级时只补扩展表脚本：

```bash
dbt-admin bootstrap --only-sql --apply --yes
```

权限迁移（预览：列出每个权限组 / 用户新增的按钮和丢弃的权限码，不写库）：

```bash
dbt-admin perm migrate
```

执行权限迁移（先把撤销脚本存成 perm-migrate-rollback-<时间>.sql，再写入）：

```bash
dbt-admin perm migrate --apply
```

查看库状态（脚本解析用 `--porcelain`）：

```bash
dbt-admin status
```

创建超级用户（密码用环境变量 `DBT_ADMIN_PASSWORD`，或在终端里无回显输入）：

```bash
dbt-admin admin create --username <用户名> --display <姓名> --group DBA --yes
```

重置密码并要求下次登录改密：

```bash
dbt-admin admin reset-password --username <用户名> --require-change --yes
```

解锁账号：

```bash
dbt-admin admin unlock --username <用户名> --yes
```

列出用户：

```bash
dbt-admin admin list
```

生成一组随机密钥（只打印，不写文件）：

```bash
dbt-admin gen-secrets
```

核对库里的密文能否用当前密钥解开（只读）：

```bash
dbt-admin check-secrets
```

演练重加密（只统计）：

```bash
dbt-admin reencrypt
```

列出与默认值不同的系统设置：

```bash
dbt-admin settings list --modified
```

修改一项系统设置（先演练，再加 `--apply --yes`）：

```bash
dbt-admin settings set --key query.max_execution_seconds --value 120
```
