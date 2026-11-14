// @author: kevinkelly
// @license: Apache Licence
// @file: ext_memrepo_test.go
// @time: 2026/10/28

package perf

import (
	"strings"
)

// memRepo 对 ExtRepo 的实现（测试用）。

var _ ExtRepo = (*memRepo)(nil)

func (m *memRepo) ExtReady(table string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.extTables == nil || m.extTables[table]
}

func (m *memRepo) GetIssueExt(id uint) (IssueExt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.extIssue[id], nil
}

func (m *memRepo) SaveIssueExt(id uint, e IssueExt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.extIssue == nil {
		m.extIssue = map[uint]IssueExt{}
	}
	if e.WorkflowID == 0 {
		delete(m.extIssue, id)
	} else {
		m.extIssue[id] = e
	}
	return nil
}

func (m *memRepo) WorkflowExists(id uint) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.workflows[id], nil
}

func (m *memRepo) ListRouteExt() (map[uint]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[uint]string{}
	for k, v := range m.extRoute {
		out[k] = v
	}
	return out, nil
}

func (m *memRepo) SaveRouteExt(id uint, re string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.extRoute == nil {
		m.extRoute = map[uint]string{}
	}
	if strings.TrimSpace(re) == "" {
		delete(m.extRoute, id)
	} else {
		m.extRoute[id] = re
	}
	return nil
}

func (m *memRepo) GetSetting(key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settings[key], nil
}

func (m *memRepo) SetSetting(key, value, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.settings == nil {
		m.settings = map[string]string{}
	}
	m.settings[key] = value
	return nil
}

func (m *memRepo) SetConfigValue(item, value, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg[item] = value
	return nil
}

func (m *memRepo) IssueCounts(q IssueQuery) (IssueCounts, error) {
	base := IssueQuery{Restrict: q.Restrict, TeamIDs: q.TeamIDs, UserID: q.UserID, Now: q.Now}
	week := q.Now.AddDate(0, 0, -7)
	m.mu.Lock()
	defer m.mu.Unlock()
	var c IssueCounts
	for _, i := range m.issues {
		if !m.visible(i, base) {
			continue
		}
		c.All++
		has := func(v string) bool {
			x := base
			x.View = v
			return m.visible(i, x)
		}
		if has(ViewTodo) {
			c.Todo++
		}
		if has(ViewTodoOverdue) {
			c.TodoOverdue++
		}
		if has(ViewOpen) {
			c.Open++
		}
		if has(ViewOverdue) {
			c.Overdue++
		}
		switch i.Status {
		case StNew:
			c.New++
		case StAssigned:
			c.Assigned++
		case StAccepted:
			c.Accepted++
		case StFixed:
			c.Fixed++
		case StClosed:
			c.Closed++
			if i.ClosedAt != nil && !i.ClosedAt.Before(week) {
				c.Closed7d++
			}
		}
		if has(ViewApprove) {
			c.Approve++
		}
		if !i.CreateTime.Before(week) {
			c.Created7d++
		}
	}
	return c, nil
}

// plainRepo 只暴露 Repo 的方法（不实现 ExtRepo），用来测试“仓库不支持扩展”时的降级。
type plainRepo struct{ Repo }
