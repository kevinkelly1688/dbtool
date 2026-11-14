// @author: kevinkelly
// @license: Apache Licence
// @file: driver_charset_test.go
// @time: 2026/10/28

package dbtadmin

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 字符集放进 cfg.Params 会被驱动（v1.10）当成会话变量发 SET charset=…，MySQL 8.0 / TiDB 报
// 1193 Unknown system variable 'charset'，连库直接失败。必须用 mysql.Charset 选项（发 SET NAMES）。
func TestMySQLConfigCharsetIsOptionNotSessionVariable(t *testing.T) {
	cfg := mysqlConfig(ConnParams{Host: "127.0.0.1", Port: "4000", User: "u", Pass: "p", DB: "d"})
	if v, ok := cfg.Params["charset"]; ok {
		t.Fatalf("Params[charset]=%q：会被当成会话变量发 SET charset=…（1193）", v)
	}
	if dsn := cfg.FormatDSN(); !strings.Contains(dsn, "charset=utf8mb4") {
		t.Fatalf("字符集选项没有生效：%s", dsn)
	}
}

// 直接用 mysql 驱动的 NewConnector(cfg)（不经 FormatDSN 重新解析）的文件，不允许再把 charset 写进 Params。
// （FormatDSN 再 sql.Open 的写法会重新解析 DSN，charset 被识别成选项，不受影响。）
var reMySQLConnector = regexp.MustCompile(`(mysql|drv)\.NewConnector\(`)

func TestNoParamsCharsetWithNewConnector(t *testing.T) {
	root := filepath.Join("..", "..", "internal")
	var bad []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		s := string(b)
		if reMySQLConnector.MatchString(s) && (strings.Contains(s, `{"charset":`) || strings.Contains(s, `Params["charset"]`)) {
			bad = append(bad, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Fatalf("这些文件用 mysql 驱动的 NewConnector 且把 charset 放进 Params（应改用 mysql.Charset 选项）：%v", bad)
	}
}
