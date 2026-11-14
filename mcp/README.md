# dbtool MCP Server

把 dbtool 接进 Claude Code 的 MCP server，共 **23 个工具**：查询、分析、工单（检查 / 提交 / 审批 / 执行 / 终止）。
走 dbtool 的 **JSON 接口 + JWT**。**零 npm 依赖**，单文件 `index.js`，只用 Node 内置模块（Node ≥ 18）。

> 当前分步交付：**查询组、分析组共 14 个 + 工单组只读 4 个（`sql_check` / `list_workflows` / `workflow_detail` / `workflow_log`，
> 都支持批量工单；`sql_check` 另带数据导出预检）已对接**；**`submit_sql_workflow`（提交 SQL 上线工单）和 `submit_export_workflow`（提交数据导出工单）已对接，
> 都会创建真实工单，批量 = 一个工单覆盖多个实例；创建前都会自动检查（上线用 `sql_check` 的检查，导出用导出预检），不通过就不创建**；
> **`cancel_workflow`（终止工单）已对接，只能撤回自己提交的工单，必须填终止原因**；
> **2 个高危工具 `approve_workflow`（批量审核通过）/ `execute_workflow`（批量执行）已实现，只接受超级管理员账号、
> 只接受批量工单**，限制见第三节。**23 个工具全部对接完，没有占位**。

---

## 一、安装

1. 把 `index.js` 放到（Windows 建议路径）：

   ```
   C:\Users\<你的用户名>\.claude\mcp-servers\dbtool\index.js
   ```

2. 注册（注册名决定工具前缀：叫 `dbtool` 就是 `mcp__dbtool__sql_query`；注册成别的名字时，
   同时设 `DBTOOL_MCP_NODE=<那个名字>`，让 CLI 回退读对的配置节点）：

   ```bash
   claude mcp add dbtool --scope user --env DBTOOL_URL=https://<dbtool地址> --env DBTOOL_USER=你的账号 --env DBTOOL_PASS=你的密码 -- node C:/Users/你的用户名/.claude/mcp-servers/dbtool/index.js
   ```

3. 重启 Claude Code，`/mcp` 里能看到 `dbtool`。

| 环境变量 | 说明 |
|---|---|
| `DBTOOL_URL` | dbtool 地址，带不带子路径都行（如 `https://host/dbtool`），末尾斜杠会去掉 |
| `DBTOOL_USER` / `DBTOOL_PASS` | **用你自己的账号**。权限、脱敏、审计都按账号走；共用别人的账号，查询日志和工单会记在对方名下 |
| `DBTOOL_TIMEOUT_MS` | 单次请求超时，默认 120000（比 dbtool 的查询超时宽一些，避免客户端先超时） |
| `DBTOOL_PUBLIC_URL` | 返回给用户的工单链接用的地址；MCP 走内网地址、用户从外网地址打开时设它，缺省同 `DBTOOL_URL` |
| `DBTOOL_MCP_NODE` | 仅 CLI 回退用：读 `~/.claude.json` 里 `mcpServers.<这个名字>.env`，默认 `dbtool` |
| `DBTOOL_INSECURE_TLS` | 设 `1` 不校验 HTTPS 证书。内网自签证书优先用 `NODE_EXTRA_CA_CERTS` 信任 CA，实在不行才用它 |

账号要求：

- 在 dbtool 的 **API 白名单**（系统设置 `security.api_user_ids`，为空 = 不限制）里；
- 如果管理员开了 `security.api_jwt_require_otp`，JWT 登录要动态口令，这个 MCP 没有处理 OTP，需要关掉或换账号；
- 有对应功能的按钮权限（见下表“需要的权限”）。

登录失败有限流和锁定（默认 5 次 / 300 秒）。MCP 对同一次登录只发一次请求，令牌快到期（不足 60 秒）时主动续登，
收到 401 再续登一次重试，不会循环重登。令牌有效期由 `security.jwt_hours` 决定（默认 4 小时）。

---

## 二、先用 CLI 自测

```bash
node C:/Users/你的用户名/.claude/mcp-servers/dbtool/index.js --cli instances
```

能列出实例就说明账号通了。不带参数跑 `--cli` 会打印全部用法。

仓库里的 `test.js` 是不依赖真实环境的自测（本地起一个模拟的 dbtool，再用 stdio 拉起 `index.js` 逐个调用工具）：

```bash
node --test test.js
```

---

## 三、工具清单（23 个）

### 查询组、分析组（已对接，14 个，全部只读）

