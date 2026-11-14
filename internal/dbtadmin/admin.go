// @author: kevinkelly
// @license: Apache Licence
// @file: admin.go
// @time: 2026/10/28

package dbtadmin

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"dbtool/internal/crypto"
	"dbtool/internal/settings"
)

// 用户名规则与 sysadmin.CreateUser 一致：字母数字与 @.+-_，另限 30 个字符（工单、审计等表里存用户名的列只有 30 位）。
var usernameRe = regexp.MustCompile(`^[\p{L}\p{N}_.@+-]+$`)

const saltChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// errDryRun 让事务回滚但不算失败。
var errDryRun = errors.New("dry-run")

// usageErr 是参数 / 输入错误（退出码 2）。
type usageErr struct{ msg string }

func (u usageErr) Error() string { return u.msg }

func usagef(format string, a ...any) error { return usageErr{fmt.Sprintf(format, a...)} }

// failErr 是业务失败（退出码 1），信息可直接展示。
type failErr struct{ msg string }

func (f failErr) Error() string { return f.msg }

func failf(format string, a ...any) error { return failErr{fmt.Sprintf(format, a...)} }

// exitOf 把错误翻译成退出码并打印。
func (e *Env) exitOf(err error) int {
	if err == nil {
		return ExitOK
	}
	var u usageErr
	var f failErr
	switch {
	case errors.As(err, &u):
		e.err("错误：%s\n", u.msg)
		return ExitUsage
	case errors.As(err, &f):
		e.err("错误：%s\n", f.msg)
		return ExitFailed
	case errors.Is(err, errNeedConfirm):
		e.err("错误：%v\n", err)
		return ExitUsage
	}
	e.err("错误：%s\n", RedactMsg(err.Error()))
	return ExitFailed
}

// ---- 参数与规则 ----

// CleanUsername 校验用户名（1–30 个字符，只含字母、数字与 @ . + - _）。
func CleanUsername(s string) (string, error) {
	s = strings.TrimSpace(s)
	if n := utf8.RuneCountInString(s); n == 0 || n > 30 || !usernameRe.MatchString(s) {
		return "", usagef("用户名需要 1 到 30 个字符，只能包含字母、数字和 @ . + - _")
	}
	return s, nil
}

func cleanDisplay(display, username string) (string, error) {
	display = strings.TrimSpace(display)
	if display == "" {
		display = truncRunes(username, 10) // dbt_sql_users.display 最长 10 个字符
	}
	if n := utf8.RuneCountInString(display); n == 0 || n > 10 {
		return "", usagef("姓名（--display）需要 1 到 10 个字符")
	}
	return display, nil
}

