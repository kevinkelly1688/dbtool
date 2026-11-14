// @author: kevinkelly
// @license: Apache Licence
// @file: accounts.go
// @time: 2026/10/28

package instmgr

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"dbtool/internal/i18n"
)

// ---- 账号列表 ----

// AccountItem 是账号管理页的一行：目标库 mysql.user 里的账号，加平台登记的备注。
type AccountItem struct {
	User          string   `json:"user"`
	Host          string   `json:"host"`
	UserHost      string   `json:"user_host"`
	Locked        *bool    `json:"is_locked"` // 版本不支持锁定时为 null
	Privileges    []string `json:"privileges"`
	Saved         bool     `json:"saved"`
	Remark        string   `json:"remark"`
	Risky         bool     `json:"risky"`
	RiskNote      string   `json:"risk_note,omitempty"`
	Protected     bool     `json:"protected"` // 平台自己连接用的账号或系统内置账号：不允许删除、锁定、改密、回收
	ProtectReason string   `json:"protect_reason,omitempty"`
	Builtin       bool     `json:"builtin"` // 数据库内置账号（含 root 本机账号）：连授权也不允许
	// 使用人 / 用途 / 到期日（旁路表 dbt_account_reg）；没登记为空。
	Owner        string `json:"owner"`
	OwnerDisplay string `json:"owner_display"`
	Purpose      string `json:"purpose"`
	ExpireDate   string `json:"expire_date"`
	ExpireState  string `json:"expire_state"` // expired 已过期 / soon 7 天内到期 / 空
	ExpireDays   int    `json:"expire_days"`
}

// AccountList 是账号列表。
type AccountList struct {
	Items        []AccountItem `json:"items"`
	SupportsLock bool          `json:"supports_lock"`
	Current      string        `json:"current"` // 平台连接该实例所用的账号 user@host
	// 下面是账号总览（按搜索词过滤后、状态筛选之前）：高危 / 已锁定 / 未录入的个数。
	Total    int  `json:"total"`
	Risky    int  `json:"risky"`
	Locked   int  `json:"locked"`
	Unsaved  int  `json:"unsaved"`
	RegReady bool `json:"reg_ready"` // 旁路表 dbt_account_reg 已建：可以登记使用人 / 用途 / 到期日
}

// sysAccounts 是数据库内置账号，不允许在平台里改动。
// 其中 root 的本机账号（localhost / 127.0.0.1 / ::1）是数据库自带的管理员，在页面上锁定、删除、改密、授权回收都会把人锁在门外。
var sysAccounts = map[string]bool{
	"mysql.sys@localhost": true, "mysql.session@localhost": true, "mysql.infoschema@localhost": true, "mariadb.sys@localhost": true,
	"root@localhost": true, "root@127.0.0.1": true, "root@::1": true,
}

func quoteAccount(user, host string) string { return quoteLit(user) + "@" + quoteLit(host) }

// isBuiltinAccount 判断是不是数据库内置账号（见 sysAccounts）。
func isBuiltinAccount(user, host string) bool { return sysAccounts[strings.ToLower(user+"@"+host)] }

// 高危口径（对齐原型 p_inst.js 的 riskOf，并按需求加上 PROCESS）：
//   - 主机为 %（或空）且拥有写 / 结构变更权限（任意范围：全局、库级、表级、列级都算）；
//   - 拥有 ALL PRIVILEGES（任意范围）；
//   - 拥有全局高危权限 FILE / SUPER / SHUTDOWN / RELOAD / PROCESS / CREATE USER / REPLICATION SLAVE，或 GRANT OPTION（任意范围）。
//
// 内置账号与平台自己连接用的账号不判定（它们本来就受保护，见 protectOf）。
var (
	riskWritePrivs  = map[string]bool{"INSERT": true, "UPDATE": true, "DELETE": true, "CREATE": true, "ALTER": true, "DROP": true, "INDEX": true, "ALL PRIVILEGES": true}
	riskGlobalPrivs = []string{"FILE", "SUPER", "SHUTDOWN", "RELOAD", "PROCESS", "CREATE USER", "REPLICATION SLAVE"}
)

// grantLine 是 SHOW GRANTS 一行解析后的结果。
type grantLine struct {
	privs  []string // 权限名（大写，列级权限的列清单已去掉）
	object string   // ON 后面的对象，如 *.*、`shop`.*、`shop`.`orders`
	option bool     // WITH GRANT OPTION
}

func (g grantLine) global() bool { return g.object == "*.*" }

// parseGrantLine 解析一行 SHOW GRANTS 输出；不是 GRANT 语句（读取失败的提示等）返回 false。
func parseGrantLine(line string) (grantLine, bool) {
	u := strings.ToUpper(strings.TrimSpace(line))
	if !strings.HasPrefix(u, "GRANT ") {
		return grantLine{}, false
	}
	on := strings.Index(u, " ON ")
	to := strings.LastIndex(u, " TO ")
	if on < 0 || to < on {
		return grantLine{}, false
	}
	g := grantLine{object: strings.TrimSpace(u[on+4 : to]), option: strings.Contains(u[to:], "WITH GRANT OPTION")}
	// 权限清单按顶层逗号切分，列级权限 SELECT (`a`, `b`) 括号里的逗号不算。
	depth, start, list := 0, 0, u[len("GRANT "):on]
	flush := func(end int) {
		p := list[start:end]
		if i := strings.Index(p, "("); i >= 0 {
			p = p[:i]
		}
		if p = strings.Join(strings.Fields(p), " "); p != "" {
			g.privs = append(g.privs, p)
		}
	}
	for i, r := range list {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				flush(i)
				start = i + 1
			}
		}
	}
	flush(len(list))
	return g, true
}