| 工具 | dbtool 接口 | 需要的权限 | 说明 |
|---|---|---|---|
| `list_instances` | `GET /api/v1/query/instances` | 能用在线查询 | 返回角色、环境；其它工具的 `instance_name` 传这里的全名，也可传数字 id |
| `list_databases` / `list_tables` / `list_columns` | `GET /api/v1/query/instances/:id/databases[/:db/tables[/:table/columns]]` | 同上 | PostgreSQL / SQL Server 的表名是 `架构.表名` |
| `describe_table` | `GET …/databases/:db/object-info?kind&name&schema` | 按钮 `query.objinfo` | 返回列信息 + 创建语句；可用 `kind` 看视图 / 过程 / 函数 / 触发器 / 事件 |
| `dict_table_info` | 同上 | 同上 | 返回对象详情里的全部小节（数据字典就是对象详情）；`db_type` 参数忽略 |
| `sql_query` | `POST /api/v1/query/execute` | 按钮 `query.run` | 平台可能禁用 `SELECT *`；命中防火墙“要求原因”时传 `reason`；`is_masked` 由 `masked_columns` 推出 |
| `query_log` | `GET /api/v1/query/history` | 能用在线查询 | 普通账号只看到自己的，管理员账号看到所有人的 |
| `slow_log` / `slow_log_detail` | `GET /api/v1/slowlog/slowlogs`、`/slowlogs/detail` | 按钮 `slowlogs.view` 或 `slowquery.view` | 可加 `db_name` |
| `partman_list` | `GET /api/v1/partman/configs` | 按钮 `partman.view` | — |
| `sql_explain` | 走 `POST /api/v1/query/execute`（自动补 `explain`） | 按钮 `query.run` | 同时返回平台的执行计划风险提示 `plan_tips` |
| `sql_optimize` | `soar` / `sqltuning`：`POST /api/v1/sqlopt/optimize` → 轮询 `GET /sqlopt/jobs/:id`；`sqladvisor` / `advise`：`POST /api/v1/sqlopt/advise` | `soar` 要按钮 `optimize.soar`、`sqltuning` 要 `optimize.tuning`；`sqladvisor` / `advise` 要按钮 `optimize.explain` | `sqladvisor` 用 dbtool 内置建议；`sqltuningadvisor`（SQL Server Tuning Advisor）dbtool 没有，会提示；soar / sqltuning 仅 MySQL 系实例 |
| `sql_analyze` | `POST /api/v1/sqlopt/analyze` → 轮询任务 | 按钮 `sqlanalyze.run` | 可选带 `instance_name` + `db_name` |

结果集输出：元信息按 JSON 展开，行一行一条；超过 25000 字符时从尾部丢行并写明 `rows_omitted`，输出仍是合法 JSON。

### 工单组（只读，已对接 4 个，都支持批量工单）

| 工具 | dbtool 接口 | 需要的权限 | 说明 |
|---|---|---|---|
| `sql_check` | `POST /api/v1/sqlworkflow/check` | 单实例要按钮 `sqlworkflow.submit`；批量要 `sqlworkflow.submit_batch`；账号所在组要关联这些实例；**数据导出预检**要 `export.submit` / `export.submit_batch` | 不创建工单。返回 `verdict`（通过 / 有警告 / 不通过）和逐条结果。**批量：`instance_names` 传 ≥ 2 个实例，一次请求带全部 `instance_ids`**，语法与规则只在第一个实例上检查，另外预检每个实例能否连接、库是否存在（`instance_checks`），有实例连不上整体算“不通过”。加 `export: true` 就是**数据导出预检**，见下面。`submit_sql_workflow` / `submit_export_workflow` 创建前都会自动跑同样的检查（不通过就不创建工单），单独用它是为了提前看结果 |
| `list_workflows` | `GET /api/v1/sqlworkflow` | 看到的是自己有权限的工单：管理员 / 审计员看全部，有审批或组内执行权限的看所在资源组，其他人只看自己的 | `kind`：`sql`（默认，对应 dbtool 的 `change`）/ `export` / `all`；`batch`：`1` 只看批量、`0` 只看单实例；`status`（也认 `navStatus`）、`tab`（我提交的 / 待我审批 / 待我执行）、`search`（纯数字按工单号）、`instance_id`、`group_id`、起止日期（只给一端会自动补另一端）。dbtool 每页固定 20 条，`limit` 超过 20 时自动连翻几页（最多 100 条）。批量工单的 `instance` 是“第一个实例名 等 N 个实例”，并带 `batch_count`；每行带 `detail_url` |
| `workflow_detail` | `GET /api/v1/sqlworkflow/:id` | 同上 | 状态、审批流与当前审批人、检查结果（`review_rows`）、执行结果（`exec_rows`）。**批量工单带 `batch`：总数、成功 / 失败 / 已关闭 / 待执行个数，以及每个实例各自的结果**；检查结果只来自第一个实例。结果行太多时自动减行，优先保留汇总行和出错的行；`max_rows` 可调 |
| `workflow_log` | 同上（取详情里的 `logs`） | 同上 | 操作日志：谁在何时提交 / 审核 / 执行 / 终止；批量工单是一个工单一份日志 |

工单链接是 `<DBTOOL_PUBLIC_URL 或 DBTOOL_URL>/sqlworkflow/<工单号>`。

#### 数据导出预检（`sql_check` 加 `export: true`）

dbtool 对导出工单的检查有错误时会**建出一张“自动审批不通过”的工单**（没有进入审批，但工单记录在那里）。预检用的是和提交时同一套检查，
`submit_export_workflow` 在创建前会**自动**跑一次（不通过就不创建，见下一节）；这里的 `sql_check` 是让你提前单独看结果、行数，或把结果给用户看。
参数：`export: true`，批量导出可再加 `export_merge: true`（合并成一个文件时，还要核对各实例返回的列是否一致）；
`instance_name` / `instance_names` / `db_name` / `sql_content` 和上线检查一样，`sql_content` 写导出用的那条 select。

- **服务端查什么**（`POST /api/v1/sqlworkflow/check` 带 `export` / `export_merge`）：是不是单条只读查询（`select` / `with`，不能加锁读取或写出文件）、
  待导出行数是否超过上限（系统设置 `export.max_rows`，默认 10000；SQL 防火墙可以把上限调得更小）、你对语句里的库表有没有查询权限、
  SQL 防火墙是否拒绝；**批量时每个实例各检查一次**，`export_merge` 时还会核对各实例返回的列是否一致。账号要有 `export.submit`（批量要 `export.submit_batch`），没有就是 403，原样转出。
