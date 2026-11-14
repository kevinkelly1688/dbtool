// @author: kevinkelly
// @license: Apache Licence
// @file: encrypturl.go
// @time: 2026/10/28

package dbtadmin

import (
	"errors"
	"flag"
	"strings"

	"dbtool/internal/crypto"
)

// cmdEncryptURL 实现 encrypt-url：把环境变量 DATABASE_URL 里的明文密码加密成 ENC(密文) 后打印。
// 结果可直接写进 dbtool.env 的 DATABASE_URL（dbtool 与 dbt-admin 都认）；只往标准输出打印这一行，提示在标准错误。
func (e *Env) cmdEncryptURL(args []string) int {
	_, done, code := e.parseFlags("encrypt-url", "把环境变量 DATABASE_URL 里的明文密码用 MIRAGE_SECRET_KEY（缺省 SECRET_KEY）加密成 ENC(密文) 并打印；\n"+
		"已经是 ENC(...) 的原样打印。要在配置里使用：把打印出的整串写进 DATABASE_URL，保持 MIRAGE_SECRET_KEY 不变（换了密钥要重新加密）。", args, func(*flag.FlagSet) {})
	if done {
		return code
	}
	raw := strings.TrimSpace(e.Getenv("DATABASE_URL"))
	if raw == "" {
		e.err("错误：必须设置环境变量 DATABASE_URL（mysql://用户:URL编码的密码@主机:3306/库名）\n")
		return ExitUsage
	}
	if e.effectiveKeyIsPublic() {
		e.err("错误：当前生效的密钥是公开的缺省值，用它加密等于没加密；请先配置自己的 MIRAGE_SECRET_KEY / SECRET_KEY（dbt-admin gen-secrets 生成）\n")
		return ExitUsage
	}
	m := mirageObjFromEnv(e.Getenv)
	if m == nil {
		e.err("错误：没有可用的密钥：MIRAGE_SECRET_KEY / SECRET_KEY 未设置或无效（长度不少于 32）\n")
		return ExitUsage
	}
	out, err := m.EncryptURL(raw)
	if err != nil {
		if errors.Is(err, crypto.ErrNoKey) {
			e.err("错误：%v\n", err)
		} else {
			e.err("错误：DATABASE_URL：%v\n", err)
		}
		return ExitUsage
	}
	// 自检：加密结果能被同一把密钥还原成同一个连接参数，才打印（避免写进配置后服务起不来）
	if p1, err1 := ParseURL(raw); err1 == nil {
		p2, err2 := ParseURLWith(out, m)
		if err2 != nil || p1.Pass != p2.Pass || p1.User != p2.User || p1.Host != p2.Host || p1.DB != p2.DB {
			e.err("错误：加密后的连接串自检失败，已放弃输出\n")
			return ExitFailed
		}
	}
	e.out("%s\n", out)
	return ExitOK
}
