// @author: kevinkelly
// @license: Apache Licence
// @file: doc.go
// @time: 2026/10/28

// Package binlog 是“Binlog 解析 / 回滚”，不依赖外部二进制。
//
// 做法：以复制客户端身份（COM_BINLOG_DUMP，go-mysql 的 replication 包）从目标 MySQL 实例拉取指定范围的 binlog 事件，
// 在进程内把行事件还原成 INSERT / UPDATE / DELETE 语句，或生成对应的回滚语句；对目标库只读（SHOW BINARY LOGS、
// 读 information_schema 的表结构、拉取 binlog），不做任何写。长任务用任务 id + 轮询，可取消，有超时与结果上限。
package binlog