- **MCP 本地先挡的**（不通过时不发检查请求，和 `submit_export_workflow` 是同一份检查，预检过了提交时这一关也过得去）：`export` / `export_merge`
  必须是明确的 `true` / `false`；`export_merge` 只能和 `export: true` 一起用、只能用于批量（去重后至少 2 个实例）；SQL 必须以 `select` / `with` 开头
  （SQL Server 另外允许 `declare` / `set` 开头的只读批处理）。
- **输出**：`verdict`（通过 / 有警告 / 不通过）、`mode`（单实例导出 / 批量导出，写明合并成一个文件还是每个实例各一个文件）、逐行结果。
  每行的 **`rows_to_export` 是待导出行数**（0 行也会写出来；出错又没有行数的行不写）；批量导出且没有出错时给 `rows_to_export_total`（各实例之和，合并成一个文件时就是文件里的行数）；
  批量还有 `instance_checks`（每个实例的结果，出错的实例能直接看到原因）；`next` 告诉你下一步：通过了可以用 `submit_export_workflow` 提交，
  不通过则说明提交会被拦下（提交前的自动预检不通过就不创建工单）。
- **副作用**：不创建工单，但**会在每个实例上执行一次只读的 `select count(*)` 统计待导出行数**（有超时），所以大表请把过滤条件写好。
  T-SQL 的 `declare` / `set` 批处理没法统计行数，只给一行警告，到执行时才按上限兜底。
- **预检不替你做的**：资源组、实例是否在该组的“可导出”清单里、导出格式、能否合并（`export.can_merge`）这几项是提交时按提交页选项（`GET /sqlworkflow/options`）校验的，
  预检不涉及（它们不通过时提交会直接报错、什么都不创建，不会留下工单）；合并成 xlsx 时合计行数不能超过 Excel 的上限（1048576 行），也是提交时才核对。

CLI：`node index.js --cli checkexport <实例[,实例…]> <库> <select 语句> [merge]`（逗号分隔多个实例 = 批量导出预检；`merge` = 核对各实例的列是否一致）。

### 提交 SQL 上线工单（已对接，**会创建真实工单并通知审核人**）

| 工具 | dbtool 接口 | 需要的权限 | 说明 |
|---|---|---|---|
| `submit_sql_workflow` | `GET /api/v1/sqlworkflow/options`（校验）→ `POST /api/v1/sqlworkflow` | 单实例要按钮 `sqlworkflow.submit`；批量要 `sqlworkflow.submit_batch`；账号要在所选资源组内 | 参数：`workflow_name`（≤ 50 字符）、`group_name`、`instance_name`（单实例）或 `instance_names`（批量，≥ 2 个）、`db_name`、`sql_content`，可选 `demand_url`、`is_backup`（缺省 true）、`run_date_start` / `run_date_end`（`YYYY-MM-DD HH:MM[:SS]`）。**批量 = 一次请求带全部 `instance_ids`，创建一个批量工单**；第一个实例是主实例 |

**创建前会自动检查**：选项校验通过后、发创建请求之前，`submit_sql_workflow` 自己先发一次和 `sql_check`（上线检查）同样的检查，
**检查不通过、或者没能完成（接口出错、连接断开、超时），都不发创建请求**——否则 dbtool 在检查有错误时会建出一张“自动审批不通过”的工单。
批量时检查里还包含“每个实例能否连接、库是否存在”，有实例连不上也是不通过。没有跳过检查的开关（传了 `skip_check` 之类的参数会被忽略）。
想先看检查结果、或把结果给用户看，可以单独用 `sql_check`。

调用顺序与保护：

1. **本地校验**（不通过时连登录都不发）：必填、工单名 ≤ 50 个字符、库名 ≤ 64 个字符、SQL 不为空、`demand_url` 要 `http(s)://` 开头且 ≤ 500 个字符、时间格式和先后顺序；
2. **解析实例**：`instance_names` 去重，名字对不上就报错并列出相近的；去重后 ≥ 2 个才算批量，只剩 1 个按单实例处理；
3. **按 dbtool 提交页的选项校验**（`GET /api/v1/sqlworkflow/options`），任何一条不满足就整体报错、**什么都不创建**：
   - 资源组存在且你在组内（大小写不同也认，请求里用 dbtool 里的规范写法；报错时列出你能用的资源组）；
   - 每个实例都在该资源组的“可上线”清单里（要带“上线”标签并关联到该组；报错时列出不在组里的实例和该组可上线的实例）；
   - 批量：有批量权限、每个实例的类型都在允许批量的类型里（目前只有 SQL Server）、类型一致、数量不超过上限（默认 100）；
4. **自动检查**（`POST /api/v1/sqlworkflow/check`，批量一次请求带全部实例；goInception 审核规则，没配置 goInception 时只做基础检查）：
   - **不通过**：不创建工单，报错里按序号列出原因（最多 8 条，每条最多 300 字，同一个原因只列一次）。语句上的问题写成“第 N 条：原因（语句开头 80 字）”，
     一批语句里能看出是哪一条；批量时有实例连不上、库不存在，写成“实例名：原因”；只有警告的行不算原因；
   - **没能完成**（检查接口报错、连接断开、超时）：同样不创建工单，**不重试**，把错误原样告诉你；超时时会提示检查可能很慢，可以调大 `DBTOOL_TIMEOUT_MS`
     （批量实例多时，要逐个确认能连接，要等一阵）；
   - **通过 / 只有警告**：继续创建。结果里写 `提交前检查: 通过`，批量还会说明“N 个实例都能连接、库存在；语法与规则只在第一个实例上检查”；有警告时原文列在下面，
     没配置 goInception 时 dbtool 的提示（“当前只做了基础检查，不校验语法、表结构与影响行数……”）也在这里；
