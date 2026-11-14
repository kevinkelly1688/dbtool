// @author: kevinkelly
// @license: Apache Licence
// @file: env.go
// @time: 2026/10/28

package dbtadmin

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"
	"time"

	"dbtool/internal/crypto"
)

// 退出码。
const (
	ExitOK     = 0
	ExitFailed = 1
	ExitUsage  = 2
)

// DefaultIterations 是新密码的 pbkdf2 迭代次数，与 dbtool 现有一致（internal/sysadmin/service.go 的 NewService）。
// 测试里有一条用例断言它与 sysadmin.NewService 的取值相同，避免两边漂移。
const DefaultIterations = 390000

// PasswordEnv 是提供管理员密码的环境变量。
const PasswordEnv = "DBT_ADMIN_PASSWORD"

// Env 是命令运行所需的外部依赖。生产用 DefaultEnv；测试里替换成内存实现。
type Env struct {
	Getenv         func(string) string
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// IsTTY 报告标准输入是否是终端（决定能否交互提问）。
	IsTTY func() bool
	// ReadPassword 在终端里无回显地读一行。
	ReadPassword func(prompt string) (string, error)
	Open         func(ctx context.Context, p ConnParams) (Store, error)
	Now          func() time.Time
	OSUser       func() string
	Version      string
	// Iterations 为 0 时用 DefaultIterations。
	Iterations int
	Rand       io.Reader
	// Decrypt 解系统设置里的 mirage 密文（取锁定策略用）；nil 表示没有配置密钥，按明文处理。
	Decrypt func(string) string

	in *bufio.Reader
}

// DefaultEnv 返回连接真实环境的 Env。
func DefaultEnv(version string) *Env {
	e := &Env{
		Getenv:  os.Getenv,
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Open:    OpenMySQL,
		Now:     time.Now,
		Version: version,
		Rand:    rand.Reader,
	}
	e.IsTTY = stdinIsTerminal
	e.ReadPassword = readPasswordNoEcho
	e.OSUser = func() string {
		if u, err := user.Current(); err == nil && u.Username != "" {
			// Windows 下形如 HOST\name：只取名字部分
			if i := strings.LastIndexAny(u.Username, `\/`); i >= 0 {
				return u.Username[i+1:]
			}
			return u.Username
		}
		if v := os.Getenv("USER"); v != "" {
			return v
		}
		return "unknown"
	}
	e.Decrypt = mirageFromEnv(os.Getenv)
	return e
}

// mirageObjFromEnv 按与 dbtool 相同的规则（MIRAGE_SECRET_KEY，缺省用 SECRET_KEY）构造 Mirage；没有密钥或密钥无效返回 nil。
func mirageObjFromEnv(getenv func(string) string) *crypto.Mirage {
	key := getenv("MIRAGE_SECRET_KEY")
	if key == "" {
		key = getenv("SECRET_KEY")
	}
	if key == "" {
		return nil
	}
	mode, iv := getenv("MIRAGE_CIPHER_MODE"), getenv("MIRAGE_CIPHER_IV")
	if iv == "" {
		iv = "1234567890abcdef"
	}
	m, err := crypto.NewMirage(key, mode, iv)
	if err != nil {
		return nil
	}
	return m
}

// mirageFromEnv 同 mirageObjFromEnv，返回解密函数；没有密钥或密钥无效返回 nil。
func mirageFromEnv(getenv func(string) string) func(string) string {
	m := mirageObjFromEnv(getenv)
	if m == nil {
		return nil
	}
	return m.Decrypt
}

func (e *Env) iterations() int {
	if e.Iterations > 0 {
		return e.Iterations
	}
	return DefaultIterations
}

func (e *Env) rand() io.Reader {
	if e.Rand != nil {
		return e.Rand
	}
	return rand.Reader
}

func (e *Env) out(format string, a ...any) { fmt.Fprintf(e.Stdout, format, a...) }
func (e *Env) err(format string, a ...any) { fmt.Fprintf(e.Stderr, format, a...) }

func (e *Env) readLine(prompt string) (string, error) {
	if e.in == nil {
		e.in = bufio.NewReader(e.Stdin)
	}
	fmt.Fprint(e.Stdout, prompt)
	s, err := e.in.ReadString('\n')
	if err != nil && (err != io.EOF || s == "") {
		return "", err
	}
	return strings.TrimSpace(s), nil
}

// errNeedConfirm 表示写操作既没有 --yes 也没有可交互的终端。
var errNeedConfirm = errors.New("写入前需要 --yes，或在终端里交互确认")

// confirm 在写入前确认：yes 为真直接通过；否则必须是终端并输入 y / yes。
func (e *Env) confirm(yes bool, what string) (bool, error) {
	if yes {
		return true, nil
	}
	if !e.IsTTY() {
		return false, errNeedConfirm
	}
	ans, err := e.readLine(what + " 输入 yes 继续：")
	if err != nil {
		return false, err
	}
	switch strings.ToLower(ans) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// Target 是连接串里不含密码的部分，用于显示。
type Target struct {
	Host, Port, DB, User string
}

func targetOf(p ConnParams) Target {
	return Target{Host: p.Host, Port: p.Port, DB: p.DB, User: p.User}
}

func (t Target) HostPort() string { return t.Host + ":" + t.Port }

func (t Target) String() string {
	return fmt.Sprintf("库 %s @ %s（用户 %s）", t.DB, t.HostPort(), t.User)
}

// connect 读取环境变量 DATABASE_URL 并连接。错误信息已是可直接展示的中文，失败时返回非 0 的退出码。
func (e *Env) connect(ctx context.Context) (Store, Target, int) {
	raw := e.Getenv("DATABASE_URL")
	if strings.TrimSpace(raw) == "" {
		e.err("错误：必须设置环境变量 DATABASE_URL（mysql://用户:URL编码的密码@主机:3306/库名）；不接受命令行传密码\n")
		return nil, Target{}, ExitUsage
	}
	p, err := ParseURLWith(raw, mirageObjFromEnv(e.Getenv))
	if err != nil {
		e.err("错误：DATABASE_URL：%v\n", err)
		return nil, Target{}, ExitUsage
	}
	tgt := targetOf(p)
	st, err := e.Open(ctx, p)
	if err != nil {
		if errors.Is(err, ErrDatabaseMissing) {
			e.err("错误：目标库 %s 在 %s 上不存在（本工具不会自动建库）。请用有建库权限的账号先执行：\n  CREATE DATABASE `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;   -- MySQL 8；5.7 / MariaDB 用 utf8mb4_general_ci\n"+
				"（表与字段都不指定字符集，全部跟随库默认；字符集必须是 utf8mb4。要与同库里其它表的排序规则保持一致时，COLLATE 写成它们的值，见 docs/charset-policy.md）\n", tgt.DB, tgt.HostPort(), tgt.DB)
		} else {
			e.err("错误：%s\n", RedactMsg(err.Error()))
		}
		return nil, tgt, ExitFailed
	}
	return st, tgt, ExitOK
}

// actor 是写审计时的操作人。
func (e *Env) actor() string { return "dbt-admin(" + e.OSUser() + ")" }
