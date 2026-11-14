// @author: kevinkelly
// @license: Apache Licence
// @file: settingscmd.go
// @time: 2026/10/28

package dbtadmin

import (
	"context"
	"dbtool/internal/model"
	"dbtool/internal/settings"
	"errors"
	"flag"
	"fmt"
	"strings"
)

// settings 子命令：在命令行读写系统设置（dbt_setting），包括页面里只读的项（程序路径类，需要 --force）。
//
//	dbt-admin settings list   [--group G] [--modified]
//	dbt-admin settings get    --key K
//	dbt-admin settings set    --key K [--value V | 环境变量 DBT_SETTING_VALUE] [--force] [--apply] [--yes]
//	dbt-admin settings reset  --key K [--force] [--apply] [--yes]
//
// 默认只演练（set / reset）；写入需要 --apply，并加 --yes 或在终端里确认。敏感项的值只能从环境变量
// DBT_SETTING_VALUE 取，不接受命令行参数；任何输出和审计里都不出现敏感值。

// SettingValueEnv 是提供敏感设置值的环境变量。
const SettingValueEnv = "DBT_SETTING_VALUE"

const settingsUsage = `用法：dbt-admin settings <子命令> [选项]

子命令：
  list    列出全部设置（键、当前值、默认值、说明）；敏感项只显示“已设置”
  get     显示一项设置（--key）
  set     修改一项（--key，值用 --value；敏感项的值用环境变量 ` + SettingValueEnv + `）
  reset   恢复一项的默认值（--key；敏感项 = 清除）

选项：--force 允许修改页面里只读的项（程序路径类）与内部状态项；--apply 才真正写入（默认只演练），并加 --yes 或在终端里确认。
`

