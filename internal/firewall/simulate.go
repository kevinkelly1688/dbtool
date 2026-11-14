// @author: kevinkelly
// @license: Apache Licence
// @file: simulate.go
// @time: 2026/10/28

package firewall

import (
	"fmt"
	"log/slog"
	"sort"
	"time"
)

// Simulator 用一组给定的规则与例外离线求值：不读规则快照、不写命中记录、不发通知。
// 管理页的语句测试台与历史回放用它——规则可以是还没保存的草稿，也可以对上万条历史语句重复求值（规则只编译一次）。
// 并发安全（编译结果只读）。
type Simulator struct {
	rules   []*compiledRule
	exs     []Exception
	observe bool
}

// ruleInvalidError 是“规则无效”的带名错误。文字与 fmt.Errorf("规则「%s」无效：%w") 一致，保留 Unwrap；
// 不直接用 %w，是为了让整句能按“规则「%s」无效：%s”这个键翻译（%w 不在格式串匹配的动词里）。
type ruleInvalidError struct {
	msg string
	err error
}

func (e *ruleInvalidError) Error() string { return e.msg }
func (e *ruleInvalidError) Unwrap() error { return e.err }

// NewSimulator 编译规则。只有 Enabled 的规则参与求值；任何一条规则无效都返回错误（带规则名），
// 因为管理页里“这条规则其实没生效”比报错更危险。globalObserve 为真时所有规则按观察态处理。
func NewSimulator(rules []Rule, exs []Exception, globalObserve bool) (*Simulator, error) {
	s := &Simulator{exs: exs, observe: globalObserve}
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		c, err := compile(r)
		if err != nil {
			return nil, &ruleInvalidError{msg: fmt.Sprintf("规则「%s」无效：%s", r.Name, err), err: err}
		}
		c.Enabled = true
		s.rules = append(s.rules, c)
	}
	sort.SliceStable(s.rules, func(i, j int) bool {
		if s.rules[i].Priority != s.rules[j].Priority {
			return s.rules[i].Priority < s.rules[j].Priority
		}
		return s.rules[i].ID < s.rules[j].ID
	})
	return s, nil
}

// Run 对一条语句求值。req.GroupIDs 是用户所在权限组（不会去读库）；req.Now 为零值时取当前时间。
// 内部错误（panic）返回 Failed 的决定，不向上抛。
func (s *Simulator) Run(req Request) (d *Decision) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("防火墙离线求值 panic", "panic", r)
			d = &Decision{Failed: true}
		}
	}()
	groups := map[uint]bool{}
	for _, id := range req.GroupIDs {
		groups[id] = true
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	return evaluate(s.rules, s.exs, &req, groups, s.observe, now, nil)
}