func cleanEmail(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if len(s) > 254 {
		return "", usagef("邮箱过长")
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || !strings.Contains(s[strings.IndexByte(s, '@'):], ".") {
		return "", usagef("邮箱格式不正确")
	}
	return s, nil
}

func dedupStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		for _, p := range strings.Split(s, ",") {
			p = strings.TrimSpace(p)
			if p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// policy 读取当前生效的密码策略：系统设置里的 security.password_min_length / security.password_complexity，读不到用 sysadmin 的默认值。
func (e *Env) policy(ctx context.Context, st Store) PasswordPolicy {
	p := DefaultPasswordPolicy()
	if v, ok, err := st.AuthSetting(ctx, settings.KeySecurityPasswordMinLength); err == nil && ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 && n <= 128 {
			p.MinLen = n
		}
	}
	if v, ok, err := st.AuthSetting(ctx, settings.KeySecurityPasswordComplexity); err == nil && ok {
		v = strings.TrimSpace(v)
		p.Complexity = strings.EqualFold(v, "true") || v == "1"
	}
	return p
}

// lockPolicy 读取登录锁定策略（系统设置 security.login_lock_attempts / security.login_lock_seconds，默认 5 次 / 300 秒，同 sysadmin）。
func (e *Env) lockPolicy(ctx context.Context, st Store) (int, time.Duration) {
	read := func(item string, def int) int {
		v, ok, err := st.ConfigValue(ctx, item)
		if err != nil || !ok {
			return def
		}
		v = strings.TrimSpace(v)
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		if e.Decrypt != nil { // 值可能是 mirage 密文
			if n, err := strconv.Atoi(strings.TrimSpace(e.Decrypt(v))); err == nil && n > 0 {
				return n
			}
		}
		return def
	}
	return read(settings.KeySecurityLoginLockAttempts, 5), time.Duration(read(settings.KeySecurityLoginLockSeconds, 300)) * time.Second
}

// readNewPassword 取得新密码：优先环境变量，其次终端交互（无回显、输入两次）；非终端又没有环境变量则报参数错误。
// 返回的错误信息不含密码。校验由调用方做（交互时允许重试）。
func (e *Env) readNewPassword() (pw string, fromEnv bool, err error) {
	if v := e.Getenv(PasswordEnv); v != "" {
		return v, true, nil
	}
	if !e.IsTTY() {
		return "", false, usagef("非交互环境必须通过环境变量 %s 提供密码（不接受命令行传密码）", PasswordEnv)
	}
	p1, err := e.ReadPassword("新密码: ")
	if err != nil {
		return "", false, failf("读取密码失败：%v", err)
	}
	p2, err := e.ReadPassword("再输入一次: ")
	if err != nil {
		return "", false, failf("读取密码失败：%v", err)
	}
	if p1 != p2 {
		return "", false, usagef("两次输入的密码不一致")
	}
	return p1, false, nil
}

// obtainPassword 取得并校验密码；交互输入时校验失败最多重试 3 次，环境变量给出的密码不通过则直接报参数错误。
func (e *Env) obtainPassword(pol PasswordPolicy, related ...string) (string, error) {
	for attempt := 0; ; attempt++ {
		pw, fromEnv, err := e.readNewPassword()
		if err != nil {
			if attempt > 0 && attempt < 3 {
				var u usageErr
				if errors.As(err, &u) {
					e.err("%s，请重试\n", u.msg)
					continue
				}
			}
			return "", err
		}
		if verr := pol.Validate(pw, related...); verr != nil {
			if fromEnv || attempt >= 2 {
				return "", usagef("密码不符合策略：%s", verr.Error())
			}
			e.err("密码不符合策略：%s，请重试\n", verr.Error())
			continue
		}
		return pw, nil
	}
}

func (e *Env) hashPassword(pw string) (string, error) {
	salt, err := randomString(e.rand(), 22, saltChars) // 22 位随机字母数字盐
	if err != nil {
		return "", err
	}
	return crypto.MakeDjangoPassword(pw, salt, e.iterations())
}

// randomString 用 r（默认 crypto/rand）生成 n 个取自 alphabet 的字符，拒绝采样保证均匀。
func randomString(r io.Reader, n int, alphabet string) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	out := make([]byte, 0, n)
	limit := 256 - 256%len(alphabet)
	buf := make([]byte, n*2)
	for len(out) < n {
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if int(b) >= limit {
				continue
			}
			out = append(out, alphabet[int(b)%len(alphabet)])
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}

// passwordManagedExternally 与 sysadmin 一致：认证方式里有 LDAP 而没有本地。
func passwordManagedExternally(ai AuthInfo) bool {
	ldap, local := false, false
	count := func(k string) {
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "ldap":
			ldap = true
		case "local":
			local = true
		}
	}
	count(ai.ProfileType)
	for _, k := range ai.Kinds {
		count(k)
	}
	if ai.ProfileType == "" && len(ai.Kinds) == 0 {
		local = true
	}
	return ldap && !local
}

// ---- 公共：连接后打印目标并确认 ----

func (e *Env) announce(tgt Target, what string) {
	e.out("目标：%s\n操作：%s\n", tgt, what)
}

// requireUsersTable 确认用户表（dbt_sql_users）存在；不存在时给出可操作的提示。
func (e *Env) requireUsersTable(ctx context.Context, st Store) error {
	ok, err := st.HasTable(ctx, tblUsers)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	msg := fmt.Sprintf("库里没有 %s：请先执行 dbt-admin bootstrap --apply 初始化基础表", tblUsers)
	return failf("%s", msg)
}

// ---- admin create ----

// CreateAdminOptions 是 admin create 的选项。
type CreateAdminOptions struct {
	Username string
	Email    string
	Display  string
	Groups   []string
	Yes      bool
	DryRun   bool
}

func (e *Env) RunAdminCreate(ctx context.Context, st Store, tgt Target, o CreateAdminOptions) int {
	return e.exitOf(e.adminCreate(ctx, st, tgt, o))
}