// assessRisk 按上面的口径判断账号是否高危，返回原因（多条用“；”连接）。
func assessRisk(host string, grants []string) (bool, string) {
	return assessRiskIn(i18n.ZH, host, grants)
}

// assessRiskIn 同 assessRisk，原因文字按 lang 生成（落进账号列表响应；中文与 assessRisk 完全一致）。
func assessRiskIn(lang, host string, grants []string) (bool, string) {
	anyHost := host == "%" || host == ""
	var write, all, option bool
	var global []string
	seen := map[string]bool{}
	for _, line := range grants {
		g, ok := parseGrantLine(line)
		if !ok {
			continue
		}
		if g.option {
			option = true
		}
		for _, p := range g.privs {
			switch {
			case p == "GRANT OPTION":
				option = true
			case p == "ALL PRIVILEGES" || p == "ALL":
				all, write = true, true
			case riskWritePrivs[p]:
				write = true
			}
			if g.global() && !seen[p] {
				for _, h := range riskGlobalPrivs {
					if p == h {
						seen[p] = true
						global = append(global, p)
					}
				}
			}
		}
	}
	var why []string
	if anyHost && write {
		why = append(why, i18n.T(lang, "任意主机 % 可连接，且拥有写 / 结构变更权限"))
	}
	if all {
		why = append(why, i18n.T(lang, "拥有 ALL PRIVILEGES"))
	}
	if len(global) > 0 || option {
		hi := append([]string{}, global...)
		if option {
			hi = append(hi, "GRANT OPTION")
		}
		why = append(why, i18n.T(lang, "含高危权限 {list}", "list", strings.Join(hi, i18n.T(lang, "、"))))
	}
	return len(why) > 0, strings.Join(why, i18n.T(lang, "；"))
}

// currentAccount 取平台连接该实例所用的账号（CURRENT_USER()）。
func (s *Service) currentAccount(se *session) string {
	if r, err := se.query("SELECT CURRENT_USER() AS cu"); err == nil {
		if recs := r.Records(); len(recs) > 0 {
			return recs[0].Str("cu")
		}
	}
	return ""
}

func protectOf(user, host, current string) (bool, string) {
	k := user + "@" + host
	switch {
	case isBuiltinAccount(user, host):
		return true, "数据库内置账号（含 root 本机账号），不允许在平台里锁定、删除、改密、授权或回收"
	case current != "" && k == current:
		return true, "平台连接这个实例用的就是该账号，改动它会让平台失去访问"
	}
	return false, ""
}

// ListAccounts 列出实例账号及其权限。saved 为 true 时只返回平台已录入的。
func (s *Service) ListAccounts(ctx context.Context, c Caller, id uint, saved bool, q string) (*AccountList, error) {
	f := AccountFilter{Q: q}
	if saved {
		f.Saved = "1"
	}
	return s.ListAccountsFiltered(ctx, c, id, f)
}

// AccountFilter 是账号列表的筛选。Saved 取 "1" 已录入 / "0" 未录入 / 空 全部；Risk、Locked 为真时只看高危 / 已锁定的。
type AccountFilter struct {
	Saved  string
	Risk   bool
	Locked bool
	Q      string
}

