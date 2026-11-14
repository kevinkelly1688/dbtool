// @author: kevinkelly
// @license: Apache Licence
// @file: exporterr_test.go
// @time: 2026/10/28

package sqlflow

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "something slow" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }

func TestExportErrText(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string // 友好提示里应有的片段；"" 表示原样返回
	}{
		{"读取超时", errors.New("read tcp 172.16.1.42:46548->192.168.2.23:1433: i/o timeout"), "读取数据时与数据库实例的网络连接超时"},
		{"写超时", errors.New("write tcp 172.16.1.42:46548->192.168.2.23:1433: i/o timeout"), "读取数据时与数据库实例的网络连接超时"},
		{"包了一层的读取超时", fmt.Errorf("查询失败: %w", errors.New("read tcp 10.0.0.1:1->10.0.0.2:1433: i/o timeout")), "读取数据时与数据库实例的网络连接超时"},
		{"拨号超时", errors.New("dial tcp 192.168.2.23:1433: i/o timeout"), "连接数据库实例超时"},
		{"net.Error 超时", timeoutErr{}, "读取数据时与数据库实例的网络连接超时"},
		{"拒绝连接", errors.New("dial tcp 192.168.2.23:1433: connect: connection refused"), "无法连接数据库实例"},
		{"连接被重置", errors.New("read tcp 1.1.1.1:1->2.2.2.2:1433: read: connection reset by peer"), "连接被中断"},
		{"连接被对端关闭", errors.New("unexpected EOF"), "连接被中断"},
		{"语句自己的错误原样", errors.New("Invalid object name 'dbo.T'."), ""},
		{"空白被去掉", errors.New("  Incorrect syntax near 'x'.\n"), ""},
	}
	for _, c := range cases {
		got := exportErrText(c.err)
		if c.want == "" {
			if got != strings.TrimSpace(c.err.Error()) {
				t.Errorf("%s: 应原样返回，得到 %q", c.name, got)
			}
			continue
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: %q 应含 %q", c.name, got, c.want)
		}
		if raw := strings.TrimSpace(c.err.Error()); !strings.HasSuffix(got, "原始错误："+raw) {
			t.Errorf("%s: 原始错误要附在最后方便排查: %q", c.name, got)
		}
		// 检查行数（提交前）、单实例导出里没有“重试失败的实例”这个入口，提示只说“重试”
		if strings.Contains(got, "工单里") || strings.Contains(got, "失败的实例") {
			t.Errorf("%s: 提示不能假定有“在工单里重试失败的实例”这个入口: %q", c.name, got)
		}
	}
}
