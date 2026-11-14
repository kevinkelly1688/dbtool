// @author: kevinkelly
// @license: Apache Licence
// @file: config.go
// @time: 2026/10/28

// Package config 加载配置：环境变量 + 可选的配置文件（dbtool.toml）。
//
// 环境变量名：
// DATABASE_URL、SECRET_KEY、DEBUG ...
// 配置文件目前只放“部署相关、不含密钥”的设置（监听地址 / 端口），见 configs/dbtool.example.toml；
// 密码、密钥等敏感项仍然只走环境变量。同一项两边都给时，环境变量优先。
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"dbtool/internal/crypto"
)

// 没设 SECRET_KEY 时使用的缺省值，仅用于开发环境。
const defaultSecretKey = "hfusaf2m4ot#7)fkw#di2bu6(cv0@opwmafx5n#6=3d%x^hpl6"

// DefaultSecretKey 返回公开的缺省 SECRET_KEY（没设 SECRET_KEY 时服务实际使用的值）。
// 只用于诊断：判断库里的密文是不是没配密钥时用这个公开缺省值加密的（见 dbt-admin check-secrets）。
func DefaultSecretKey() string { return defaultSecretKey }

type Config struct {
	Debug       bool
	Listen      string
	SecretKey   string
	DatabaseURL string // mysql://user:pass@host:port/dbname

	// MirageCipherMode / MirageCipherIV 是 mirage 加密字段（见 internal/crypto）的加密模式与 IV。
	MirageSecretKey  string
	MirageCipherMode string
	MirageCipherIV   string

	// PublicURL 是本系统对外访问的地址，用于通知消息里的工单链接，例如 https://dba.example.com。默认 http://127.0.0.1:端口。
	PublicURL string
	// PublicURLSet 表示 PUBLIC_URL 是显式配置的（而不是回落到本机地址）。邮件通知只在显式配置时才在正文里放链接。
	PublicURLSet bool

	// RequireGoInception 开启后，库表权限校验必须使用 goInception；未配置则拒绝查询。生产环境建议开启。
	RequireGoInception bool

	// BatchDBTypes 是允许批量上线的实例类型（环境变量 BATCH_DB_TYPES，逗号分隔），默认只有 mssql。
	BatchDBTypes []string

	// ExportBaseDir 是数据导出文件的基准目录（环境变量 EXPORT_BASE_DIR）：export.local_path 为相对路径时相对于它，
	// 默认当前目录。
	ExportBaseDir string

	// DefaultLang 是界面默认语言（环境变量 DEFAULT_LANG：zh-CN / en，默认 zh-CN）：没选过语言的访客看到它，
	// 用户在页面上切换后以他自己的选择（cookie）为准。
	DefaultLang string

	// Version 是页面左下角（侧栏底部）显示的版本号，来自配置文件 [site] version；留空时 cmd/server 回落到构建时注入的版本。
	Version string
	// ICP 是页面底部显示的 ICP 备案号（配置文件 [site] icp），留空则不显示。
	ICP string
	// ICPURL 是备案号的链接（配置文件 [site] icp_url），只在 ICP 非空时有意义，默认工信部备案查询站。
	ICPURL string

	// ConfigFile 是实际读取的配置文件路径，没有配置文件时为空。
	ConfigFile string
	// ListenFrom 说明 Listen 的来源（环境变量 LISTEN / 配置文件 / 默认值），启动日志里打印，方便排查“改了端口不生效”。
	ListenFrom string
	// Warnings 是加载过程中发现的、不影响启动但值得提醒的事（例如环境变量覆盖了配置文件里的端口），由 main 打到日志。
	Warnings []string
}

// DefaultPort 是没有任何配置时的监听端口。
const DefaultPort = 8000

// ConfigFileEnv 指定配置文件路径的环境变量；命令行 -config 优先于它。
const ConfigFileEnv = "DB_TOOL_CONFIG"

// fileConfig 是配置文件（TOML）的结构；字段名即文件里的键。新增配置项在这里加字段，并在 Load 里合并。
type fileConfig struct {
	Server struct {
		Host string `toml:"host"` // 监听地址，省略或留空表示所有网卡
		Port *int   `toml:"port"` // 监听端口，1–65535；用指针区分“没写”和“写了 0”
	} `toml:"server"`
	Site struct {
		Version string `toml:"version"` // 页面左下角显示的版本号；省略或留空用构建时的版本
		ICP     string `toml:"icp"`     // ICP 备案号，例如 京ICP备12345678号-1；省略或留空则不显示
		ICPURL  string `toml:"icp_url"` // 备案号的链接，省略用 DefaultICPURL
	} `toml:"site"`
}