func (e *Env) cmdSettings(ctx context.Context, args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(e.Stdout, settingsUsage)
		if len(args) == 0 {
			return ExitUsage
		}
		return ExitOK
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list", "get", "set", "reset":
	default:
		e.err("错误：未知的子命令 %q\n\n%s", sub, settingsUsage)
		return ExitUsage
	}

	var o struct {
		group, key, value           string
		modified, force, apply, yes bool
	}
	_, done, code := e.parseFlags("settings "+sub, "读写系统设置（dbt_setting）。", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&o.group, "group", "", "list：只列这个分组（security / sms / query / firewall / review / notify / goinception / export / partition / retention / perf / inspect / optimizer）")
		fs.BoolVar(&o.modified, "modified", false, "list：只列与默认值不同的项")
		fs.StringVar(&o.key, "key", "", "get / set / reset：设置键，如 query.max_execution_seconds")
		fs.StringVar(&o.value, "value", "", "set：新值（敏感项不接受，用环境变量 "+SettingValueEnv+"）")
		fs.BoolVar(&o.force, "force", false, "允许修改页面里只读的项与内部状态项")
		fs.BoolVar(&o.apply, "apply", false, "真正写入（默认只演练）")
		fs.BoolVar(&o.yes, "yes", false, "不再交互确认（脚本用）")
	})
	if done {
		return code
	}

	st, tgt, c := e.connect(ctx)
	if c != ExitOK {
		return c
	}
	defer st.Close()

	svc := settings.New(cliSettingsStore{ctx: ctx, st: st, e: e}, e.settingsCipher())
	if !svc.Ready() {
		e.err("错误：库里没有 dbt_setting 表。请先执行 deploy/sql/260_setting.sql（dbt-admin bootstrap --only-sql --apply）\n")
		return ExitFailed
	}

	switch sub {
	case "list":
		return e.settingsList(svc, o.group, o.modified)
	case "get":
		return e.settingsGet(svc, o.key)
	}

	// set / reset
	key := strings.TrimSpace(o.key)
	def := settings.Find(key)
	if def == nil {
		e.err("错误：没有这个设置项 %q（dbt-admin settings list 查看全部）\n", key)
		return ExitUsage
	}
	value := ""
	if sub == "set" {
		fromEnv := e.Getenv(SettingValueEnv)
		switch {
		case def.Secret():
			if o.value != "" {
				e.err("错误：%s 是敏感项，不接受 --value（会留在命令历史里）；请用环境变量 %s 提供\n", key, SettingValueEnv)
				return ExitUsage
			}
			if fromEnv == "" {
				e.err("错误：缺少新值：请设置环境变量 %s\n", SettingValueEnv)
				return ExitUsage
			}
			value = fromEnv
			if e.effectiveKeyIsPublic() {
				e.err("错误：当前生效的密钥是公开的缺省值，拒绝把敏感值加密到这把密钥上；请先配置自己的 MIRAGE_SECRET_KEY（dbt-admin gen-secrets）\n")
				return ExitUsage
			}
			if e.settingsCipher() == nil {
				e.err("错误：没有可用的加密密钥（MIRAGE_SECRET_KEY / SECRET_KEY 无效）\n")
				return ExitUsage
			}
		case o.value != "":
			value = o.value
		case fromEnv != "":
			value = fromEnv
		default:
			e.err("错误：缺少新值：--value 或环境变量 %s\n", SettingValueEnv)
			return ExitUsage
		}
		if _, err := def.Normalize(value); err != nil {
			e.err("错误：%s：%v\n", key, err)
			return ExitUsage
		}
	}
	if def.Locked != "" && !o.force {
		e.err("错误：%s 在页面里是只读的（%s）；确认要改请加 --force\n", key, def.Locked)
		return ExitUsage
	}

	action := map[string]string{"set": "修改", "reset": "恢复默认"}[sub]
	e.out("设置%s（%s；不输出敏感值）\n目标库：%s @ %s\n", action, map[bool]string{true: "写入模式", false: "默认只演练"}[o.apply], tgt.DB, tgt.HostPort())
	cur := svc.String(key)
	if def.Secret() {
		cur = map[bool]string{true: "（已设置）", false: "（未设置）"}[svcHas(svc, key)]
	}
	newShow := value
	if def.Secret() {
		newShow = "（新值，不显示）"
	}
	if sub == "reset" {
		newShow = def.Default
		if def.Secret() {
			newShow = "（清除）"
		}
	}
	e.out("  %s（%s）\n  当前：%s\n  改为：%s\n", key, def.Label, orDash(cur), orDash(newShow))
	if def.Risk != "" {
		e.out("  ⚠ 高危项：%s\n", def.Risk)
	}
	if !o.apply {
		e.out("这是演练：没有写任何东西。确认无误后加 --apply（并加 --yes 或在终端里确认）执行。\n")
		return ExitOK
	}
	ok, cerr := e.confirm(o.yes, fmt.Sprintf("即将在 %s 上%s系统设置 %s。", tgt, action, key))
	if cerr != nil {
		e.err("错误：%v\n", cerr)
		return ExitUsage
	}
	if !ok {
		e.err("已取消，没有写入任何东西\n")
		return ExitFailed
	}
	actor := settings.Actor{Username: e.actor(), Display: e.actor()}
	ch, err := svc.Set(actor, key, value, settings.Options{Force: o.force, Source: "cli"})
	if err != nil {
		var se *settings.Error
		if errors.As(err, &se) {
			e.err("错误：%s\n", se.Message)
			return ExitUsage
		}
		e.err("错误：写入失败，没有任何改动：%s\n", RedactMsg(err.Error()))
		return ExitFailed
	}
	if ch == nil {
		e.out("没有变化：当前值已经是这个值。\n")
		return ExitOK
	}
	e.out("已%s。dbtool 服务最多 %d 秒内生效，不需要重启。\n", action, int(settings.DefaultTTL.Seconds()))
	return ExitOK
}

func svcHas(svc *settings.Service, key string) bool {
	_, ok := svc.Lookup(key)
	return ok
}

