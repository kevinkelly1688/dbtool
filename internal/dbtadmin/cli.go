// @author: kevinkelly
// @license: Apache Licence
// @file: cli.go
// @time: 2026/10/28

package dbtadmin

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const topUsage = `dbt-admin：dbtool 的初始化与管理员命令行工具（全新部署：初始化元数据库、创建 / 重置管理员、生成密钥）。

用法：
  dbt-admin <命令> [选项]

命令：
  bootstrap             初始化元数据库：基础表 + 基础数据（bootstrap/*.sql），再执行 sql/*.sql 扩展表脚本；默认只演练
  status                只读：显示库状态（dbt_sql_users 是否为空、超级用户数、已应用的脚本数）
  check-secrets         只读自检：库里的加密值（系统设置、实例账号 / 密码）用哪把密钥加密的，当前密钥解不解得开；不输出任何密钥 / 密文
  reencrypt             在库里原地把含加密列的表（实例 / 隧道 / 实例账号 / 系统设置 / 2FA）从旧密钥换成当前密钥；默认只演练；
                        解不开的值原样保留，不输出任何密钥 / 密文 / 明文
  settings              读写系统设置（list / get / set / reset）；包括页面里只读的程序路径类项（--force）
  perm migrate          权限体系一次性迁移：把原权限码展开成按钮授权，并标记为已迁移（默认只演练，--apply 写入并先存撤销脚本）
  admin create          创建超级用户
  admin reset-password  重置密码（清零失败计数 / 解除锁定）
  admin unlock          解锁因登录失败被锁定的账号
  admin activate        启用用户
  admin deactivate      停用用户（不允许停用最后一个启用的超级用户）
  admin list            列出用户
  gen-secrets           生成一组随机密钥（SECRET_KEY / MIRAGE_*）并打印
  encrypt-url           把环境变量 DATABASE_URL 里的明文密码加密成 ENC(密文) 后打印（密钥用 MIRAGE_SECRET_KEY，缺省 SECRET_KEY）
  version               打印版本

连接信息只从环境变量读取（不接受命令行传密码）：
  DATABASE_URL          mysql://用户:URL编码的密码@主机:3306/库名   （密码里的 @ : / ? # % 必须 URL 编码）
  DBT_ADMIN_PASSWORD    admin create / reset-password 的新密码（不设置则在终端里交互输入，无回显）
  TZ                    与 dbtool 服务一致（库里的时间是无时区的本地时间）

安全约定：默认只演练；bootstrap 写入需要 --apply，所有写入需要 --yes 或在终端里确认，确认前显示目标库名与主机。
输出与日志里不会出现密码、密钥、哈希。

退出码：0 成功；1 失败；2 参数错误。
各命令的选项：dbt-admin <命令> --help。完整说明见 docs/admin-cli.md。
`

// Main 是 dbt-admin 的入口，返回退出码。
func Main(ctx context.Context, args []string, e *Env) int {
	if len(args) == 0 {
		fmt.Fprint(e.Stderr, topUsage)
		return ExitUsage
	}
	// 明确拒绝把密码 / 连接串写在命令行上（也避免 flag 包的错误信息回显参数值）
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name := strings.ToLower(strings.TrimLeft(a, "-"))
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		switch name {
		case "password", "passwd", "pwd", "database-url", "dsn", "url":
			e.err("错误：不接受命令行传密码 / 连接串：连接信息用环境变量 DATABASE_URL，新密码用环境变量 %s 或终端交互输入\n", PasswordEnv)
			return ExitUsage
		}
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(e.Stdout, topUsage)
		return ExitOK
	case "version", "--version", "-version":
		fmt.Fprintf(e.Stdout, "dbt-admin %s\n", e.Version)
		return ExitOK
	case "gen-secrets":
		_, done, code := e.parseFlags("gen-secrets", "生成一组随机密钥并打印到标准输出（KEY=VALUE 四行）；提示信息在标准错误。", args[1:], func(*flag.FlagSet) {})
		if done {
			return code
		}
		return e.RunGenSecrets(ctx)
	case "encrypt-url":
		return e.cmdEncryptURL(args[1:])
	case "bootstrap":
		return e.cmdBootstrap(ctx, args[1:])
	case "check-secrets":
		return e.cmdCheckSecrets(ctx, args[1:])
	case "reencrypt":
		return e.cmdReencrypt(ctx, args[1:])
	case "settings":
		return e.cmdSettings(ctx, args[1:])
	case "status":
		return e.cmdStatus(ctx, args[1:])
	case "admin":
		return e.cmdAdmin(ctx, args[1:])
	case "perm":
		return e.cmdPerm(ctx, args[1:])
	}
	e.err("错误：未知命令 %q\n\n%s", args[0], topUsage)
	return ExitUsage
}

