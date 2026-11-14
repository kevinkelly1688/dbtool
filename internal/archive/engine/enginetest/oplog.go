// @author: kevinkelly
// @license: Apache Licence
// @file: oplog.go
// @time: 2026/10/28

package enginetest

import (
	"fmt"
	"strings"
	"sync"

	"dbtool/internal/archive/engine"
)

// Op 是替身记录的一次操作。
type Op struct {
	Kind string   // src.read | src.delete | src.insert_ignore | dst.insert | dst.verify | dst.ensure | file.put
	PKs  []string // 涉及的主键（单列主键时是值，复合主键用 "," 连接）
	N    int
}

// OpLog 按时间顺序记录所有替身上的操作，用来断言“先备份、校验，后删除”。
type OpLog struct {
	mu  sync.Mutex
	ops []Op
}

func (l *OpLog) add(kind string, pks [][]string, n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var keys []string
	for _, pk := range pks {
		keys = append(keys, strings.Join(pk, ","))
	}
	l.ops = append(l.ops, Op{Kind: kind, PKs: keys, N: n})
}

func (l *OpLog) Ops() []Op {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Op(nil), l.ops...)
}

// Count 返回某类操作的次数。
func (l *OpLog) Count(kind string) int {
	n := 0
	for _, o := range l.Ops() {
		if o.Kind == kind {
			n++
		}
	}
	return n
}

// CheckBackupBeforeDelete 检查每次 src.delete 涉及的主键，之前都已经在 dst.verify 里被校验过（目标端确实查到）。
// 文件备份的校验不在 OpLog 里体现（文件由引擎直接校验行数），所以只对有库表备份的运行有意义。
func (l *OpLog) CheckBackupBeforeDelete() error {
	verified := map[string]bool{}
	for i, o := range l.Ops() {
		switch o.Kind {
		case "dst.verify":
			for _, k := range o.PKs {
				verified[k] = true
			}
		case "src.delete":
			for _, k := range o.PKs {
				if !verified[k] {
					return fmt.Errorf("第 %d 个操作：删除了主键 %s，但此前没有对它做过备份校验", i, k)
				}
			}
		}
	}
	return nil
}

// Collector 是 engine.Sink 的实现：收集事件与进度。
type Collector struct {
	mu       sync.Mutex
	events   []engine.Event
	progress []engine.Progress
}

func (c *Collector) Event(e engine.Event) {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
}

func (c *Collector) Progress(p engine.Progress) {
	c.mu.Lock()
	c.progress = append(c.progress, p)
	c.mu.Unlock()
}

func (c *Collector) Events() []engine.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]engine.Event(nil), c.events...)
}

func (c *Collector) Progresses() []engine.Progress {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]engine.Progress(nil), c.progress...)
}

// Find 返回 Kind 相同且 Msg 包含 substr 的事件（substr 为空只比较 Kind）。
func (c *Collector) Find(kind, substr string) []engine.Event {
	var out []engine.Event
	for _, e := range c.Events() {
		if e.Kind == kind && strings.Contains(e.Msg, substr) {
			out = append(out, e)
		}
	}
	return out
}

// LastProgress 返回最后一条进度（没有则零值）。
func (c *Collector) LastProgress() engine.Progress {
	ps := c.Progresses()
	if len(ps) == 0 {
		return engine.Progress{}
	}
	return ps[len(ps)-1]
}
