// @author: kevinkelly
// @license: Apache Licence
// @file: config_test.go
// @time: 2026/10/28

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dbtool/internal/crypto"
)

// 每个用例都隔离环境：不受开发机上已有的 LISTEN / DB_TOOL_CONFIG 影响，也不会读到工作目录里的 dbtool.toml。
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "mysql://u:p@127.0.0.1:3306/archery")
	t.Setenv("LISTEN", "")
	t.Setenv(ConfigFileEnv, "")
	t.Setenv("PUBLIC_URL", "")
	t.Chdir(t.TempDir())
}

func writeConf(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "dbtool.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultsWithoutFile(t *testing.T) {
	isolate(t)
	c, err := LoadFile("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8000" || c.ListenFrom != "默认值" || c.ConfigFile != "" || c.PublicURL != "http://127.0.0.1:8000" {
		t.Errorf("默认值: %+v", c)
	}
}

func TestPortFromFile(t *testing.T) {
	isolate(t)
	p := writeConf(t, "[server]\nport = 9090\n")
	c, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9090" || c.ConfigFile != p || !strings.Contains(c.ListenFrom, "配置文件") || c.PublicURL != "http://127.0.0.1:9090" {
		t.Errorf("端口应取自配置文件: %+v", c)
	}
}

func TestHostAndPortFromFile(t *testing.T) {
	isolate(t)
	c, err := LoadFile(writeConf(t, "[server]\nhost = \"10.1.2.3\"\nport = 8081\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "10.1.2.3:8081" || c.PublicURL != "http://10.1.2.3:8081" {
		t.Errorf("host+port: %s %s", c.Listen, c.PublicURL)
	}
	// 只写 host 不写 port：端口回落到默认 8000
	c, err = LoadFile(writeConf(t, "[server]\nhost = \"127.0.0.1\"\n"))
	if err != nil || c.Listen != "127.0.0.1:8000" {
		t.Errorf("只写 host: %v %v", c, err)
	}
}

func TestSiteFromFile(t *testing.T) {
	isolate(t)
	c, err := LoadFile("")
	if err != nil || c.Version != "" || c.ICP != "" || c.ICPURL != DefaultICPURL {
		t.Errorf("没有配置文件时版本 / 备案号为空，备案链接为默认值: %+v %v", c, err)
	}
	c, err = LoadFile(writeConf(t, "[site]\nversion = \" 1.05 \"\nicp = \"京ICP备12345678号-1\"\nicp_url = \"http://example.com/beian\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != "1.05" || c.ICP != "京ICP备12345678号-1" || c.ICPURL != "http://example.com/beian" {
		t.Errorf("[site] 应被读入（首尾空白去掉）: %q %q %q", c.Version, c.ICP, c.ICPURL)
	}
	// 只写了备案号：链接用默认值；[site] 与 [server] 互不影响
	c, err = LoadFile(writeConf(t, "[server]\nport = 9090\n[site]\nicp = \"沪ICP备1号\"\n"))
	if err != nil || c.ICPURL != DefaultICPURL || c.Listen != ":9090" || c.Version != "" {
		t.Errorf("只写 icp: %+v %v", c, err)
	}
}

// 随包发布的示例文件必须能被程序加载；把 [site] 里注释掉的三项取消注释后同样要能加载。
func TestExampleFileLoads(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "configs", "dbtool.example.toml")) // 先读：isolate 会切换工作目录
	if err != nil {
		t.Fatal(err)
	}
	isolate(t)
	c, err := LoadFile(writeConf(t, string(raw)))
	if err != nil || c.Listen != ":8866" || c.Version != "" || c.ICP != "" {
		t.Fatalf("原样加载示例文件: %+v %v", c, err)
	}
	body := string(raw)
	for _, key := range []string{"version", "icp", "icp_url"} {
		if !strings.Contains(body, "# "+key+" = ") {
			t.Fatalf("示例文件缺少 [site] 的 %s", key)
		}
		body = strings.Replace(body, "# "+key+" = ", key+" = ", 1)
	}
	c, err = LoadFile(writeConf(t, body))
	if err != nil || c.Version != "1.05" || c.ICP != "京ICP备12345678号-1" || c.ICPURL != DefaultICPURL {
		t.Fatalf("取消注释后加载示例文件: %+v %v", c, err)
	}
}

func TestInvalidICPURL(t *testing.T) {
	isolate(t)
	for _, u := range []string{"javascript:alert(1)", "beian.miit.gov.cn", "ftp://example.com", "https://"} {
		if _, err := LoadFile(writeConf(t, "[site]\nicp = \"x\"\nicp_url = \""+u+"\"\n")); err == nil || !strings.Contains(err.Error(), "site.icp_url") {
			t.Errorf("icp_url=%q 应报错，得到 %v", u, err)
		}
	}
	if _, err := LoadFile(writeConf(t, "[site]\nicpp = \"x\"\n")); err == nil || !strings.Contains(err.Error(), "不认识") {
		t.Errorf("[site] 里拼错的键应报错: %v", err)
	}
}

