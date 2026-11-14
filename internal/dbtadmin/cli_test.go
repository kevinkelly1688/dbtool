// @author: kevinkelly
// @license: Apache Licence
// @file: cli_test.go
// @time: 2026/10/28

package dbtadmin

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"dbtool/internal/crypto"
)

func TestCLIHelpAndUsage(t *testing.T) {
	te := newTestEnv(t, newMemStore())
	if code := te.run("--help"); code != ExitOK {
		t.Errorf("--help: %d", code)
	}
	help := te.out.String()
	for _, w := range []string{"bootstrap", "admin create", "reset-password", "unlock", "activate", "deactivate", "admin list", "gen-secrets", "DATABASE_URL", "DBT_ADMIN_PASSWORD", "退出码"} {
		if !strings.Contains(help, w) {
			t.Errorf("帮助里缺少 %q", w)
		}
	}
	te.reset()
	if code := te.run(); code != ExitUsage {
		t.Errorf("无参数应返回 2: %d", code)
	}
	te.reset()
	if code := te.run("frobnicate"); code != ExitUsage {
		t.Errorf("未知命令应返回 2: %d", code)
	}
	te.reset()
	if code := te.run("admin"); code != ExitUsage {
		t.Errorf("admin 缺子命令应返回 2: %d", code)
	}
	te.reset()
	if code := te.run("admin", "frobnicate"); code != ExitUsage {
		t.Errorf("未知子命令应返回 2: %d", code)
	}
	te.reset()
	if code := te.run("version"); code != ExitOK || !strings.Contains(te.out.String(), "dbt-admin test") {
		t.Errorf("version: %d %q", code, te.out.String())
	}
	// 各子命令 --help 返回 0 并列出选项
	for _, c := range [][]string{{"bootstrap", "--help"}, {"status", "-h"}, {"admin", "create", "--help"}, {"admin", "reset-password", "--help"},
		{"admin", "unlock", "--help"}, {"admin", "activate", "--help"}, {"admin", "deactivate", "--help"}, {"admin", "list", "--help"}, {"gen-secrets", "--help"}} {
		te.reset()
		if code := te.run(c...); code != ExitOK {
			t.Errorf("%v: %d\n%s", c, code, te.errb.String())
		}
		if !strings.Contains(te.out.String(), "用法：dbt-admin") {
			t.Errorf("%v 的帮助不完整: %q", c, te.out.String())
		}
	}
	te.reset()
	te.run("bootstrap", "--help")
	mustContain(t, "bootstrap --help", te.out.String(), "--apply", "--only-sql", "--sql-root", "--yes", "dbt_bootstrap_applied")
	te.reset()
	te.run("admin", "create", "--help")
	mustContain(t, "create --help", te.out.String(), "--username", "--email", "--display", "--group", "DBT_ADMIN_PASSWORD")
	te.reset()
	te.run("admin", "reset-password", "--help")
	mustContain(t, "reset --help", te.out.String(), "--require-change")
}

func TestCLIRejectsPasswordOnCommandLine(t *testing.T) {
	te := newTestEnv(t, newMemStore())
	for _, args := range [][]string{
		{"admin", "create", "--username", "u", "--password", "TOPSECRETPW12"},
		{"admin", "create", "--username", "u", "--password=TOPSECRETPW12"},
		{"bootstrap", "-password", "TOPSECRETPW12"},
		{"admin", "reset-password", "--username", "u", "--database-url", "mysql://root:TOPSECRETPW12@h/db"},
	} {
		te.reset()
		if code := te.run(args...); code != ExitUsage {
			t.Errorf("%v 应返回 2: %d", args, code)
		}
		mustNotContain(t, "输出", te.all(), "TOPSECRETPW12")
		mustContain(t, "stderr", te.errb.String(), "环境变量")
	}
	// 位置参数不回显
	te.reset()
	if code := te.run("admin", "create", "--username", "u", "TOPSECRETPW12"); code != ExitUsage {
		t.Errorf("位置参数应返回 2: %d", code)
	}
	mustNotContain(t, "输出", te.all(), "TOPSECRETPW12")
	// 未知选项
	te.reset()
	if code := te.run("bootstrap", "--frob"); code != ExitUsage {
		t.Errorf("未知选项: %d", code)
	}
}

