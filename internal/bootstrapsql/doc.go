// @author: kevinkelly
// @license: Apache Licence
// @file: doc.go
// @time: 2026/10/28

// Package bootstrapsql 只有测试：校验 deploy/bootstrap 下的空库初始化脚本。
//
//   - 001_base_schema.sql   基础表结构（只含建表语句）；
//   - 002_base_data.sql     内容类型、权限、内置权限组及授权、实例标签等基础数据。
//
// 这两个文件直接维护（设计与取舍见 docs/bootstrap-design.md），由命令行工具 dbt-admin 读取执行。
// 本包的测试不连接任何数据库：用仓库已有的 MySQL 语法解析器逐条解析语句，并与 internal/btnperm 的登记表、
// deploy/sql 的建表脚本、internal/** 里的表名引用做交叉检查。
//
// 本系统的所有表统一 dbt_ 前缀：001 里的基础表 = dbt_ + 未加前缀的表名（仅内容类型表是例外，叫 dbt_content_type），
// 外键约束名同样加前缀（超过 64 字符的用原名前 55 字符 + sha1 前 4 位）；
// 规则的镜像在 names_test.go，prefix_lint_test.go 保证 001 / 002 / deploy/sql 里不再出现未加前缀的基础表名，
// refs_test.go 保证 Go 代码的 SQL 里没有旧表名（internal/dbtadmin 按设计除外）。
package bootstrapsql