// ListAccountsFiltered 列出实例账号：先按搜索词收窄，统计高危 / 已锁定 / 未录入个数，再按状态筛选。
func (s *Service) ListAccountsFiltered(ctx context.Context, c Caller, id uint, f AccountFilter) (*AccountList, error) {
	if f.Saved != "" && f.Saved != "0" && f.Saved != "1" {
		return nil, bad("范围只能是 1（已录入）或 0（未录入）")
	}
	se, err := s.open(ctx, c, id, supportsAccount, "账号管理")
	if err != nil {
		return nil, err
	}
	defer se.close()
	lockOK := se.version().supportsAccountLock()
	var r *Result
	if lockOK {
		r, err = se.query("SELECT user AS u, host AS h, account_locked AS locked FROM mysql.user ORDER BY user, host")
		if err != nil {
			lockOK = false // 个别分支的 mysql.user 没有这一列
		}
	}
	if !lockOK {
		r, err = se.query("SELECT user AS u, host AS h FROM mysql.user ORDER BY user, host")
	}
	if err != nil {
		return nil, wrapTarget(err)
	}
	savedBy := map[string]AccountRecord{}
	savedList, err := s.repo.SavedAccounts(id)
	if err != nil {
		return nil, err
	}
	for _, a := range savedList {
		savedBy[a.User+"@"+a.Host] = a
	}
	regs := map[string]AccountReg{}
	regReady := s.ext != nil && s.ext.AccountRegReady()
	if regReady {
		if m, err := s.ext.AccountRegs(id); err == nil {
			regs = m
		} else {
			slog.Warn("读取账号登记扩展失败", "instance", id, "err", err)
		}
	}
	current := s.currentAccount(se)
	q := strings.ToLower(strings.TrimSpace(f.Q))
	out := &AccountList{Items: []AccountItem{}, SupportsLock: lockOK, Current: current, RegReady: regReady}
	for _, rec := range r.Records() {
		user, host := rec.Str("u"), rec.Str("h")
		if q != "" && !strings.Contains(strings.ToLower(user+"@"+host), q) {
			continue
		}
		sv, isSaved := savedBy[user+"@"+host]
		it := AccountItem{User: user, Host: host, UserHost: quoteAccount(user, host), Privileges: []string{}, Saved: isSaved, Remark: sv.Remark}
		if g, ok := regs[regKey(user, host)]; ok {
			it.Owner, it.OwnerDisplay, it.Purpose, it.ExpireDate = g.Owner, g.OwnerDisplay, g.Purpose, g.ExpireDate
			it.ExpireState, it.ExpireDays = expireState(g.ExpireDate, s.now())
		}
		if lockOK {
			l := strings.EqualFold(rec.Str("locked"), "Y")
			it.Locked = &l
		}
		if gr, err := se.query("SHOW GRANTS FOR ?@?", user, host); err == nil {
			for _, row := range gr.Rows {
				if len(row) > 0 {
					it.Privileges = append(it.Privileges, fmt.Sprint(row[0]))
				}
			}
		} else {
			it.Privileges = append(it.Privileges, c.tr("（读取权限失败：{err}）", "err", clip(err.Error(), 120)))
		}
		it.Protected, it.ProtectReason = protectOf(user, host, current)
		it.Builtin = isBuiltinAccount(user, host)
		if !it.Protected { // 内置账号与平台自己连接用的账号已受保护，不再判定高危（同原型）
			it.Risky, it.RiskNote = assessRiskIn(c.Lang, host, it.Privileges)
		}
		out.Total++
		if it.Risky {
			out.Risky++
		}
		if it.Locked != nil && *it.Locked {
			out.Locked++
		}
		if !isSaved {
			out.Unsaved++
		}
		if (f.Saved == "1" && !isSaved) || (f.Saved == "0" && isSaved) || (f.Risk && !it.Risky) || (f.Locked && (it.Locked == nil || !*it.Locked)) {
			continue
		}
		out.Items = append(out.Items, it)
	}
	return out, nil
}

// ---- 创建账号 ----

// AccountCreate 是创建账号的请求：Host 可以用 | 分隔多个主机，每个主机创建一个账号。
type AccountCreate struct {
	User      string `json:"user"`
	Host      string `json:"host"`
	Password  string `json:"password"`
	Password2 string `json:"password2"`
	Remark    string `json:"remark"`
	// 使用人 / 用途 / 到期日：登记在旁路表 dbt_account_reg，可不填。
	Owner      string `json:"owner"`
	Purpose    string `json:"purpose"`
	ExpireDate string `json:"expire_date"`
	Confirm    bool   `json:"confirm"`
}

// CreateResult 是创建账号的结果：部分主机失败时 Created 与 Failed 都有内容。
type CreateResult struct {
	Created []string `json:"created"`
	Failed  []string `json:"failed"`
}

