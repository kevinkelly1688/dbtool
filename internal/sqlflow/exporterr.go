// @author: kevinkelly
// @license: Apache Licence
// @file: exporterr.go
// @time: 2026/10/28

package sqlflow

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// exportErrText 把导出（取数 / 检查行数）时遇到的网络类错误翻译成说得清原因和下一步的话，原始错误附在最后，DBA 排查时还能看到地址和端口；
// 其它错误（语法错误、表不存在……）原样返回。
func exportErrText(err error) string {
	raw := errText(err)
	low := strings.ToLower(raw)
	var ne net.Error
	timeout := (errors.As(err, &ne) && ne.Timeout()) || strings.Contains(low, "i/o timeout")
	switch {
	case timeout && strings.Contains(low, "dial tcp"):
		return fmt.Sprintf("连接数据库实例超时，实例可能不可达或网络不通。请确认实例地址、端口和网络 / 防火墙后重试。原始错误：%s", raw)
	case timeout:
		return fmt.Sprintf("读取数据时与数据库实例的网络连接超时（实例长时间没有返回数据）。常见原因：查询太慢（没走索引、数据量大），或网络 / 防火墙中断了长时间的连接。可缩小查询范围、加上过滤条件，或避开业务高峰后重试；仍然超时请联系 DBA 检查网络。原始错误：%s", raw)
	case strings.Contains(low, "connection refused"), strings.Contains(low, "no such host"):
		return fmt.Sprintf("无法连接数据库实例：地址解析失败或端口没有服务在监听。请确认实例地址、端口和实例是否在运行后重试。原始错误：%s", raw)
	case strings.Contains(low, "connection reset"), strings.Contains(low, "broken pipe"), strings.Contains(low, "bad connection"),
		strings.Contains(low, "unexpected eof"), strings.HasSuffix(low, ": eof"):
		return fmt.Sprintf("与数据库实例的连接被中断（实例重启、网络抖动，或被防火墙 / 会话终止断开）。请稍后重试。原始错误：%s", raw)
	}
	return raw
}