func (e *Env) adminCreate(ctx context.Context, st Store, tgt Target, o CreateAdminOptions) error {
	username, err := CleanUsername(o.Username)
	if err != nil {
		return err
	}
	display, err := cleanDisplay(o.Display, username)
	if err != nil {
		return err
	}
	email, err := cleanEmail(o.Email)
	if err != nil {
		return err
	}
	groups := dedupStrings(o.Groups)
	if err := e.requireUsersTable(ctx, st); err != nil {
		return err
	}
	what := fmt.Sprintf("创建超级用户 %s（姓名 %s）", username, display)
	if len(groups) > 0 {
		what += "，加入权限组 " + strings.Join(groups, "、")
	}
	e.announce(tgt, what)

	// 先在只读事务里查重与组（失败尽早报出，不去问密码）
	if err := e.precheckCreate(ctx, st, username, groups); err != nil {
		return err
	}
	if o.DryRun {
		e.out("这是演练（--dry-run）：没有写入任何东西。\n")
		return nil
	}
	ok, err := e.confirm(o.Yes, fmt.Sprintf("即将在 %s 上%s。", tgt, what))
	if err != nil {
		return err
	}
	if !ok {
		return failf("已取消，没有写入任何东西")
	}
	pw, err := e.obtainPassword(e.policy(ctx, st), username, email, display)
	if err != nil {
		return err
	}
	hash, err := e.hashPassword(pw)
	pw = ""
	if err != nil {
		return err
	}

	auditOK, err := st.HasTable(ctx, tblAuditLog)
	if err != nil {
		return err
	}
	var uid uint
	err = st.InTx(ctx, func(tx Tx) error {
		if ex, err := tx.GetUser(username); err != nil {
			return err
		} else if ex != nil {
			return failf("用户 %s 已存在（重置密码请用 admin reset-password）", username)
		}
		ids, err := resolveGroups(tx, groups)
		if err != nil {
			return err
		}
		uid, err = tx.InsertUser(NewUser{Username: username, Display: display, Email: email, PasswordHash: hash,
			IsSuperuser: true, IsStaff: true, IsActive: true, DateJoined: e.Now()})
		if err != nil {
			return err
		}
		if err := tx.AddUserGroups(uid, ids); err != nil {
			return err
		}
		if !auditOK {
			return nil
		}
		return tx.InsertAudit(e.auditEntry("CLI-创建超级用户", fmt.Sprintf("目标用户 %s（#%d）；超级用户：是；启用：是；权限组 %s", username, uid, joinOrDash(groups))))
	})
	if err != nil {
		if isDuplicate(err) {
			return failf("用户 %s 已存在", username)
		}
		return err
	}
	e.out("已创建超级用户 %s（id=%d）。\n", username, uid)
	if !auditOK {
		e.out("提示：%s 不存在，未写审计\n", tblAuditLog)
	}
	return nil
}

func (e *Env) precheckCreate(ctx context.Context, st Store, username string, groups []string) error {
	return ignoreDry(st.InTx(ctx, func(tx Tx) error {
		if ex, err := tx.GetUser(username); err != nil {
			return err
		} else if ex != nil {
			return failf("用户 %s 已存在（重置密码请用 admin reset-password）", username)
		}
		if _, err := resolveGroups(tx, groups); err != nil {
			return err
		}
		return errDryRun
	}))
}

func ignoreDry(err error) error {
	if errors.Is(err, errDryRun) {
		return nil
	}
	return err
}

