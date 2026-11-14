# 权限码清单

> 本页由 `tools/build_help.py` 根据 `js/perm.js` 的 `PERM_TREE` 自动生成，请勿手工编辑。"高危"按钮点击后需要二次确认（部分还要键入确认文本）；"仅超级用户"表示锁定码，服务端只认超级用户，不能授予权限组。

共 **6** 个模块、**31** 个页面、**143** 个权限码：查看 39 个、操作 48 个、高危 56 个；其中 36 个仅超级用户，107 个可授予权限组。

## SQL 审核（18）

### SQL 上线

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `sqlworkflow.view_own` | 查看自己的工单 | 查看 |  |
| `sqlworkflow.view_group` | 查看资源组内所有工单 | 查看 |  |
| `sqlworkflow.submit` | 提交工单 | 操作 |  |
| `sqlworkflow.submit_batch` | 提交批量上线 | 操作 |  |
| `sqlworkflow.skip_check` | 跳过语法检查 | 高危 | 是 |
| `sqlworkflow.cancel_own` | 终止自己的工单 | 操作 |  |
| `sqlworkflow.approve` | 审批通过 | 操作 |  |
| `sqlworkflow.reject` | 驳回 / 终止他人工单 | 操作 |  |
| `sqlworkflow.alter_window` | 修改可执行时间 | 操作 |  |
| `sqlworkflow.execute_group` | 立即执行（资源组内） | 高危 |  |
| `sqlworkflow.execute_own` | 立即执行（仅自己的） | 高危 |  |
| `sqlworkflow.schedule` | 定时执行 | 高危 |  |
| `sqlworkflow.manual_finish` | 确认手工执行结束 | 高危 |  |
| `sqlworkflow.rollback` | 查看 / 下载回滚语句 | 操作 |  |
| `sqlworkflow.osc` | 在线改表控制（暂停 / 恢复 / 终止） | 高危 |  |
| `sqlworkflow.batch_op` | 批量实例 重试 / 关闭 | 高危 |  |

### SQL 分析

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `sqlanalyze.view` | 查看分析报告 | 查看 |  |
| `sqlanalyze.run` | 上传并开始分析 | 操作 |  |

## SQL 查询（34）

### 在线查询

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `query.view` | 打开在线查询 | 查看 |  |
| `query.run` | 执行查询（含执行计划） | 操作 |  |
| `query.objinfo` | 查看对象信息 / 可编程对象（表结构、建表语句、定义） | 操作 |  |
| `query.group_run` | 资源组内实例按管理员行数上限查询 | 操作 |  |
| `query.admin_run` | 查看 / 查询全部实例，不受库表授权限制 | 高危 |  |

### 查慢日志

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `slowlogs.view` | 查看慢日志与明细 | 查看 |  |

### 分区维护

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `partman.view` | 查看配置与维护日志 | 查看 |  |
| `partman.add` | 新增配置 | 操作 |  |
| `partman.edit` | 编辑配置 | 操作 |  |
| `partman.run` | 立即执行维护 | 高危 |  |
| `partman.delete` | 删除配置 | 高危 |  |

### 数据归档

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `archive.view` | 查看归档任务与运行记录 | 查看 |  |
| `archive.apply` | 新建 / 修改归档任务（提交申请） | 操作 |  |
| `archive.dryrun` | 试运行 | 操作 |  |
| `archive.pause` | 暂停 / 恢复 | 操作 |  |
| `archive.run` | 立即执行 | 高危 |  |
| `archive.stop` | 终止运行 | 高危 |  |
| `archive.restore` | 恢复数据（回灌） | 高危 |  |
| `archive.delete` | 停用 / 删除任务 | 高危 |  |
| `archive.cfg_view` | 查看备份存储与安全策略 | 查看 |  |
| `archive.store_edit` | 编辑备份存储 | 高危 | 是 |
| `archive.policy_edit` | 编辑安全策略 | 高危 | 是 |

