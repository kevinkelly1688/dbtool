// @author: kevinkelly
// @license: Apache Licence
// @file: guard.go
// @time: 2026/10/28

package settings

import "sync"

// Guard 是保存前的环境校验：Def.Check 只看值本身的写法，Guard 还要看服务器上的文件、已登记的实例等“外部事实”。
// 由持有这些事实的模块登记（如 sqlopt 校验 SOAR 程序文件、测试库不是生产库）。
// 页面与接口保存时一律执行；命令行加 --force 时跳过（运维在服务器上操作，自己负责）。值为空（清除 / 恢复默认）不校验。
type Guard func(value string) error

var (
	guardMu sync.RWMutex
	guards  = map[string]Guard{}
)

// RegisterGuard 登记（或替换）某个设置项的保存前校验；g 为 nil 表示撤销。
func RegisterGuard(key string, g Guard) {
	guardMu.Lock()
	defer guardMu.Unlock()
	if g == nil {
		delete(guards, key)
		return
	}
	guards[key] = g
}

func guardOf(key string) Guard {
	guardMu.RLock()
	defer guardMu.RUnlock()
	return guards[key]
}