// parseFlags 解析子命令选项。done=true 表示已经处理完毕（--help 或参数错误），code 为退出码。
func (e *Env) parseFlags(name, summary string, args []string, define func(*flag.FlagSet)) (*flag.FlagSet, bool, int) {
	fs := flag.NewFlagSet("dbt-admin "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	define(fs)
	usage := func(w io.Writer) {
		fmt.Fprintf(w, "用法：dbt-admin %s [选项]\n%s\n\n选项：\n%s", name, summary, flagHelp(fs))
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(e.Stdout)
			return fs, true, ExitOK
		}
		e.err("错误：%v\n", err)
		usage(e.Stderr)
		return fs, true, ExitUsage
	}
	if fs.NArg() > 0 {
		// 不回显位置参数：用户可能把密码直接写在了命令行上
		e.err("错误：不接受位置参数（所有输入都用 --选项；密码只能通过环境变量或终端提供）\n")
		return fs, true, ExitUsage
	}
	return fs, false, ExitOK
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

const skipCharsetUsage = "跳过“目标库默认字符集必须是 utf8mb4”的预检（需同时加 --yes；不建议）。风险：表与字段都不指定字符集，会按库默认值建表，" +
	"非 utf8mb4 的库得到非 utf8mb4 的表（4 字节字符写入失败 / 被截断），事后统一需要重建表"

// sqlRoot 解析脚本根目录：--sql-root，缺省为可执行文件所在目录。
func sqlRoot(flagVal string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return filepath.Dir(exe), nil
}

func (e *Env) cmdBootstrap(ctx context.Context, args []string) int {
	var (
		root     string
		apply    bool
		onlySQL  bool
		yes      bool
		skipChar bool
	)
	_, done, code := e.parseFlags("bootstrap",
		"初始化元数据库。默认只演练：打印目标库、将执行的文件与语句数，不写任何东西。\n"+
			"全新空库：执行 <root>/bootstrap/*.sql（基础表 + 基础数据），再执行 <root>/sql/*.sql 扩展表脚本；"+tblUsers+" 已有数据则拒绝。\n"+
			"升级（出现了新的 sql/*.sql 扩展表脚本）：加 --only-sql，只执行 <root>/sql/*.sql。全部脚本幂等，可重跑；已应用的文件记录在 "+TblApplied+"。\n"+
			"所有表都带 dbt_ 前缀。\n"+
			"目标库必须已存在（本工具不会建库）。连接串来自环境变量 DATABASE_URL。",
		args, func(fs *flag.FlagSet) {
			fs.StringVar(&root, "sql-root", "", "脚本根目录（含 bootstrap/ 与 sql/）；默认是可执行文件所在目录；源码树里用 --sql-root deploy")
			fs.BoolVar(&apply, "apply", false, "真正写入（默认只演练）")
			fs.BoolVar(&onlySQL, "only-sql", false, "只执行 sql/*.sql 扩展表脚本（升级：基础表已存在）；不检查 "+tblUsers)
			fs.BoolVar(&yes, "yes", false, "写入前不再交互确认（非交互环境必须加）")
			fs.BoolVar(&yes, "y", false, "同 --yes")
			fs.BoolVar(&skipChar, "skip-charset-check", false, skipCharsetUsage)
		})
	if done {
		return code
	}
	if skipChar && !yes {
		e.err("错误：--skip-charset-check 必须同时加 --yes（跳过后表会按库默认字符集建出来，非 utf8mb4 的库会得到非 utf8mb4 的表）\n")
		return ExitUsage
	}
	r, err := sqlRoot(root)
	if err != nil {
		e.err("错误：无法确定脚本根目录，请用 --sql-root 指定：%v\n", err)
		return ExitUsage
	}
	if fi, err := os.Stat(r); err != nil || !fi.IsDir() {
		e.err("错误：脚本根目录 %s 不存在或不是目录\n", r)
		return ExitUsage
	}
	if yes && !apply {
		e.err("提示：没有 --apply，--yes 不起作用，下面只演练\n")
	}
	st, tgt, code := e.connect(ctx)
	if st == nil {
		return code
	}
	defer st.Close()
	return e.RunBootstrap(ctx, st, tgt, BootstrapOptions{Root: r, Apply: apply, OnlySQL: onlySQL, Yes: yes, SkipCharsetCheck: skipChar})
}

func (e *Env) cmdStatus(ctx context.Context, args []string) int {
	var porcelain, skipChar, yes bool
	_, done, code := e.parseFlags("status", "只读：显示目标库状态（含库默认字符集 / 校验规则；默认字符集不是 utf8mb4 时返回 1）。连接串来自环境变量 DATABASE_URL。", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&porcelain, "porcelain", false, "输出 key=value 行（db / host / users_table=absent|empty|nonempty / users / superusers / bootstrap_applied / charset / collation / charset_check=ok|warn|fail|unknown=N|unknown），供脚本解析")
		fs.BoolVar(&skipChar, "skip-charset-check", false, "库默认字符集不是 utf8mb4 时也不返回失败（需同时加 --yes）；状态里仍如实显示")
		fs.BoolVar(&yes, "yes", false, "与 --skip-charset-check 配合使用；status 本身只读，不写任何东西")
		fs.BoolVar(&yes, "y", false, "同 --yes")
	})
	if done {
		return code
	}
	if skipChar && !yes {
		e.err("错误：--skip-charset-check 必须同时加 --yes\n")
		return ExitUsage
	}
	st, tgt, code := e.connect(ctx)
	if st == nil {
		return code
	}
	defer st.Close()
	return e.RunStatus(ctx, st, tgt, StatusOptions{Porcelain: porcelain, SkipCharsetCheck: skipChar})
}

