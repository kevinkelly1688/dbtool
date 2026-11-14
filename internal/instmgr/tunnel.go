// @author: kevinkelly
// @license: Apache Licence
// @file: tunnel.go
// @time: 2026/10/28

package instmgr

import (
	"context"
	"net"

	"golang.org/x/crypto/ssh"

	"dbtool/internal/sshtunnel"
)

// dialSSH 按实例配置的 SSH 隧道（dbt_ssh_tunnel 表）登录跳板机，支持密码或私钥内容（pkey）；
// 私钥文件路径（pkey_path）暂不支持。
//
// 实现在公共包 internal/sshtunnel（其他模块通过 store.EngineConn 走同一套隧道代码）。
// 实例管理自己的管理连接仍是“按需登录、用完关闭”，不经过本地转发器。
//
// 不校验跳板机的主机公钥：平台没有地方保存 known_hosts。
// 这意味着无法防御到跳板机的中间人攻击，上线说明里已列为已知限制。
func dialSSH(ctx context.Context, t *TunnelInfo) (*ssh.Client, error) {
	return sshtunnel.Dial(ctx, sshtunnel.Config{Host: t.Host, Port: t.Port, User: t.User, Password: t.Password,
		Pkey: t.Pkey, PkeyPassword: t.PkeyPassword}, connectTimeout)
}

// sshDialFunc 让 MySQL 驱动通过 SSH 客户端拨号到目标地址（相当于本地端口转发，但不占用本地端口）。
func sshDialFunc(cli *ssh.Client) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return sshtunnel.DialFunc(cli)
}