### 数据导出

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `export.view` | 查看导出工单 | 查看 |  |
| `export.view_batch` | 查看批量导出工单 | 查看 |  |
| `export.submit` | 提交导出 | 操作 |  |
| `export.submit_batch` | 提交批量导出 | 操作 |  |
| `export.download` | 下载导出文件 | 高危 |  |

### 数据字典

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `dict.view` | 查看表 / 视图 / 过程等元数据 | 查看 |  |
| `dict.export` | 导出 HTML 字典 | 高危 |  |

### 权限申请

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `qpriv.view` | 查看申请与审批进度 | 查看 |  |
| `qpriv.apply` | 申请 / 撤回查询权限 | 操作 |  |
| `qpriv.review` | 审批查询权限申请 | 操作 |  |

### 查询权限

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `qpriv.list` | 查看已授予的查询权限 | 查看 |  |
| `qpriv.manage` | 修改 / 回收他人权限 | 高危 |  |

## SQL 优化（6）

### 优化工具

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `optimize.explain` | 执行计划 / 索引建议 / 格式化 | 查看 |  |
| `optimize.tuning` | SQLTuning | 操作 |  |
| `optimize.soar` | SOAR | 操作 |  |

### 慢查日志

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `slowquery.view` | 查看统计与明细 | 查看 |  |
| `slowquery.advise` | 获取优化建议 | 操作 |  |
| `redis.slowlog_reset` | 重置 Redis 慢日志（SLOWLOG RESET） | 高危 | 是 |

## 性能工单（14）

### 工单列表 / 详情 / 排行榜

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `perf.view` | 查看工单与排行榜 | 查看 |  |
| `perf.handle` | 受理 / 申请验证 / 申请延期 / 申请豁免 | 操作 |  |
| `perf.comment` | 评论 | 操作 |  |
| `perf.create` | 手工建单 | 操作 |  |
| `perf.assign` | 指派 | 操作 |  |
| `perf.verify` | 验证通过 / 不通过 | 操作 |  |
| `perf.approve` | 审批延期 / 豁免 | 操作 |  |
| `perf.adjust` | 调整级别 / 切换处理方 | 操作 |  |
| `perf.reopen` | 重开 / 重新诊断 | 操作 |  |
| `perf.close_dup` | 误报关闭 | 高危 |  |

### 采集 / 团队 / SLA 配置

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `perfcfg.view` | 查看配置 | 查看 |  |
| `perfcfg.collect` | 修改采集配置 / 试采集 | 操作 |  |
| `perfcfg.team` | 修改团队配置与路由规则 | 操作 |  |
| `perfcfg.sla` | 修改 SLA 策略 | 高危 |  |

## 实例管理（37）

### 实例列表

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `instance.view` | 查看实例 | 查看 |  |
| `instance.test` | 测试连接 | 操作 |  |
| `instance.add` | 添加实例 | 高危 |  |
| `instance.edit` | 编辑 / 启停实例 | 高危 |  |
| `instance.delete` | 删除实例 | 高危 |  |
| `instance.purge_binlog` | Binlog 清理 | 高危 |  |

### 会话管理

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `session.open` | 打开会话管理 | 查看 |  |
| `session.process` | 查看进程 | 查看 |  |
| `session.tablespace` | 查看 Top 表空间 | 查看 |  |
| `session.trx` | 查看事务 | 查看 |  |
| `session.locks` | 查看锁信息 | 查看 |  |
| `session.kill` | 终止会话 | 高危 |  |

### 数据库管理

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `database.view` | 查看数据库 | 查看 |  |
| `database.edit` | 编辑 / 录入负责人与备注 | 操作 |  |
| `database.create` | 创建数据库 | 高危 |  |

### 账号管理

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `account.view` | 查看账号与权限 | 查看 |  |
| `account.create` | 创建账号 | 高危 |  |
| `account.edit` | 编辑 / 录入登记信息 | 操作 |  |
| `account.grant` | 授权 / 回收 | 高危 |  |
| `account.pwd` | 修改密码 | 高危 |  |
| `account.lock` | 锁定 / 解锁 | 高危 |  |
| `account.delete` | 删除账号 | 高危 |  |

