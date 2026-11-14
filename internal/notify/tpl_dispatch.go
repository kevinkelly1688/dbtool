// @author: kevinkelly
// @license: Apache Licence
// @file: tpl_dispatch.go
// @time: 2026/10/28

package notify

import (
	"log/slog"
	"sync"
	"time"
)

// 本文件：Dispatcher 取模板、套模板。
//
// 查找顺序：渠道专属 → 全局 → 系统默认（事件里现成的 Title / Content，原样发送）。
// 模板按 TTL 缓存；管理页保存后调用 InvalidateTemplates，同进程立即生效，其它进程最多等一个 TTL。

// TemplateSource 是 Source / Admin 的可选能力：返回所有启用的模板。
type TemplateSource interface {
	ActiveTemplates() ([]Template, error)
}

const tplTTL = 20 * time.Second

type tplCache struct {
	mu   sync.Mutex
	at   time.Time
	m    map[string]*Template // key: 消息键 + "|" + 范围
	have bool
}

func tplKey(msgKey, scope string) string { return msgKey + "|" + scope }

func (c *tplCache) get(src TemplateSource, now time.Time) map[string]*Template {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.have && now.Sub(c.at) < tplTTL {
		return c.m
	}
	list, err := src.ActiveTemplates()
	if err != nil {
		slog.Warn("读取消息模板失败，沿用上一次的结果", "err", err)
		if !c.have {
			c.m = map[string]*Template{}
			c.have = true
		}
		c.at = now // 失败时也别每条消息都查库
		return c.m
	}
	m := make(map[string]*Template, len(list))
	for i := range list {
		t := list[i]
		if t.Enabled {
			m[tplKey(t.EventKey, t.Scope)] = &t
		}
	}
	c.m, c.at, c.have = m, now, true
	return m
}

func (c *tplCache) reset() {
	c.mu.Lock()
	c.have = false
	c.mu.Unlock()
}

// InvalidateTemplates 让下一条消息重新读取模板（管理页保存 / 删除模板后调用）。
func (d *Dispatcher) InvalidateTemplates() { d.tpls.reset() }

func (d *Dispatcher) templateSource() TemplateSource {
	if d.Templates != nil {
		return d.Templates
	}
	if ts, ok := d.Source.(TemplateSource); ok {
		return ts
	}
	return nil
}

// findTemplate 查找事件对该渠道生效的模板；没有返回 nil。
func (d *Dispatcher) findTemplate(msgKey string, chID uint) *Template {
	src := d.templateSource()
	if src == nil || msgKey == "" {
		return nil
	}
	m := d.tpls.get(src, d.Now())
	if chID != 0 {
		if t := m[tplKey(msgKey, ScopeChannel(chID))]; t != nil {
			return t
		}
	}
	return m[tplKey(msgKey, ScopeGlobal)]
}

// render 按模板渲染事件；没有模板、事件不属于任何消息、或模板渲染失败时返回 nil（发系统默认文案）。
func (d *Dispatcher) render(ch Channel, ev Event) *Message {
	key := MessageKey(ev)
	if key == "" {
		return nil
	}
	t := d.findTemplate(key, ch.ID)
	if t == nil {
		return nil
	}
	m, err := t.Render(MessageByKey(key), ev)
	if err != nil {
		slog.Warn("消息模板渲染失败，改用系统默认文案", "message", key, "scope", t.Scope, "err", err)
		return nil
	}
	return m
}

// RenderWith 用给定的模板（比如管理页里还没保存的草稿）渲染事件，返回渲染结果；供管理页的预览 / 测试发送用。
func RenderWith(t Template, ev Event) (*Message, error) {
	key := t.EventKey
	if key == "" {
		key = MessageKey(ev)
	}
	return t.Render(MessageByKey(key), ev)
}

// PreviewTemplate 用草稿模板生成某个渠道的预览（不发送）。
func (d *Dispatcher) PreviewTemplate(ch Channel, ev Event, t Template) (*Preview, error) {
	m, err := RenderWith(t, ev)
	if err != nil {
		return nil, err
	}
	ev.rich = m
	return d.Preview(ch, ev)
}

// WithRich 返回带上已渲染消息的事件副本：用它发送 / 预览时不再按已保存的模板渲染（管理页的草稿预览与测试发送用）。
func (e Event) WithRich(m *Message) Event {
	e.rich = m
	return e
}