5. **提交**：只发一次请求。dbtool 服务端会再做一遍检查（语法与规则、DDL / DML 要选名字里带 DDL / DML 的资源组、批量时每个实例能否连接、库是否存在……），
   被服务端拒绝时原样转出错误，不重试；
6. **结果**：工单号、状态、dbtool 的提示、检查结论、当前审批人和工单链接。两种要留意：
   - 状态是“自动审批不通过”：检查刚通过，这里又不通过——只有警告时，多半是管理员把自动驳回设成了“有警告也驳回”（系统设置 `review.auto_reject_level=warning`）；
     检查是通过的，多半是检查之后表结构或数据变了。**工单已创建但没有进入审批**，原因在链接里，改好 SQL 重新提交；
   - 状态是“审核通过”：这张工单**无需审批**（审批流为空），现在就可以执行；
7. **请求发出去了却没收到响应**（超时、连接被断）：工单可能已经创建，会提示先用 `list_workflows`（`search` 填工单名）核对，**不会自动重试**，避免重复创建。

两点要知道：

- **检查不通过一律不创建，包括管理员本来允许“带错误也提交”的情形**：系统设置 `review.auto_reject_level=never` 时，带错误的工单不会被自动驳回、而是交给审批人决定；
  MCP 的自动检查仍会拦下（导出工单的预检同理）。要这样提交，请到网页上操作。
- 自动检查和创建之间有一段空隙，表结构、数据、权限可能在这期间变化，所以仍可能出现上面说的“自动审批不通过”，结果里会说明。

不提供：跳过检查（`skip_check`）、定时执行。数据导出见下一节的 `submit_export_workflow`。工具描述里要求调用前把工单名、资源组、目标实例和 SQL 给用户看、得到明确同意。

CLI：`node index.js --cli submit <资源组> <实例[,实例…]> <库> <工单名> <SQL>`（逗号分隔多个实例 = 批量工单，只创建一个；创建前同样会自动检查，不通过退出码为 1）。

### 提交数据导出工单（已对接，**会创建真实工单并通知审核人**）

| 工具 | dbtool 接口 | 需要的权限 | 说明 |
|---|---|---|---|
| `submit_export_workflow` | `GET /api/v1/sqlworkflow/options`（校验）→ `POST /api/v1/sqlworkflow`（带 `export: true`） | 单实例要 `export.submit`；批量要 `export.submit_batch`；账号要在所选资源组内，并有实例的库 / 表查询权限 | 参数：`workflow_name`（≤ 50 字符）、`group_name`、`instance_name`（单实例）或 `instance_names`（批量，**至少 2 个不同的实例**）、`db_name`、`sql_content`（一条 select / with 查询），可选 `export_format`（`csv` 缺省 / `xlsx` / `json` / `xml` / `sql`）、`export_merge`（仅批量：各实例的数据合并成一个文件，缺省每个实例各一个文件）、`demand_url`、`run_date_start` / `run_date_end`。**批量 = 一次请求带全部 `instance_ids`，创建一个批量导出工单**；第一个实例是主实例 |

**创建前会自动预检**：选项校验通过后、发创建请求之前，`submit_export_workflow` 自己先发一次和 `sql_check`（`export: true`）同样的导出预检，
**预检不通过、或者没能完成（接口出错、连接断开、超时），都不发创建请求**——否则 dbtool 在导出检查有错误时会建出一张“自动审批不通过”的工单。
没有跳过预检的开关（传了 `skip_precheck` 之类的参数也会被忽略）。想先看行数、或想把预检结果给用户看，可以单独用 `sql_check`，见上一节。
和上线工单一样，管理员把 `review.auto_reject_level` 设成 `never`（本来允许带错误提交、交给审批人决定）时，这里仍会拦下，要这样提交请到网页上操作。

调用顺序与保护：

1. **本地校验**（不通过时连登录都不发）：和上线工单一样的必填、长度、链接、时间格式检查；另外 `export_format` 必须是五种之一，
   `export_merge` 只能是 `true` / `false`（认不出来的值直接报错，不会悄悄当成“不合并”）；
2. **解析实例**：同上线工单（`instance_names` 去重，去重后 ≥ 2 个才算批量；只剩 1 个按单实例处理，返回里会写明）；
3. **SQL 与合并的本地检查**（解析实例之后、读选项之前）：SQL 必须以 `select` / `with` 开头（先去掉开头的空白和注释；MySQL 系还认 `#` 注释；
   SQL Server 另外允许 `declare` / `set` 开头的只读批处理），`export_merge` 只能用于批量。这里只挡明显不对的，
   “单条、只读、行数上限、库表查询权限、SQL 防火墙”由 dbtool 服务端检查；
4. **按 dbtool 提交页的选项校验**（`GET /api/v1/sqlworkflow/options`），任何一条不满足就整体报错、**什么都不创建**：
   - 资源组存在且你在组内；
   - 导出权限：单实例看 `export.allowed`，批量看 `export.batch_allowed`；
   - 每个实例都在该资源组的**“可导出”清单**（`export_instances`）里——实例要开启“查询”、你要有它的库 / 表查询权限，并关联到该组。
     这和上线用的清单不是一回事：能上线的实例不一定能导出，报错时会列出该组可导出的实例；
   - 批量：每个实例的类型都在允许批量的类型里（目前只有 SQL Server）、类型一致、数量不超过上限（默认 100）；
   - 导出格式在 `export.formats` 里；
   - 合并成一个文件：`export.can_merge` 为 `true`（要有批量导出权限，且管理员已经更新过 `dbt_sql_export_ext` 表）；
