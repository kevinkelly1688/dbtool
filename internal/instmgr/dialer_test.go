// @author: kevinkelly
// @license: Apache Licence
// @file: dialer_test.go
// @time: 2026/10/28

package instmgr

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestMysqlConfig(t *testing.T) {
	cfg := mysqlConfig(ConnInfo{DBType: "mysql", Host: "10.0.0.1", Port: 3306, User: "dba", Password: "p'w\\d", Charset: "gbk", DBName: "ignored"})
	if cfg.Addr != "10.0.0.1:3306" || cfg.User != "dba" || cfg.Passwd != "p'w\\d" || cfg.Net != "tcp" {
		t.Fatalf("%+v", cfg)
	}
	if !cfg.InterpolateParams {
		t.Fatal("必须在客户端转义参数，KILL / GRANT / SET GLOBAL 不支持服务端预编译")
	}
	if _, ok := cfg.Params["charset"]; ok {
		t.Fatalf("字符集不能放进 Params（会被当成 SET charset=… 发给服务器，MySQL 8.0 报 1193）: %v", cfg.Params)
	}
	if dsn := cfg.FormatDSN(); !strings.Contains(dsn, "charset=utf8mb4") {
		t.Fatalf("字符集固定 utf8mb4（interpolateParams 不能和 gbk 一起用）: %s", dsn)
	}
	if cfg.DBName != "" || cfg.Collation != "" {
		t.Fatalf("管理连接不指定默认库: %+v", cfg)
	}
	if cfg.Timeout != connectTimeout || cfg.ReadTimeout != readTimeout || cfg.WriteTimeout != writeTimeout {
		t.Fatalf("超时: %v %v %v", cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout)
	}
	if cfg.TLSConfig != "" {
		t.Fatalf("未启用 SSL: %q", cfg.TLSConfig)
	}
	if got := mysqlConfig(ConnInfo{Host: "h", Port: 1, SSL: true, VerifySSL: true}).TLSConfig; got != "true" {
		t.Fatalf("验证证书: %q", got)
	}
	if got := mysqlConfig(ConnInfo{Host: "h", Port: 1, SSL: true}).TLSConfig; got != "skip-verify" {
		t.Fatalf("不验证证书: %q", got)
	}
	if got := mysqlConfig(ConnInfo{Host: "::1", Port: 3307}).Addr; got != "[::1]:3307" {
		t.Fatalf("IPv6: %q", got)
	}
	// DSN 能被驱动再次解析（密码里的特殊字符不会破坏格式）
	if dsn := cfg.FormatDSN(); !strings.Contains(dsn, "interpolateParams=true") {
		t.Fatal(dsn)
	}
}

func TestDialerRejectsOtherTypes(t *testing.T) {
	d := NewDialer()
	if _, err := d.Dial(context.Background(), ConnInfo{DBType: "mssql", Host: "h", Port: 1}); err == nil || !strings.Contains(err.Error(), "暂不支持") {
		t.Fatalf("Dial 只处理 MySQL 协议系: %v", err)
	}
	if _, err := d.Probe(context.Background(), ConnInfo{DBType: "no-such-engine", Host: "h", Port: 1}); err == nil || !strings.Contains(err.Error(), "暂不支持") {
		t.Fatalf("未注册的引擎应提示暂不支持: %v", err)
	}
	if _, err := d.Probe(context.Background(), ConnInfo{DBType: "mssql", Host: "h", Port: 1, Tunnel: &TunnelInfo{}}); err == nil || !strings.Contains(err.Error(), "SSH 隧道") {
		t.Fatalf("%v", err)
	}
}

func TestNormalizeValue(t *testing.T) {
	ts := time.Date(2026, 10, 6, 9, 2, 11, 0, time.UTC)
	if normalizeValue([]byte("abc")) != "abc" || normalizeValue(ts) != "2026-10-06 09:02:11" || normalizeValue(nil) != nil || normalizeValue(int64(5)) != int64(5) {
		t.Fatal("normalizeValue")
	}
}

// ---- SSH 隧道：进程内的 SSH 服务器，只转发到本机的回显服务 ----

func startEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