func TestCLIDatabaseURLHandling(t *testing.T) {
	te := newTestEnv(t, newMemStore())
	delete(te.env, "DATABASE_URL")
	if code := te.run("status"); code != ExitUsage {
		t.Errorf("缺 DATABASE_URL 应返回 2: %d", code)
	}
	mustContain(t, "stderr", te.errb.String(), "DATABASE_URL")
	te.reset()
	te.env["DATABASE_URL"] = "postgres://u:LEAKYPASS@h/db"
	if code := te.run("status"); code != ExitUsage {
		t.Errorf("非 mysql 连接串应返回 2: %d", code)
	}
	mustNotContain(t, "输出", te.all(), "LEAKYPASS")
	te.reset()
	te.env["DATABASE_URL"] = "mysql://u:LEAKYPASS@h:3306/bad-name"
	if code := te.run("status"); code != ExitUsage {
		t.Errorf("库名非法应返回 2: %d", code)
	}
	mustNotContain(t, "输出", te.all(), "LEAKYPASS")
	// 连接串里的 %xx 解码后进入连接参数，但输出不含
	te.reset()
	te.env["DATABASE_URL"] = testDBURL
	if code := te.run("status"); code != ExitOK {
		t.Errorf("status: %d\n%s", code, te.all())
	}
	mustNotContain(t, "输出", te.all(), "S3cretDb", "Pw@S3cret")
}

func TestAdminListNeedsNoUsernameButWriteDoes(t *testing.T) {
	te, _ := adminEnv(t)
	if code := te.run("admin", "unlock"); code != ExitUsage {
		t.Errorf("缺 --username: %d", code)
	}
	if code := te.run("admin", "list"); code != ExitOK {
		t.Errorf("list: %d", code)
	}
}

func TestGenSecrets(t *testing.T) {
	te := newTestEnv(t, nil)
	if code := te.run("gen-secrets"); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	vals := map[string]string{}
	for _, ln := range strings.Split(strings.TrimSpace(te.out.String()), "\n") {
		k, v, ok := strings.Cut(ln, "=")
		if !ok {
			t.Fatalf("stdout 只应有 KEY=VALUE 行，得到 %q", ln)
		}
		vals[k] = v
	}
	if len(vals) != 4 {
		t.Fatalf("应有 4 个键: %v", vals)
	}
	if len(vals["SECRET_KEY"]) < 50 {
		t.Errorf("SECRET_KEY 至少 50 字符: %d", len(vals["SECRET_KEY"]))
	}
	if len(vals["MIRAGE_SECRET_KEY"]) < 32 {
		t.Errorf("MIRAGE_SECRET_KEY 至少 32 字符: %d", len(vals["MIRAGE_SECRET_KEY"]))
	}
	if len(vals["MIRAGE_CIPHER_IV"]) != 16 {
		t.Errorf("MIRAGE_CIPHER_IV 必须 16 字节: %d", len(vals["MIRAGE_CIPHER_IV"]))
	}
	if vals["MIRAGE_CIPHER_MODE"] != "ECB" {
		t.Errorf("MIRAGE_CIPHER_MODE = %q", vals["MIRAGE_CIPHER_MODE"])
	}
	if vals["SECRET_KEY"] == vals["MIRAGE_SECRET_KEY"] {
		t.Error("SECRET_KEY 与 MIRAGE_SECRET_KEY 不应相同")
	}
	// 提示在 stderr，stdout 干净
	mustContain(t, "stderr", te.errb.String(), "存入密码库", "不能再改", "ECB")
	mustNotContain(t, "stderr", te.errb.String(), vals["SECRET_KEY"], vals["MIRAGE_SECRET_KEY"], vals["MIRAGE_CIPHER_IV"])
	// 生成的密钥能被 Mirage 实现接受
	if _, err := crypto.NewMirage(vals["MIRAGE_SECRET_KEY"], vals["MIRAGE_CIPHER_MODE"], vals["MIRAGE_CIPHER_IV"]); err != nil {
		t.Errorf("生成的 MIRAGE_* 不被 crypto.NewMirage 接受: %v", err)
	}
}

func TestGenSecretsEntropyAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		var out, errb bytes.Buffer
		te := newTestEnv(t, nil)
		te.Env.Stdout, te.Env.Stderr = &out, &errb
		te.Env.Rand = nil // 真实 crypto/rand
		if code := te.run("gen-secrets"); code != ExitOK {
			t.Fatal(code)
		}
		for _, ln := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			k, v, _ := strings.Cut(ln, "=")
			if k == "MIRAGE_CIPHER_MODE" {
				continue
			}
			if seen[v] {
				t.Fatalf("重复的随机值 %s", k)
			}
			seen[v] = true
			// 字符集：只有字母数字
			for _, r := range v {
				if !strings.ContainsRune(alnum, r) {
					t.Fatalf("%s 含非字母数字字符 %q", k, r)
				}
			}
			// 粗略的熵检查：64 位的值至少出现 20 种不同字符
			if len(v) == 64 {
				d := map[rune]bool{}
				for _, r := range v {
					d[r] = true
				}
				if len(d) < 20 {
					t.Fatalf("%s 的字符种类过少（%d）", k, len(d))
				}
			}
		}
	}
}

func TestRandomStringUniformRejection(t *testing.T) {
	// 构造输入：前两个字节 >= 248（超出 62*4 的上限）必须被丢弃
	r := bytes.NewReader([]byte{255, 250, 0, 1, 61, 62, 124, 247, 0, 0, 0, 0})
	s, err := randomString(r, 6, alnum)
	if err != nil {
		t.Fatal(err)
	}
	want := string([]byte{alnum[0], alnum[1], alnum[61], alnum[0], alnum[0], alnum[61]})
	if s != want {
		t.Errorf("得到 %q，期望 %q", s, want)
	}
	// 熵源耗尽返回错误，不返回部分结果
	if _, err := randomString(bytes.NewReader([]byte{1, 2, 3}), 10, alnum); err == nil {
		t.Error("随机源不足应报错")
	}
}

func TestEncryptURLCommand(t *testing.T) {
	te := newTestEnv(t, newMemStore())
	te.env["DATABASE_URL"] = "mysql://dbtool:s3cret%21@127.0.0.1:3306/dbtool"

	// 公开缺省密钥 / 没有密钥：拒绝
	te.env["MIRAGE_SECRET_KEY"], te.env["SECRET_KEY"] = "", ""
	if code := te.run("encrypt-url"); code != ExitUsage {
		t.Errorf("没有密钥应拒绝: %d %s", code, te.errb.String())
	}
	te.reset()
	te.env["MIRAGE_SECRET_KEY"] = "0123456789abcdef0123456789abcdef-TESTKEY"
	if code := te.run("encrypt-url"); code != ExitOK {
		t.Fatalf("encrypt-url: %d %s", code, te.errb.String())
	}
	out := strings.TrimSpace(te.out.String())
	if strings.Contains(out, "s3cret") || !strings.Contains(out, "ENC(") || strings.Contains(te.errb.String(), "s3cret") {
		t.Fatalf("输出里不能有明文密码: %q", out)
	}
	// 输出的连接串 dbt-admin 自己能用：解析出同样的密码
	m, merr := crypto.NewMirage(te.env["MIRAGE_SECRET_KEY"], "ECB", "")
	if merr != nil {
		t.Fatal(merr)
	}
	p, err := ParseURLWith(out, m)
	if err != nil || p.Pass != "s3cret!" || p.User != "dbtool" || p.Host != "127.0.0.1" || p.DB != "dbtool" {
		t.Fatalf("解析: %+v %v", p, err)
	}
	// 用密文连接串走一次真实流程（status）：连接参数里是明文密码
	te.reset()
	te.env["DATABASE_URL"] = out
	var got ConnParams
	open := te.Open
	te.Open = func(ctx context.Context, cp ConnParams) (Store, error) { got = cp; return open(ctx, cp) }
	if code := te.run("status"); code != ExitOK {
		t.Fatalf("status: %d %s", code, te.errb.String())
	}
	if got.Pass != "s3cret!" {
		t.Errorf("Open 收到的应是解密后的密码: %q", got.Pass)
	}
	// 密钥不对：给出明确提示，不会拿密文去连库
	te.reset()
	te.env["DATABASE_URL"] = out
	te.env["MIRAGE_SECRET_KEY"] = "another-key-another-key-another-key-0000"
	if code := te.run("status"); code == ExitOK || !strings.Contains(te.errb.String(), "解不开") {
		t.Errorf("密钥不对应报错: %d %s", code, te.errb.String())
	}
}