5. **自动预检**（`POST /api/v1/sqlworkflow/check`，带 `export` / `export_merge`；批量一次请求带全部实例，每个实例各统计一次待导出行数）：
   - **不通过**：不创建工单，报错里按序号列出原因（最多 8 条，每条最多 300 字，同一个原因只列一次），例如行数超过上限、没有库表查询权限、被防火墙拒绝、
     查询执行失败、合并时各实例的列不一致；
   - **没能完成**（检查接口报错、连接断开、超时）：同样不创建工单，**不重试**，把错误原样告诉你；超时时会提示统计行数的查询可能很慢，
     可以缩小范围或调大 `DBTOOL_TIMEOUT_MS`（批量实例多、表又大时，预检要等每个实例统计完）；
   - **通过 / 只有警告**：继续创建。结果里写 `提交前预检: 通过（待导出 N 行）`，批量写 `N 个实例共 M 行待导出`；有警告（比如 T-SQL 批处理统计不了行数、防火墙的提示）时原文列在下面；
   - 代价：预检在每个实例上执行一次 `select count(*)`，创建时服务端会再统计一次，所以大表请把过滤条件写好；
6. **提交**：只发一次请求，被服务端拒绝时原样转出错误，不重试。服务端会再检查一遍；
7. **结果**：工单号、状态、dbtool 的提示、导出格式（批量时写明“合并成一个文件”还是“每个实例各导出一个文件”）、预检结论、当前审批人和工单链接。几种状态：
   - “等待审批”（正常）：审核人批准后，还要有执行权限的人执行导出才会生成文件（`execute_workflow` 对批量导出工单同样适用）；
   - “自动审批不通过”：预检刚通过，这里又不通过，多半是**预检之后数据或权限变了**（行数增长、权限被收回……），**工单已创建但没有进入审批**，
     原因在工单链接或 `workflow_detail` 的 `review_rows` 里，改好后重新提交；
   - “审核通过”：这张工单无需审批，只差执行导出；
8. **请求发出去了却没收到响应**（超时、连接被断）：工单可能已经创建，会提示先用 `list_workflows`（`kind` 填 `export`，`search` 填工单名）核对，**不会自动重试**。

不提供：**导出原值（不脱敏）**——dbtool 只允许超级用户选，并要写理由，MCP 不发这个字段，是否脱敏一律按平台规则（返回里会写明）；
跳过检查；定时导出。工具描述里要求调用前把工单名、资源组、目标实例、库、SQL 和导出格式给用户看、得到明确同意。

CLI：`node index.js --cli export <资源组> <实例[,实例…]> <库> <工单名> <select 语句> [csv|xlsx|json|xml|sql] [merge]`
（逗号分隔多个实例 = 批量导出，只创建一个工单；最后的 `merge` = 合并成一个文件）。

### 终止（撤回）工单（已对接，**会终止工单，不能恢复**）

| 工具 | dbtool 接口 | 需要的权限 | 说明 |
|---|---|---|---|
| `cancel_workflow` | `GET /api/v1/sqlworkflow/:id`（读详情）→ `GET /api/v1/sqlworkflow?q=<工单号>`（读提交人）→ `POST /api/v1/sqlworkflow/:id/cancel`，body `{remark}` | 按钮 `sqlworkflow.cancel_own`（撤回自己提交的；有 `sqlworkflow.reject` 也行） | 参数：`workflow_id`、`remark`（终止原因，**必填**，最多 500 个字符）。成功后工单状态变成“人工终止流程”，通知相关人，定时执行的计划一并清除 |

限制全部在 MCP 里先做，被拒绝时**不会发出终止请求**（为了读工单要登录、读详情和列表，这些都是只读的），依次检查：

1. **终止原因**：必填，空的或只有空白直接拒绝。这一步在登录之前，不通过时连登录请求都不发；
2. **状态**：只接受“等待审批 / 审核通过 / 定时执行”（dbtool 允许终止的全部状态）；排队中、执行中、已结束、已终止、自动审批不通过的都拒绝，
   取消正在执行的也不在这里。状态不对时连提交人都不去查；
3. **只能撤回自己提交的**：提交人的登录名只在工单列表里有（详情里的 `engineer` 是显示名，不能拿来比），所以到列表里按工单号找那一行；
   找不到、提交人为空、或登录名和当前账号不同（不区分大小写），一律拒绝，提示“别人的工单请审批人到网页上驳回”。超级管理员也一样；
4. **dbtool 自己的判断**：详情里的 `can_cancel` 是 dbtool 算好的“这个账号能不能终止这张工单”（页面上按钮显示用的也是它），为 `false`
   （比如账号没有终止按钮权限）也拒绝。

上线 / 导出、单实例 / 批量、三种状态都可以：终止的是整张工单（批量工单是一个工单，不会拆开）。

终止原因会加上“（经 MCP 操作）”标记，平台的操作日志里显示为 `审核取消：取消原因：<原因>（经 MCP 操作）`（审批备注同理）；原因再长，标记也不会被截掉——
原因最多留 490 个字符，超出的截断并以 `…` 结尾，加上标记正好 500 个字符。

只发一次请求，被服务端拒绝时原样转出错误，不重试，也不会谎报成功；请求发出去了却没收到响应（超时、连接被断）时，工单可能已经被终止，
会提示先用 `workflow_detail` 核对，不要直接重试。响应里的状态不是“人工终止流程”、或没有状态时，结果里会提醒你到链接核对。

按你的选择，**不要求再传一次工单号确认**（`approve_workflow` / `execute_workflow` 要）。

没有做的：取消正在执行（`POST /sqlworkflow/:id/cancel-run`）、取消定时计划（`/schedule/cancel`，工单回到“审核通过”）、标记执行有异常（`/mark-exception`）、
驳回别人的工单（dbtool 里那是审批人的动作）。