// DefaultICPURL 是没有配置 site.icp_url 时备案号指向的地址（工信部 ICP/IP 地址/域名信息备案管理系统）。
const DefaultICPURL = "https://beian.miit.gov.cn/"

// resolveSite 把配置文件里的 [site] 合并进 Config，并校验备案号链接只能是 http(s)。
func resolveSite(fc *fileConfig, file string, c *Config) error {
	c.Version = strings.TrimSpace(fc.Site.Version)
	c.ICP = strings.TrimSpace(fc.Site.ICP)
	c.ICPURL = strings.TrimSpace(fc.Site.ICPURL)
	if c.ICPURL == "" {
		c.ICPURL = DefaultICPURL
	}
	if u, err := url.Parse(c.ICPURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("配置文件 %s：site.icp_url 必须是 http:// 或 https:// 开头的网址，当前为 %q", file, c.ICPURL)
	}
	return nil
}

// searchPaths 返回没有显式指定配置文件时依次查找的位置：当前目录、可执行文件所在目录、/etc/dbtool。
func searchPaths() []string {
	paths := []string{"dbtool.toml"}
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Join(filepath.Dir(exe), "dbtool.toml"))
	}
	return append(paths, "/etc/dbtool/dbtool.toml")
}

// readFile 读取并解析配置文件。explicit 非空（命令行或 DB_TOOL_CONFIG）时文件必须存在；
// 否则按 searchPaths 找第一个存在的，都没有则返回空配置（全部走环境变量与默认值）。
func readFile(explicit string) (*fileConfig, string, error) {
	path := explicit
	if path == "" {
		path = os.Getenv(ConfigFileEnv)
	}
	candidates := []string{path}
	if path == "" {
		candidates = searchPaths()
	}
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && path == "" {
				continue // 默认位置里没有就算了
			}
			return nil, "", fmt.Errorf("读取配置文件 %s: %w", p, err)
		}
		fc := &fileConfig{}
		dec := toml.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields() // 拼错键名要报错，而不是悄悄不生效
		if err := dec.Decode(fc); err != nil {
			var se *toml.StrictMissingError
			if errors.As(err, &se) && len(se.Errors) > 0 {
				row, _ := se.Errors[0].Position()
				return nil, "", fmt.Errorf("配置文件 %s 第 %d 行有不认识的配置项（请检查拼写）", p, row)
			}
			var de *toml.DecodeError
			if errors.As(err, &de) {
				row, col := de.Position()
				if strings.Contains(de.Error(), "unknown field") { // 部分版本把未知键也报成 DecodeError
					return nil, "", fmt.Errorf("配置文件 %s 第 %d 行第 %d 列有不认识的配置项（请检查拼写）", p, row, col)
				}
				return nil, "", fmt.Errorf("配置文件 %s 第 %d 行第 %d 列有误: %s", p, row, col, de.Error())
			}
			return nil, "", fmt.Errorf("解析配置文件 %s: %w", p, err)
		}
		return fc, p, nil
	}
	return &fileConfig{}, "", nil
}

// resolveListen 按 环境变量 LISTEN > 配置文件 > 默认 的顺序确定监听地址，并校验端口。
func resolveListen(fc *fileConfig, file string, c *Config) error {
	fileListen := ""
	if fc.Server.Port != nil || fc.Server.Host != "" {
		port := DefaultPort
		if fc.Server.Port != nil {
			port = *fc.Server.Port
		}
		if port < 1 || port > 65535 {
			return fmt.Errorf("配置文件 %s：server.port 必须是 1–65535 之间的整数，当前为 %d", file, port)
		}
		fileListen = net.JoinHostPort(strings.TrimSpace(fc.Server.Host), strconv.Itoa(port))
	}
	envListen := env("LISTEN", "")
	switch {
	case envListen != "":
		if err := checkListen(envListen); err != nil {
			return fmt.Errorf("环境变量 LISTEN=%q 不合法: %w", envListen, err)
		}
		c.Listen, c.ListenFrom = envListen, "环境变量 LISTEN"
		if fileListen != "" && fileListen != envListen {
			c.Warnings = append(c.Warnings, fmt.Sprintf("环境变量 LISTEN=%s 覆盖了配置文件 %s 里的 %s；要改用配置文件，请删除环境变量 LISTEN", envListen, file, fileListen))
		}
	case fileListen != "":
		c.Listen, c.ListenFrom = fileListen, "配置文件 "+file
	default:
		c.Listen, c.ListenFrom = ":"+strconv.Itoa(DefaultPort), "默认值"
	}
	return nil
}

