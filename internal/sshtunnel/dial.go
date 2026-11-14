// @author: kevinkelly
// @license: Apache Licence
// @file: dial.go
// @time: 2026/10/28

// Package sshtunnel 提供 SSH 隧道的公共实现：登录跳板机、经跳板机拨号，以及供所有模块共用的“本地转发器”。
//
// 为什么要转发器：各模块通过 store.EngineConn(inst) 取得 engine.Conn（Host/Port 字符串）再各自建连接，
// EngineConn 没有错误返回值、也没有“用完关闭”的钩子，所以隧道不能做成“拨号函数”。这里的做法是：
// 为“隧道配置 + 目标 host:port”起一个只监听本机回环地址的转发器，
// EngineConn 返回改写后的 127.0.0.1:本地端口；转发器在每个入站连接上才去拨 SSH（懒连接、复用、保活、断线重连），
// 一段时间没有任何连接就自动关闭监听。
//
// 已知限制（上线说明里已列出）：不校验跳板机的主机公钥——平台没有地方保存 known_hosts，
// 因此无法防御到跳板机的中间人攻击。
package sshtunnel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"
)

// Config 是一条 SSH 隧道的连接信息（凭据已解密）。
type Config struct {
	Host         string
	Port         int
	User         string
	Password     string
	Pkey         string // 私钥内容（PEM / OpenSSH）；私钥文件路径（pkey_path）不支持
	PkeyPassword string
}

// Addr 返回跳板机地址 host:port。
func (c Config) Addr() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }

// Fingerprint 返回配置的指纹（含凭据的 SHA-256）：任何一项变化（改了跳板机、账号、密码、私钥）都会得到新值，
// 转发器据此区分；指纹不可逆，可以安全地出现在日志里（只取前缀）。
func (c Config) Fingerprint() string {
	h := sha256.New()
	for _, s := range []string{c.Host, strconv.Itoa(c.Port), c.User, c.Password, c.Pkey, c.PkeyPassword} {
		h.Write([]byte(strconv.Itoa(len(s))))
		h.Write([]byte{':'})
		h.Write([]byte(s))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Dial 按配置登录跳板机，支持密码或私钥内容。timeout 同时用于 TCP 连接，握手与认证的总时限是它的 2 倍。
//
//nolint:gosec // HostKeyCallback 见包注释
func Dial(ctx context.Context, t Config, timeout time.Duration) (*ssh.Client, error) {
	var auths []ssh.AuthMethod
	if t.Pkey != "" {
		var signer ssh.Signer
		var err error
		if t.PkeyPassword != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(t.Pkey), []byte(t.PkeyPassword))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(t.Pkey))
		}
		if err != nil {
			return nil, errors.New("SSH 私钥无法解析（需要 PEM/OpenSSH 格式；有密码保护时请填写密钥密码）")
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if t.Password != "" {
		auths = append(auths, ssh.Password(t.Password))
	}
	if len(auths) == 0 {
		return nil, errors.New("SSH 隧道没有可用的认证方式（只支持密码或私钥内容，不支持私钥文件路径）")
	}
	cfg := &ssh.ClientConfig{User: t.User, Auth: auths, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: timeout} //nolint:gosec // 见包注释
	addr := t.Addr()
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(2 * timeout)) // 握手与认证的总时限
	sc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(sc, chans, reqs), nil
}

// DialFunc 让数据库驱动通过 SSH 客户端拨号到目标地址（相当于本地端口转发，但不占用本地端口）。
// ctx 取消时立即返回；拨号最终成功了也会关掉，不留连接。
func DialFunc(cli *ssh.Client) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		type res struct {
			c   net.Conn
			err error
		}
		ch := make(chan res, 1)
		go func() {
			c, err := cli.Dial(network, addr)
			ch <- res{c, err}
		}()
		select {
		case r := <-ch:
			return r.c, r.err
		case <-ctx.Done():
			go func() {
				if r := <-ch; r.c != nil {
					_ = r.c.Close()
				}
			}()
			return nil, ctx.Err()
		}
	}
}