func (e *Env) settingsList(svc *settings.Service, group string, modifiedOnly bool) int {
	if group != "" {
		known := false
		for _, g := range settings.Groups {
			if g.Key == group {
				known = true
			}
		}
		if !known {
			e.err("错误：没有这个分组 %q\n", group)
			return ExitUsage
		}
	}
	items := svc.Items()
	last := ""
	n := 0
	for _, it := range items {
		if group != "" && it.Group != group {
			continue
		}
		if modifiedOnly && !it.Modified {
			continue
		}
		if it.Group != last {
			e.out("\n[%s] %s\n", it.Group, settings.GroupLabel(it.Group))
			last = it.Group
		}
		val := it.Effective
		mark := " "
		switch {
		case it.Secret():
			val = "（未设置）"
			if it.SecretSet {
				val, mark = "（已设置）", "*"
			}
			if it.Undecryptable {
				val, mark = "（密文解不开）", "!"
			}
		case it.Undecryptable:
			val, mark = "（密文解不开）", "!"
		case it.Modified:
			mark = "*"
		}
		lock := ""
		if it.Locked != "" {
			lock = " [只读]"
		}
		e.out("%s %-40s %s%s\n", mark, it.Key, orDash(val), lock)
		n++
	}
	e.out("\n共 %d 项（* 表示与默认值不同 / 敏感项已设置）\n", n)
	return ExitOK
}

func (e *Env) settingsGet(svc *settings.Service, key string) int {
	def := settings.Find(strings.TrimSpace(key))
	if def == nil {
		e.err("错误：没有这个设置项 %q（dbt-admin settings list 查看全部）\n", key)
		return ExitUsage
	}
	for _, it := range svc.Items() {
		if it.Key != def.Key {
			continue
		}
		e.out("键：%s\n名称：%s\n分组：%s › %s\n类型：%s\n", it.Key, it.Label, settings.GroupLabel(it.Group), it.Section, it.Type)
		switch {
		case it.Secret():
			e.out("当前值：%s\n", map[bool]string{true: "（已设置）", false: "（未设置）"}[it.SecretSet])
		default:
			e.out("当前值：%s\n默认值：%s\n", orDash(it.Effective), orDash(it.Default))
		}
		if it.Help != "" {
			e.out("说明：%s\n", it.Help)
		}
		if it.Locked != "" {
			e.out("只读：%s\n", it.Locked)
		}
		if it.Risk != "" {
			e.out("高危：%s\n", it.Risk)
		}
		if it.UpdatedAt != nil {
			e.out("最后修改：%s %s（第 %d 版）\n", it.UpdatedAt.Format("2006-01-02 15:04:05"), it.UpdatedBy, it.Version)
		}
		break
	}
	return ExitOK
}

// settingsCipher 返回当前生效的加解密器（取法与服务一致）；没有可用密钥返回 nil。
func (e *Env) settingsCipher() settings.Cipher {
	cands, _, _ := e.secretCandidates()
	if len(cands) == 0 || cands[0].m == nil {
		return nil
	}
	return cands[0].m
}

// cliSettingsStore 把 dbtadmin.Store 适配成 settings.Store。
type cliSettingsStore struct {
	ctx context.Context
	st  Store
	e   *Env
}

func (a cliSettingsStore) Load() (map[string]settings.Row, bool, error) {
	return a.st.SettingRows(a.ctx)
}

func (a cliSettingsStore) Apply(ws []settings.Write, hs []settings.History, au *model.AuditEntry) error {
	return a.st.InTx(a.ctx, func(tx Tx) error {
		if err := tx.ApplySettings(ws, hs); err != nil {
			return err
		}
		if au != nil {
			err := tx.InsertAudit(AuditEntry{UserName: a.e.actor(), UserDisplay: a.e.actor(), Action: "CLI-" + au.Action, ExtraInfo: au.ExtraInfo, At: a.e.Now()})
			if err != nil && !errors.Is(err, ErrTableMissing) {
				return err
			}
		}
		return nil
	})
}

func (a cliSettingsStore) Recent(string, int, int) ([]settings.History, error) { return nil, nil }