// CreateAccounts 创建账号（没有任何权限，需要再授权），并在平台登记备注与密码。
func (s *Service) CreateAccounts(ctx context.Context, c Caller, id uint, in AccountCreate) (*CreateResult, error) {
	if err := validAccountUser(in.User); err != nil {
		return nil, err
	}
	hosts, err := splitHosts(in.Host)
	if err != nil {
		return nil, err
	}
	if in.Password != in.Password2 {
		return nil, bad("两次输入的密码不一致")
	}
	if err := validPassword(in.Password); err != nil {
		return nil, err
	}
	remark, err := cleanText("备注", in.Remark, 255, false)
	if err != nil {
		return nil, err
	}
	if !in.Confirm {
		return nil, bad("请先确认创建")
	}
	reg, err := s.cleanReg(in.User, "", in.Owner, in.Purpose, in.ExpireDate)
	if err != nil {
		return nil, err
	}
	if !reg.empty() && !(s.ext != nil && s.ext.AccountRegReady()) {
		return nil, errExtMissing("登记使用人 / 用途 / 到期日", "071_account_reg.sql")
	}
	for _, h := range hosts { // 数据库内置账号（含 root 本机账号）的名字是保留的，不能在平台里创建（不用连目标库就能判断）
		if isBuiltinAccount(in.User, h) {
			inst, err := s.visibleInstance(c, id)
			if err != nil {
				return nil, err
			}
			e := forbidden(fmt.Sprintf("账号 %s 是数据库内置账号的保留名，不能在平台里创建", quoteAccount(in.User, h)))
			s.auditRefused(c, "创建数据库账号", inst, e.Message) // i18n-ignore 审计动作名，存库的枚举
			return nil, e
		}
	}
	se, err := s.open(ctx, c, id, supportsAccount, "账号管理")
	if err != nil {
		return nil, err
	}
	defer se.close()
	res := &CreateResult{Created: []string{}, Failed: []string{}}
	var logs []string
	for _, h := range hosts {
		stmt := fmtStmt("CREATE USER ?@? IDENTIFIED BY ?", in.User, h, secret(in.Password))
		if _, err := se.exec("CREATE USER ?@? IDENTIFIED BY ?", in.User, h, secret(in.Password)); err != nil {
			res.Failed = append(res.Failed, fmt.Sprintf("%s：%s", quoteAccount(in.User, h), err.Error()))
			logs = append(logs, stmt+" 失败："+err.Error()) // i18n-ignore 审计详情，存库的文本不翻译
			continue
		}
		res.Created = append(res.Created, h)
		logs = append(logs, stmt+" 成功") // i18n-ignore 审计详情，存库的文本不翻译
		pw := in.Password
		if err := s.repo.SaveAccountRecord(id, AccountRecord{User: in.User, Host: h, Remark: remark, Password: &pw}); err != nil {
			res.Failed = append(res.Failed, c.tr("{account} 已创建，但平台登记失败：{err}", "account", quoteAccount(in.User, h), "err", err.Error()))
		}
		if !reg.empty() {
			rg := reg
			rg.Host = h
			if err := s.ext.SaveAccountReg(id, rg, clipRunes(c.Username, 30), s.now()); err != nil {
				res.Failed = append(res.Failed, c.tr("{account} 已创建，但登记使用人 / 用途 / 到期日失败：{err}", "account", quoteAccount(in.User, h), "err", err.Error()))
			}
		}
	}
	s.audit(c, "创建数据库账号", fmt.Sprintf("实例：%s；%s", instLabel(se.inst), strings.Join(logs, "；"))) // i18n-ignore 审计，存库的文本不翻译
	if len(res.Created) == 0 {
		return nil, newErr(502, "target_error", fmt.Sprintf("创建失败：%s", strings.Join(res.Failed, c.tr("；"))))
	}
	return res, nil
}

// ---- 录入账号信息 ----

// AccountInfo 是录入/修改平台登记信息的请求（不改目标库）。Password 为空表示不修改已登记的密码。
type AccountInfo struct {
	User     string `json:"user"`
	Host     string `json:"host"`
	Password string `json:"password"`
	Remark   string `json:"remark"`
	// 使用人 / 用途 / 到期日（旁路表 dbt_account_reg）；省略（null）表示不修改，三项都改成空串表示清除登记。
	Owner      *string `json:"owner"`
	Purpose    *string `json:"purpose"`
	ExpireDate *string `json:"expire_date"`
}

// SaveAccountInfo 登记账号的备注与密码。
func (s *Service) SaveAccountInfo(c Caller, id uint, in AccountInfo) error {
	inst, err := s.visibleInstance(c, id)
	if err != nil {
		return err
	}
	if !supportsAccount(inst.DBType) {
		return unsupported(unsupportedMsg(inst.DBType, "账号管理"))
	}
	user, err := cleanText("账号", in.User, 128, true)
	if err != nil {
		return err
	}
	host, err := cleanText("主机", in.Host, 64, true)
	if err != nil {
		return err
	}
	remark, err := cleanText("备注", in.Remark, 255, false)
	if err != nil {
		return err
	}
	if len(in.Password) > 128 {
		return bad("密码过长（最多 128 个字符）")
	}
	var reg *AccountReg // 非 nil：要写入（或清除）使用人 / 用途 / 到期日
	if in.Owner != nil || in.Purpose != nil || in.ExpireDate != nil {
		ready := s.ext != nil && s.ext.AccountRegReady()
		cur := AccountReg{}
		if ready {
			if m, err := s.ext.AccountRegs(id); err == nil {
				cur = m[regKey(user, host)]
			}
		}
		pick := func(p *string, old string) string {
			if p == nil {
				return old
			}
			return *p
		}
		r, err := s.cleanReg(user, host, pick(in.Owner, cur.Owner), pick(in.Purpose, cur.Purpose), pick(in.ExpireDate, cur.ExpireDate))
		if err != nil {
			return err
		}
		if !ready && !r.empty() {
			return errExtMissing("登记使用人 / 用途 / 到期日", "071_account_reg.sql")
		}
		if ready {
			reg = &r
		}
	}
	rec := AccountRecord{User: user, Host: host, Remark: remark}
	if in.Password != "" {
		pw := in.Password
		rec.Password = &pw
	}
	if err := s.repo.SaveAccountRecord(id, rec); err != nil {
		return err
	}
	note := ""
	if in.Password != "" {
		note = "，更新了登记的密码" // i18n-ignore 审计详情，存库的文本不翻译
	}
	var regErr error
	if reg != nil {
		var err error
		if reg.empty() {
			err = s.ext.DeleteAccountReg(id, user, host)
			note += "，清除了使用人 / 用途 / 到期日" // i18n-ignore 审计详情，存库的文本不翻译
		} else {
			err = s.ext.SaveAccountReg(id, *reg, clipRunes(c.Username, 30), s.now())
			note += fmt.Sprintf("，使用人：%s，用途：%s，到期日：%s", nonEmptyStr(reg.Owner, "—"), nonEmptyStr(reg.Purpose, "—"), nonEmptyStr(reg.ExpireDate, "不限期")) // i18n-ignore 审计详情，存库的文本不翻译
		}
		if err != nil {
			regErr = newErr(500, "register_failed", fmt.Sprintf("备注已保存，但登记使用人 / 用途 / 到期日失败：%s", err.Error()))
			note += "（登记失败：" + err.Error() + "）" // i18n-ignore 审计详情，存库的文本不翻译
		}
	}
	s.audit(c, "登记账号信息", fmt.Sprintf("实例：%s，账号：%s，备注：%s%s", instLabel(inst), quoteAccount(user, host), remark, note)) // i18n-ignore 审计，存库的文本不翻译
	return regErr
}