CLI：`node index.js --cli cancel <工单号> <终止原因>`。

### 批量工单（SQL Server）的设计要求

dbtool 的批量上线 / 批量导出是**一个工单覆盖多个实例**（同一段 SQL 依次在各实例上执行），不是一个实例一个工单。
`sql_check`、`submit_sql_workflow`、`submit_export_workflow` 都已按这个方式对接，没有“为每个实例循环提交一个工单”的回退：

| 约束 | 来源 |
|---|---|
| 请求里用 `instance_ids`（数组）；`instance_names` 传 ≥ 2 个实例名，内部映射成 id | `POST /api/v1/sqlworkflow`、`/sqlworkflow/check` |
| 只有 SQL Server 允许批量（服务端 `BATCH_DB_TYPES`，默认 `mssql`），所有实例必须同一类型、同属所选资源组 | `GET /api/v1/sqlworkflow/options` 的 `batch.types` |
| 一个批量工单最多 100 个实例；批量导出至少 2 个 | `batch.max`、服务端校验 |
| 需要按钮 `sqlworkflow.submit_batch`（批量上线）/ `export.submit_batch`（批量导出） | 服务端校验 |
| 批量不支持“跳过检查”；检查的语法与规则只在**第一个**实例上做，另外预检每个实例能否连接、库是否存在 | `PreviewBatch` |
| 批量导出可选 `export_merge`：各实例数据合并进同一个文件（要求各实例返回的列一致，由服务端在提交时核对） | `options.export.can_merge` |
| 批量工单详情里有每个实例各自的结果；失败实例的“重试 / 关闭”是高危操作，不开放给 MCP | `GET /api/v1/sqlworkflow/:id` 的 `batch_instances` |

提交前先用 `GET /api/v1/sqlworkflow/options` 校验资源组、实例归属、批量类型和数量，不满足就整体报错，什么都不创建（两个提交工具都已这样做）。

### 批量审核通过 / 批量执行（高危，仅超级管理员）

审批和执行是高危操作（“须人工审批后才执行”是平台的安全边界）。这两个工具的限制都在 MCP 里先做，
**被拒绝时不会对工单发出任何请求**（唯一的请求是为了核对身份而做的一次重新登录）：

| 工具 | dbtool 接口 | 说明 |
|---|---|---|
| `approve_workflow` | `POST /api/v1/sqlworkflow/:id/pass`，body `{remark}` | 审核通过**当前这一级**审批（多级审批要再调用一次；返回里会说明还有没有后续审批、当前审批人是谁）。备注自动加上“（经 MCP 操作）”，审批记录里能看出这一步是经 MCP 做的；备注再长，标记也不会被截掉（备注最多留 490 个字符） |
| `execute_workflow` | `POST /api/v1/sqlworkflow/:id/execute`，body `{mode: "auto", executor?}` | 立即执行整张批量工单（所有实例，后台异步执行，用 `workflow_detail` 看每个实例的结果）。`executor` 不传就用 dbtool 的缺省（SQL Server 只有原生执行） |

两个工具共同的限制，依次检查（`approve_workflow` 的“提交人是自己”那一条排在第 4、5 条之间），任何一条不满足就直接拒绝：

1. **工单号与确认**：必须再传一次 `confirm_workflow_id`，且等于 `workflow_id`（防止模型误调）。这一步在登录之前，不通过时连登录请求都不发；
2. **超级管理员**：每次调用都**重新登录**取新令牌，解码令牌载荷里的超级用户标记 `su`，不是 `true` 就拒绝。
   不能用旧令牌：`su` 是登录时的快照，最长保留 `security.jwt_hours`（默认 4 小时），期间账号被降级也会被旧令牌放过；
3. **只接受批量工单**：先读工单详情，`is_batch` 不是 `true`（单实例工单）就拒绝，提示到网页上操作；
4. **状态**：`approve_workflow` 只接受“等待审批”；`execute_workflow` 只接受“审核通过”（定时执行的工单不接受，要到网页上处理）；
5. **账号在这张工单上有没有权限**：读详情里 dbtool 自己算好的 `can_review` / `can_execute`（和页面上按钮显示用的是同一个判断），为 `false` 就拒绝；

`approve_workflow` 另有一条：**提交人是当前账号自己的工单，一律拒绝审核通过**（超级管理员也一样）。
提交人的登录名只在工单列表里有（详情里的 `engineer` 是显示名，不能拿来比），所以会到列表里按工单号找那一行；
找不到、提交人为空、或登录名相同（不区分大小写），都拒绝。

为什么要加这一条：dbtool 自己的“禁止自审”（`approval.forbid_self_approve`，默认开）对**超级管理员不生效**——只有权限申请、归档、
数据导出这几类工单对超级管理员也禁止自审，普通的 SQL 上线工单（含批量上线）超级管理员可以自己审批。没有这条，
同一个超级管理员账号配上 MCP，提交、审批、执行可以全由 AI 走完，没有第二个人。

dbtool 服务端仍会按自己的规则再校验一遍（按钮权限 `sqlworkflow.approve` / `execute_group` / `execute_own`、审批流当前级、
可执行时间窗、同库 DDL 冲突等），被服务端拒绝时原样转出错误，只发一次，不重试，也不会谎报成功。

没有做的：定时执行、手工确认结束、失败实例的重试 / 关闭、驳回别人的工单（撤回自己提交的见上一节的 `cancel_workflow`）。

需要知道的三点：

- **`execute_workflow` 没有“提交人不能是自己”的限制**，只有 `approve_workflow` 有（按你的要求）。也就是说，超级管理员自己提交的批量工单，
  只要另一位审批人通过了，这个账号可以用 MCP 执行它；
