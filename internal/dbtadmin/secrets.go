// @author: kevinkelly
// @license: Apache Licence
// @file: secrets.go
// @time: 2026/10/28

package dbtadmin

import (
	"context"
	"io"
)

const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// Secrets 是一组随机生成的密钥。
type Secrets struct {
	SecretKey        string // SECRET_KEY（≥ 50 字符；这里 64）
	MirageSecretKey  string // 加密实例账号密码等字段的密钥（≥ 32 字符；这里 64）
	MirageCipherMode string // 固定 ECB
	MirageCipherIV   string // 16 字节（ECB 模式不用，CBC 才用；仍随机生成，避免沿用公开的默认值）
}

// GenerateSecrets 用 r（生产为 crypto/rand）生成一组密钥。字符集只含字母数字，复制进 env 文件 / shell 不需要转义。
func GenerateSecrets(r io.Reader) (Secrets, error) {
	var s Secrets
	var err error
	if s.SecretKey, err = randomString(r, 64, alnum); err != nil {
		return s, err
	}
	if s.MirageSecretKey, err = randomString(r, 64, alnum); err != nil {
		return s, err
	}
	if s.MirageCipherIV, err = randomString(r, 16, alnum); err != nil {
		return s, err
	}
	s.MirageCipherMode = "ECB"
	return s, nil
}

// RunGenSecrets 生成并打印密钥：stdout 只有 KEY=VALUE 四行（便于脚本捕获），提示信息走 stderr。
func (e *Env) RunGenSecrets(_ context.Context) int {
	s, err := GenerateSecrets(e.rand())
	if err != nil {
		e.err("错误：生成随机数失败：%v\n", err)
		return ExitFailed
	}
	e.out("SECRET_KEY=%s\nMIRAGE_SECRET_KEY=%s\nMIRAGE_CIPHER_MODE=%s\nMIRAGE_CIPHER_IV=%s\n", s.SecretKey, s.MirageSecretKey, s.MirageCipherMode, s.MirageCipherIV)
	e.err("\n以上密钥由 crypto/rand 生成，只显示这一次，没有写入任何文件。\n" +
		"  - 立即存入密码库 / 保密的配置，再写进 /etc/dbtool/dbtool.env（权限 0640，不要放进工单 / 聊天 / 仓库）；\n" +
		"  - MIRAGE_* 一旦开始使用（保存了实例账号等密文）就不能再改，改了所有密文都解不开；SECRET_KEY 同理；\n" +
		"  - MIRAGE_CIPHER_MODE 固定为 ECB，新部署保持 ECB 即可。\n")
	return ExitOK
}