func (e *Env) cmdCheckSecrets(ctx context.Context, args []string) int {
	_, done, code := e.parseFlags("check-secrets", "只读自检：判断系统设置（dbt_setting）和实例账号 / 密码里“看起来是密文”的值能不能用当前密钥解开，解不开时指出是否是用公开缺省密钥加密的。"+
		"密钥来自环境变量 SECRET_KEY / MIRAGE_SECRET_KEY / MIRAGE_CIPHER_MODE / MIRAGE_CIPHER_IV（与 dbtool 服务一致，可先 set -a; . /etc/dbtool/dbtool.env; set +a），连接串来自 DATABASE_URL。"+
		"只输出分类计数与配置项名 / 实例名，不输出任何密钥、密文或明文值；有解不开的值时返回 1。", args, func(*flag.FlagSet) {})
	if done {
		return code
	}
	st, tgt, code := e.connect(ctx)
	if st == nil {
		return code
	}
	defer st.Close()
	return e.RunCheckSecrets(ctx, st, tgt)
}

func (e *Env) cmdReencrypt(ctx context.Context, args []string) int {
	var apply, yes bool
	_, done, code := e.parseFlags("reencrypt", "在目标库里原地把 dbt_sql_instance / dbt_ssh_tunnel / dbt_instance_account / dbt_setting / dbt_2fa_config 的加密列"+
		"从旧密钥换成 dbtool 当前生效的密钥（与服务取法一致：MIRAGE_SECRET_KEY，缺省 SECRET_KEY）。默认只演练：只统计，不写。\n"+
		"旧密钥依次尝试：OLD_MIRAGE_SECRET_KEY（缺省 OLD_SECRET_KEY；模式 / IV 用 OLD_MIRAGE_CIPHER_MODE / OLD_MIRAGE_CIPHER_IV）、SECRET_KEY、公开缺省密钥。\n"+
		"旧密钥解得开的值重新加密，明文值按当前密钥加密，已是当前密钥的不动，谁都解不开的原样保留（只列名字）；可重复运行。\n"+
		"写入在一个事务里完成，条件更新（值被别人改过则不动），并写一条系统审计；写入前请先备份这些表。连接串来自 DATABASE_URL。", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&apply, "apply", false, "真正写入（默认只演练）")
		fs.BoolVar(&yes, "yes", false, "写入前不再交互确认（非交互环境必须加）")
		fs.BoolVar(&yes, "y", false, "同 --yes")
	})
	if done {
		return code
	}
	if yes && !apply {
		e.err("提示：没有 --apply，--yes 不起作用，下面只演练\n")
	}
	st, tgt, code := e.connect(ctx)
	if st == nil {
		return code
	}
	defer st.Close()
	return e.RunReencrypt(ctx, st, tgt, ReencryptOptions{Apply: apply, Yes: yes})
}

const adminUsage = `用法：dbt-admin admin <子命令> [选项]

子命令：
  create          创建超级用户（--username 必填；密码来自 DBT_ADMIN_PASSWORD 或终端交互）
  reset-password  重置密码，并清零失败计数 / 解除锁定（--require-change 要求下次登录改密）
  unlock          解锁账号（失败计数清零）
  activate        启用用户
  deactivate      停用用户（不允许停用最后一个启用的超级用户）
  list            列出用户（用户名、姓名、超级用户、启用、锁定状态、最近登录）

各子命令的选项：dbt-admin admin <子命令> --help。
写操作显示目标库名与主机，并需要 --yes 或终端确认；--dry-run 只检查不写。
`