- 批量**导出**工单也是批量工单，这两个工具同样适用：审核通过后 `execute_workflow` 会在各实例上执行导出查询并生成文件
  （导出只有一种执行方式，`executor` 不用传）。dbtool 对数据导出工单在超级管理员身上也禁止自审，所以超级管理员自己提交的批量导出工单，
  同一个账号审核通过会被 MCP（以及 dbtool）拒绝，要另一位审批人处理；
- 批量导出工单如果在网页上选了“导出原值（不脱敏）”，审核通过 / 执行的返回里会多一行 `⚠ 这是“导出原值（不脱敏）”的导出工单`，提醒调用方注意；MCP 不会因此拒绝。
  （`submit_export_workflow` 自己不提供这个选项。）

CLI 用法（同样的限制）：`node index.js --cli approve <工单号> <再确认一次的工单号> [备注]`、`node index.js --cli execute <工单号> <再确认一次的工单号> [goinception|native]`。

---

## 四、安全边界

1. **平台侧**：权限、库表授权、只读会话、防火墙、数据脱敏、审计都由 dbtool 控制；MCP 拿到的是脱敏后的结果。
2. **客户端侧**：`sql_query` 只放行 `select` / `show` / `explain` / `desc` / `with` 开头的语句（先去掉开头的空白和注释）。
3. 工单组：只读的 4 个工具不创建也不改动任何工单（`sql_check` 的数据导出预检会在每个实例上执行一次只读的 `select count(*)` 统计行数，有超时）。
   `submit_sql_workflow` / `submit_export_workflow` 会**创建真实工单并通知审核人**，但只是创建待审批工单，
   真正执行在审核人批准之后；提交前按 dbtool 的提交页选项校验、批量只创建一个工单、不自动重试、不提供跳过检查，
   两个提交工具创建前都会自动检查一次（上线用 `sql_check` 的检查，导出用导出预检），不通过或没能完成都不创建，没有跳过的开关；
   导出工单也不提供“导出原值（不脱敏）”和定时导出；返回的工单链接必须原样转给用户核对。
   `approve_workflow` / `execute_workflow` 是高危操作：只接受超级管理员账号（每次重新登录核对）、只接受批量工单、必须再传一次工单号确认，
   审核通过还拒绝提交人是自己的工单（见上文）。这些工具的描述里都要求调用前先把工单内容给用户看并得到明确同意。
   注意：同一个超级管理员账号用 MCP 能提交、审核通过（他人提交的）和执行；提交人是自己的工单审核通过被拒绝，但 dbtool 的“无需审批”工单会直接进入“审核通过”，
   这种工单提交后就能被 `execute_workflow` 执行（见上文提交结果里的提醒）。
   `cancel_workflow` 会终止工单（不能恢复）：只能撤回自己提交的工单（超级管理员也一样，别人的工单是审批人的“驳回”，请到网页上做）、
   只接受等待审批 / 审核通过 / 定时执行的工单、必须填终止原因；不要求再传一次工单号确认。
4. 账号密码只从环境变量（或 CLI 回退的 `~/.claude.json` 本节点 env）读取，不写日志；所有错误信息出口都会抹掉密码和令牌。
5. JWT 由 dbtool 的 `SECRET_KEY` 签发，这把密钥不要外泄。

---

## 五、验证状态

