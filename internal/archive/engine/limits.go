// @author: kevinkelly
// @license: Apache Licence
// @file: limits.go
// @time: 2026/10/28

package engine

// 对外公开的取值范围，供上层（GET /meta 的 limits）与前端提示使用；校验的权威仍是 Validate，这里只是把它的常量导出，避免两处各写一份。
const (
	PartMBMin       = minPartMB // 文件备份单文件上限（MB）下限；0 表示用默认值
	PartMBMax       = maxPartMB
	RunMaxRowsMin   = minRows // 单次运行最多处理行数下限（任务级）
	DefaultPartMB   = defPartMB
	DeleteSleepMsLo = minDeleteSleepMs // move / purge 的批间休眠下限
)
