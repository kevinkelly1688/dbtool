// @author: kevinkelly
// @license: Apache Licence
// @file: engineconn_test.go
// @time: 2026/10/28

package store

import (
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"dbtool/internal/crypto"
	"dbtool/internal/model"
	"dbtool/internal/sshtunnel"
)

func testMirage(t *testing.T) *crypto.Mirage {
	t.Helper()
	m, err := crypto.NewMirage("0123456789abcdef0123456789abcdef", "ECB", "")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// 未配置隧道的实例：EngineConn 的结果与改造前完全一致，且不会碰元数据库（DB 为 nil）也不会建立转发器。
func TestEngineConnWithoutTunnelUnchanged(t *testing.T) {
	m := testMirage(t)
	s := &Store{Mirage: m} // DB 为 nil：任何数据库访问都会 panic
	inst := &model.Instance{ID: 1, Host: "10.0.0.5", Port: 3307, User: m.Encrypt("dbu"), Password: m.Encrypt("dbp"), DBName: "shop",
		Charset: "utf8mb4", IsSSL: true, VerifySSL: true}
	c := s.EngineConn(inst)
	if c.Host != "10.0.0.5" || c.Port != 3307 || c.User != "dbu" || c.Password != "dbp" || c.DBName != "shop" ||
		c.Charset != "utf8mb4" || !c.SSL || !c.VerifySSL {
		t.Fatalf("%+v", c)
	}
}

// 配置了隧道的实例：Host/Port 被改写为本地转发器地址，经它能连到目标；凭据与其余字段不变。
func TestEngineConnWithTunnelRoutesThroughForwarder(t *testing.T) {
	t.Cleanup(sshtunnel.Shutdown)
	m := testMirage(t)
	s := &Store{Mirage: m}

	// 目标：回显服务
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	eh, ep, _ := net.SplitHostPort(echo.Addr().String())
	epn, _ := strconv.Atoi(ep)

	// 跳板机：密码登录，只做 direct-tcpip
	sshAddr := startPasswordSSH(t, "tunnel", "tunnel-pw")
	sh, sp, _ := net.SplitHostPort(sshAddr)
	spn, _ := strconv.Atoi(sp)

	tid := uint(1)
	inst := &model.Instance{ID: 1, DBType: "mysql", Host: eh, Port: epn, User: m.Encrypt("dbu"), Password: m.Encrypt("dbp"), DBName: "shop",
		TunnelID: &tid,
		Tunnel:   &model.Tunnel{ID: 1, Host: sh, Port: spn, User: strptr(m.Encrypt("tunnel")), Password: strptr(m.Encrypt("tunnel-pw"))}}
	c := s.EngineConn(inst)
	if c.Host != "127.0.0.1" || c.Port == epn || c.Port == 0 {
		t.Fatalf("应改写为本地转发器: %+v", c)
	}
	if c.User != "dbu" || c.Password != "dbp" || c.DBName != "shop" {
		t.Fatalf("凭据与库名不变: %+v", c)
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(c.Host, strconv.Itoa(c.Port)), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = conn.Write([]byte("hi"))
	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "hi" {
		t.Fatalf("经隧道回显: %q %v", buf, err)
	}
}

func strptr(s string) *string { return &s }

func startPasswordSSH(t *testing.T, user, password string) string {
	t.Helper()
	_, hostPriv, _ := generateKey()
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
		if c.User() == user && string(pw) == password {
			return nil, nil
		}
		return nil, io.ErrUnexpectedEOF
	}}
	cfg.AddHostKey(hostSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				sc, chans, reqs, err := ssh.NewServerConn(nc, cfg)
				if err != nil {
					nc.Close()
					return
				}
				defer sc.Close()
				go ssh.DiscardRequests(reqs)
				for nch := range chans {
					if nch.ChannelType() != "direct-tcpip" {
						_ = nch.Reject(ssh.UnknownChannelType, "no")
						continue
					}
					var p struct {
						Host     string
						Port     uint32
						OrigHost string
						OrigPort uint32
					}
					_ = ssh.Unmarshal(nch.ExtraData(), &p)
					dst, err := net.Dial("tcp", net.JoinHostPort(p.Host, strconv.Itoa(int(p.Port))))
					if err != nil {
						_ = nch.Reject(ssh.ConnectionFailed, err.Error())
						continue
					}
					ch, chReqs, _ := nch.Accept()
					go ssh.DiscardRequests(chReqs)
					go func() { defer ch.Close(); defer dst.Close(); _, _ = io.Copy(ch, dst) }()
					go func() { defer ch.Close(); defer dst.Close(); _, _ = io.Copy(dst, ch) }()
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func generateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}
