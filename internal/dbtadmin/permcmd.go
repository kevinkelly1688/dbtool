// @author: kevinkelly
// @license: Apache Licence
// @file: permcmd.go
// @time: 2026/10/28

package dbtadmin

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"dbtool/internal/btnperm"
)

// perm 子命令：权限体系的一次性迁移（原权限码 → 按钮权限）。
//
//	dbt-admin perm migrate [--apply] [--yes] [--rollback-file 路径]
//
// 规则见 internal/btnperm/migrate.go：把权限组 / 用户单独持有的原权限码按登记表展开成按钮授权，丢弃没有对应按钮的早期遗留码，
// 再把迁移状态标成已迁移。只增不减，可重复执行；默认只演练，--apply 才写，并先把逐条撤销的 SQL 存成文件。

// PermMigrator 是迁移的数据入口（真实实现包装 btnperm 的 GORM 仓库）。
type PermMigrator interface {
	Plan() (*btnperm.MigratePlan, error)
	Apply(plan *btnperm.MigratePlan, by string, at time.Time) (*btnperm.MigrateResult, error)
}

// ErrPermTables 表示按钮权限表还没创建。
var ErrPermTables = errors.New("按钮权限表还没创建（deploy/sql/019_btnperm.sql）：请先执行 dbt-admin bootstrap --only-sql --apply")

type gormMigrator struct{ repo *btnperm.GormRepo }

func (m gormMigrator) check() error {
	ok, err := m.repo.TablesReady()
	if err != nil {
		return err
	}
	if !ok {
		return ErrPermTables
	}
	return nil
}

func (m gormMigrator) Plan() (*btnperm.MigratePlan, error) {
	if err := m.check(); err != nil {
		return nil, err
	}
	return btnperm.PlanMigrate(m.repo)
}

func (m gormMigrator) Apply(plan *btnperm.MigratePlan, by string, at time.Time) (*btnperm.MigrateResult, error) {
	if err := m.check(); err != nil {
		return nil, err
	}
	return btnperm.ApplyMigrate(m.repo, plan, by, at)
}

// permTimeout 是迁移命令的总期限：任何一步卡住都报错退出，不让升级脚本无限等下去。
const permTimeout = 2 * time.Minute

// PermMigrator 在当前连接的库上建迁移入口。
// 必须走固定的那个连接 s.conn：dbt-admin 的连接池只开一个连接（MaxOpenConns(1)，会话级 SET 要在整个运行期里有效），它已经被 s.conn 占着，
// 如果让 GORM 另从池里取连接，第一条查询就会永远等不到空闲连接（1.79 就是这样卡住了升级）。
func (s *mysqlStore) PermMigrator(ctx context.Context) (PermMigrator, error) {
	gdb, err := gorm.Open(mysql.New(mysql.Config{Conn: s.conn, SkipInitializeWithVersion: true}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, err
	}
	return gormMigrator{repo: btnperm.NewGormRepo(gdb.WithContext(ctx))}, nil
}

const permUsage = `用法：dbt-admin perm <子命令> [选项]

子命令：
  migrate   把权限组 / 用户单独持有的原权限码一次性展开成按钮授权，并标记为已迁移（默认只演练，列出每个组和用户的变化）

migrate 选项：
  --apply                真正写入（默认只演练）；并加 --yes 或在终端里确认
  --yes                  不再交互确认（脚本用）
  --rollback-file 路径    写入前把“逐条撤销这次迁移”的 SQL 存到这个文件（默认：当前目录下 perm-migrate-rollback-<时间>.sql）

迁移只增不减，已有的权限组按钮、单用户授权 / 禁止一律不动；可重复执行，第二次没有任何新增。服务最多 15 秒内生效，不需要重启。
`

func (e *Env) cmdPerm(ctx context.Context, args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(e.Stdout, permUsage)
		if len(args) == 0 {
			return ExitUsage
		}
		return ExitOK
	}
	if args[0] != "migrate" {
		e.err("错误：未知的子命令 %q\n\n%s", args[0], permUsage)
		return ExitUsage
	}
	var o struct {
		apply, yes bool
		rollback   string
	}
	_, done, code := e.parseFlags("perm migrate", "把原权限码一次性迁移成按钮授权。", args[1:], func(fs *flag.FlagSet) {
		fs.BoolVar(&o.apply, "apply", false, "真正写入（默认只演练）")
		fs.BoolVar(&o.yes, "yes", false, "不再交互确认（脚本用）")
		fs.StringVar(&o.rollback, "rollback-file", "", "写入前保存逐条撤销的 SQL 的文件路径")
	})
	if done {
		return code
	}
	ctx, cancel := context.WithTimeout(ctx, permTimeout)
	defer cancel()
	st, tgt, c := e.connect(ctx)
	if c != ExitOK {
		return c
	}
	defer st.Close()
	mg, err := st.PermMigrator(ctx)
	if err != nil {
		e.err("错误：%s\n", RedactMsg(err.Error()))
		return ExitFailed
	}
	plan, err := mg.Plan()
	if err != nil {
		e.err("错误：%s\n", RedactMsg(err.Error()))
		return ExitFailed
	}
	e.out("权限迁移（%s）\n目标库：%s @ %s\n", map[bool]string{true: "写入模式", false: "默认只演练"}[o.apply], tgt.DB, tgt.HostPort())
	printPermPlan(e, plan)
	if plan.Empty() {
		e.out("没有需要写入的内容：已经迁移过了。\n")
		return ExitOK
	}
	if !o.apply {
		e.out("这是演练：没有写任何东西。确认无误后加 --apply（并加 --yes 或在终端里确认）执行。\n")
		return ExitOK
	}
	ok, cerr := e.confirm(o.yes, fmt.Sprintf("即将在 %s 上把原权限码迁移成按钮授权（一次性迁移，之后权限只认按钮）。", tgt))
	if cerr != nil {
		e.err("错误：%v\n", cerr)
		return ExitUsage
	}
	if !ok {
		e.err("已取消，没有写入任何东西\n")
		return ExitFailed
	}
	now := time.Now()
	by := e.actor()
	path := o.rollback
	if path == "" {
		path = "perm-migrate-rollback-" + now.Format("20060102-150405") + ".sql"
	}
	if err := writeRollbackFile(path, tgt, plan, by, now); err != nil {
		e.err("错误：保存撤销脚本失败，没有写入任何东西：%v\n", err)
		return ExitFailed
	}
	e.out("已保存撤销脚本：%s\n", path)
	res, err := mg.Apply(plan, by, now)
	if err != nil {
		e.err("错误：写入失败，没有任何改动：%s\n", RedactMsg(err.Error()))
		return ExitFailed
	}
	e.out("已迁移：权限组新增按钮 %d 条，单用户新增允许 %d 条，标记为已迁移：%v。服务最多 15 秒内生效，不需要重启。\n", res.GroupAdds, res.UserAdds, res.Strict)
	e.out("需要撤销时执行 %s（只删这次新增的行）。\n", path)
	return ExitOK
}

