// @author: kevinkelly
// @license: Apache Licence
// @file: resolver.go
// @time: 2026/10/28

package sshtunnel

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"dbtool/internal/engine"
	"dbtool/internal/model"
)

// 环境变量（均可选）：
//
//	SSH_TUNNEL_LISTEN_HOST     转发器监听地址，默认 127.0.0.1（只允许本机访问）
//	SSH_TUNNEL_ADVERTISE_HOST  写进连接信息的地址，默认 127.0.0.1
//	SSH_TUNNEL_IDLE_TIMEOUT    转发器空闲多久后关闭，Go 时长写法（如 10m），默认 10m
const (
	envListenHost    = "SSH_TUNNEL_LISTEN_HOST"
	envAdvertiseHost = "SSH_TUNNEL_ADVERTISE_HOST"
	envIdleTimeout   = "SSH_TUNNEL_IDLE_TIMEOUT"
)

var (
	defaultOnce sync.Once
	defaultMgr  *Manager
)

// Default 返回进程内共用的管理器（参数来自环境变量）。
func Default() *Manager {
	defaultOnce.Do(func() { defaultMgr = NewManager(OptionsFromEnv(os.Getenv)) })
	return defaultMgr
}

// OptionsFromEnv 从环境变量读取管理器参数；取值无效时忽略并记日志。
func OptionsFromEnv(getenv func(string) string) Options {
	o := Options{
		ListenHost:    strings.TrimSpace(getenv(envListenHost)),
		AdvertiseHost: strings.TrimSpace(getenv(envAdvertiseHost)),
	}
	if v := strings.TrimSpace(getenv(envIdleTimeout)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= time.Minute {
			o.IdleTimeout = d
		} else {
			slog.Warn("SSH_TUNNEL_IDLE_TIMEOUT 无效（需要不小于 1m 的时长，如 10m），使用默认值", "value", v)
		}
	}
	if o.ListenHost != "" && o.ListenHost != "127.0.0.1" && o.ListenHost != "localhost" && o.ListenHost != "::1" {
		slog.Warn("SSH 隧道转发器监听在非回环地址，能访问该地址的人都可以经跳板机连到内网数据库，请用防火墙限制", "listen", o.ListenHost)
	}
	return o
}

// Shutdown 关闭进程内全部转发器（进程退出时调用，可重复调用）。
func Shutdown() {
	if defaultMgr != nil {
		defaultMgr.CloseAll()
	}
}

// Resolver 把“配置了隧道的实例”的连接信息改写成走本地转发器的地址。
type Resolver struct {
	// Load 按 ID 读取 dbt_ssh_tunnel（inst.Tunnel 没有预加载时用）；不存在返回 (nil, nil)。可为空：此时只用预加载的 inst.Tunnel。
	Load func(id uint) (*model.Tunnel, error)
	// Decrypt 解密 dbt_ssh_tunnel 的 mirage 加密字段。
	Decrypt func(string) string
	// Mgr 为空用 Default()。
	Mgr *Manager
}

// NewGormResolver 用元数据库句柄和字段解密函数创建 Resolver。
func NewGormResolver(db *gorm.DB, decrypt func(string) string) *Resolver {
	r := &Resolver{Decrypt: decrypt}
	if db != nil {
		r.Load = func(id uint) (*model.Tunnel, error) {
			var t model.Tunnel
			err := db.First(&t, id).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			return &t, nil
		}
	}
	return r
}

// Enabled 实例是否配置了隧道。
func Enabled(inst *model.Instance) bool {
	return inst != nil && (inst.TunnelID != nil || inst.Tunnel != nil)
}

// Apply 对配置了隧道的实例，返回 Host/Port 改写为本地转发器地址的连接信息；没有配置隧道时原样返回 c。
// 隧道配置缺失 / 无法建立转发器时，返回指向“总是失败”的转发器的地址，连接时报出原因（不会静默直连）。
func (r *Resolver) Apply(inst *model.Instance, c engine.Conn) engine.Conn {
	if !Enabled(inst) {
		return c
	}
	mgr := r.Mgr
	if mgr == nil {
		mgr = Default()
	}
	proto := ProtoFor(inst.DBType)
	fail := func(id string, err error) engine.Conn {
		h, p, e := mgr.Fail(id, err, c.Host, c.Port, proto)
		if e != nil {
			slog.Warn("SSH 隧道不可用", "instance", inst.InstanceName, "err", err, "fallback_err", e)
			// 连失败转发器都建不起来：给一个必然解析失败的主机名，错误里带上原因
			c.Host, c.Port = "ssh-tunnel-unavailable.invalid", 1
			return c
		}
		c.Host, c.Port = h, p
		return c
	}
	t := inst.Tunnel
	if t == nil && inst.TunnelID != nil {
		if r.Load == nil {
			return fail(fmt.Sprint("noload-", *inst.TunnelID), errors.New("SSH 隧道配置无法读取"))
		}
		var err error
		t, err = r.Load(*inst.TunnelID)
		if err != nil {
			return fail(fmt.Sprint("load-", *inst.TunnelID), fmt.Errorf("读取 SSH 隧道配置失败：%w", err))
		}
		if t == nil {
			return fail(fmt.Sprint("missing-", *inst.TunnelID), fmt.Errorf("实例关联的 SSH 隧道（ID %d）不存在，请在实例管理里重新选择", *inst.TunnelID))
		}
	}
	cfg := r.config(t)
	h, p, err := mgr.Forward(cfg, c.Host, c.Port, proto)
	if err != nil {
		return fail("forward", err)
	}
	c.Host, c.Port = h, p
	return c
}

func (r *Resolver) config(t *model.Tunnel) Config {
	dec := func(p *string) string {
		if p == nil || r.Decrypt == nil {
			if p != nil {
				return *p
			}
			return ""
		}
		return r.Decrypt(*p)
	}
	return Config{Host: t.Host, Port: t.Port, User: dec(t.User), Password: dec(t.Password), Pkey: dec(t.Pkey), PkeyPassword: dec(t.PkeyPassword)}
}
