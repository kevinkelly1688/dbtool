// @author: kevinkelly
// @license: Apache Licence
// @file: fake.go
// @time: 2026/10/28

// Package btnpermtest 给各模块的 HTTP 测试用：内存的按钮权限检查器，可以按用户直接授予 / 收回按钮，不连任何数据库。
//
//	chk := btnpermtest.New()
//	servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){chk.Hook, register}})
//	chk.Grant(2, "account.create")   // bob(2) 拥有该按钮；装配后按钮校验只看它
package btnpermtest

import (
	"sync"

	"dbtool/internal/btnperm"
	"dbtool/internal/server"
)

// Checker 实现 btnperm.Checker。
type Checker struct {
	mu    sync.Mutex
	allow map[uint]map[string]bool
	Set   btnperm.Setting
}

// New 创建空的检查器（谁都没有任何按钮）。
func New() *Checker {
	return &Checker{allow: map[uint]map[string]bool{}, Set: btnperm.DefaultSetting()}
}

// Hook 装配到服务：放在 servertest.Options.Hooks 的最前面。
func (c *Checker) Hook(s *server.Server, _ server.Routes) { s.SetButtonChecker(c) }

// Grant 授予用户按钮。
func (c *Checker) Grant(uid uint, codes ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.allow[uid] == nil {
		c.allow[uid] = map[string]bool{}
	}
	for _, code := range codes {
		c.allow[uid][code] = true
	}
}

// Revoke 收回按钮。
func (c *Checker) Revoke(uid uint, codes ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, code := range codes {
		delete(c.allow[uid], code)
	}
}

func (c *Checker) HasButton(uid uint, code string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.allow[uid][code], nil
}

func (c *Checker) AllowedButtons(uid uint) (map[string]bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]bool{}
	for k, v := range c.allow[uid] {
		out[k] = v
	}
	return out, nil
}

func (c *Checker) Setting() (btnperm.Setting, error) { return c.Set.Normalize(), nil }