// ---- 授权 / 回收 ----

// GrantReq 是授权或回收的结构化请求：服务端据此生成语句，不接受前端传整条 GRANT。
type GrantReq struct {
	User    string   `json:"user"`
	Host    string   `json:"host"`
	Op      string   `json:"op"`    // grant / revoke
	Scope   string   `json:"scope"` // global / db / table / column
	DBs     []string `json:"dbs"`
	Tables  []string `json:"tables"`
	Columns []string `json:"columns"`
	Privs   []string `json:"privs"`
	Confirm bool     `json:"confirm"`
}

var (
	privDB     = []string{"SELECT", "INSERT", "UPDATE", "DELETE", "CREATE", "ALTER", "INDEX", "DROP", "CREATE TEMPORARY TABLES", "SHOW VIEW", "CREATE ROUTINE", "ALTER ROUTINE", "EXECUTE", "CREATE VIEW", "EVENT", "TRIGGER", "GRANT OPTION", "LOCK TABLES", "REFERENCES"}
	privGlobal = []string{"SELECT", "INSERT", "UPDATE", "DELETE", "FILE", "CREATE", "ALTER", "INDEX", "DROP", "CREATE TEMPORARY TABLES", "SHOW VIEW", "CREATE ROUTINE", "ALTER ROUTINE", "EXECUTE", "CREATE VIEW", "EVENT", "TRIGGER", "GRANT OPTION", "SUPER", "PROCESS", "RELOAD", "SHUTDOWN", "SHOW DATABASES", "LOCK TABLES", "REFERENCES", "REPLICATION CLIENT", "REPLICATION SLAVE", "CREATE USER"}
	privTable  = []string{"SELECT", "INSERT", "UPDATE", "DELETE", "CREATE", "ALTER", "INDEX", "DROP", "SHOW VIEW", "CREATE VIEW", "TRIGGER", "GRANT OPTION", "REFERENCES"}
	privColumn = []string{"SELECT", "INSERT", "UPDATE", "REFERENCES"}
	// 这些权限能改变整个实例或读写服务器文件，预览里单独警告。
	privDanger = map[string]bool{"FILE": true, "SUPER": true, "PROCESS": true, "RELOAD": true, "SHUTDOWN": true, "CREATE USER": true,
		"REPLICATION CLIENT": true, "REPLICATION SLAVE": true, "GRANT OPTION": true, "ALL PRIVILEGES": true}
)

type grantStmt struct {
	sql  string
	args []any
	alt  *grantStmt // 回收时已授出的库级权限库名可能没有转义，回退用
}

func (g grantStmt) display() string { return fmtStmt(g.sql, g.args...) + ";" }

// GrantPlan 是授权预览。
type GrantPlan struct {
	Statements []string `json:"statements"`
	Warnings   []string `json:"warnings"`
	stmts      []grantStmt
}

// normPrivs 规整并校验权限名：大写、去重，只接受该范围白名单里的权限，按白名单顺序输出。
// ALL PRIVILEGES 只在全局和库级可选（高危，预览里会警告）；选了它就不再列其他权限（GRANT OPTION 例外，它不包含在 ALL 里）。
func normPrivs(scope string, in []string) ([]string, error) {
	var allowed []string
	allowAll := false
	switch scope {
	case "global":
		allowed, allowAll = privGlobal, true
	case "db":
		allowed, allowAll = privDB, true
	case "table":
		allowed = privTable
	case "column":
		allowed = privColumn
	default:
		return nil, bad("权限类型只能是 global / db / table / column")
	}
	want := map[string]bool{}
	for _, p := range in {
		p = strings.Join(strings.Fields(strings.ToUpper(p)), " ")
		if p == "GRANT" {
			p = "GRANT OPTION"
		}
		if p == "ALL" {
			p = "ALL PRIVILEGES"
		}
		ok := allowAll && p == "ALL PRIVILEGES"
		for _, a := range allowed {
			ok = ok || a == p
		}
		if !ok {
			return nil, bad(fmt.Sprintf("该范围不支持权限 %s", strconv.Quote(p)))
		}
		want[p] = true
	}
	if len(want) == 0 {
		return nil, bad("请选择要授予/回收的权限")
	}
	if want["ALL PRIVILEGES"] {
		out := []string{"ALL PRIVILEGES"}
		if want["GRANT OPTION"] {
			out = append(out, "GRANT OPTION")
		}
		return out, nil
	}
	var out []string
	for _, a := range allowed {
		if want[a] {
			out = append(out, a)
		}
	}
	return out, nil
}