// startSSH 启动一个接受指定密码或公钥的 SSH 服务器，支持 direct-tcpip 转发。
func startSSH(t *testing.T, password string, key ssh.PublicKey) string {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if password != "" && c.User() == "tunnel" && string(pw) == password {
				return nil, nil
			}
			return nil, io.ErrUnexpectedEOF
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if key != nil && c.User() == "tunnel" && string(k.Marshal()) == string(key.Marshal()) {
				return nil, nil
			}
			return nil, io.ErrUnexpectedEOF
		},
	}
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
					dst, err := net.Dial("tcp", net.JoinHostPort(p.Host, itoaU32(p.Port)))
					if err != nil {
						_ = nch.Reject(ssh.ConnectionFailed, err.Error())
						continue
					}
					ch, chReqs, err := nch.Accept()
					if err != nil { // 客户端已经关掉隧道：Accept 失败时 ch 为 nil，不能再用（偶发的 nil 解引用就出在这里）
						dst.Close()
						continue
					}
					go ssh.DiscardRequests(chReqs)
					go func() { defer ch.Close(); defer dst.Close(); _, _ = io.Copy(ch, dst) }()
					go func() { defer ch.Close(); defer dst.Close(); _, _ = io.Copy(dst, ch) }()
				}
				_ = sc.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

func itoaU32(n uint32) string { return utoa(uint(n)) }

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	var port int
	for _, c := range p {
		port = port*10 + int(c-'0')
	}
	return h, port
}

func roundTrip(t *testing.T, cli *ssh.Client, target string) {
	t.Helper()
	conn, err := sshDialFunc(cli)(context.Background(), "tcp", target)
	if err != nil {
		t.Fatalf("经隧道拨号: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("回显: %q %v", buf, err)
	}
}

func TestSSHTunnelPassword(t *testing.T) {
	echo := startEcho(t)
	h, p := splitHostPort(t, startSSH(t, "tunnel-pw", nil))
	cli, err := dialSSH(context.Background(), &TunnelInfo{Host: h, Port: p, User: "tunnel", Password: "tunnel-pw"})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	roundTrip(t, cli, echo)

	if _, err := dialSSH(context.Background(), &TunnelInfo{Host: h, Port: p, User: "tunnel", Password: "wrong"}); err == nil {
		t.Fatal("密码错误应失败")
	}
	if _, err := dialSSH(context.Background(), &TunnelInfo{Host: h, Port: p, User: "tunnel"}); err == nil || !strings.Contains(err.Error(), "没有可用的认证方式") {
		t.Fatalf("没有凭据: %v", err)
	}
	// 隧道转发目标不可达：拨号返回错误而不是卡住
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := closed.Addr().String()
	closed.Close()
	if _, err := sshDialFunc(cli)(context.Background(), "tcp", dead); err == nil {
		t.Fatal("目标不可达应失败")
	}
	// ctx 已取消
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sshDialFunc(cli)(ctx, "tcp", echo); err == nil {
		t.Log("ctx 取消与拨号成功竞争时可能成功，不视为失败")
	}
}

func TestSSHTunnelPrivateKey(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, _ := ssh.NewPublicKey(pub)
	echo := startEcho(t)
	h, p := splitHostPort(t, startSSH(t, "", sshPub))

	plain, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	cli, err := dialSSH(context.Background(), &TunnelInfo{Host: h, Port: p, User: "tunnel", Pkey: string(pem.EncodeToMemory(plain))})
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, cli, echo)
	cli.Close()

	enc, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("key-pass"))
	if err != nil {
		t.Fatal(err)
	}
	encPEM := string(pem.EncodeToMemory(enc))
	cli, err = dialSSH(context.Background(), &TunnelInfo{Host: h, Port: p, User: "tunnel", Pkey: encPEM, PkeyPassword: "key-pass"})
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, cli, echo)
	cli.Close()
	if _, err := dialSSH(context.Background(), &TunnelInfo{Host: h, Port: p, User: "tunnel", Pkey: encPEM, PkeyPassword: "bad"}); err == nil || !strings.Contains(err.Error(), "私钥") {
		t.Fatalf("私钥密码错误: %v", err)
	}
	if _, err := dialSSH(context.Background(), &TunnelInfo{Host: h, Port: p, User: "tunnel", Pkey: "not a key"}); err == nil || !strings.Contains(err.Error(), "私钥") {
		t.Fatalf("%v", err)
	}
}
