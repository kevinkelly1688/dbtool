// @author: kevinkelly
// @license: Apache Licence
// @file: bootstrap.go
// @time: 2026/10/28

package dbtadmin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// BootstrapOptions 是 bootstrap 命令的选项。
type BootstrapOptions struct {
	Root    string // 脚本根目录：<Root>/bootstrap/*.sql、<Root>/sql/*.sql
	Apply   bool   // 真正写入（默认只演练）
	OnlySQL bool   // 只执行 sql/*.sql 扩展表脚本（升级：基础表已存在）
	Yes     bool   // 写入前不再交互确认
	// SkipCharsetCheck 跳过“目标库默认字符集必须是 utf8mb4”的预检（--skip-charset-check；调用方保证同时带了 Yes）。
	SkipCharsetCheck bool
}

// script 是一个已读入并切分好的脚本文件。
type script struct {
	Rel   string // 相对根目录的名字，如 bootstrap/001_base_schema.sql（也是 dbt_bootstrap_applied.file）
	SHA   string
	Stmts []Stmt
}

// listSQL 返回 dir 下的 *.sql 文件名（升序）。目录不存在返回 (nil, false, nil)。
func listSQL(dir string) (names []string, exists bool, err error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	for _, de := range ents {
		n := de.Name()
		if de.IsDir() || strings.HasPrefix(n, ".") || !strings.EqualFold(filepath.Ext(n), ".sql") {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	return names, true, nil
}

// loadScripts 读入并切分 root/<group>/*.sql。
func loadScripts(root, group string) ([]script, bool, error) {
	names, exists, err := listSQL(filepath.Join(root, group))
	if err != nil || !exists {
		return nil, exists, err
	}
	var out []script
	for _, n := range names {
		p := filepath.Join(root, group, n)
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, true, err
		}
		stmts, err := SplitSQL(string(b), SplitOptions{})
		if err != nil {
			return nil, true, fmt.Errorf("%s/%s %v", group, n, err)
		}
		sum := sha256.Sum256(b)
		out = append(out, script{Rel: group + "/" + n, SHA: hex.EncodeToString(sum[:]), Stmts: stmts})
	}
	return out, true, nil
}

func totalStmts(ss []script) int {
	n := 0
	for _, s := range ss {
		n += len(s.Stmts)
	}
	return n
}

// usersState 返回 dbt_sql_users 的状态：absent（表不存在）、empty、nonempty。
func usersState(ctx context.Context, st Store) (string, int64, error) {
	ok, err := st.HasTable(ctx, tblUsers)
	if err != nil {
		return "", 0, err
	}
	if !ok {
		return "absent", 0, nil
	}
	n, err := st.CountRows(ctx, tblUsers)
	if err != nil {
		return "", 0, err
	}
	if n == 0 {
		return "empty", 0, nil
	}
	return "nonempty", n, nil
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// RunBootstrap 执行 bootstrap 命令，返回退出码。
func (e *Env) RunBootstrap(ctx context.Context, st Store, tgt Target, opt BootstrapOptions) int {
	info, err := st.ServerInfo(ctx)
	if err != nil {
		e.err("错误：读取服务器信息失败：%s\n", RedactMsg(err.Error()))
		return ExitFailed
	}
	if info.Database == "" {
		e.err("错误：连接没有选中任何库\n")
		return ExitFailed
	}
	e.out("目标：%s，MySQL %s\n", tgt, info.Version)
	mode := "演练（不写入任何东西）"
	if opt.Apply {
		mode = "应用（写入）"
	}
	e.out("模式：%s\n", mode)

	// ---- 库默认字符集预检（表 / 字段都不指定字符集，建出来的表完全取决于它；不是 utf8mb4 直接拒绝） ----
	if _, _, ok := e.charsetGate(ctx, st, info.Version, info.Database, opt.SkipCharsetCheck); !ok {
		return ExitFailed
	}
	e.out("提示：本工具不会修改已存在的表（包括它们的字符集 / 校验规则）；只用 CREATE TABLE IF NOT EXISTS 补缺失的表。\n")

	// ---- 读入并校验所有脚本（任何一个文件有语法层面的问题，在写入之前就报出来） ----
	var plan []script
	if !opt.OnlySQL {
		bs, exists, err := loadScripts(opt.Root, "bootstrap")
		if err != nil {
			e.err("错误：读取脚本失败：%v\n", err)
			return ExitFailed
		}
		if !exists || len(bs) == 0 {
			e.err("错误：在 %s 下没有找到 bootstrap/*.sql（基础表与基础数据脚本）。\n"+
				"  - 用 make dist 生成的安装包里有 bootstrap/；源码树里用 --sql-root deploy；\n"+
				"  - 只想执行 sql/*.sql 扩展表脚本（升级：基础表已存在）请加 --only-sql。\n", opt.Root)
			return ExitFailed
		}
		plan = append(plan, bs...)
	}
	ss, exists, err := loadScripts(opt.Root, "sql")
	if err != nil {
		e.err("错误：读取脚本失败：%v\n", err)
		return ExitFailed
	}
	if !exists || len(ss) == 0 {
		e.err("错误：在 %s 下没有找到 sql/*.sql\n", opt.Root)
		return ExitFailed
	}
	plan = append(plan, ss...)

	// ---- 前置检查 ----
	state, nUsers, err := usersState(ctx, st)
	if err != nil {
		e.err("错误：检查 %s 失败：%s\n", tblUsers, RedactMsg(err.Error()))
		return ExitFailed
	}
	baseTables, err := st.CountBaseTables(ctx)
	if err != nil {
		e.err("错误：统计表数量失败：%s\n", RedactMsg(err.Error()))
		return ExitFailed
	}
	switch {
	case opt.OnlySQL:
		e.out("流程：仅执行 sql/*.sql 扩展表脚本（--only-sql）\n")
		if state == "absent" {
			e.out("提示：库里没有 %s，看起来是空库；全新安装请去掉 --only-sql，先建基础表与基础数据\n", tblUsers)
		}
	default:
		e.out("流程：bootstrap/*.sql（基础表 + 基础数据）→ sql/*.sql（扩展表脚本）\n")
		switch state {
		case "nonempty":
			e.err("错误：%s 里已有 %d 个用户，拒绝执行基础表 / 基础数据脚本（本流程只用于全新的空库）。\n"+
				"  - 升级（出现了新的 sql/*.sql 扩展表脚本）：只执行扩展表脚本，加 --only-sql。\n", tblUsers, nUsers)
			return ExitFailed
		case "empty":
			e.out("前置检查：%s 存在但为空，允许（脚本幂等）\n", tblUsers)
		default:
			e.out("前置检查：%s 不存在，允许\n", tblUsers)
		}
		if baseTables > 0 {
			e.out("提示：库里已有 %d 张表：完整的 bootstrap 会写入基础数据，请确认这个库确实是准备初始化的新库。\n", baseTables)
		}
	}

	applied, err := st.AppliedSHAs(ctx)
	if err != nil {
		e.err("错误：读取已应用记录失败：%s\n", RedactMsg(err.Error()))
		return ExitFailed
	}
	e.out("计划执行 %d 个文件、%d 条语句：\n", len(plan), totalStmts(plan))
	changed := 0
	for _, f := range plan {
		note := ""
		if prev, ok := applied[f.Rel]; ok {
			if prev == f.SHA {
				note = "（已应用过，内容相同；幂等重跑）"
			} else {
				changed++
				note = fmt.Sprintf("（注意：文件内容与上次记录不同 %s → %s；脚本幂等，仍会重跑）", short(prev), short(f.SHA))
			}
		}
		e.out("  %-44s %4d 条%s\n", f.Rel, len(f.Stmts), note)
	}
	if changed > 0 {
		e.out("提示：有 %d 个文件的内容与上次应用时不同（升级后的新版本）；重跑是安全的，只会补上新增的部分。\n", changed)
	}

	if !opt.Apply {
		e.out("\n这是演练：没有向库里写入任何东西。确认无误后加 --apply（并加 --yes 或在终端里确认）执行。\n")
		return ExitOK
	}

	// ---- 写入 ----
	ok, err := e.confirm(opt.Yes, fmt.Sprintf("即将在 %s 上执行上述 %d 个文件。", tgt, len(plan)))
	if err != nil {
		e.err("错误：%v\n", err)
		return ExitUsage
	}
	if !ok {
		e.err("已取消，没有写入任何东西\n")
		return ExitFailed
	}
	before := baseTables
	if err := st.EnsureAppliedTable(ctx); err != nil {
		e.err("错误：创建 %s 失败（账号需要有 CREATE 权限）：%s\n", TblApplied, RedactMsg(err.Error()))
		return ExitFailed
	}
	stmtsRun, filesRun := 0, 0
	for _, f := range plan {
		e.out("执行 %s（%d 条）...", f.Rel, len(f.Stmts))
		for i, s := range f.Stmts {
			if err := st.ExecStmt(ctx, s.Text); err != nil {
				e.out(" 失败\n")
				e.err("错误：%s 第 %d 条语句失败（共 %d 条；位于文件第 %d 行）：%s\n  语句：%s\n"+
					"已执行的文件与语句保持原样；脚本幂等，修复原因后可直接重跑。\n",
					f.Rel, i+1, len(f.Stmts), s.Line, RedactMsg(err.Error()), Redact(s.Text, 200))
				return ExitFailed
			}
			stmtsRun++
		}
		if err := st.RecordApplied(ctx, f.Rel, f.SHA, e.Version, e.Now()); err != nil {
			e.out(" 失败\n")
			e.err("错误：记录 %s 失败：%s\n", f.Rel, RedactMsg(err.Error()))
			return ExitFailed
		}
		filesRun++
		e.out(" 完成\n")
	}

	// ---- 审计与摘要 ----
	extra := fmt.Sprintf("库 %s；文件 %d 个，语句 %d 条", tgt.DB, filesRun, stmtsRun)
	switch {
	case opt.OnlySQL:
		e.writeAudit(ctx, st, "CLI-执行建表脚本", extra+"；--only-sql")
	default:
		e.writeAudit(ctx, st, "CLI-初始化基础表与数据", extra)
	}

	after, err := st.CountBaseTables(ctx)
	if err != nil {
		after = before
	}
	permRows := tblPermission + " 不存在"
	if ok, err := st.HasTable(ctx, tblPermission); err == nil && ok {
		if n, err := st.CountRows(ctx, tblPermission); err == nil {
			permRows = fmt.Sprintf("%s %d 行", tblPermission, n)
		}
	}
	e.out("\n完成：执行 %d 个文件、%d 条语句；本次新建 %d 张表（含 %s），库内现有 %d 张表；%s。\n",
		filesRun, stmtsRun, after-before, TblApplied, after, permRows)
	switch {
	case opt.OnlySQL:
		e.out("下一步：重启 dbtool 服务使新表生效。\n")
	default:
		e.out("下一步：\n" +
			"  1. 创建管理员：  dbt-admin admin create --username <用户名>   （密码用环境变量 " + PasswordEnv + " 或交互输入）\n" +
			"  2. 配置密钥：    dbt-admin gen-secrets   （新部署；写入 dbtool.env 后 SECRET_KEY / MIRAGE_* 不可再改）\n" +
			"  3. 启动服务：    systemctl start dbtool（或 install.sh），再访问 /healthz 与登录页\n")
	}
	return ExitOK
}

// writeAudit 在 dbt_audit_log 存在时写一条审计；不存在或失败只提示，不影响命令结果。
func (e *Env) writeAudit(ctx context.Context, st Store, action, extra string) {
	ok, err := st.HasTable(ctx, tblAuditLog)
	if err != nil || !ok {
		e.out("提示：%s 不存在，未写审计\n", tblAuditLog)
		return
	}
	err = st.InTx(ctx, func(tx Tx) error {
		return tx.InsertAudit(e.auditEntry(action, extra))
	})
	if err != nil {
		e.err("警告：写审计失败：%s\n", RedactMsg(err.Error()))
	}
}

func (e *Env) auditEntry(action, extra string) AuditEntry {
	a := e.actor()
	return AuditEntry{UserName: a, UserDisplay: a, Action: action, ExtraInfo: extra, At: e.Now()}
}

// StatusOptions 是 status 命令的选项。
type StatusOptions struct {
	Porcelain        bool // 输出 key=value 行，供脚本解析
	SkipCharsetCheck bool // 不因库默认字符集不是 utf8mb4 而返回失败（调用方保证同时带了 --yes）
}

// RunStatus 打印库的状态（只读）。库默认字符集不是 utf8mb4 时退出码为 ExitFailed（输出仍完整打印）。
func (e *Env) RunStatus(ctx context.Context, st Store, tgt Target, opt StatusOptions) int {
	porcelain := opt.Porcelain
	info, err := st.ServerInfo(ctx)
	if err != nil {
		e.err("错误：读取服务器信息失败：%s\n", RedactMsg(err.Error()))
		return ExitFailed
	}
	state, nUsers, err := usersState(ctx, st)
	if err != nil {
		e.err("错误：检查 %s 失败：%s\n", tblUsers, RedactMsg(err.Error()))
		return ExitFailed
	}
	supers := 0
	if state == "nonempty" {
		us, err := st.ListUsers(ctx)
		if err != nil {
			e.err("错误：读取用户失败：%s\n", RedactMsg(err.Error()))
			return ExitFailed
		}
		for _, u := range us {
			if u.IsSuperuser && u.IsActive {
				supers++
			}
		}
	}
	applied, err := st.AppliedSHAs(ctx)
	if err != nil {
		e.err("错误：读取已应用记录失败：%s\n", RedactMsg(err.Error()))
		return ExitFailed
	}
	// 库默认字符集：读失败按“未知”处理（不是 utf8mb4 的结论只在读到时才下）
	cs, csErr := st.DatabaseCharset(ctx)
	level, csLines := charsetOK, []string(nil)
	if csErr == nil {
		level, csLines = EvaluateCharset(cs, info.Version, info.Database)
	}
	csStatus := map[charsetLevel]string{charsetOK: "ok", charsetWarn: "warn", charsetRefuse: "fail"}[level]
	if csErr != nil {
		csStatus = "unknown"
	}
	code := ExitOK
	if level == charsetRefuse && !opt.SkipCharsetCheck {
		code = ExitFailed
	}
	if porcelain {
		e.out("db=%s\nhost=%s\nusers_table=%s\nusers=%d\nsuperusers=%d\nbootstrap_applied=%d\ncharset=%s\ncollation=%s\ncharset_check=%s\n",
			tgt.DB, tgt.HostPort(), state, nUsers, supers, len(applied), cs.Charset, cs.Collation, csStatus)
		if code != ExitOK {
			for _, l := range csLines {
				e.err("%s\n", l)
			}
		}
		return code
	}
	e.out("目标：%s，MySQL %s\n", tgt, info.Version)
	if csErr != nil {
		e.out("库默认字符集 / 校验规则：读取失败（%s）\n", RedactMsg(csErr.Error()))
	} else {
		e.out("库默认字符集 / 校验规则：%s（所有表、所有字段都不指定，新建的表全部跟随它；已存在的表不会被本工具修改）\n", cs)
		for _, l := range csLines {
			if level == charsetRefuse {
				e.err("%s\n", l)
			} else {
				e.out("%s\n", l)
			}
		}
	}
	e.out("%s：%s（%d 个用户，其中启用的超级用户 %d 个）\n", tblUsers, map[string]string{"absent": "不存在", "empty": "为空", "nonempty": "有数据"}[state], nUsers, supers)
	e.out("已记录应用的脚本文件：%d 个\n", len(applied))
	switch state {
	case "absent", "empty":
		e.out("结论：这是空库，可以执行 dbt-admin bootstrap --apply\n")
	default:
		if supers == 0 {
			e.out("结论：没有启用的超级用户，可用 dbt-admin admin create 创建\n")
		} else {
			e.out("结论：库里已有数据；bootstrap 只能加 --only-sql（执行 sql/*.sql 扩展表脚本）\n")
		}
	}
	return code
}
