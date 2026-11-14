// @author: kevinkelly
// @license: Apache Licence
// @file: quote.go
// @time: 2026/10/28

package partman

import "strconv"

// qstr 返回 s 的 Go 风格引号写法，等同于格式串里的 %q（与 model.go 里给标识符加反引号的 quote 不是一回事）。
// 面向用户的错误文案里不直接写 %q，而是写 %s 再传 qstr(x)：输出与原来逐字相同，
// 但翻译目录按格式串整句匹配时，参数里的引号不会被再转义一次。
func qstr(s string) string { return strconv.Quote(s) }