// checkListen 校验 host:port 形式的监听地址（host 可省略，如 ":8000"）。
func checkListen(addr string) error {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("应为 主机:端口 或 :端口，例如 0.0.0.0:8000")
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("端口必须是 1–65535 之间的整数")
	}
	return nil
}

// Load 加载配置，配置文件按默认位置查找（见 LoadFile）。
func Load() (*Config, error) { return LoadFile("") }

// LoadFile 加载配置。configFile 为命令行 -config 给的路径：非空时文件必须存在；
// 为空时依次看环境变量 DB_TOOL_CONFIG，再在 ./dbtool.toml、可执行文件所在目录、/etc/dbtool/dbtool.toml 里找，都没有就只用环境变量。
func LoadFile(configFile string) (*Config, error) {
	fc, file, err := readFile(configFile)
	if err != nil {
		return nil, err
	}
	c := &Config{
		Debug:              envBool("DEBUG", false),
		ConfigFile:         file,
		SecretKey:          env("SECRET_KEY", defaultSecretKey),
		DatabaseURL:        env("DATABASE_URL", ""),
		MirageSecretKey:    env("MIRAGE_SECRET_KEY", ""),
		MirageCipherMode:   env("MIRAGE_CIPHER_MODE", "ECB"),
		MirageCipherIV:     env("MIRAGE_CIPHER_IV", "1234567890abcdef"),
		PublicURL:          strings.TrimRight(env("PUBLIC_URL", ""), "/"),
		RequireGoInception: envBool("REQUIRE_GOINCEPTION", false),
		BatchDBTypes:       splitList(env("BATCH_DB_TYPES", "mssql")),
		ExportBaseDir:      env("EXPORT_BASE_DIR", "."),
		DefaultLang:        env("DEFAULT_LANG", "zh-CN"),
	}
	switch strings.ToLower(strings.ReplaceAll(c.DefaultLang, "_", "-")) {
	case "zh", "zh-cn":
		c.DefaultLang = "zh-CN"
	case "en", "en-us":
		c.DefaultLang = "en"
	default:
		return nil, fmt.Errorf("环境变量 DEFAULT_LANG=%q 不是支持的界面语言（可选 zh-CN / en）", c.DefaultLang)
	}
	if err := resolveListen(fc, file, c); err != nil {
		return nil, err
	}
	if err := resolveSite(fc, file, c); err != nil {
		return nil, err
	}
	c.PublicURLSet = c.PublicURL != ""
	if c.PublicURL == "" {
		// 没配对外地址时按监听端口回落到本机地址（监听在所有网卡时用 127.0.0.1）
		host, port, _ := net.SplitHostPort(c.Listen)
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		c.PublicURL = "http://" + net.JoinHostPort(host, port)
	}
	if c.MirageSecretKey == "" {
		c.MirageSecretKey = c.SecretKey
	}
	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL 未设置")
	}
	return c, nil
}

// MySQLDSN 把 DATABASE_URL(mysql://...) 转成 go-sql-driver 的 DSN。
func (c *Config) MySQLDSN() (string, error) {
	u, err := url.Parse(c.DatabaseURL)
	if err != nil {
		return "", fmt.Errorf("解析 DATABASE_URL: %w", err)
	}
	if u.Scheme != "mysql" {
		return "", fmt.Errorf("暂只支持 mysql:// ，当前为 %q", u.Scheme)
	}
	pass, _ := u.User.Password()
	if crypto.IsEncryptedPassword(pass) { // mysql://用户:ENC(密文)@…：用 MIRAGE_SECRET_KEY（缺省 SECRET_KEY）解开
		m, err := crypto.NewMirage(c.MirageSecretKey, c.MirageCipherMode, c.MirageCipherIV)
		if err != nil {
			return "", fmt.Errorf("DATABASE_URL 里的密码是加密的，但密钥无效: %w", err)
		}
		if pass, err = m.ResolvePassword(pass); err != nil {
			return "", fmt.Errorf("DATABASE_URL: %w", err)
		}
	}
	host := u.Host
	if u.Port() == "" {
		host += ":3306"
	}
	name := strings.TrimPrefix(u.Path, "/")
	return fmt.Sprintf("%s:%s@tcp(%s)/%s?charset=utf8mb4&parseTime=True&loc=Local",
		u.User.Username(), pass, host, name), nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