func (e *Env) cmdAdmin(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(e.Stderr, adminUsage)
		return ExitUsage
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "-h", "--help", "help":
		fmt.Fprint(e.Stdout, adminUsage)
		return ExitOK
	case "create", "reset-password", "unlock", "activate", "deactivate", "list":
	default:
		e.err("错误：未知子命令 %q\n\n%s", sub, adminUsage)
		return ExitUsage
	}

	var (
		username, email, display string
		groups                   stringList
		requireChange            bool
		yes, dryRun, apply       bool
	)
	summary := map[string]string{
		"create": "创建超级用户（is_superuser / is_staff / is_active 均为 1），可加入权限组。用户名已存在则报错。\n" +
			"密码来自环境变量 " + PasswordEnv + "，否则在终端里无回显输入两次；需符合 dbtool 的密码策略（默认最少 9 位、不能全是数字、不能是常见弱口令、不能包含用户名）。",
		"reset-password": "重置用户密码（密码来源同 create），清零失败计数并解除锁定。LDAP 认证的用户密码由外部系统管理，拒绝重置。",
		"unlock":         "解锁账号：登录失败计数清零。",
		"activate":       "启用用户。",
		"deactivate":     "停用用户。不允许停用最后一个启用的超级用户。",
		"list":           "列出用户（只读）。",
	}[sub]
	_, done, code := e.parseFlags("admin "+sub, summary, rest, func(fs *flag.FlagSet) {
		if sub != "list" {
			fs.StringVar(&username, "username", "", "用户名（必填；1–30 个字符，字母数字与 @ . + - _）")
			fs.BoolVar(&yes, "yes", false, "不再交互确认（非交互环境必须加）")
			fs.BoolVar(&yes, "y", false, "同 --yes")
			fs.BoolVar(&dryRun, "dry-run", false, "只检查并打印将执行的操作，不写")
			fs.BoolVar(&apply, "apply", false, "与 bootstrap 保持一致的写法；admin 命令确认后即写入，此选项无额外作用")
		}
		if sub == "create" {
			fs.StringVar(&email, "email", "", "邮箱（可选）")
			fs.StringVar(&display, "display", "", "姓名（最多 10 个字符；缺省取用户名前 10 个字符）")
			fs.Var(&groups, "group", "加入的权限组名称（可重复，或逗号分隔）；权限组必须已存在")
		}
		if sub == "reset-password" {
			fs.BoolVar(&requireChange, "require-change", false, "要求该用户下次登录先修改密码（需要 "+tblUserProfile+" 表）")
		}
	})
	if done {
		return code
	}
	_ = apply
	// 先在不碰库的前提下校验参数：参数错误不应该表现为“连不上库”
	if sub != "list" {
		if strings.TrimSpace(username) == "" {
			e.err("错误：必须提供 --username\n")
			return ExitUsage
		}
		un, err := CleanUsername(username)
		if err != nil {
			return e.exitOf(err)
		}
		if sub == "create" {
			if _, err := cleanDisplay(display, un); err != nil {
				return e.exitOf(err)
			}
			if _, err := cleanEmail(email); err != nil {
				return e.exitOf(err)
			}
		}
	}
	st, tgt, code := e.connect(ctx)
	if st == nil {
		return code
	}
	defer st.Close()
	switch sub {
	case "create":
		return e.RunAdminCreate(ctx, st, tgt, CreateAdminOptions{Username: username, Email: email, Display: display, Groups: groups, Yes: yes, DryRun: dryRun})
	case "reset-password":
		return e.RunAdminResetPassword(ctx, st, tgt, ResetPasswordOptions{Username: username, RequireChange: requireChange, Yes: yes, DryRun: dryRun})
	case "unlock":
		return e.RunAdminUnlock(ctx, st, tgt, UserOpOptions{Username: username, Yes: yes, DryRun: dryRun})
	case "activate":
		return e.RunAdminSetActive(ctx, st, tgt, UserOpOptions{Username: username, Yes: yes, DryRun: dryRun}, true)
	case "deactivate":
		return e.RunAdminSetActive(ctx, st, tgt, UserOpOptions{Username: username, Yes: yes, DryRun: dryRun}, false)
	default:
		return e.RunAdminList(ctx, st, tgt)
	}
}

// flagHelp 列出选项（统一显示成 --name 形式，-y 之类的别名并到主选项后面）。
func flagHelp(fs *flag.FlagSet) string {
	var b strings.Builder
	alias := map[string]string{"y": "yes"}
	fs.VisitAll(func(f *flag.Flag) {
		if _, ok := alias[f.Name]; ok {
			return
		}
		typ, usage := flag.UnquoteUsage(f)
		head := "  --" + f.Name
		for a, main := range alias {
			if main == f.Name {
				head += ", -" + a
			}
		}
		if typ != "" {
			head += " " + typ
		}
		b.WriteString(head + "\n        " + strings.ReplaceAll(usage, "\n", "\n        ") + "\n")
	})
	return b.String()
}
