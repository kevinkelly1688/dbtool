# 字符集与校验规则策略

> **适用版本**：以 `deploy/RELEASE_VERSION` 为准；对 `deploy/bootstrap/*.sql`、`deploy/sql/*.sql` 与 `dbt-admin` 的行为适用。
> **读者**：部署实施人员 / DBA（建库、排查 `Illegal mix of collations`）、维护建表脚本的开发者。
> **文档类型**：策略与技术参考（策略、理由、工具如何保证、已有库如何查看与统一、排障）。
> **相关文档**：[bootstrap-design.md](bootstrap-design.md)（001 / 002 脚本）· [admin-cli.md](admin-cli.md)（`bootstrap` / `status` 的预检与 `--skip-charset-check`）· [ops-manual.md](ops-manual.md) 2.5、3.2（建库）。

**一句话**：**dbtool 创建的所有表、所有字段，都不指定字符集（`CHARACTER SET` / `CHARSET`）与校验规则（`COLLATE`），全部跟随数据库默认值。** 库默认值由部署者在建库时决定，必须是 `utf8mb4`。

## 目录

- [1. 策略](#1-策略)
- [2. 为什么这样做](#2-为什么这样做)
- [3. 建库语句](#3-建库语句)
- [4. 工具怎么保证](#4-工具怎么保证)
- [5. 库默认校验规则的选择](#5-库默认校验规则的选择)
- [6. 跨库 JOIN / 外键的校验规则风险](#6-跨库-join--外键的校验规则风险)
- [7. 已有库：怎么查看与统一](#7-已有库怎么查看与统一)
- [8. 连接字符集仍用 utf8mb4](#8-连接字符集仍用-utf8mb4)
- [9. 验证状态与已知限制](#9-验证状态与已知限制)
- [10. 常见问题与排障](#10-常见问题与排障)
- [11. 变更记录](#11-变更记录)

## 1. 策略

| 项 | 规定 |
|---|---|
| 表选项 | 不写 `DEFAULT CHARSET=…` / `CHARSET=…` / `COLLATE=…` |
| 列定义 | 不写 `CHARACTER SET …` / `CHARSET …` / `COLLATE …`，也不用 `BINARY` / `ASCII` / `UNICODE` 属性、`NCHAR` / `NVARCHAR`（它们隐含字符集 / 二进制校验规则） |
| 索引、表达式 | 不写 `COLLATE` |
| 库默认值 | 字符集必须是 `utf8mb4`；校验规则推荐 `utf8mb4_0900_ai_ci`（MySQL 8）或 `utf8mb4_general_ci`（5.7 / MariaDB） |
| 连接字符集 | 仍然是 `utf8mb4`（驱动参数 `charset=utf8mb4` / `SET NAMES utf8mb4`），它不属于表定义，见第 8 节 |

适用范围：本项目自己创建的表（统一 `dbt_` 前缀）——`deploy/bootstrap/001_base_schema.sql`、`deploy/sql/*.sql` 扩展表脚本、以及 `dbt-admin` 自己的 `dbt_bootstrap_applied`。

不在范围内（它们是用户业务数据，保留源端的字符集才是正确的）：
- 归档功能在目标库里按源表结构（`SHOW CREATE TABLE`）建的备份表（`internal/archive/engine`）；
- 实例管理页面里用户主动选择字符集的“创建数据库”（`internal/instmgr`）；
- 巡检对被监控实例的字符集检查、在线查询“对象信息”里展示的目标库字符集。

## 2. 为什么这样做

- **单一入口**：字符集只在“建库”这一处决定，改库默认值即可影响之后新建的全部表，不需要改任何建表脚本。
- **避免混用**：表上写死 `utf8mb4_general_ci`、另一张写死 `utf8mb4_0900_ai_ci`，跨表 `JOIN` / `UNION` / 外键就会报 `Illegal mix of collations`。
- **可移植**：`utf8mb4_0900_ai_ci` 在 MySQL 5.7 / MariaDB 上不存在，写进脚本会让整个 001 在这些版本上执行失败；不指定就没有这个问题。

代价：建出来的表**完全取决于库默认值**。所以 `dbt-admin bootstrap` / `status` 都会先检查库默认字符集（第 4 节）。

## 3. 建库语句

```sql
-- MySQL 8.0
CREATE DATABASE db_tool CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

-- MySQL 5.7 / MariaDB（没有 utf8mb4_0900_ai_ci）
CREATE DATABASE db_tool CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
```

- **库已建好但默认值不对**：`ALTER DATABASE db_tool CHARACTER SET utf8mb4 COLLATE <校验规则>;` 只改库的默认值，**只影响之后新建的表**，不会改动任何已有表（因此在空库上做是安全的；库里已有表时它们的字符集保持原样，见第 7 节）。
- 库名不要写死 `db_tool`：以你的 `DATABASE_URL` 为准。
- 建库 SQL 里 `COLLATE` 不写时，MySQL 8 得到 `utf8mb4_0900_ai_ci`；同一套库里各表保持一致即可。

## 4. 工具怎么保证

| 环节 | 做法 |
|---|---|
| `deploy/bootstrap/001_base_schema.sql` | 直接维护，不写字符集 / 校验规则；表名 / 外键约束名统一 `dbt_` 前缀（见 [bootstrap-design.md](bootstrap-design.md) 3.1.1），与字符集互不影响；`internal/bootstrapsql` 的测试保证里面不出现字符集 / 校验规则 |
| `deploy/sql/*.sql` 扩展表脚本 | 手写脚本里不写；`131_redis_inspect.sql` 一直如此 |
| `dbt-admin bootstrap` / `status` | 读 `information_schema.SCHEMATA` 的 `DEFAULT_CHARACTER_SET_NAME` / `DEFAULT_COLLATION_NAME` 并显示：不是 `utf8mb4` → 拒绝并给出 `ALTER DATABASE …`；校验规则不在 `utf8mb4_general_ci` / `utf8mb4_0900_ai_ci` / `utf8mb4_unicode_ci` 内 → 警告；`--skip-charset-check`（必须同时 `--yes`）跳过并打印风险。演练输出会写明“已存在的表的字符集不会被本工具修改” |
| 回归保护 | `internal/bootstrapsql/charset_lint_test.go`：用 MySQL 语法树检查 `deploy/bootstrap/*.sql` 与 `deploy/sql/*.sql` 的每条 `CREATE TABLE` / `ALTER TABLE`（含字符串里的预处理 DDL），出现任何字符集 / 校验规则即失败 |

本工具对已有的表**从不执行 `ALTER`**，更不会转换字符集。

## 5. 库默认校验规则的选择

001 与 `deploy/sql` 不指定任何校验规则，所有唯一键（用户名、实例名、权限组名、资源组名、推送地址等）是否区分大小写完全取决于库默认校验规则；推荐的两种（`utf8mb4_0900_ai_ci`、`utf8mb4_general_ci`）都不区分大小写，二者的差别：

| 库默认值 | 行为 |
|---|---|
| `utf8mb4_0900_ai_ci`（MySQL 8 推荐） | `NO PAD`：**末尾空格有意义**（`'a'` 与 `'a '` 在唯一键里是两个值）；4 字节字符（emoji）按字符区分；`ß` 与 `ss` 等少数字符的等价关系不同 |
| `utf8mb4_general_ci`（5.7 / MariaDB） | 末尾空格没有意义（`'a'` 与 `'a '` 相同）；所有增补字符（含 emoji）在比较 / 唯一键里都相等 |

## 6. 跨库 JOIN / 外键的校验规则风险

- 同一条语句里比较、`JOIN`、`UNION` 两个**校验规则不同**的字符串列，MySQL 报 `Illegal mix of collations`（1267）；外键两端的字符串列字符集 / 校验规则必须一致（否则 1215 / 3780）。
- 在**全新库**上用 001 + `deploy/sql` 建表时，所有表跟随同一个库默认值，不会出现这个问题。
- 风险出在**已有表**：在一个已有表的库里执行 `bootstrap`（推荐新库），新建的表跟随的是库默认值，可能和库里已有的表不同。跨表按字符串列 `JOIN` / `UNION` 的查询（例如把几种巡检报告表用 `UNION ALL` 合并成一个列表）在两端校验规则不同时就会失败。所以在已有表的库里安装时，**库默认值要与库里已有的主体表一致**，`bootstrap` 的演练输出会显示它。
- 跨库（在 `db_tool` 里直接引用另一个库的表）同理。
- 连接字符集对此没有帮助：字符串常量使用连接的校验规则，但列与列之间的比较只看列自己的校验规则。

## 7. 已有库：怎么查看与统一

下面都是只读查询，把 `db_tool` 换成你的库名。本工具**不提供、也不会自动执行**任何修改字符集 / 校验规则的语句。

```sql
-- 1) 库默认值（dbt-admin status 也会显示）
SELECT SCHEMA_NAME, DEFAULT_CHARACTER_SET_NAME, DEFAULT_COLLATION_NAME
  FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = 'db_tool';

-- 2) 各表的校验规则分布
SELECT TABLE_COLLATION, COUNT(*) AS tables
  FROM information_schema.TABLES
 WHERE TABLE_SCHEMA = 'db_tool' AND TABLE_TYPE = 'BASE TABLE'
 GROUP BY TABLE_COLLATION;

-- 3) 与库默认值不同的表
SELECT t.TABLE_NAME, t.TABLE_COLLATION
  FROM information_schema.TABLES t
  JOIN information_schema.SCHEMATA s ON s.SCHEMA_NAME = t.TABLE_SCHEMA
 WHERE t.TABLE_SCHEMA = 'db_tool' AND t.TABLE_TYPE = 'BASE TABLE'
   AND t.TABLE_COLLATION <> s.DEFAULT_COLLATION_NAME
 ORDER BY t.TABLE_COLLATION, t.TABLE_NAME;

-- 4) 列的校验规则与所在表不同的列（混用的“重灾区”）
SELECT c.TABLE_NAME, c.COLUMN_NAME, c.CHARACTER_SET_NAME, c.COLLATION_NAME, t.TABLE_COLLATION
  FROM information_schema.COLUMNS c
  JOIN information_schema.TABLES t
    ON t.TABLE_SCHEMA = c.TABLE_SCHEMA AND t.TABLE_NAME = c.TABLE_NAME
 WHERE c.TABLE_SCHEMA = 'db_tool' AND t.TABLE_TYPE = 'BASE TABLE'
   AND c.COLLATION_NAME IS NOT NULL AND c.COLLATION_NAME <> t.TABLE_COLLATION
 ORDER BY c.TABLE_NAME, c.ORDINAL_POSITION;

-- 5) 不是 utf8mb4 的列
SELECT TABLE_NAME, COLUMN_NAME, CHARACTER_SET_NAME, COLLATION_NAME
  FROM information_schema.COLUMNS
 WHERE TABLE_SCHEMA = 'db_tool'
   AND CHARACTER_SET_NAME IS NOT NULL AND CHARACTER_SET_NAME <> 'utf8mb4'
 ORDER BY TABLE_NAME, ORDINAL_POSITION;

-- 6) 已有外键两端的字符串列校验规则是否一致（不一致的外键在统一时需要一起处理）
SELECT k.TABLE_NAME, k.COLUMN_NAME, c1.COLLATION_NAME AS child_collation,
       k.REFERENCED_TABLE_NAME, k.REFERENCED_COLUMN_NAME, c2.COLLATION_NAME AS parent_collation
  FROM information_schema.KEY_COLUMN_USAGE k
  JOIN information_schema.COLUMNS c1
    ON c1.TABLE_SCHEMA = k.TABLE_SCHEMA AND c1.TABLE_NAME = k.TABLE_NAME AND c1.COLUMN_NAME = k.COLUMN_NAME
  JOIN information_schema.COLUMNS c2
    ON c2.TABLE_SCHEMA = k.REFERENCED_TABLE_SCHEMA AND c2.TABLE_NAME = k.REFERENCED_TABLE_NAME AND c2.COLUMN_NAME = k.REFERENCED_COLUMN_NAME
 WHERE k.TABLE_SCHEMA = 'db_tool' AND k.REFERENCED_TABLE_NAME IS NOT NULL
   AND c1.COLLATION_NAME <> c2.COLLATION_NAME;
```

统一的原则（由 DBA 人工评估后执行，不在本工具范围内）：
- 已有表的字符集 / 校验规则**不会**因为改了库默认值或重新执行 `bootstrap` 而改变；需要统一就要对表做转换，这会**重建表、持锁、占用额外磁盘**，大表要排窗口，先备份；
- 外键两端、以及要被 `JOIN` 的字符串列要一起统一，否则会因 1215 / 3780 / 1267 失败；
- 统一之前先用第 3 / 4 / 6 条查询评估范围；统一之后重跑 `dbt-admin status`，`charset_check` 应为 `ok`。

## 8. 连接字符集仍用 utf8mb4

`DATABASE_URL` 的连接、`internal/config` 的 DSN（`charset=utf8mb4`）、对被管理实例的连接等，都用 `utf8mb4` 作连接字符集（驱动在建立连接后发 `SET NAMES utf8mb4`）。这是**会话属性**，只决定客户端与服务器之间传输用什么编码，**不是**表定义，也不受本策略约束：库默认值是 `utf8mb4` 时二者一致，不会发生转换。

## 9. 验证状态与已知限制

| 项 | 状态 | 说明 |
|---|---|---|
| 001 / `deploy/sql/*.sql` 不出现任何字符集 / 校验规则 | 已验证（静态） | `internal/bootstrapsql/charset_lint_test.go`：用 MySQL 语法树检查每条 `CREATE TABLE` / `ALTER TABLE`（含字符串里的预处理 DDL） |
| 去掉字符集后的 001 / `deploy/sql` 在真实 MySQL（8.0 / 5.7 / MariaDB）上的执行结果、建出的列字符集与校验规则 | 未验证 | 测试只做语法树检查与静态对照 |
| `dbt-admin` 的库默认字符集预检 | 仅假后端验证 | 假驱动测试；在 `latin1` / `utf8mb3` 库上的真实拒绝效果需在演练库上实测 |

已知限制：

- 建出来的表**完全取决于库默认值**；本工具对已有表**从不执行 `ALTER`**，更不会转换字符集（第 4、7 节）。
- 本工具不提供、也不会自动执行任何修改字符集 / 校验规则的语句；统一已有库需要 DBA 人工评估（第 7 节）。
- 库默认校验规则决定唯一键是否区分大小写、末尾空格是否有意义（第 5 节）。

## 10. 常见问题与排障

| 现象 | 可能原因 | 处理 |
|---|---|---|
| `dbt-admin bootstrap` 退出码 1，提示库默认字符集不是 `utf8mb4` | 库的默认字符集是 `utf8mb3` / `latin1` / `gbk` 等 | 按输出里的修复命令 `ALTER DATABASE … CHARACTER SET utf8mb4 COLLATE <校验规则>`（只影响之后新建的表；空库上做是安全的），然后重跑；确需跳过加 `--skip-charset-check --yes`（风险见第 4 节） |
| `dbt-admin bootstrap` 警告校验规则不在常见范围 | 库默认校验规则是 `utf8mb4_bin`、`_as_cs` 等 | 不阻止；注意大小写 / 重音敏感性对唯一键的影响（第 5 节），确认符合预期 |
| `dbt-admin status` 显示 `charset_check=unknown`（退出码不受影响）；`bootstrap` 拒绝并提示“读取库默认字符集失败” | 账号读不到 `information_schema.SCHEMATA` 里该库的记录 | 换有可见性的账号（建表账号）执行，或用 `--skip-charset-check --yes` 并自行确认库默认值 |
| 查询 / 页面报 `Illegal mix of collations`（1267） | 同一条语句里比较、`JOIN`、`UNION` 的两个字符串列校验规则不同（常见于已有表的库里后建了新表） | 用第 7 节的查询 2–4 找出差异；库默认值要与库里已有的主体表一致；统一前先评估锁表与磁盘 |
| 建外键报 1215 / 3780 | 外键两端字符串列的字符集 / 校验规则不一致 | 用第 7 节查询 6 核对，两端一起统一 |
| emoji 等 4 字节字符写入失败 / 被截断 | 表是 `utf8mb3` 建出来的（库默认值不是 `utf8mb4` 时跳过了预检） | 用第 7 节查询 5 找出非 `utf8mb4` 的列；评估后转换，或重建空库 |

## 11. 变更记录

只记录文档里已有的事实，不含日期与版本号（来源没有给出）。

- 策略定为“所有表、所有字段都不指定字符集与校验规则，全部跟随库默认值”：此前表上写着 `utf8mb4` / `utf8mb4_general_ci`（`docs/release-notes.md` “升级前必读”第 4 项）。
- `dbt-admin bootstrap` / `status` 增加库默认字符集预检与 `--skip-charset-check`（须同时 `--yes`）。