- **对真实 dbtool 服务验证过**（本地 demo 服务）：登录、实例 / 库 / 表 / 字段、`sql_query`、`sql_explain`、`query_log`、
  `SELECT *` 被拒的提示、写语句被客户端拦下、实例名无法解析时的提示、stdio 协议（initialize / tools/list / tools/call）；
  **单实例**的 `sql_check`、`list_workflows`、`workflow_detail`、`workflow_log`；`sql_check` 对 MySQL 实例做批量时，
  服务端返回“批量上线只支持：mssql”，MCP 原样转出。
  `approve_workflow` / `execute_workflow` 的拒绝路径也对着真实 demo 服务验证过：非超级管理员账号（`dev`）被拒，说明真实令牌里
  确实带 `su` 且能解出来；超级管理员账号（`admin`）通过身份校验后，因为工单是单实例而被拒；确认的工单号不一致被拒；
  以上都没有改动工单（状态仍是“等待审批”）。详情里的 `can_review` / `can_execute` / `is_batch` 字段、列表里“登录名 `engineer`
  和显示名 `engineer_display` 分开”这一点，也都在真实服务的返回里确认过。
  `submit_sql_workflow` 的**单实例**提交对着真实 demo 服务验证过（工单创建成功，状态、当前审批人、工单链接都对）；提交前校验的几种拒绝
  也对着真实服务验证过：批量实例类型不符、资源组写错、实例不在资源组里、工单名超长，被拒绝时都没有创建工单（工单列表里始终只有那一张）。
  `submit_export_workflow` 的**单实例**导出对着真实 demo 服务验证过：创建成功（状态“等待审批”、当前审批人、工单链接都对；
  `list_workflows` 用 `kind=export` 能看到，`workflow_detail` 里有导出格式和统计出的行数）；`instance_names` 里两个名字其实是同一个实例时去重后按单实例提交；
  检查必败的导出（表不存在）得到一张“自动审批不通过”的工单，原因在 `workflow_detail` 的 `review_rows` 里；
  拒绝路径也验证过，被拒绝时都没有创建工单：没有导出权限的账号（当时的 `dev`，真实选项里 `export.allowed` 为 `false`；1.80 起演示账号按预设角色授权，`dev` 是 RD 预设、已有导出权限，现在要重现请换一个没授予 `export.submit` 的账号）、批量类型不符
  （“批量导出只支持：mssql”）、单实例要合并、SQL 不是 select、格式写错、资源组写错。
  批量导出的请求体（`instance_ids` + `export` + `export_merge`）直接发给真实服务，它按“批量导出”处理（对 MySQL 实例回复“批量导出只支持：mssql”），
  说明字段名对得上。
  `sql_check` 的**数据导出预检**对着真实 demo 服务验证过（单实例）：通过的；检查必败的（表不存在：得到“不通过”和服务端的原话 `Table 'shop.…' doesn't exist`，
  这条 SQL 之前真实提交时建出的就是“自动审批不通过”的工单）；没有导出权限的账号（当时的 `dev`，403 原样转出；同上）；批量对 MySQL 实例（服务端按类型拒绝）。
  本地拒绝的几种（不是 select、单实例要合并）都没有发出检查请求；预检做完，导出工单列表里仍然是空的；不带 `export` 的上线检查输出没有变化。
  **`submit_export_workflow` 创建前的自动预检**也对着真实 demo 服务验证过：通过的导出，服务端访问日志里的顺序是 `options` → `check` → 创建 → 读详情，结果里有
  `提交前预检: 通过（待导出 N 行）`；检查必败的导出（表不存在，也就是之前真实提交时建出“自动审批不通过”工单的那条 SQL），访问日志里只有 `options` → `check`，
  **没有创建请求**，报错里是服务端的原话，导出工单列表里没有它；把 `skip_precheck` / `skip_check` 之类的参数一起传进去也一样被拦下（这类参数被忽略）。
  demo 的数据库引擎是模拟的，对任何统计都返回同一个行数，所以**0 行、超过行数上限、库表无权限、SQL 防火墙拒绝、合并时列不一致**这几种结果只用模拟服务验证过
  （`rows_to_export` 的取法按服务端源码：`affected_rows` 总会输出，没统计时是 0，所以要排除出错的行和 T-SQL 批处理那一行）。
  **`submit_sql_workflow` 创建前的自动检查**同样对着真实 demo 服务验证过（demo 没配置 goInception，用的是基础检查）：把 SELECT 当上线工单提交（基础检查会驳回），
  报错是“第 1 条：仅支持DML和DDL语句……（select 1 as n）”，一批语句里夹着一条 SELECT 时指出是第几条，**没有创建工单**，上线工单列表里没有它，工单号也没有被占掉；
  通过的上线工单，服务端访问日志里的顺序是 `options` → `check` → 创建 → 读详情，结果里有 `提交前检查: 通过` 和 dbtool 的“未配置 goInception”提示；
  批量对 MySQL 实例在选项校验就被拦下，访问日志里没有 `check`；把 `skip_check` / `skip_precheck` 一起传进去也一样被拦下。
  goInception 的审核规则、批量时有实例连不上或库不存在、检查接口超时，这几种只用模拟服务验证过。
  **`cancel_workflow`** 对着真实 demo 服务验证过：提交人撤回自己的上线工单（`dev`，“等待审批”）、超级管理员撤回自己的上线工单和导出工单（`admin`）、
  “审核通过”（lead、dba 两级审批通过后）的工单被提交人撤回，状态都变成“人工终止流程”，操作日志里是“审核取消：取消原因：<原因>（经 MCP 操作）”；
  被拒绝的几种都没有发出终止请求（服务端访问日志里只有那 4 次终止）：超级管理员 `admin` 想终止 `dev` 提交的工单（只能撤回自己的）、没写终止原因、
  终止已经终止的工单、终止已经“已正常结束”的工单。
  “定时执行”状态的工单、`can_cancel` 为 `false`、服务端拒绝终止、终止请求连接断开这几种只用模拟服务验证过。
- **只用按源码写的模拟服务验证过**（demo 没有接这些模块，或 demo 里没有 SQL Server 实例）：`describe_table` /
  `dict_table_info`、`slow_log` / `slow_log_detail`、`partman_list`、`sql_optimize`、`sql_analyze`，令牌过期重登，
  **批量工单**的 `sql_check` / `list_workflows` / `workflow_detail` / `workflow_log`，**批量提交**（一次请求、一个工单、
  `instance_ids`）以及提交时的各种状态（自动审批不通过、无需审批、服务端拒绝、连接断开、令牌失效重发），**批量导出**
  （一次请求、一个工单，含 `export_merge` 合并成一个文件）的成功路径，**批量导出预检**（`sql_check` 带 `export` / `export_merge`），**导出创建前自动预检**里批量 / 合并成一个文件 / 有警告 / 没能完成（接口出错、连接断开、超时）这几种情形，**上线创建前自动检查**里批量、有警告、没能完成，**`cancel_workflow`** 里定时执行的工单、批量工单、`can_cancel` 为 `false`、服务端拒绝、连接断开、响应里状态不对，以及 `approve_workflow` /
  `execute_workflow` 的成功路径（真正发出 `pass` / `execute`、多级审批、服务端拒绝）。请求的路径、参数和返回字段取自
  dbtool 的处理函数与类型定义，**没有对着真实部署跑过**，首次使用请先 `--cli` 试一遍；**第一次批量提交、批量导出、审批、执行请挑一张不要紧的批量工单试**。
  demo 服务的批量类型固定是 `mssql`（`cmd/demo` 不读 `BATCH_DB_TYPES`），所以真实服务上的批量路径只验证到了“请求被识别成批量导出并按类型拒绝”这一步。
- 23 个工具都已对接，没有占位。
- 没有在真实 SQL Server / goInception 上验证过任何东西。