func TestEnvOverridesFileWithWarning(t *testing.T) {
	isolate(t)
	t.Setenv("LISTEN", "0.0.0.0:7000")
	c, err := LoadFile(writeConf(t, "[server]\nport = 9090\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "0.0.0.0:7000" || c.ListenFrom != "环境变量 LISTEN" {
		t.Errorf("环境变量应优先: %+v", c)
	}
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "覆盖") {
		t.Errorf("端口冲突应给出提醒: %v", c.Warnings)
	}
	// 两边一致不提醒
	t.Setenv("LISTEN", ":9090")
	c, err = LoadFile(writeConf(t, "[server]\nport = 9090\n"))
	if err != nil || len(c.Warnings) != 0 {
		t.Errorf("一致时不应提醒: %v %v", c.Warnings, err)
	}
}

func TestInvalidPort(t *testing.T) {
	isolate(t)
	for _, port := range []string{"0", "70000", "-1"} {
		if _, err := LoadFile(writeConf(t, "[server]\nport = "+port+"\n")); err == nil || !strings.Contains(err.Error(), "server.port") {
			t.Errorf("port=%s 应报错: %v", port, err)
		}
	}
	// 类型不对（字符串）
	if _, err := LoadFile(writeConf(t, "[server]\nport = \"abc\"\n")); err == nil {
		t.Error("字符串端口应报错")
	}
	t.Setenv("LISTEN", "not-an-addr")
	if _, err := LoadFile(""); err == nil || !strings.Contains(err.Error(), "LISTEN") {
		t.Errorf("非法 LISTEN 应报错: %v", err)
	}
}

func TestUnknownKeyAndSyntaxErrors(t *testing.T) {
	isolate(t)
	// 拼错键名不能悄悄忽略
	if _, err := LoadFile(writeConf(t, "[server]\nprot = 9090\n")); err == nil || !strings.Contains(err.Error(), "不认识") {
		t.Errorf("未知键应报错: %v", err)
	}
	if _, err := LoadFile(writeConf(t, "[server\nport = 1\n")); err == nil || !strings.Contains(err.Error(), "第") {
		t.Errorf("语法错误应带行列号: %v", err)
	}
}

func TestExplicitFileMustExist(t *testing.T) {
	isolate(t)
	if _, err := LoadFile(filepath.Join(t.TempDir(), "nope.toml")); err == nil {
		t.Error("显式指定的配置文件不存在应报错")
	}
	t.Setenv(ConfigFileEnv, filepath.Join(t.TempDir(), "nope.toml"))
	if _, err := LoadFile(""); err == nil {
		t.Error("DB_TOOL_CONFIG 指向的文件不存在应报错")
	}
}

func TestEnvVarPointsToFileAndCwdDefault(t *testing.T) {
	isolate(t)
	t.Setenv(ConfigFileEnv, writeConf(t, "[server]\nport = 9191\n"))
	c, err := LoadFile("")
	if err != nil || c.Listen != ":9191" {
		t.Fatalf("DB_TOOL_CONFIG: %v %v", c, err)
	}
	// 命令行路径优先于环境变量
	c, err = LoadFile(writeConf(t, "[server]\nport = 9292\n"))
	if err != nil || c.Listen != ":9292" {
		t.Fatalf("-config 应优先: %v %v", c, err)
	}
	// 没有显式指定时，当前目录的 dbtool.toml 自动生效
	t.Setenv(ConfigFileEnv, "")
	if err := os.WriteFile("dbtool.toml", []byte("[server]\nport = 9393\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = LoadFile("")
	if err != nil || c.Listen != ":9393" || c.ConfigFile != "dbtool.toml" {
		t.Fatalf("当前目录 dbtool.toml: %v %v", c, err)
	}
}

func TestMissingDatabaseURLStillRequired(t *testing.T) {
	isolate(t)
	t.Setenv("DATABASE_URL", "")
	if _, err := LoadFile(""); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("DATABASE_URL 仍应必填: %v", err)
	}
}

// DATABASE_URL 的密码可以是 ENC(密文)：用 MIRAGE_SECRET_KEY（缺省 SECRET_KEY）解开后连库。
func TestMySQLDSNEncryptedPassword(t *testing.T) {
	key := "0123456789abcdef0123456789abcdef-TESTKEY"
	m, err := crypto.NewMirage(key, "ECB", "1234567890abcdef")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := m.EncryptURL("mysql://dbtool:p%40ss@127.0.0.1:3306/dbtool")
	if err != nil {
		t.Fatal(err)
	}
	c := &Config{DatabaseURL: enc, MirageSecretKey: key, MirageCipherMode: "ECB", MirageCipherIV: "1234567890abcdef"}
	dsn, err := c.MySQLDSN()
	if err != nil || !strings.HasPrefix(dsn, "dbtool:p@ss@tcp(127.0.0.1:3306)/dbtool?") {
		t.Fatalf("DSN: %q %v", dsn, err)
	}
	// 密钥不对：报错，不拿密文去连库，错误里不带连接串
	c2 := &Config{DatabaseURL: enc, MirageSecretKey: "another-key-another-key-another-key-0000", MirageCipherMode: "ECB", MirageCipherIV: "1234567890abcdef"}
	if _, err := c2.MySQLDSN(); err == nil || strings.Contains(err.Error(), "ENC(") {
		t.Errorf("密钥不对应报错且不回显密文: %v", err)
	}
	// 明文密码照旧
	c3 := &Config{DatabaseURL: "mysql://u:plain@h:3306/d"}
	if dsn, err := c3.MySQLDSN(); err != nil || !strings.HasPrefix(dsn, "u:plain@tcp(h:3306)/d?") {
		t.Errorf("明文: %q %v", dsn, err)
	}
}