func writeRollbackFile(path string, tgt Target, plan *btnperm.MigratePlan, by string, now time.Time) error {
	var b strings.Builder
	fmt.Fprintf(&b, "-- dbt-admin perm migrate 的撤销脚本（%s，目标库 %s @ %s）\n", now.Format("2006-01-02 15:04:05"), tgt.DB, tgt.HostPort())
	b.WriteString("-- 只删这次迁移新增的行；迁移前就有的权限组按钮、单用户授权 / 禁止不在其中。撤销后迁移状态回到“未迁移”。\n")
	for _, s := range plan.RollbackSQL(by) {
		b.WriteString(s + "\n")
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

func printPermPlan(e *Env, plan *btnperm.MigratePlan) {
	state := map[string]string{btnperm.ModeStrict: "已迁移", btnperm.ModeCompat: "未迁移"}[plan.Mode]
	e.out("当前迁移状态：%s%s\n", state, map[bool]string{true: "（执行后标记为已迁移）", false: ""}[plan.SetStrict])
	e.out("基础按钮（所有登录用户固定开放，不写库）：%s\n\n", strings.Join(plan.Baseline, "、"))
	e.out("权限组：\n")
	for _, g := range plan.Groups {
		e.out("  %-10s 成员 %d 人；原权限码 %d 个 → 新增按钮 %d 个（本来就有 %d 个）；丢弃没有对应按钮的原权限码 %d 个\n", g.Group.Name, g.Group.Members, len(g.Legacy), len(g.Add), len(g.Already), len(g.Unmapped))
		if len(g.Unmapped) > 0 {
			e.out("             丢弃：%s\n", strings.Join(g.Unmapped, "、"))
		}
	}
	if len(plan.Users) > 0 {
		e.out("\n用户单独授予的原权限码：\n")
		for _, u := range plan.Users {
			e.out("  %-12s 原权限码 %s → 新增单用户“允许”%d 个（%s）；所在权限组已覆盖 %d 个；已有单用户设置 %d 个；丢弃 %d 个\n", u.User.Username, strings.Join(u.Legacy, "、"), len(u.Add), strings.Join(u.Add, "、"), len(u.Covered), len(u.Existing), len(u.Unmapped))
		}
	}
	ga, ua := plan.Counts()
	e.out("\n合计：权限组新增按钮 %d 条，单用户新增允许 %d 条。\n", ga, ua)
}
