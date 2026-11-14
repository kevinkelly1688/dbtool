// @author: kevinkelly
// @license: Apache Licence
// @file: doc.go
// @time: 2026/10/28

// Package dict 读取目标库的对象元数据（表 / 视图 / 函数 / 存储过程 / 触发器 / 定时任务），给在线查询工作台使用：
// 库表树里右键表名的“对象信息”（基本信息 + 定义语句）、左侧“可编程对象”页签的列表。
//
// 原来独立的“数据字典”页面（/data_dictionary、/api/v1/dict/*、HTML / ZIP 导出）已经去掉，本包只剩上面两个接口，
// 登录即可使用，授权与库表树一致：能访问该实例 + 库名过滤规则 + 非系统库。
//
// 结构：
//
//   - service.go   业务：实例可见范围、库名过滤、对象详情与可编程对象列表；
//   - dialect.go   各类实例的元数据查询接口，mysql.go（MySQL 系）、mssql.go（SQL Server）、engines.go（PostgreSQL / MongoDB / ClickHouse）是实现；
//   - opener.go    目标库连接（database/sql，超时，MaxOpenConns=2），测试里换成假实现；
//   - sensitive.go 敏感列标记：详情的“列信息”里标出命中脱敏规则的列（只读 dbt_masking_policy）。
package dict