### 参数配置

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `param.open` | 打开参数配置 | 查看 |  |
| `param.view` | 查看参数与修改历史 | 查看 |  |
| `param.edit` | 修改参数值 | 高危 |  |
| `param.template` | 添加 / 编辑参数模板 | 操作 |  |

### 性能巡检

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `inspect.view` | 查看报告与定时计划 | 查看 |  |
| `inspect.run` | 立即 / 重新巡检 | 操作 |  |
| `inspect.cancel` | 取消进行中的巡检 | 操作 |  |
| `inspect.delete` | 删除报告 | 高危 |  |
| `inspect.plan_edit` | 新建 / 编辑定时计划 | 操作 |  |
| `inspect.plan_toggle` | 暂停 / 启用 / 删除定时计划 | 高危 |  |
| `inspect.plan_run` | 立即运行定时计划 | 高危 |  |

### Binlog 解析

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `binlog.view` | 打开 Binlog 解析、查看 binlog 文件列表 | 查看 |  |
| `binlog.parse` | 解析 binlog 还原 SQL | 操作 |  |
| `binlog.rollback` | 生成回滚 SQL | 高危 |  |
| `binlog.download` | 下载解析 / 回滚结果文件 | 高危 |  |

## 系统管理（34）

### 审批流程配置

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `flow.view` | 查看审批流 | 查看 | 是 |
| `flow.edit` | 变更审批流 | 高危 | 是 |

### 资源组管理

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `rg.view` | 查看资源组 | 查看 | 是 |
| `rg.edit` | 新建 / 编辑资源组与通知群 | 操作 | 是 |
| `rg.relations` | 调整成员与实例 | 操作 | 是 |
| `rg.delete` | 删除 / 恢复资源组 | 高危 | 是 |

### 用户管理

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `user.view` | 查看用户 | 查看 | 是 |
| `user.edit` | 新建 / 编辑用户 | 操作 | 是 |
| `user.unlock` | 解锁账号 | 操作 | 是 |
| `user.reset` | 重置密码 / 重置 2FA | 高危 | 是 |
| `user.disable` | 停用 / 启用 | 高危 | 是 |
| `user.grant_super` | 授予超级用户 | 高危 | 是 |

### 权限组管理

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `ag.view` | 查看权限组 | 查看 | 是 |
| `ag.edit` | 新建 / 编辑权限与成员 | 高危 | 是 |
| `ag.delete` | 删除权限组 | 高危 | 是 |
| `ag.btnperm` | 按钮权限与生效模式 | 高危 | 是 |

### 登录与安全设置

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `sec.view` | 查看 | 查看 | 是 |
| `sec.edit` | 修改 | 高危 | 是 |

### goInception 配置

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `goinception.view` | 查看连接信息与配置 | 查看 | 是 |
| `goinception.conn` | 修改连接信息 / 备份库 | 高危 | 是 |
| `goinception.file` | 修改配置文件（仅保存） | 高危 | 是 |
| `goinception.restart` | 保存并重启 / 仅重启 | 高危 | 是 |
| `goinception.rollback` | 回滚历史版本 | 高危 | 是 |

### 登录方式

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `authp.view` | 查看登录方式 | 查看 | 是 |
| `authp.test` | 测试连接 / 测试登录 | 操作 | 是 |
| `authp.edit` | 编辑配置 / 入组规则 | 高危 | 是 |
| `authp.toggle` | 启用 / 停用 / 排序 / 删除 | 高危 | 是 |

### 通知渠道

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `notify.view` | 查看渠道 | 查看 | 是 |
| `notify.edit` | 新建 / 编辑渠道与 @ 规则 | 操作 | 是 |
| `notify.test` | 测试发送 | 操作 | 是 |

### Redis 上线规则

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `redisrule.view` | 查看 Redis 上线规则 | 查看 | 是 |
| `redisrule.edit` | 新增 / 修改 / 启停 / 删除规则 | 高危 | 是 |

### 系统审计

| 权限码 | 名称 | 类型 | 仅超级用户 |
|---|---|---|---|
| `audit.view` | 查看审计日志 | 查看 |  |
| `audit.export` | 导出审计日志 | 高危 |  |
