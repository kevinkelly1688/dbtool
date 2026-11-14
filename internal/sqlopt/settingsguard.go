// @author: kevinkelly
// @license: Apache Licence
// @file: settingsguard.go
// @time: 2026/10/28

package sqlopt

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gorm.io/gorm"

	"dbtool/internal/firewall"
	"dbtool/internal/instext"
	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// SOAR 的三项设置（程序路径、配置文件、测试库）可以在“系统设置”页面里改，改动会让服务去执行服务器上的程序、
// 在一个数据库里建表删表，所以保存前要核对这些事实（settings.RegisterGuard，只对页面 / 接口生效，命令行 --force 不受限）：
//   - 程序：存在、是普通文件（不是符号链接 / 目录）、可执行，且不能被“其他用户”改写（文件与所在目录都一样）；文件名由 Def.Check 限定为 soar；
//   - 配置文件：存在、普通文件、不超过 1MB、不能被其他用户改写；
//   - 测试库：不能指向已登记的生产实例（含已停用的）；实例没有标注环境时也拒绝，因为无法证明它不是生产库。
//     只按地址比对（主机名会先解析成 IP），经代理 / 别名 / 隧道连到生产库的认不出来，这一点写在帮助里，由管理员把关。

const maxSoarConfigBytes = 1 << 20

func registerSettingsGuards(db *gorm.DB) {
	settings.RegisterGuard(settings.KeyOptimizerSoarPath, guardSoarProgram)
	settings.RegisterGuard(settings.KeyOptimizerSoarConfig, guardSoarConfigFile)
	settings.RegisterGuard(settings.KeyOptimizerSoarTestDSN, guardSoarTestDSN(func() ([]model.Instance, error) {
		var out []model.Instance
		err := db.Session(&gorm.Session{NewDB: true}).Preload("Tags").Find(&out).Error
		return out, err
	}, lookupHost))
}

// statPlain 取文件信息：必须存在、不是符号链接、是普通文件；在类 Unix 系统上还要求不能被其他用户改写。
func statPlain(path string) (os.FileInfo, error) {
	fi, err := os.Lstat(path)
	switch {
	case os.IsNotExist(err):
		return nil, errors.New("文件不存在")
	case err != nil:
		return nil, errors.New("无法访问该文件，请检查服务运行账号的权限")
	case fi.Mode()&os.ModeSymlink != 0:
		return nil, errors.New("不能是符号链接，请填真实文件的路径")
	case !fi.Mode().IsRegular():
		return nil, errors.New("不是普通文件")
	}
	if runtime.GOOS != "windows" { // Windows 的权限位是模拟的，不能用来判断
		if fi.Mode().Perm()&0o002 != 0 {
			return nil, errors.New("文件可被其他用户改写，请先收紧权限（例如 chmod o-w）")
		}
		if di, err := os.Stat(filepath.Dir(path)); err == nil && di.Mode().Perm()&0o002 != 0 && di.Mode()&os.ModeSticky == 0 {
			return nil, errors.New("文件所在目录可被其他用户改写，请先收紧权限（例如 chmod o-w）")
		}
	}
	return fi, nil
}

func guardSoarProgram(path string) error {
	fi, err := statPlain(path)
	if err != nil {
		return err
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o111 == 0 {
		return errors.New("文件没有执行权限")
	}
	return nil
}

func guardSoarConfigFile(path string) error {
	fi, err := statPlain(path)
	if err != nil {
		return err
	}
	if fi.Size() > maxSoarConfigBytes {
		return errors.New("配置文件超过 1MB，不像是 SOAR 的配置")
	}
	f, err := os.Open(path)
	if err != nil {
		return errors.New("无法读取该文件，请检查服务运行账号的权限")
	}
	return f.Close()
}

// lookupHost 把主机名解析成 IP 文本（已是 IP 的原样规整）；解析失败返回 nil，调用方退回到按文字比较。
func lookupHost(host string) []string {
	if ip := net.ParseIP(host); ip != nil {
		return []string{ip.String()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil {
			a = ip.String()
		}
		out = append(out, a)
	}
	return out
}

func sameHost(a, b string, resolve func(string) []string) bool {
	if strings.EqualFold(a, b) {
		return true
	}
	ips := map[string]bool{}
	for _, x := range resolve(a) {
		ips[x] = true
	}
	for _, x := range resolve(b) {
		if ips[x] {
			return true
		}
	}
	return false
}

// guardSoarTestDSN 校验测试库地址不是已登记的生产实例。list 读出全部实例（含已停用的、带标签），resolve 解析主机名。
func guardSoarTestDSN(list func() ([]model.Instance, error), resolve func(string) []string) settings.Guard {
	return func(dsn string) error {
		host, port, err := settings.SoarDSNAddr(dsn)
		if err != nil {
			return err
		}
		insts, err := list()
		if err != nil {
			return errors.New("读取实例清单失败，无法确认测试库不是生产库，请稍后重试")
		}
		for i := range insts {
			in := &insts[i]
			p := in.Port
			if p == 0 {
				p = 3306
			}
			if p != port || !sameHost(host, in.Host, resolve) {
				continue
			}
			env := firewall.EnvOfInstance(in.ID, in.Tags)
			switch {
			case env == "":
				return fmt.Errorf("该地址是已登记的实例「%s」，但它的环境没有设置，无法确认不是生产库：请先在实例管理里把它的环境设为非生产", in.InstanceName)
			case instext.EnvIsProd(env):
				return fmt.Errorf("该地址是已登记的生产实例「%s」：SOAR 会在测试库里建表，不能指向生产库", in.InstanceName)
			}
		}
		return nil
	}
}