func quoteList(label string, names []string, max int) ([]string, error) {
	if len(names) == 0 {
		return nil, bad(fmt.Sprintf("请选择%s", label))
	}
	if len(names) > max {
		return nil, bad(fmt.Sprintf("一次最多选择 %d 个%s", max, label))
	}
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		if _, err := quoteIdent(n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// buildGrant 生成授权/回收语句。账号、库、表、列都走校验与引用，权限只取白名单。
func buildGrant(req GrantReq) (*GrantPlan, error) {
	if req.Op != "grant" && req.Op != "revoke" {
		return nil, bad("操作类型只能是 grant 或 revoke")
	}
	if req.User == "" && req.Host == "" {
		return nil, bad("缺少账号")
	}
	privs, err := normPrivs(req.Scope, req.Privs)
	if err != nil {
		return nil, err
	}
	plan := &GrantPlan{}
	var warns []string
	for _, p := range privs {
		if privDanger[p] {
			warns = append(warns, fmt.Sprintf("包含高危权限 %s", p))
		}
	}
	verb, prep := "GRANT", "TO"
	if req.Op == "revoke" {
		verb, prep = "REVOKE", "FROM"
	}
	add := func(on string, privSQL string) {
		plan.stmts = append(plan.stmts, grantStmt{sql: verb + " " + privSQL + " ON " + on + " " + prep + " ?@?", args: []any{req.User, req.Host}})
	}
	list := strings.Join(privs, ", ")
	switch req.Scope {
	case "global":
		add("*.*", list)
	case "db":
		dbs, err := quoteList("数据库", req.DBs, 100)
		if err != nil {
			return nil, err
		}
		for _, d := range dbs {
			esc := escapeDBPattern(d)
			on := "`" + strings.ReplaceAll(esc, "`", "``") + "`.*"
			add(on, list)
			if esc != d && req.Op == "revoke" {
				// 已授出的库级权限库名可能没有转义：回收时转义形式报“没有这条授权”就回退
				alt := grantStmt{sql: verb + " " + list + " ON `" + strings.ReplaceAll(d, "`", "``") + "`.* " + prep + " ?@?", args: []any{req.User, req.Host}}
				plan.stmts[len(plan.stmts)-1].alt = &alt
			}
			if esc != d {
				warns = append(warns, fmt.Sprintf("库名 %s 含 _ 或 %%，库级授权里它们是通配符，已转义为只匹配这个库", d))
			}
		}
	case "table":
		dbs, err := quoteList("数据库", req.DBs, 1)
		if err != nil {
			return nil, err
		}
		tbs, err := quoteList("表", req.Tables, 100)
		if err != nil {
			return nil, err
		}
		qdb, _ := quoteIdent(dbs[0])
		for _, t := range tbs {
			qt, _ := quoteIdent(t)
			add(qdb+"."+qt, list)
		}
	case "column":
		dbs, err := quoteList("数据库", req.DBs, 1)
		if err != nil {
			return nil, err
		}
		tbs, err := quoteList("表", req.Tables, 1)
		if err != nil {
			return nil, err
		}
		cols, err := quoteList("列", req.Columns, 100)
		if err != nil {
			return nil, err
		}
		var qcols []string
		for _, col := range cols {
			q, _ := quoteIdent(col)
			qcols = append(qcols, q)
		}
		parts := make([]string, len(privs))
		for i, p := range privs {
			parts[i] = p + " (" + strings.Join(qcols, ", ") + ")"
		}
		qdb, _ := quoteIdent(dbs[0])
		qt, _ := quoteIdent(tbs[0])
		add(qdb+"."+qt, strings.Join(parts, ", "))
	}
	for _, st := range plan.stmts {
		plan.Statements = append(plan.Statements, st.display())
	}
	plan.Warnings = uniq(warns)
	return plan, nil
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// requireAccount 确认账号确实存在于目标库，并拒绝对受保护账号的改动（forbidProtected）。
func (s *Service) requireAccount(se *session, user, host string, forbidProtected bool) error {
	if user == "" && host == "" {
		return bad("缺少账号")
	}
	r, err := se.query("SELECT 1 AS n FROM mysql.user WHERE user = ? AND host = ?", user, host)
	if err != nil {
		return wrapTarget(err)
	}
	if len(r.Rows) == 0 {
		return notFound(fmt.Sprintf("账号 %s 不存在", quoteAccount(user, host)))
	}
	if isBuiltinAccount(user, host) { // 内置账号（含 root 本机账号）：任何操作都不允许，授权也不行；平台自用账号则只拒绝回收（见下）
		_, why := protectOf(user, host, "")
		return forbidden(fmt.Sprintf("不能对账号 %s 做这个操作：%s", quoteAccount(user, host), why))
	}
	if forbidProtected {
		if p, why := protectOf(user, host, s.currentAccount(se)); p {
			return forbidden(fmt.Sprintf("不能对账号 %s 做这个操作：%s", quoteAccount(user, host), why))
		}
	}
	return nil
}

// requireAccountAudited 同 requireAccount；被拒绝（受保护 / 内置账号）时按 action 留一条“（拒绝）”审计。预览类接口不用它（预览不是操作）。
func (s *Service) requireAccountAudited(c Caller, action string, se *session, user, host string, forbidProtected bool) error {
	err := s.requireAccount(se, user, host, forbidProtected)
	if refusedBy(err) {
		s.auditRefused(c, action, se.inst, err.Error())
	}
	return err
}

// PreviewGrant 生成授权/回收语句预览（第一步），不执行。需要连接目标库确认账号存在。
func (s *Service) PreviewGrant(ctx context.Context, c Caller, id uint, req GrantReq) (*GrantPlan, error) {
	plan, err := buildGrant(req)
	if err != nil {
		return nil, err
	}
	se, err := s.open(ctx, c, id, supportsAccount, "账号管理")
	if err != nil {
		return nil, err
	}
	defer se.close()
	if err := s.requireAccount(se, req.User, req.Host, req.Op == "revoke"); err != nil {
		return nil, err
	}
	if plan.Warnings == nil {
		plan.Warnings = []string{}
	}
	return plan, nil
}

// Grant 执行授权/回收（第二步）。语句由服务端根据结构化参数重新生成。
func (s *Service) Grant(ctx context.Context, c Caller, id uint, req GrantReq) (*GrantPlan, error) {
	plan, err := buildGrant(req)
	if err != nil {
		return nil, err
	}
	if !req.Confirm {
		return nil, bad("请先确认授权变更")
	}
	se, err := s.open(ctx, c, id, supportsAccount, "账号管理")
	if err != nil {
		return nil, err
	}
	defer se.close()
	action := "账号授权" // i18n-ignore 审计动作名，存库的枚举
	if req.Op == "revoke" {
		action = "账号回收权限" // i18n-ignore 审计动作名，存库的枚举
	}
	if err := s.requireAccountAudited(c, action, se, req.User, req.Host, req.Op == "revoke"); err != nil {
		return nil, err
	}
	var logs []string
	var failure error
	for _, st := range plan.stmts {
		_, err := se.exec(st.sql, st.args...)
		used := st
		if err != nil && isNoSuchGrant(err) && st.alt != nil {
			used = *st.alt
			_, err = se.exec(used.sql, used.args...)
		}
		if err != nil {
			logs = append(logs, used.display()+" 失败："+err.Error()) // i18n-ignore 审计详情，存库的文本不翻译
			failure = err
			break
		}
		logs = append(logs, used.display()+" 成功") // i18n-ignore 审计详情，存库的文本不翻译
	}
	s.audit(c, action, fmt.Sprintf("实例：%s，账号：%s；%s", instLabel(se.inst), quoteAccount(req.User, req.Host), strings.Join(logs, "；"))) // i18n-ignore 审计，存库的文本不翻译
	if failure != nil {
		return nil, wrapTarget(failure)
	}
	if plan.Warnings == nil {
		plan.Warnings = []string{}
	}
	return plan, nil
}

// ---- 改密 / 锁定 / 删除 ----

// PasswordReq 是改密请求。
type PasswordReq struct {
	User      string `json:"user"`
	Host      string `json:"host"`
	Password  string `json:"password"`
	Password2 string `json:"password2"`
	Confirm   bool   `json:"confirm"`
}

// ResetPassword 修改库里账号的密码，同时更新平台登记的密码。
func (s *Service) ResetPassword(ctx context.Context, c Caller, id uint, in PasswordReq) error {
	if in.Password != in.Password2 {
		return bad("两次输入的密码不一致")
	}
	if err := validPassword(in.Password); err != nil {
		return err
	}
	if !in.Confirm {
		return bad("请先确认修改密码")
	}
	se, err := s.open(ctx, c, id, supportsAccount, "账号管理")
	if err != nil {
		return err
	}
	defer se.close()
	if err := s.requireAccountAudited(c, "修改数据库账号密码", se, in.User, in.Host, true); err != nil { // i18n-ignore 审计动作名，存库的枚举
		return err
	}
	stmt := fmtStmt("ALTER USER ?@? IDENTIFIED BY ?", in.User, in.Host, secret(in.Password))
	_, execErr := se.exec("ALTER USER ?@? IDENTIFIED BY ?", in.User, in.Host, secret(in.Password))
	outcome := "成功" // i18n-ignore 审计详情，存库的文本不翻译
	if execErr != nil {
		outcome = "失败：" + execErr.Error() // i18n-ignore 审计详情，存库的文本不翻译
	}
	s.audit(c, "修改数据库账号密码", fmt.Sprintf("实例：%s，%s，结果：%s", instLabel(se.inst), stmt, outcome)) // i18n-ignore 审计，存库的文本不翻译
	if execErr != nil {
		return wrapTarget(execErr)
	}
	pw := in.Password
	if err := s.repo.SaveAccountRecord(id, AccountRecord{User: in.User, Host: in.Host, Password: &pw, KeepRemark: true}); err != nil {
		return newErr(500, "register_failed", fmt.Sprintf("密码已在数据库里修改，但更新平台登记的密码失败：%s", err.Error()))
	}
	return nil
}

// LockReq 是锁定/解锁请求。
type LockReq struct {
	User    string `json:"user"`
	Host    string `json:"host"`
	Lock    bool   `json:"lock"`
	Confirm bool   `json:"confirm"`
}

// SetAccountLock 锁定或解锁账号（ALTER USER … ACCOUNT LOCK / UNLOCK）。
func (s *Service) SetAccountLock(ctx context.Context, c Caller, id uint, in LockReq) error {
	if !in.Confirm {
		return bad("请先确认操作")
	}
	se, err := s.open(ctx, c, id, supportsAccount, "账号管理")
	if err != nil {
		return err
	}
	defer se.close()
	if !se.version().supportsAccountLock() {
		return unsupported("该数据库版本不支持锁定账号（MySQL 5.7.6+ / MariaDB 10.4.2+）")
	}
	verb, action := "UNLOCK", "解锁数据库账号" // i18n-ignore 审计动作名，存库的枚举
	if in.Lock {
		verb, action = "LOCK", "锁定数据库账号" // i18n-ignore 审计动作名，存库的枚举
	}
	if err := s.requireAccountAudited(c, action, se, in.User, in.Host, true); err != nil {
		return err
	}
	sql := "ALTER USER ?@? ACCOUNT " + verb
	_, execErr := se.exec(sql, in.User, in.Host)
	outcome := "成功" // i18n-ignore 审计详情，存库的文本不翻译
	if execErr != nil {
		outcome = "失败：" + execErr.Error() // i18n-ignore 审计详情，存库的文本不翻译
	}
	s.audit(c, action, fmt.Sprintf("实例：%s，%s，结果：%s", instLabel(se.inst), fmtStmt(sql, in.User, in.Host), outcome)) // i18n-ignore 审计，存库的文本不翻译
	return wrapTarget(execErr)
}

// DropReq 是删除账号请求。
type DropReq struct {
	User    string `json:"user"`
	Host    string `json:"host"`
	Confirm bool   `json:"confirm"`
}

// DropAccount 删除库里的账号，并删除平台登记。
func (s *Service) DropAccount(ctx context.Context, c Caller, id uint, in DropReq) error {
	if !in.Confirm {
		return bad("请先确认删除")
	}
	se, err := s.open(ctx, c, id, supportsAccount, "账号管理")
	if err != nil {
		return err
	}
	defer se.close()
	if err := s.requireAccountAudited(c, "删除数据库账号", se, in.User, in.Host, true); err != nil { // i18n-ignore 审计动作名，存库的枚举
		return err
	}
	_, execErr := se.exec("DROP USER ?@?", in.User, in.Host)
	outcome := "成功" // i18n-ignore 审计详情，存库的文本不翻译
	if execErr != nil {
		outcome = "失败：" + execErr.Error() // i18n-ignore 审计详情，存库的文本不翻译
	}
	s.audit(c, "删除数据库账号", fmt.Sprintf("实例：%s，%s，结果：%s", instLabel(se.inst), fmtStmt("DROP USER ?@?", in.User, in.Host), outcome)) // i18n-ignore 审计，存库的文本不翻译
	if execErr != nil {
		return wrapTarget(execErr)
	}
	if err := s.repo.DeleteAccountRecord(id, in.User, in.Host); err != nil {
		return newErr(500, "register_failed", fmt.Sprintf("账号已删除，但清理平台登记失败：%s", err.Error()))
	}
	if s.ext != nil && s.ext.AccountRegReady() { // 使用人 / 用途 / 到期日登记也一并清掉；失败只记日志
		if err := s.ext.DeleteAccountReg(id, in.User, in.Host); err != nil {
			slog.Warn("清理账号登记扩展失败", "instance", id, "err", err)
		}
	}
	return nil
}

// ListObjects 给授权对话框提供库、表、列的下拉：只传 db 返回库里的表，传 db 与 table 返回列，都不传返回库名（不含系统库）。
func (s *Service) ListObjects(ctx context.Context, c Caller, id uint, db, table string) ([]string, error) {
	se, err := s.open(ctx, c, id, supportsAccount, "账号管理")
	if err != nil {
		return nil, err
	}
	defer se.close()
	var r *Result
	switch {
	case db == "":
		r, err = se.query("SELECT schema_name AS n FROM information_schema.schemata WHERE schema_name NOT IN (" + sysSchemas + ") ORDER BY schema_name")
	case table == "":
		r, err = se.query("SELECT table_name AS n FROM information_schema.tables WHERE table_schema = ? ORDER BY table_name LIMIT 5000", db)
	default:
		r, err = se.query("SELECT column_name AS n FROM information_schema.columns WHERE table_schema = ? AND table_name = ? ORDER BY ordinal_position LIMIT 5000", db, table)
	}
	if err != nil {
		return nil, wrapTarget(err)
	}
	out := []string{}
	for _, rec := range r.Records() {
		out = append(out, rec.Str("n"))
	}
	return out, nil
}

// AccountOwners 返回登记“使用人”可选的用户（所有启用的平台用户）。
func (s *Service) AccountOwners(c Caller, id uint) ([]UserOption, error) {
	if _, err := s.visibleInstance(c, id); err != nil {
		return nil, err
	}
	return s.owners()
}
