# 在线查询

菜单：数据服务 › 在线查询。一个工作台式的只读查询页：选实例和库、写 SQL、立即看结果，所有限制规则提前展示，被拦截时有明确的原因和出路。数据变更不能在这里做，一律走 [SQL 上线](#help/sqlworkflow)。

## 适用范围

| 实例类型 | 语句形式 | 库表权限粒度 | 执行计划 | 数据脱敏的列定位 | 说明 |
|---|---|---|---|---|---|
| MySQL（`mysql`） | SQL | 库级 + 表级（goInception 解析语句涉及的表） | ✔ | 精确（goInception 语法树） | 支持 SET 打头的脚本 |
| PolarDB（`polardb`）/ ADB（`adb`） | SQL | 库级（词法解析库名） | ✔ | 精确（goInception 语法树） | 支持 SET 打头的脚本 |
| PolarDB-X（`polardb-x`） | SQL | 库级 | ✔ | 保守（按列名与表达式引用） | 没有在真实 PolarDB-X 上验证过 goInception 取列来源 |
| SelectDB / Doris | SQL | 库级 | ✔ | 保守（按列名与表达式引用） | 不支持只读会话时降级为仅靠语句检查 |
| SQL Server（`mssql`） | T-SQL 只读批处理 | 库级 | ✖ | 精确（结果集来源分析），分析失败时保守 | 行数上限用 `TOP n` 改写，有字节上限；批处理可以用表变量攒中间结果，见下文 |
| Redis | 命令（只读白名单） | 库级（db 编号） | ✖ | 不脱敏 | 一次一条命令，见下文 |
| PostgreSQL（`pgsql`） | SQL | 库级 + 表级（`schema.table`） | ✖ | 精确（结果列来源） | 一次一条语句 |
| MongoDB（`mongo`） | shell 风格语句 | 库级 + 集合级 | ✖ | 按字段路径 | 一次一条语句 |
| ClickHouse（`clickhouse`） | SQL | 库级 + 表级 | ✖ | 保守（按列名与表达式引用） | 一次一条语句 |

> 注意：MySQL、PolarDB、PolarDB-X、SelectDB、Doris、ADB 都精确到表校验（库权限或表权限）；只有 SQL Server 只校验库权限，对它申请表权限不能让查询通过，请申请库权限（见 [权限申请与查询权限](#help/qpriv)）。Oracle、Cassandra、Elasticsearch 等类型没有在线查询入口。

## 前提条件与所需权限

| 前提 | 说明 |
|---|---|
| 页面访问 | `query.view`（打开在线查询） |
| 执行 | `query.run`（执行查询，含执行计划和「运行全部」）；没有时「运行」「运行全部」「执行计划」置灰并提示 |
| 对象信息 | `query.objinfo`（查看对象信息 / 可编程对象）；没有时隐藏左侧「可编程对象」页签和右键「对象信息」 |
| 实例可见 | 实例属于你所在资源组，且是启用状态；持有 `query.admin_run` 或超级用户可见全部实例 |
| 库表权限 | 对语句涉及的每个库 / 表有有效的查询权限（未过期、未被回收），否则提示无权限并给出「申请权限」入口 |
| 资源组管理员查询 | `query.group_run`：对资源组内带“支持查询”（`can_read`）标签的实例，不受库表授权限制，行数上限取管理员上限 |
| 全部实例查询 | `query.admin_run`（高危）：不受库表授权限制，行数上限取系统的管理员上限（默认 5000） |

## 操作指南

### 界面与基本用法

1. 顶部选择**实例**和**库**（下拉只列你所在资源组关联的实例；也可用 `/query?inst=<实例ID或实例名>&db=<库>` 直接预选）；页面会用徽标标出环境（实例管理里设置的环境，没设置时由实例标签推断生产 / 测试，仅作提示；按生产对待的环境显示红色徽标和顶部色条）和主从角色；
2. 左侧对象树列出库里的表，搜索框**只按表名过滤**（不匹配字段名和注释）。名字太长显示不全时，**用鼠标拖动左栏右边缘**调整宽度（范围 180–720 像素，右边的编辑器至少留 360 像素；双击边缘或聚焦后按 Home 恢复默认 240，聚焦后按 ← → 微调），宽度记在这台浏览器里，下次打开沿用；「可编程对象」页签和 Redis 的 key 浏览用同一个宽度。点表名会在编辑器里生成“展开字段”的 SELECT（不用 `*`）；点表名左侧的 ▸ 展开字段，点字段名把它插入编辑器光标处；右键表名（视图同理）打开「对象信息」（见下文）；
3. 在编辑器里写 SQL，点「运行」（或 `Ctrl + Enter`）：有选中文本只运行选中部分，否则运行光标所在的语句；
4. 结果在下方表格显示：返回行数、耗时、脱敏列数；超出行数上限会被截断并提示“已按 N 行截断，限制来自 xxx 权限”，并给出「去提交数据导出工单」的链接，需要完整数据时走[数据导出](#help/export)；
5. 可开多个**查询标签页**分别写 SQL，每个标签各自保存自己的结果和执行计划，SQL 保存在浏览器本地，刷新后恢复；关闭写有 SQL 的标签会先确认（SQL 会丢失）；
6. 点「执行计划」只看 `EXPLAIN` 结果，不执行语句（Redis、SQL Server、PostgreSQL、MongoDB、ClickHouse 没有这个页签，见下文各自的章节）；执行计划页签里 type 按风险着色，并提示全表扫描、函数使索引失效、filesort、前导 `%`、深分页等问题；
7. 工具栏上的「格式化」整理编辑器里的 SQL；「示例 SQL」下拉按当前库的第一张表生成示例，分为正常查询、规则示例和会被拦截的反例，选中后放进编辑器（编辑器里已有内容时放进新标签页）；「返回行数」决定没写 LIMIT 时自动加上的行数；
8. 工具栏下方的**提示条**只在出了问题、需要你处理时才显示：权限 14 天内到期（橙色）、其余无权限的表（带「去申请」链接）、在当前库暂无查询权限、数据脱敏开关没开但库里有敏感字段（Redis 不脱敏）；没有这些就整条隐藏。正常状态的说明（管理员 / 资源组或授权的权限范围与行数上限、到期日、脱敏已开启、是否禁止 `SELECT *`、超时秒数、只读语句范围）都放在鼠标停在「运行」按钮上的提示里；
9. 结果被截断时会估算总行数；敏感列的表头和值用绿色显示（悬停表头可看字段类型）。

对象树里的标记：

| 标记 | 含义 |
|---|---|
| 🔒 | 你对这张表还没有查询权限；点击会直接打开权限申请并预填库表 |
| 🛡 + 数字 | 表里有敏感字段（数字是个数），查询结果中按脱敏规则显示；悬停可看字段名，展开字段后敏感字段带 🛡 |
| ◈ | 视图；ClickHouse 的 ▦ 字典、物化视图等在表名右侧标注种类 |
| 🔑 | 主键字段（展开字段后显示，同时显示字段类型） |
| 右侧数字 | 表的估算行数 |

```sql
-- 典型查询：带条件、带索引列、明确列清单
SELECT id, order_no, status, amount, created_at
FROM order_db.orders
WHERE user_id = 100231 AND created_at >= '2026-10-01'
ORDER BY id DESC
LIMIT 50;
```

### 结果表格：复制与预览

| 想做的事 | 怎么操作 |
|---|---|
| 复制一个单元格 | 鼠标移到单元格上，右侧出现工具条，点第一个图标；或点一下单元格后按 `Ctrl + C` |
| 看完整内容（长文本、多行 SQL、JSON） | **双击单元格**，或点工具条的“眼睛”图标；预览带行号、长行自动换行，像 SQL 的内容会高亮；右上角“复制”复制的是完整内容，`Esc` 关闭 |
| 复制整列 | 把鼠标移到列名上，点列名后面的复制图标：**复制列值**（每行一个）、**复制列名和列值**、**复制为 IN 列表**（`('a', 'b', 1)`，去重并跳过 NULL） |
| 复制整个结果 | 结果栏右侧的“复制”按钮（制表符分隔，可直接贴进表格软件） |

- **NULL**：复制单元格得到文字 `NULL`；复制整列或整个结果时 NULL 是空的，保证和别的列逐行对齐。
- **值里的换行**：复制整列 / 整个结果时折成空格（一行一个值）；要原样的多行文本，用单元格复制或预览里的复制。
- **IN 列表的引号**：字符串用单引号；MySQL 系、ClickHouse 会把反斜杠写成 `\\`，PostgreSQL、SQL Server 只处理单引号。Redis、MongoDB 没有这一项。
- **脱敏列**：复制和预览看到的就是页面上显示的（已脱敏的）值。
- **键盘**：点一下表格后，方向键移动选中的单元格，`Enter` 预览，`Ctrl + C` 复制，`Esc` 取消选中。
- **超长内容**：预览最多显示前 500,000 个字符并提示；复制仍然是完整内容。

### 对象信息与可编程对象

**对象信息**：在左侧「表」页签里**右键表名**（视图同理），选「对象信息」，弹出这张表的：

- **基本信息**：数据库、表名、估算行数、数据 / 索引大小、引擎、字符集、说明等（SQL Server 另有主键，自增列会标注“（自增）”，以及当前自增值）；
- **列信息**：列名、类型、是否可空、默认值、键、额外、注释——**自增列在“额外”里标注**（MySQL 是 `auto_increment`，SQL Server 是 `identity（自增）`）；字段配置了数据脱敏的，最后一列“脱敏”写 **Y**（整行淡色突出，没配置的留空）。除“注释”外的列一律一行显示，注释太长时才换行；基本信息的属性多时在宽窗口里分左右两栏显示（序号连续）；
- **索引信息**：索引名、列、是否唯一、类型（含主键）；
- **创建语句**：带行号和高亮。SQL Server 的字段写成 `[Id] int IDENTITY(1,1) NOT NULL`（种子、步长），并在 `-- INDEXES` 下列出主键 / 唯一约束 / 索引，之后是外键、检查约束和触发器；这份语句由系统目录还原，**仅供查看参考，不保证可直接执行**（不含文件组、分区方案、压缩、填充因子等物理选项）。

右上角的复制图标复制整条创建语句，`Esc` 关闭。只读，不会改动任何东西。PolarDB-X 等兼容实现的系统表缺列时，表信息会退到基础列、列信息改用 `SHOW FULL COLUMNS`，各小节读取失败只在该小节写原因。

**可编程对象**（MySQL 系、SQL Server）：左侧切到「可编程对象」页签，按类别列出当前库的**视图、函数、存储过程、触发器**（MySQL 还有定时任务），组标题上是数量，点组标题展开 / 收起。

- **双击**对象（或用键盘选中后按 `Enter`）查看定义；右键也可以打开「对象信息」。内容只读，右上角可以复制定义语句；
- SQL Server 的对象带架构名（如 `dbo.GetMonthOfDays`）；
- 搜索框只按对象名过滤，命中的类别自动展开；
- 某一类读取失败（例如账号没有查看定义的权限）时，原因显示在该类别下，不影响其它类别；
- Redis、PostgreSQL、MongoDB、ClickHouse 没有这个页签（对象信息仍可用于它们的表 / 视图）。

> 提示：对象信息和可编程对象需要按钮权限 `query.objinfo`（需要在权限组里勾选，TL / RD / QA / Default 预设已包含），不需要额外的菜单权限；范围与左侧库表树一致：能访问该实例、库名在允许范围内、不是系统库。它不检查表的查询权限：没有表权限（🔒）的表也能看结构，看不到数据。

### 一次运行多条语句：运行全部（一条语句一个结果页签）

脚本里写了好几条语句、想一次看到每条的结果时，点工具栏的「运行全部」（或 `Ctrl + Shift + Enter`）：每条语句各占一个结果页签，另有一个「汇总」页签。「运行」的行为不变（运行选中的文本，没选中就运行光标所在的一条）。

- **运行哪些**：编辑器里的全部语句；选中了文本就只运行选区碰到的那几条整句（按钮上显示“（N 条）”）。一次最多 20 条，超过会提示、一条都不执行。
- **每条各自走一遍查询流程**：库表权限、SQL 防火墙、脱敏、行数上限、超时、审计日志都按条处理，执行历史里每条各记一次，不会因为打包运行而放宽。MySQL 系脚本开头连续的 `SET` 不单独成页签，随每条语句一起提交（同一个会话里先执行）。
- **页签**：「汇总」列出每条的状态、行数、耗时和说明，点一行跳到该页签；「结果 N」就是原来的结果面板（悬停复制 / 预览、双击预览、方向键、列头复制整列都照旧）。成功的页签不加标记，失败 / 超时是红字，被拦截是橙字，执行中转圈，没跑到的是空心圆。右键页签可以复制 SQL、定位到语句、只重新运行这一条、固定、关闭 / 关闭其他 / 关闭右侧；点页签上的 × 或中键也能关。
- **编辑器**：每条已运行语句第一行的行号槽里有个带序号的状态圆点，点它跳到对应页签；当前页签对应的语句在编辑器里高亮。语句改过之后对不上结果，圆点就消失。
- **批量选项**：遇到错误时“停止”（默认，后面的语句标成“未执行”）或“继续执行”；执行方式“依次”（默认）或“同时执行，最多 3 条”（适合互不相干的查询，3 条就是每人同时执行的上限）。选择保存在浏览器本地。
- **固定页签**：鼠标停到页签上会出现 📌，点它，下一次运行（「运行」或「运行全部」）不会替换这个页签，方便和新结果对比。固定后页签顶上有一条橙色边、图钉带橙色描边，并弹出提示；再点一次取消固定。
- **取消与重试**：点「取消」停掉正在执行的那条，后面没跑的标成“已取消”。汇总里可以“重试没成功的”“全部重新运行”，页签里可以“重新运行”（沿用这批的实例和库，依次执行，遇错不停）。
- **防火墙要求填写原因**：整批只弹一次，原因用于这批里需要原因的语句；不填则这几条标成“已取消”。
- 只有一个结果、又没有固定的页签时不出页签条，和以前一样；点「执行计划」会多开一个「执行计划」页签。Redis、MongoDB 一次只能一条命令 / 语句，没有「运行全部」。结果只保存在页面里，刷新即丢。

### SET 打头的脚本（MySQL 系）

脚本开头可以写 `SET @变量 := 值;`（值可以是常量，也可以是只读子查询）或 `SET block_encryption_mode = 'aes-256-cbc';` 这几个会话参数，后面的查询就能引用 `@变量`。把整份脚本**选中后运行**，会在同一个会话里按顺序全部执行，显示**最后一条查询**的结果（前面的查询通常是“校验”：子查询返回多行就报错中断）；不选中、把光标放在某条语句上运行时只运行这一条，脚本开头的 `SET` 会自动随它一起执行。脚本里只要有一条是写语句、其它 `SET`、`INTO` 等，整份都不会执行，错误里会指出是第几条；变量里存的敏感列同样会脱敏，子查询涉及的表同样要有查询权限，脚本里的每条查询也都会各自经过 [SQL 防火墙](#help/firewall)（任一条被拒绝则整份不执行）。没有 `SET` 打头的多条语句仍然只执行第 1 条（页面提示“检测到 N 条语句，本次只执行第 1 条”）。

脚本的上限：`SET` 语句最多 20 条、总长度不超过 8000 字符，脚本语句总数不超过 40 条，其中查询不超过 10 条。

### DECLARE / SET 打头的批处理（SQL Server）

SQL Server 的在线查询除单条 `SELECT` / `WITH` 外，还能运行 `DECLARE @变量` / `SET @变量` 打头、最后用 `SELECT`（或 `WITH … SELECT`）输出结果的只读批处理：先 `DECLARE @SiteId INT = 1008;`，用 `IF … THROW 50001, N'未配置', 1;` 做校验，再查询。批处理里可以用 `DECLARE @t TABLE (…)` 声明表变量，再 `INSERT [INTO] @t SELECT … / VALUES (…)`（`INSERT` 前可以带 CTE）把中间结果攒起来，最后一条 `SELECT` 从表变量里统计；表变量只活在这个批处理里，不碰库里的数据（数据放在 SQL Server 的 tempdb 里，中间结果攒得太大会占用 tempdb 空间，别拿它搬大批量数据）。

- 只返回第一条产出结果的查询的结果集（`SELECT @a = …` 这种只给变量赋值的不算）；用 `GO` 分隔的多个批只执行第 1 个（提示“检测到 N 条语句，本次只执行第 1 条”，可选中某个批再运行）；批处理里没有输出结果的 `SELECT`（例如只有声明和 `INSERT`）时提示“这段批处理没有输出结果的 SELECT”。
- 行数上限 `TOP n` 加在这条产出结果的 `SELECT` 上，不会加到 `INSERT` 里的 `SELECT` 上；它带 `UNION`、`OFFSET`、`TOP … PERCENT` 时不改写，由引擎在客户端截断到上限。
- 仍然拒绝（提示前面带“不允许的 T-SQL：”）：写真实表、`#临时表`、没用 `DECLARE @t TABLE (…)` 声明的变量、`INSERT TOP (n) INTO @t`，提示“INSERT 只能写入 DECLARE @变量 TABLE (…) 声明的表变量，不能写入表或临时表”；`INSERT … EXEC`、`SELECT … INTO`、`OUTPUT`（包括不带 `INTO` 的 `INSERT … OUTPUT`：它会把写入的行当成一个结果集先返回，绕过按 `SELECT` 列表做的脱敏）、对表变量的 `UPDATE` / `DELETE`、`EXEC` 和动态 SQL、`WHILE`、游标、`USE`、`OPENROWSET`、事务控制（`BEGIN TRAN` / `COMMIT` / `ROLLBACK`）、`RAISERROR`、会加锁或覆盖会话隔离级别的表提示（`UPDLOCK` / `XLOCK` / `TABLOCK` / `TABLOCKX` / `HOLDLOCK` / `SERIALIZABLE` / `REPEATABLEREAD` / `READCOMMITTED` / `READCOMMITTEDLOCK`，写在 `WITH (…)`、旧写法 `(…)` 或 `OPTION (TABLE HINT …)` 里都一样；`NOLOCK` / `READUNCOMMITTED` 不受影响，在线查询本来就是 `READ UNCOMMITTED`，不对业务表加共享锁），提示“只读查询里不允许出现 xxx”；除 `SET @变量` 外的 `SET`（如 `SET NOCOUNT ON`）、参数里用了变量的 `THROW`、`NEXT VALUE FOR`、四部分名称（链接服务器）也被拒绝，各有自己的提示。不是 `SELECT` / `WITH` / `DECLARE` / `SET` 打头的语句（包括直接 `INSERT`）提示“SQL Server 在线查询仅支持 SELECT / WITH 查询（…）”。
- 变量、表变量带出的敏感字段同样脱敏（见 [数据脱敏](#help/masking)）；语句里 `FROM` / `JOIN` 涉及的库（含 `INSERT … SELECT` 读的表）都要有库权限。

### Redis 实例（命令模式）

选到 Redis 实例时，页面切换为**命令模式**（切回 MySQL / SQL Server 实例即恢复原样）：

- **库**是 Redis 的 db 编号，下拉显示 `db0 (键数)`（键数取不到就不显示）；查询权限只有**库级**（在权限申请里选库，不能选表）。
- **左侧是“key 浏览”**：搜索框输入匹配模式（如 `user:*`，没有通配符会自动补 `*`），**回车**后用 `SCAN` 分页列出 key 和类型，点 key 会在编辑器里生成对应的读取命令（`GET` / `HGETALL` / `LRANGE k 0 99` / `SMEMBERS` / `ZRANGE k 0 99 WITHSCORES` / `XRANGE k - + COUNT 100`），“加载更多”继续扫描。
- 编辑器是纯文本；**一次只能运行一条命令**：选中文本则运行选中部分，否则运行光标所在的**行**。没有格式化和执行计划；“示例命令”下拉里有各类型的例子和会被拦截的反例。
- **只读白名单**（固定在代码里，共 60 个命令，不可配置）：`GET MGET STRLEN GETRANGE EXISTS TYPE TTL PTTL EXPIRETIME PEXPIRETIME`、`HGET HMGET HGETALL HLEN HKEYS HVALS HEXISTS HSTRLEN HSCAN HRANDFIELD`、`LRANGE LLEN LINDEX LPOS`、`SMEMBERS SCARD SISMEMBER SMISMEMBER SRANDMEMBER SSCAN SINTER SUNION SDIFF`、`ZRANGE ZREVRANGE ZRANGEBYSCORE ZREVRANGEBYSCORE ZRANGEBYLEX ZCARD ZSCORE ZMSCORE ZRANK ZREVRANK ZCOUNT ZLEXCOUNT ZSCAN`、`SCAN DBSIZE XLEN XRANGE XREVRANGE BITCOUNT GETBIT PFCOUNT OBJECT（仅 ENCODING / IDLETIME / FREQ）MEMORY（仅 USAGE）GEOPOS GEODIST GEORADIUS_RO GEOSEARCH`。写命令、`KEYS`、`SORT`、阻塞命令、订阅、`EVAL` / `SCRIPT`、`CONFIG` / `INFO` / `CLIENT`、`FLUSH*`、`SELECT`（库在页面上选）、`AUTH`（含参数里出现 `AUTH`）等一律拒绝并说明原因；数据变更请走 [SQL 上线](#help/sqlworkflow) 的 Redis 工单。
- **限量**：`SCAN` / `HSCAN` / `SSCAN` / `ZSCAN` 的 `COUNT` 缺省补 100、最多 1000；`LRANGE` / `ZRANGE` / `XRANGE` / `ZRANGEBYSCORE` 等单次最多 1000 条，超出会自动收窄并在结果上方提示；`HGETALL` / `HKEYS` / `HVALS` / `SMEMBERS` / `SINTER` 先探测元素个数，**超过 5000 会被拒绝**，请改用 `HSCAN` / `SSCAN … COUNT 100` 分页；`MGET` / `EXISTS` / `HMGET` 等多键命令一次最多 500 个键；结果行数受你的查询权限上限约束，超限标记“已截断”；每人同时最多 3 条 Redis 命令。
- **结果表**：字符串 / 数字是 `value` 一行（空值显示 `(nil)`）；数组是 `index, value`；哈希是 `field, value`；带分数的有序集合是 `member, score`；`SCAN` 系列第一行是 `(cursor)` 和下一游标，结果上方有“下一页”按钮；Stream 是 `id, field, value`。含不可见字符的值按 `\xHH` 转义显示并有提示。
- **Redis 查询结果不做脱敏**（Redis 没有列脱敏规则），请勿查看敏感数据；拦截、失败、超时的命令同样记入历史和系统审计。

### PostgreSQL / MongoDB / ClickHouse 实例

选到这三类实例时，页面按类型切换（切回 MySQL / SQL Server / Redis 即恢复原样）。**权限沿用库级 + 表级**（不同于 Redis，可以申请到具体表），不需要新的按钮权限码。

- **共同点**：编辑器、示例下拉、左侧对象树按类型切换；**没有格式化、没有“执行计划”页签**（PostgreSQL / ClickHouse 的 `EXPLAIN` 当普通语句直接运行，结果显示为表格）；一次只能运行一条语句；查询结果按“安全合规 › 数据脱敏”的配置脱敏（PostgreSQL 精确匹配、ClickHouse 保守匹配、MongoDB 按字段路径；脱敏总开关 `query.mask_enabled` 关闭时，提示条和结果上方都会提示“不做脱敏”）；结果里的 `warnings`（黄色条）与 `notices`（蓝色条）逐条显示，截断说明直接来自引擎（行数 / 字节 / 响应上限）；查询日志、历史、被拦截记录、系统审计照常记录；不提供“去导出”入口和总行数估算；每人同时最多 3 条这类查询（三种类型共用计数），超出提示稍后再试。
- **行数上限**：沿用现有规则，取你选的行数、权限里的行数上限、系统管理员上限中的较小值；字节上限与超时用引擎默认值（以系统“最大执行时间”更短者为准）。

#### PostgreSQL（`pgsql`）

- **库** = PostgreSQL database；**表**写作 `schema.table`。左侧树按 schema 分组（`public` 排第一，点分组标题折叠），点表名生成 `select 列… from public.表 limit 100`——**必须带 schema**：没写 schema 的表按 `public` 处理，访问其它 schema 要写 `schema.table`。
- 放行：`SELECT`（含 `WITH … SELECT`、`UNION`、窗口函数）、`VALUES`、`TABLE 名`、`EXPLAIN [VERBOSE …]`（**不带 `ANALYZE`**）、`SHOW 参数名`；顶层 `SELECT` 没有 `LIMIT` 时自动补 `LIMIT`（查询日志里能看到实际执行的语句）。
- 拒绝（原因在错误条里原文显示）：写操作与 DDL（`INSERT / UPDATE / DELETE / CREATE / DROP / COPY / SET / BEGIN …`）、多条语句、`SELECT … INTO`、`FOR UPDATE / SHARE` 行锁、可写 CTE、`EXPLAIN ANALYZE`、`pg_sleep()`、`pg_read_file()`、`dblink` 等危险函数，以及 `pg_authid`、`pg_shadow`、`pg_stat_activity`、`pg_stat_statements` 等敏感系统视图。列名恰好是 `update`、`insert`、`delete`、`into` 等词时要加双引号。
- 权限：`pg_catalog`、`information_schema` 免表级权限；其它系统 schema 拒绝。FROM 里调用了**自定义函数**的语句要求该库的**库级**权限。跨库引用（`其它库.schema.表`）被拒绝。库名、表名**区分大小写**（未加引号的标识符按小写折叠后比较）。
- 说明：行数是统计估算（`pg_class.reltuples`）；时间戳带服务端 / 账号默认时区的偏移；超出 JS 安全整数范围的 `bigint`、`numeric` 以字符串显示；`bytea` 显示十六进制，超过 64 KB 截断；单元格最多 64 KB。

#### MongoDB（`mongo`）

- **库** = database，**表** = 集合。编辑器是纯文本（不做语法高亮）；左侧是集合列表，点集合名生成 `db.集合.find({}).limit(10)`（编辑器非空时放进新标签），点 ▸ 展开的**字段是对集合前 100 个文档的采样**，不是 schema；搜索框按集合名过滤。
- **一次只能运行一条语句**：选中文本则运行选中部分，否则运行**编辑器的全部内容**（不按分号或空行切分）。
- 放行：`find / findOne / countDocuments / count / estimatedDocumentCount / distinct / aggregate / getIndexes / stats / getCollectionNames`，可接 `.sort() .skip() .limit() .count() .toArray() .pretty()`；`aggregate` 只放行白名单阶段（`$match $project $group $sort $limit $unwind $lookup $facet …`，`$lookup` 等只能引用同库集合名）；支持 `ObjectId()`、`ISODate()`、`NumberLong()` 等常见构造函数和正则字面量。`find` 没带 `limit` 或超限时自动收窄到行数上限并提示。
- 拒绝：所有写 / DDL / 管理方法、`runCommand`、`eval`、`getSiblingDB`（切库请用页面库下拉）、`explain`、`$where` / `$function` / `$accumulator`（服务端 JavaScript）、`$out` / `$merge`、`$collStats` / `$indexStats` 等阶段、`system.*` 集合和 `admin / local / config` 库、第二条语句。
- 权限：集合级权限按语句涉及的集合判断（含 `$lookup` 的目标集合）；`db.getCollectionNames()`、`db.stats()` 要库级权限。
- 说明：`int64`、`Decimal128` 以字符串显示；`ObjectId("…")`、日期按 ISO 毫秒 UTC；嵌套文档 / 数组是紧凑 JSON 字符串；顶层字段超过 200 个时多出的折叠进 `_extra` 列；排序 / 分组不使用磁盘，超内存时请加索引或缩小范围。

#### ClickHouse（`clickhouse`）

- **库** = database，**表** = 表 / 视图 / 字典（◈ 视图、▦ 字典、物化视图在右侧标注）；点表名生成 `select 列… from 表 limit 100`（库由页面库下拉决定，语句里可写 `库.表` 访问其它有权限的库）。
- 放行：`SELECT`（含 CTE、`UNION`、各类 `JOIN`、`ARRAY JOIN`、`FINAL`、`SAMPLE`、窗口函数）、`SHOW`（`DATABASES / TABLES / DICTIONARIES / CREATE … / COLUMNS / INDEX`）、`DESCRIBE`、`EXISTS`、`EXPLAIN [AST|SYNTAX|PLAN|PIPELINE|ESTIMATE]`；表函数只放行 `numbers(N)` 与 `values(…)`。语句末尾的 `FORMAT xxx` 被忽略并提示（输出格式固定）。
- 拒绝：写 / DDL / 管理语句（`INSERT / ALTER / CREATE / DROP / OPTIMIZE / SYSTEM / SET / USE …`）、多条语句、任何位置的 `SETTINGS`、`INTO OUTFILE`、`sleep()` 等危险函数、`file() / url() / s3() / remote() / mysql() …` 表函数、`merge()`（无法确定涉及的表）、`system` 库里除摘要表（`databases / tables / columns / parts / data_skipping_indices …`）以外的表（`system.users`、`system.query_log` 等）。列名叫 `settings`、`format` 时要用反引号。
- 说明：`UInt64 / Int64`、`Decimal` 以字符串显示；没有时区声明的 `DateTime` 不带偏移；查询带 `readonly=1`，**如果目标账号的 profile 本身是 `readonly=1`，服务端会拒绝请求里的设置（错误码 164），需要 DBA 把账号 profile 改为 `readonly=2`**；全表排序 / 聚合靠超时、读取量、内存上限兜底。

### 权限不足怎么办

错误条里会写出具体缺少权限的库表，并带「申请权限」按钮，点开后库表已预填。提交后走[权限申请与审批](#help/qpriv)，通过后立即生效，无需重新登录。

### 我的 SQL：收藏与执行历史

工具栏的「我的 SQL」按钮打开右侧面板，里面有两个标签：

**收藏**（只有你自己能看到，每人最多 500 条）：

- 点「添加」：把编辑器里**选中的文本**（没选中则是光标所在的语句）收藏起来，可以给它写一句注释；同一条 SQL 不会重复收藏，再次添加只会更新注释；
- 搜索注释或 SQL，空格分隔多个关键词，需全部包含；点「添加时间」「最后使用时间」表头排序；
- **双击 SQL**（或点「使用」）把它粘贴到上方编辑器的光标处，不会覆盖你已经写的内容；每次使用会更新「最后使用时间」；
- 可以「编辑」注释和 SQL，「删除」需要再点一次确认。

**执行历史**：

- 普通用户只能看到**自己**执行过的 SQL，不能看别人的；
- 按语句模糊搜索：空格分隔的多个关键词，需全部包含，不区分大小写；`%`、`_` 按字面匹配，命中的词会高亮；
- 可按实例、日期、状态（成功 / 被拦截 / 失败 / 超时 / 已取消）过滤；
- 每条可以「粘贴」到编辑器（双击 SQL 同样）、放到「新标签」（尽量带上当时的实例和库）、「收藏」或「复制」；
- **超级用户**能看到所有人的执行历史（多一列「用户」），并且可以按**用户模糊过滤**（输入用户名或姓名的一部分即可），再叠加语句过滤，看到“谁、什么时间、在哪个库、执行了什么”。

## 字段、参数与选项说明

| 名称 | 含义 | 取值与默认 | 约束 |
|---|---|---|---|
| 返回行数 | 没写 LIMIT 时自动加上的行数 | 页面下拉 | 实际生效 = min(你选的行数, 各表权限里的最小限制)；无表的 SELECT 上限 100 行 |
| 查询超时 `query.max_execution_seconds` | 超过后终止查询（会话变量 `max_execution_time` + `KILL QUERY`） | 默认 60 秒，范围 1–3600 | 系统配置项 |
| 管理员行数上限 `query.admin_row_limit` | 超级用户 / `query.admin_run` / `query.group_run` 的行数上限 | 默认 5000，范围 1–1000000 | 系统配置项 |
| 禁止 `SELECT *` `query.forbid_select_star` | 拒绝选择列表里带通配符的查询 | 默认关闭 | `COUNT(*)` 不受影响 |
| 脱敏总开关 `query.mask_enabled` | 在线查询结果是否脱敏 | 默认关闭 | 关闭时已配置的脱敏字段也不生效；防火墙“强制脱敏”仍生效 |
| 脱敏出错时拦截 `query.block_on_mask_error` | 脱敏出错时是否不返回数据 | 默认关闭（放行并提示“本次结果未脱敏”） | 见下文数据脱敏 |
| 单元格字节上限 `query.max_cell_bytes` | 超出的单元格被截断并提示 | 默认 4194304 字节 | 仅 SQL Server 在线查询遵守 |
| 结果集字节上限 `query.max_result_bytes` | 超出只返回前面的行并提示 | 默认 33554432 字节 | 仅 SQL Server 在线查询遵守 |
| 并发上限 | 每人同时执行的 Redis 命令 / PostgreSQL、MongoDB、ClickHouse 查询数 | 各 3 条（后三类共用计数） | 超出提示稍后再试 |

## 规则、限制与注意事项

### 查询规则（普通用户更严格）

| 规则 | 说明 |
|---|---|
| 只读语句 | 只能执行 SELECT、WITH（CTE，主语句必须是 SELECT 且不含写操作）、结构类 SHOW（DATABASES / TABLES / COLUMNS / INDEX / CREATE TABLE / TABLE STATUS）、DESC / DESCRIBE 和 EXPLAIN；禁止加锁读（`FOR UPDATE`、`LOCK IN SHARE MODE`）、写文件（`INTO OUTFILE / DUMPFILE`）、查 `mysql` 系统库 |
| EXPLAIN ANALYZE | 会真实执行语句，被拦截，请使用普通 EXPLAIN |
| 禁用危险函数 | 禁止 `sleep()`、`benchmark()`、`load_file()`、`get_lock()` 等 |
| 禁用 `SELECT *` | 管理员可开启；系统只判断选择列表里的通配符，`COUNT(*)` 不会误杀 |
| 自动 LIMIT | 没写 LIMIT 时自动加上；实际生效行数 = min(你选的行数, 各表权限里的最小限制)，无表的 SELECT 上限 100 行 |
| 超时 | 超过最大执行时间（默认 60 秒）自动终止：查询前先设置会话变量 `max_execution_time`（MySQL 5.7.8+，仅对 SELECT 生效），客户端超时或取消时再发 `KILL QUERY`；页面提示建议加条件或走数据导出；运行中可点「取消」，取消同样会终止数据库端的语句 |
| 库表权限 | 必须对语句涉及的每个库 / 表有有效的库权限或表权限（表级精确度见“适用范围”），否则提示“你无 库.表 的查询权限”并给出「申请权限」按钮；EXPLAIN 与结构类 SHOW 不校验库表权限 |
| 多语句 | 会明确提示“已执行第 N 条”，建议选中后单条执行 |
| SQL 防火墙 | 命中“要求填写原因”规则的语句，运行前弹窗要求写原因（4–200 字），原因随语句进入审计；其它规则的效果见 [SQL 防火墙](#help/firewall) |
| 只读会话 | MySQL 系查询在服务端只读会话里执行（`SET SESSION TRANSACTION READ ONLY`），会写数据的存储函数被数据库拒绝并记为“被拦截”；不支持只读会话的服务端（MySQL 5.6.5 之前、TiDB 默认、Doris / SelectDB / ADB）自动降级为仅靠语句检查 |
| 普通用户的 SHOW | 非结构类 SHOW 语句仅管理员可执行 |

> 注意：管理员（超级用户或持有 `query.admin_run`）不受库表授权限制，行数上限取系统的管理员上限（默认 5000）；持有 `query.group_run` 的人可按管理员行数上限查询资源组内带 `can_read` 标签的实例。

> 警告：Redis 查询结果不做脱敏，且 `query.mask_enabled` 默认关闭。生产环境上线前请确认脱敏规则已配置、总开关已开启，不要依赖查询结果“看起来已脱敏”。

### 数据脱敏

手机号、证件号、地址等敏感列会按管理员配置的脱敏规则显示为脱敏后的值，表头和值显示为绿色，结果栏显示“已脱敏：列名”。**管理员同样会被脱敏。**规则在“安全合规 › 数据脱敏”配置，详见 [数据脱敏](#help/masking)。

- MySQL / PolarDB / ADB 用 goInception 语法树精确定位每个结果列的来源库表字段；**goInception 未配置时**降级为按结果列名与表达式引用保守匹配（页面提示，可能多脱敏，不同库表里同名的列也会被脱敏，`concat(phone, '')` 这类表达式列按引用的字段名处理）；系统开启了 `REQUIRE_GOINCEPTION` 时未配置则直接拒绝查询；
- PolarDB-X、SelectDB、Doris、ClickHouse 不调用 goInception，一律按列名与表达式引用保守匹配；
- 调用存储函数 / 自定义函数的结果列、取自自定义表值函数的列，没有可追溯的来源字段，一律整列按该实例已配置字段里最严的一条规则脱敏（ClickHouse 除外），哪怕函数不碰敏感数据；SQL Server 脚本里的变量、表变量带出的敏感字段同样脱敏。规则见 [数据脱敏](#help/masking)；
- **脱敏出错时**：默认（`query.block_on_mask_error` 关闭）放行未脱敏的结果并提示“数据脱敏出错，本次结果未脱敏”；开启该项、严格模式下的业务错误、或有防火墙“强制脱敏”的列时，脱敏出错会直接拦截，不返回数据。
- **查视图**：配置在基表字段上的脱敏，经过视图改名、套表达式也会脱敏（平台会读取视图定义顺着追，见[数据脱敏](#help/masking)的 2.2 第 6 条）。读不到视图定义（账号没有查看视图定义的权限、视图加密等）同样算脱敏出错，按上一条处理；放行时提示“视图 xxx 的定义读不到…”，其余的列已经脱敏，只有经过那个视图的列可能漏。

## 审计与安全说明

| 项 | 说明 |
|---|---|
| 查询日志 | 每次成功的查询写入查询日志（`dbt_query_log`，含完整语句、实例、库、行数、耗时、是否命中脱敏）；日志写不进去时不返回结果 |
| 被拦截 / 失败 / 超时 / 已取消 | 同样记录原因，可在“执行历史”按状态过滤（需先执行 `deploy/sql/050_query_reject.sql`，没执行时只有成功记录）；被拦截的语句同时进入系统审计的“被拒绝语句” |
| 越权防护 | 先做语句检查，再做防火墙，最后做库表权限校验；没有库表权限的语句不会被拿去 EXPLAIN，探测不到表的大小 |
| 凭据 | 错误提示里不含目标库账号密码；连接失败只给出原因类别 |
| 保留 | 查询日志与拒绝记录的保留天数由系统配置 `retention.query_log_days` 等控制（默认不清理） |

## 常见错误提示与排障

| 提示 | 原因与处理 |
|---|---|
| 无权限 | 缺少库 / 表查询权限或权限已过期，点「申请权限」 |
| 你没有提交 SQL 查询的权限 | 缺少 `query.run`，联系管理员 |
| 你所在组未关联该实例 | 实例不在你的资源组里，联系管理员 |
| 不支持的查询语法类型 / 仅支持 SELECT / SHOW / EXPLAIN | 写了 DML / DDL，请改走 [SQL 上线](#help/sqlworkflow) |
| 禁止使用 `SELECT *` | 从左侧点表名自动展开字段 |
| 不允许的 T-SQL：INSERT 只能写入 DECLARE @变量 TABLE (…) 声明的表变量，不能写入表或临时表（SQL Server） | 批处理里的 `INSERT` 写了真实表、`#临时表`，或写的变量不是用 `DECLARE @t TABLE (…)` 声明的；改成表变量，要写真实表请走 [SQL 上线](#help/sqlworkflow) |
| 不允许的 T-SQL：只读查询里不允许出现 xxx / 这段批处理没有输出结果的 SELECT（SQL Server） | 批处理里夹带了 `EXEC`、`INTO`、事务控制、锁提示、对表变量的 `UPDATE` / `DELETE` 等：去掉提示里指出的语句；或只有声明和 `INSERT`、没有输出结果的 `SELECT`：最后用一条 `SELECT` 输出结果 |
| 查询超时 | 加条件 / 加索引列过滤，或改用[数据导出](#help/export) |
| 数据脱敏出错，本次结果未脱敏 | 脱敏流程异常且未开启拦截；请联系管理员检查 goInception 与脱敏配置 |
| 视图 xxx 的定义读不到或看不懂，经过这些视图的结果列可能没有脱敏 | 查询账号读不到这个视图的定义（没有查看视图定义的权限、视图加密等）且未开启拦截；请联系管理员，见[数据脱敏](#help/masking)第 8 节 |
| 命令不在只读白名单内 / 会修改数据（Redis） | 在线查询只放行固定的只读命令；改用白名单里的等价读命令（如 `KEYS` 换成 `SCAN`），写入请走 SQL 上线的 Redis 工单 |
| key 太大，超过全量读取阈值（Redis） | `HGETALL` / `SMEMBERS` 等遇到超过 5000 个元素的 key，按提示改用 `HSCAN` / `SSCAN` 分页 |
| 语句检查被拒绝（PostgreSQL / MongoDB / ClickHouse） | 错误条里是引擎给出的原因（写操作、多条语句、危险函数、敏感系统表、`$where` / `$out`、`SETTINGS` 等），按提示改成只读写法；这些类型的写入请联系 DBA |
| 同时执行的查询已达 3 条（PostgreSQL / MongoDB / ClickHouse） | 等其中一条完成或取消后再试；三种类型共用计数 |
| 连接失败 / 认证失败 / 目标库报错（HTTP 422） | 目标实例不可达、账号不对或目标库自己的报错（如 SQL Server 的权限错误），原文显示且不含密码；联系 DBA 检查账号、网络和 ClickHouse 账号 profile（`readonly=2`）。目标库错误统一用 HTTP 422 返回，避免被反向代理的 5xx 错误页替换 |

## 验证状态与已知限制

| 项 | 状态 |
|---|---|
| MySQL 8.0 在线查询、库表级权限、KILL QUERY 与超时、查询日志 | 在真实 MySQL 8.0 上冒烟验证过 |
| goInception 表解析与语法树脱敏 | 真实 goInception 未联调，开发环境用词法解析与假实现测试；上线前请核对多表连接、子查询、UNION、带库名的表 |
| SQL Server（只读批处理含表变量写入、结果集来源分析）、PostgreSQL（结果列来源反查）、ClickHouse、MongoDB | 只用假后端验证过，需要在真实实例上各查一遍（含视图、别名、`concat(phone,'')` 这类表达式）；存储函数 / 自定义函数的识别只用语句文本验证过 |
| PolarDB-X | 没有在真实实例上验证过；脱敏为保守匹配；SQL 防火墙的预估扫描行数不支持 |
| SSH 隧道 | 实例配置了隧道时经本地转发连接；不校验跳板机主机公钥，无法防御到跳板机的中间人攻击 |
| 运行全部（多结果页签） | 页面逻辑用假后端的无头浏览器测试（`scripts/frontend-test/run_batch_tabs.mjs`）验证过；每条语句走的是原来的查询接口，没有改动服务端；没有在真实的多实例线上环境里用过，多条语句同时执行时的并发表现以真实实例为准 |
| 收藏 | 需要先在元数据库执行 `deploy/sql/250_query_favorite.sql`（升级时安装脚本会自动执行）；没执行时收藏标签会提示这一点，其它功能不受影响 |
| 脱敏精度 | 保守匹配没有表级精度；表级“免脱敏名单”没有做，所有人一律脱敏 |

## 相关页面

| 想了解 | 章节 |
|---|---|
| 申请库表权限 | [权限申请与查询权限](#help/qpriv) |
| 脱敏规则怎么配 | [数据脱敏](#help/masking) |
| 语句管控规则 | [SQL 防火墙](#help/firewall) |
| 导出完整数据 | [数据导出](#help/export) |
| 变更数据 | [SQL 上线](#help/sqlworkflow) |
| 执行计划与索引建议 | [SQL 体检与智能调优](#help/optimize) |
