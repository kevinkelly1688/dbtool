// @author: kevinkelly
// @license: Apache Licence
// @file: duty.go
// @time: 2026/10/28

package server

import (
	"log/slog"
	"time"
)

// 顶栏时钟左边的“值班”芯片：内容由 internal/duty 提供（在路由登记时用 SetDutyProvider 装配），
// 没有装配（测试、演示、没有元数据库）或者没有值班信息时整块不输出，页面与原来完全一样。

// DutyChip 是顶栏“值班”芯片的数据；nil 表示不显示。
type DutyChip struct {
	State    string // on 当前有人值班 / none 当前时段没人
	Name     string // 值班人的显示名（State 为 on 时有值）
	Range    string // 本班的时间段，如 08:00–19:00
	Override bool   // 当前这班是临时调班
	JSON     string // 给前端脚本的完整快照，放在芯片的 data-duty 属性里
}

// DutyProvider 提供顶栏的值班芯片。实现要快（读缓存）、不阻塞，出错返回 nil。
type DutyProvider interface {
	Chip(now time.Time) *DutyChip
}

// SetDutyProvider 装配值班芯片的提供者。
func (s *Server) SetDutyProvider(p DutyProvider) { s.duty = p }

// HasDuty 报告值班表模块是否已装配（侧栏菜单判断“值班表”入口要不要出现）。
func (s *Server) HasDuty() bool { return s.duty != nil }

// dutyChip 取顶栏芯片的数据；提供者出任何问题都按“不显示”处理，不能影响页面渲染。
func (s *Server) dutyChip() (chip *DutyChip) {
	if s.duty == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("顶栏值班信息出错", "panic", r)
			chip = nil
		}
	}()
	return s.duty.Chip(time.Now())
}