func resolveGroups(tx Tx, names []string) ([]uint, error) {
	if len(names) == 0 {
		return nil, nil
	}
	m, err := tx.GroupIDs(names)
	if err != nil {
		return nil, err
	}
	var ids []uint
	var missing []string
	for _, n := range names {
		if id, ok := m[n]; ok {
			ids = append(ids, id)
		} else {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return nil, failf("权限组不存在：%s（权限组在基础数据 / 内置权限组脚本里创建；可先 dbt-admin admin create 不带 --group，登录后在页面里分配）", strings.Join(missing, "、"))
	}
	return ids, nil
}

func joinOrDash(s []string) string {
	if len(s) == 0 {
		return "无"
	}
	return strings.Join(s, "、")
}

// isDuplicate 判断是否是唯一键冲突（MySQL 1062），避免依赖驱动类型。
func isDuplicate(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Error 1062")
}

// ---- admin reset-password ----

type ResetPasswordOptions struct {
	Username      string
	RequireChange bool
	Yes           bool
	DryRun        bool
}

func (e *Env) RunAdminResetPassword(ctx context.Context, st Store, tgt Target, o ResetPasswordOptions) int {
	return e.exitOf(e.adminResetPassword(ctx, st, tgt, o))
}

func (e *Env) adminResetPassword(ctx context.Context, st Store, tgt Target, o ResetPasswordOptions) error {
	username, err := CleanUsername(o.Username)
	if err != nil {
		return err
	}
	if err := e.requireUsersTable(ctx, st); err != nil {
		return err
	}
	var u *User
	err = ignoreDry(st.InTx(ctx, func(tx Tx) error {
		var err error
		if u, err = tx.GetUser(username); err != nil {
			return err
		}
		if u == nil {
			return failf("用户 %s 不存在", username)
		}
		ai, err := tx.AuthInfo(u.ID)
		if err != nil {
			return err
		}
		if passwordManagedExternally(ai) {
			return failf("用户 %s 使用 LDAP 认证，密码由外部系统管理，不能在这里重置", username)
		}
		return errDryRun
	}))
	if err != nil {
		return err
	}
	what := fmt.Sprintf("重置用户 %s 的密码，并清零登录失败计数 / 解除锁定", username)
	if o.RequireChange {
		what += "；要求其下次登录先修改密码"
	}
	e.announce(tgt, what)
	if !u.IsActive {
		e.out("提示：该用户当前是停用状态；重置密码不会启用它（用 admin activate）\n")
	}
	if o.DryRun {
		e.out("这是演练（--dry-run）：没有写入任何东西。\n")
		return nil
	}
	ok, err := e.confirm(o.Yes, fmt.Sprintf("即将在 %s 上%s。", tgt, what))
	if err != nil {
		return err
	}
	if !ok {
		return failf("已取消，没有写入任何东西")
	}
	pw, err := e.obtainPassword(e.policy(ctx, st), u.Username, u.Email, u.Display)
	if err != nil {
		return err
	}
	hash, err := e.hashPassword(pw)
	pw = ""
	if err != nil {
		return err
	}
	auditOK, err := st.HasTable(ctx, tblAuditLog)
	if err != nil {
		return err
	}
	err = st.InTx(ctx, func(tx Tx) error {
		cur, err := tx.GetUser(username)
		if err != nil {
			return err
		}
		if cur == nil {
			return failf("用户 %s 不存在", username)
		}
		ai, err := tx.AuthInfo(cur.ID)
		if err != nil {
			return err
		}
		if err := tx.SetPassword(cur.ID, hash); err != nil {
			return err
		}
		switch {
		case o.RequireChange:
			if err := tx.SetMustChange(cur.ID, true, e.actor(), e.Now()); err != nil {
				if errors.Is(err, ErrTableMissing) {
					return failf("要求下次登录改密需要 %s 表（deploy/sql/080_sysadmin_ext.sql）；表不存在，未做任何修改。可去掉 --require-change，或先执行 dbt-admin bootstrap --only-sql --apply", tblUserProfile)
				}
				return err
			}
		case ai.MustChange: // 之前遗留的“强制改密”标记：管理员明确设了新密码，清掉（同 sysadmin 的重置密码）
			if err := tx.SetMustChange(cur.ID, false, e.actor(), e.Now()); err != nil && !errors.Is(err, ErrTableMissing) {
				return err
			}
		}
		if !auditOK {
			return nil
		}
		extra := "目标用户 " + cur.Username + "；失败计数已清零"
		if o.RequireChange {
			extra += "；首次登录强制改密"
		}
		return tx.InsertAudit(e.auditEntry("CLI-重置密码", extra))
	})
	if err != nil {
		return err
	}
	e.out("已重置用户 %s 的密码，失败计数已清零（锁定已解除）。\n", username)
	if o.RequireChange {
		e.out("该用户下次登录必须先修改密码。\n")
	}
	if !auditOK {
		e.out("提示：%s 不存在，未写审计\n", tblAuditLog)
	}
	return nil
}

// ---- admin unlock / activate / deactivate ----

type UserOpOptions struct {
	Username string
	Yes      bool
	DryRun   bool
}

func (e *Env) RunAdminUnlock(ctx context.Context, st Store, tgt Target, o UserOpOptions) int {
	return e.exitOf(e.adminUserOp(ctx, st, tgt, o, "unlock"))
}

func (e *Env) RunAdminSetActive(ctx context.Context, st Store, tgt Target, o UserOpOptions, active bool) int {
	op := "deactivate"
	if active {
		op = "activate"
	}
	return e.exitOf(e.adminUserOp(ctx, st, tgt, o, op))
}

func (e *Env) adminUserOp(ctx context.Context, st Store, tgt Target, o UserOpOptions, op string) error {
	username, err := CleanUsername(o.Username)
	if err != nil {
		return err
	}
	label := map[string]string{"unlock": "解锁账号", "activate": "启用用户", "deactivate": "停用用户"}[op]
	action := map[string]string{"unlock": "CLI-解锁账号", "activate": "CLI-启用用户", "deactivate": "CLI-停用用户"}[op]
	if err := e.requireUsersTable(ctx, st); err != nil {
		return err
	}

	auditOK, err := st.HasTable(ctx, tblAuditLog)
	if err != nil {
		return err
	}
	// plan 先检查（dry-run 与正式执行共用同一段逻辑），apply 为真才写。
	run := func(tx Tx, apply bool) (note string, changed bool, err error) {
		u, err := tx.GetUser(username)
		if err != nil {
			return "", false, err
		}
		if u == nil {
			return "", false, failf("用户 %s 不存在", username)
		}
		switch op {
		case "unlock":
			if u.FailedLoginCount == 0 && u.LastFailedAt == nil {
				return fmt.Sprintf("用户 %s 没有登录失败记录，无需解锁", username), false, nil
			}
			if apply {
				if err := tx.ClearFailedLogins(u.ID); err != nil {
					return "", false, err
				}
			}
			return fmt.Sprintf("用户 %s 已解锁（原失败 %d 次）", username, u.FailedLoginCount), true, nil
		case "activate":
			if u.IsActive {
				return fmt.Sprintf("用户 %s 已经是启用状态，无需操作", username), false, nil
			}
			if apply {
				if err := tx.SetActive(u.ID, true); err != nil {
					return "", false, err
				}
			}
			return fmt.Sprintf("用户 %s 已启用", username), true, nil
		default: // deactivate
			if !u.IsActive {
				return fmt.Sprintf("用户 %s 已经是停用状态，无需操作", username), false, nil
			}
			if u.IsSuperuser {
				n, err := tx.CountOtherActiveSuperusers(u.ID)
				if err != nil {
					return "", false, err
				}
				if n == 0 {
					return "", false, failf("不能停用最后一个启用的超级用户 %s；请先创建或启用另一个超级用户（admin create / activate）", username)
				}
			}
			if apply {
				if err := tx.SetActive(u.ID, false); err != nil {
					return "", false, err
				}
			}
			return fmt.Sprintf("用户 %s 已停用", username), true, nil
		}
	}
	var planNote string
	var planChanged bool
	err = ignoreDry(st.InTx(ctx, func(tx Tx) error {
		var err error
		planNote, planChanged, err = run(tx, false)
		if err != nil {
			return err
		}
		return errDryRun
	}))
	if err != nil {
		return err
	}
	e.announce(tgt, label+" "+username)
	if !planChanged {
		e.out("%s。\n", planNote)
		return nil
	}
	if o.DryRun {
		e.out("这是演练（--dry-run）：将会：%s。没有写入任何东西。\n", planNote)
		return nil
	}
	ok, err := e.confirm(o.Yes, fmt.Sprintf("即将在 %s 上%s %s。", tgt, label, username))
	if err != nil {
		return err
	}
	if !ok {
		return failf("已取消，没有写入任何东西")
	}
	err = st.InTx(ctx, func(tx Tx) error {
		note, changed, err := run(tx, true)
		if err != nil {
			return err
		}
		if !changed {
			planNote, planChanged = note, false
			return nil
		}
		planNote = note
		if !auditOK {
			return nil
		}
		return tx.InsertAudit(e.auditEntry(action, "目标用户 "+username))
	})
	if err != nil {
		return err
	}
	e.out("%s。\n", planNote)
	if planChanged && !auditOK {
		e.out("提示：%s 不存在，未写审计\n", tblAuditLog)
	}
	return nil
}

// ---- admin list ----

func (e *Env) RunAdminList(ctx context.Context, st Store, tgt Target) int {
	if err := e.requireUsersTable(ctx, st); err != nil {
		return e.exitOf(err)
	}
	us, err := st.ListUsers(ctx)
	if err != nil {
		return e.exitOf(err)
	}
	lockCnt, lockWin := e.lockPolicy(ctx, st)
	now := e.Now()
	e.out("目标：%s\n", tgt)
	tw := tabwriter.NewWriter(e.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "用户名\t姓名\t超级用户\t启用\t锁定\t最近登录")
	for _, u := range us {
		lock := "-"
		if u.FailedLoginCount >= lockCnt && u.LastFailedAt != nil {
			if until := u.LastFailedAt.Add(lockWin); until.After(now) {
				lock = fmt.Sprintf("锁定（剩余 %d 秒）", int(until.Sub(now).Seconds())+1)
			}
		}
		if lock == "-" && u.FailedLoginCount > 0 {
			lock = fmt.Sprintf("失败 %d 次", u.FailedLoginCount)
		}
		last := "从未登录"
		if u.LastLogin != nil {
			last = u.LastLogin.Format("2006-01-02 15:04:05")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", u.Username, orDash(u.Display), yn(u.IsSuperuser), yn(u.IsActive), lock, last)
	}
	tw.Flush()
	e.out("共 %d 个用户\n", len(us))
	return ExitOK
}

func yn(b bool) string {
	if b {
		return "是"
	}
	return "否"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
