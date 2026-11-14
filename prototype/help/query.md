# 在线查询

菜单：SQL 查询 › 在线查询。一个工作台式的只读查询页：选实例和库、写 SQL、立即看结果，所有限制规则提前展示，被拦截时有明确的原因和出路。

## 界面与基本用法

1. 顶部选择**实例**和**库**（下拉只列你所在资源组关联的实例，含 Redis 实例和 PostgreSQL / MongoDB / ClickHouse 实例，见下面对应的章节）；页面会用徽标标出环境（生产 / 测试）和主从角色；
2. 左侧对象树列出库里的表，搜索框按表名、字段名和注释过滤（命中字段的表自动展开）。点表名：编辑器为空时生成"展开字段"的 SELECT（不用 `*`），编辑器里已有内容时把表名插入光标处；点表名左侧的 ▸ 展开字段，点字段名把它插入光标处；悬停在表名 / 字段名上可看注释；
3. 在编辑器里写 SQL，点「运行」（或 `Ctrl + Enter`）：有选中文本只运行选中部分，否则运行光标所在的语句；
4. 结果在下方表格显示：返回行数、耗时、脱敏列数；超出行数上限会被截断并提示"已按 N 行截断，限制来自 xxx 权限"和估计的总行数，并给出「去提交数据导出工单」的链接，需要完整数据时走[数据导出](#help/export)；结果栏的「复制结果」复制整个结果（制表符分隔），「需要导出？」直达导出工单；表格下方可展开「实际执行的 SQL」，查看自动加 LIMIT 之后真正发出的语句；
5. 可开多个**查询标签页**分别写 SQL，标签内容保存在浏览器本地，刷新后恢复；
6. 点「执行计划」只看 `EXPLAIN` 结果，不执行语句；表格下方给出分析提示：全表扫描及候选索引建议（仅供参考，以 EXPLAIN 为准）、WHERE 里对列使用函数导致索引失效、`ORDER BY` 的 filesort、`LIKE` 前导通配符、深分页等；
7. 工具栏上的「格式化」整理编辑器里的 SQL；「示例 SQL」下拉按当前库的表生成示例，分为正常查询、规则演示和会被拦截的反例，选中后放进编辑器（编辑器里已有内容时放进新标签页）；「返回行数」决定没写 LIMIT 时自动加上的行数；
8. 工具栏下方的**提示条**按你在当前库上的权限实时显示：行数上限、权限到期日（14 天内变橙色）、其余无权限的表（带「去申请」链接）、本库敏感列数、超时秒数、是否禁止 `SELECT *`，没有 `query.run` 权限时显示「运行」置灰的原因；Redis、PostgreSQL、MongoDB、ClickHouse 另显示"不做脱敏"和只读语句范围。

对象树里的标记：

| 标记 | 含义 |
|---|---|
| 🔒 | 你对这张表还没有查询权限；点击会直接给出「去申请查询权限」入口并预填库表 |
| ▦ / ◈ | 表 / 视图；右侧是估算行数（视图显示“视图”） |
| 🔑 | 主键字段（展开字段后显示，同时显示字段类型） |
| 🛡 | 敏感字段，查询结果中按脱敏规则显示 |

```sql
-- 典型查询：带条件、带索引列、明确列清单
SELECT id, order_no, status, amount, created_at
FROM order_db.orders
WHERE user_id = 100231 AND created_at >= '2026-10-01'
ORDER BY id DESC
LIMIT 50;
```

## 查询规则（普通用户更严格）

| 规则 | 说明 |
|---|---|
| 只读语句 | 只能执行 SELECT、结构类 SHOW（DATABASES / TABLES / COLUMNS / INDEX / CREATE TABLE / TABLE STATUS）和 EXPLAIN；禁止加锁读、写文件、查 `mysql.user` |
| 禁用危险函数 | 禁止 `sleep()`、`benchmark()`、`load_file()`、`get_lock()` 等 |
| 禁用 `SELECT *` | 管理员可开启；系统只判断选择列表里的通配符，`COUNT(*)` 不会误杀 |
| 自动 LIMIT | 没写 LIMIT 时自动加上；实际生效行数 = min(你选的行数, 各表权限里的最小限制)，无表的 SELECT 上限 100 行 |
| 超时 | 超过最大执行时间（默认 60 秒）自动终止（`KILL QUERY`），页面提示建议加条件或走数据导出；运行中可点「取消」 |
| 库表权限 | 必须对语句涉及的每一张表有有效的库权限或表权限，否则提示"你无 库.表 的查询权限"并给出「申请权限」按钮 |
| 只读视图实例 | 带 `read_view` 标签的实例，**所有人（含管理员）**只能查询以 `v_` 开头的对象 |
| 多语句 | 会明确提示"已执行第 N 条"，建议选中后单条执行 |

> 管理员（超级用户或持有 `query.admin_run`）不受库表授权限制，行数上限取系统的管理员上限（默认 5000）；资源组内实例可由持有 `query.group_run` 的人按管理员行数上限查询。以上特权同样不能越过 `read_view` 规则。

### 数据脱敏

手机号、证件号、地址等敏感列会按管理员配置的脱敏规则显示为脱敏后的值，表头带标记并显示"已脱敏：列名"。**管理员同样会被脱敏。**

> ⚠ 当 goInception 不可用时，脱敏会降级为按列名匹配，`concat(phone, '')` 这类表达式列可能无法识别。生产环境请确认 goInception 已配置，不要依赖查询结果的"看起来已脱敏"。

## Redis 实例（命令模式）

选到 Redis 实例时，页面切换为**命令模式**（切回 MySQL / SQL Server 实例即恢复原样）：

- **库**是 Redis 的 db 编号，下拉显示 `db0 (键数)`（键数取不到就不显示）；查询权限只有**库级**（在[权限申请](#help/qpriv)里选库，不能选表）。
- **左侧是“key 浏览”**：搜索框输入匹配模式（如 `user:*`，没有通配符会自动补 `*`），**回车**后用 `SCAN` 分页列出 key 和类型，点 key 会在编辑器里生成对应的读取命令（`GET` / `HGETALL` / `LRANGE k 0 99` / `SMEMBERS` / `ZRANGE k 0 99 WITHSCORES` / `XRANGE k - + COUNT 100`；编辑器里已有内容时放进新标签页），“加载更多”继续扫描。key 浏览同样要求你对该库有查询权限。
- 编辑器是纯文本；**一次只能运行一条命令**：选中文本则运行选中部分，否则运行光标所在的**行**（光标在空行时按全部内容检查，多于一行会被拒绝并提示）。没有格式化和执行计划（按钮置灰）；“示例命令”下拉里有各类型的例子、限量演示和会被拦截的反例。
- **只读白名单**（固定，不可配置，共 60 个命令名）：`GET MGET STRLEN GETRANGE EXISTS TYPE TTL PTTL EXPIRETIME PEXPIRETIME`、`HGET HMGET HGETALL HLEN HKEYS HVALS HEXISTS HSTRLEN HSCAN HRANDFIELD`、`LRANGE LLEN LINDEX LPOS`、`SMEMBERS SCARD SISMEMBER SMISMEMBER SRANDMEMBER SSCAN SINTER SUNION SDIFF`、`ZRANGE ZREVRANGE ZRANGEBYSCORE ZREVRANGEBYSCORE ZRANGEBYLEX ZCARD ZSCORE ZMSCORE ZRANK ZREVRANK ZCOUNT ZLEXCOUNT ZSCAN`、`SCAN DBSIZE XLEN XRANGE XREVRANGE BITCOUNT GETBIT PFCOUNT OBJECT（仅 ENCODING / IDLETIME / FREQ）MEMORY（仅 USAGE）GEOPOS GEODIST GEORADIUS_RO GEOSEARCH`。写命令、`KEYS`、`SORT`、阻塞命令、订阅、`EVAL` / `SCRIPT`、`CONFIG` / `INFO` / `CLIENT`、`FLUSH*`、`SELECT`（库在页面上选）、`AUTH`（含参数里出现 `AUTH`）等一律拒绝并说明原因；数据变更请走 [SQL 上线](#help/sqlworkflow) 的 Redis 工单。
- **限量**：`SCAN` / `HSCAN` / `SSCAN` / `ZSCAN` 的 `COUNT` 缺省补 100、最多 1000；`LRANGE` / `ZRANGE` / `XRANGE` / `ZRANGEBYSCORE` 等单次最多 1000 条，超出会自动收窄并在结果上方提示（`LRANGE k -100000 -1` 这类负下标会先探测长度再换算）；`HGETALL` / `HKEYS` / `HVALS` / `SMEMBERS` / `SINTER` 先探测元素个数，**超过 5000 会被拒绝**，请改用 `HSCAN` / `SSCAN … COUNT 100` 分页；不带区间的 `BITCOUNT` 遇到超过 16MB 的字符串同样被拒绝；结果行数受你的查询权限上限约束，超限标记“已截断”；单个值超过 64KB 会截断显示；每人同时最多 3 条 Redis 命令。
- **结果表**：字符串 / 数字是 `value` 一行（空值显示 `(nil)`）；数组是 `index, value`；哈希是 `field, value`；带分数的有序集合是 `member, score`；`SCAN` 系列第一行是 `(cursor)` 和下一游标，结果上方有“下一页”按钮；Stream 是 `id, field, value`。含不可见字符的值按 `\xHH` 转义显示并有提示。
- ⚠ **Redis 查询结果不做脱敏**（Redis 没有列脱敏规则），请勿查看敏感数据；拦截、失败、超时的命令同样记入历史和系统审计。

## PostgreSQL / MongoDB / ClickHouse 实例

第一期这三类实例只支持**在线查询**和**数据字典**（含权限申请里的库表选择）；SQL 上线、巡检、会话 / 账号 / 参数、慢日志、分区、导出、归档等页面不会出现它们（直接访问也提示“暂不支持”）。选到这三类实例时，在线查询页按类型调整，切回 MySQL / SQL Server / Redis 完全恢复原样：

| | PostgreSQL（`pgsql`） | MongoDB（`mongo`） | ClickHouse（`clickhouse`） |
|---|---|---|---|
| “库” / “表” | 库 = PostgreSQL database；表 = `schema.表名`（含视图 / 物化视图），库表树按 schema 分组 | 库 = database；表 = 集合（collection），可展开看字段（采样前 100 个文档推断，不是 schema） | 库 = database；表 = 表 / 视图 / 字典（视图 ◈、字典 ☷ 用图标区分） |
| 写什么 | SQL：`SELECT` / `WITH … SELECT` / `VALUES` / `TABLE` / `EXPLAIN`（不带 `ANALYZE`）/ `SHOW 参数名` | mongo shell 风格语句：`find` / `findOne` / `countDocuments` / `count` / `estimatedDocumentCount` / `distinct` / `aggregate` / `getIndexes` / `stats`，以及 `db.getCollectionNames()` / `db.stats()` | SQL：`SELECT` / `SHOW`（库表结构类）/ `DESCRIBE` / `EXISTS` / `EXPLAIN`（AST / SYNTAX / PLAN / PIPELINE / ESTIMATE） |
| 点表名生成 | `select 列… from public.表 limit 100`（**带 schema**） | `db.集合.find({}).limit(10)` | `select 列… from 表 limit 100` |
| 行数上限 | 没有 LIMIT 时补 `LIMIT n+1`；自己写的 LIMIT 不改写，超过上限的部分由客户端截断 | `find` 缺省 / 超限的 `limit` 被收窄并提示；`aggregate` 末尾补 `$limit` | 不改写 SQL，由服务端的结果行数上限 + 客户端截断 |
| 常见拦截 | `pg_sleep` / `pg_read_file` / `set_config` 等函数、`pg_authid` / `pg_stat_activity` 等敏感系统视图、`FOR UPDATE`、可写 CTE、`SELECT … INTO`、多语句 | 写 / 管理方法、`runCommand`、`$where` / `$function`（服务端 JavaScript）、`$out` / `$merge`、`getSiblingDB`、`system.*` 集合、多语句 | `SETTINGS` / `INTO` / 自选 `FORMAT`、`file` / `remote` / `url` 等表函数（只放行 `numbers(N)`）、`system` 库里除摘要类以外的表、`merge()`、`sleep`、外部引擎表 |

要点：

- **每次一条语句**：选中文本则只运行选中部分，否则运行编辑器的全部内容；多于一条会被拒绝并提示选中其中一条。原因文案来自各引擎的检查器（与后端一致）。
- **库与对象的写法**：库由页面顶部的库下拉决定（不要写 `USE` / `getSiblingDB`）；PostgreSQL 的表名要带 schema，没写 schema 的表按 `public` 处理，其它 schema 写 `schema.表`；ClickHouse 访问其它有权限的库的表写成 `库.表`；点库表树里的表 / 集合名会按各自的写法生成示例语句（见上表）。
- **权限**：沿用查询权限，库级与表级都支持（不同于 Redis）；库名、表名**区分大小写**，PostgreSQL 权限里不带点的表名视为 `public` 下的表。语句涉及的每个表 / 集合都要有库级或表级权限，生效行数取各对象限制的最小值；PostgreSQL 的 `pg_catalog` / `information_schema`、ClickHouse 的 `system` 摘要表和 `INFORMATION_SCHEMA` 免表级权限；`SHOW` / `DESCRIBE` / `EXISTS` 属结构查询，不校验表权限；PostgreSQL 在 `FROM` 里调用自定义函数、Mongo 的 `db.stats()` / `db.getCollectionNames()` 要求**库级**权限；`EXPLAIN` 同样要求涉及对象的权限。
- **没有执行计划页签、没有格式化（Mongo）**：PostgreSQL / ClickHouse 的 `EXPLAIN` 当普通语句运行，结果是普通结果表；「执行计划」按钮置灰并给出原因（MongoDB 的「格式化」同样置灰）。
- **结果**：整数 / 大整数（超过 2^53 的 `int8`、ClickHouse 的 `UInt64`、Mongo 的 `NumberLong`）、`numeric` / `Decimal`、日期时间、数组、嵌套文档、`bytea` 都按引擎约定转成可显示的文本；表头下方是列类型；超长单元格（超过 64KB）截断并标注原长；上方的黄色条是引擎的警告（自动补 LIMIT、截断、二进制转义等），蓝色条是静态改写提示。
- ⚠ **这三类的查询结果不做脱敏**（没有列脱敏规则），页面固定提示“该类型的查询结果不做脱敏，请勿查看敏感数据”；拦截、失败、超时的语句同样记入历史和系统审计。
- **只读视图（`read_view`）标签不适用**：实例带该标签时，库表树、执行都提示“该类型（…）不支持只读视图模式”，请管理员去掉标签。
- 超时统一按系统配置的最大执行时间提示（默认 60 秒）；每人最多同时 3 条（三类共用）的并发限制原型不模拟。

## 权限不足怎么办

错误条里会写出具体缺少权限的库表，并带「申请权限」按钮，点开后库表已预填。提交后走[权限申请与审批](#help/qpriv)，通过后立即生效，无需重新登录。

## 历史记录

工具栏的「历史」按钮打开右侧面板：

- 列出**你自己**执行过的 SQL（仅成功执行的会记录）；
- 搜索支持空格分隔的多个关键词，需全部包含；`%`、`_` 按字面匹配；
- 可按实例和日期过滤，每条可"放到新标签"或复制，也可加星收藏；
- 持有审计权限的人可以按用户过滤，看到"谁、什么时间、在哪个库、执行了什么"。

## 常见错误提示

| 提示 | 原因与处理 |
|---|---|
| 无权限 | 缺少库 / 表查询权限或权限已过期，点「申请权限」 |
| 仅支持 SELECT / SHOW / EXPLAIN | 写了 DML / DDL，请改走 [SQL 上线](#help/sqlworkflow) |
| 禁止使用 SELECT * | 从左侧点表名自动展开字段 |
| 只读视图实例只能查询 v_ 开头的对象 | 该实例带 `read_view` 标签 |
| 查询超时 | 加条件 / 加索引列过滤，或改用[数据导出](#help/export) |
| 脱敏失败 | 脱敏流程异常时为保护数据会阻断返回，请联系管理员 |
| 命令不在只读白名单内 / 会修改数据（Redis） | 在线查询只放行固定的只读命令；改用白名单里的等价读命令（如 `KEYS` 换成 `SCAN`），写入请走 SQL 上线的 Redis 工单 |
| key 太大，超过全量读取阈值（Redis） | `HGETALL` / `SMEMBERS` 等遇到超过 5000 个元素的 key，按提示改用 `HSCAN` / `SSCAN` 分页 |
| 你无 Redis dbN 的查询权限（Redis） | Redis 只有库级权限，点「去申请查询权限」选对应的 db 编号 |
| 在线查询只允许只读语句（…）：不允许调用函数 xxx（PostgreSQL）/ xxx 是写操作 / 管理操作（MongoDB）/ 不允许 xxx（ClickHouse） | 语句被各引擎的检查器拒绝，原因就写在提示里；改用只读写法，数据变更请走 SQL 工单 |
| 你无 库.表 表的查询权限（PostgreSQL / MongoDB / ClickHouse） | 这三类支持库级和表级权限，点「去申请查询权限」（表名 PostgreSQL 为 `schema.表`） |
| 该类型（…）不支持只读视图模式 | 实例带 `read_view` 标签，这三类不支持该模式，联系管理员去掉标签 |
| 查询超过 N 秒已终止 / 超过读取行数上限（ClickHouse 158） / 服务端内存不足以完成排序（MongoDB） | 加过滤条件、走索引 / 分区键、缩小时间范围 |

> 原型体验：rd_zhao 只能查订单中心的实例，且需要先有对应库表权限；admin / dba_li 可查看全部实例。打开 mysql-order-view 试试只读视图规则。
> 新类型体验：admin / dba_li 可查看全部；rd_zhao 能看到订单中心的 pg-order-prod-m（只有 `public.orders` / `public.order_items` 两张表的表权限）、mongo-order-prod（只有 `orders` 集合）、ch-order-olap（olap_order 库级权限），打开 pg-bi-prod-m / mongo-user-prod / ch-dw-prod（不在他的资源组）看不到；示例下拉里有各类型的放行、限量、超时和被拦截的例子。数据为虚构的确定性数据，不连任何真实服务；个别复杂写法（窗口函数、`ARRAY JOIN`、`$facet` 等）原型没有模拟，会提示“原型未模拟”。
> Redis 体验：选 redis-order-cache-m（或 redis-session-prod）试试 key 浏览和各类型命令；rd_zhao 只有 redis-order-cache-m 的 db0 权限，切到 db1 或 redis-order-cache-s（权限已过期）会提示申请；rd_zhou 所在资源组没有关联 Redis 实例，下拉里看不到。原型不模拟“每人最多 3 条并发命令”，数据为虚构的确定性键空间。
