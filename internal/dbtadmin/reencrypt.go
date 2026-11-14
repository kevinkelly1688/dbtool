// @author: kevinkelly
// @license: Apache Licence
// @file: reencrypt.go
// @time: 2026/10/28

package dbtadmin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/go-sql-driver/mysql"

	"dbtool/internal/config"
	"dbtool/internal/crypto"
)

// reencrypt：在目标库里原地把含加密列的表（实例 / 隧道 / 实例账号 / 系统设置 / 2FA）从旧密钥换成 dbtool 当前生效的密钥（可重复运行）。
//
// 用在“已经原样复制过来、读出来是乱码”的库上（转换规则见 crypto.Rekeyer）：
//   - 旧密钥解得开 → 解出明文，用当前密钥重新加密；明文 → 用当前密钥加密；
//   - 已经是当前密钥的密文 → 不动（所以重复运行是安全的，混合状态也一样）；
//   - 谁都解不开 → 原样保留，只列名字，由管理员在页面里重新保存；
//   - 加密后超出列宽的明文保留明文。
//
// 安全：默认只演练；写入需要 --apply 且 --yes（或终端确认）；所有改动在一个事务里，条件更新（值被别人改过则不动）；
// 输出、日志与审计里只有数量与名字（实例名 / 隧道名 / 配置项名），没有任何密钥、密文或明文值。

// ReencryptOptions 是 reencrypt 的选项。
type ReencryptOptions struct {
	Apply bool
	Yes   bool
}

type rekeyCounts struct {
	reencrypted, plain, alreadyNew, ambiguous, undecryptable, tooLong int
	undecryptableRefs, ambiguousRefs                                  map[string]bool
}

type cellUpdate struct {
	cell     Cell
	newValue string
}

type rekeyPlan struct {
	updates []cellUpdate
	counts  map[string]*rekeyCounts // "表.列" → 统计
	order   []string
}

func (p *rekeyPlan) total() (re, pl, bad int) {
	for _, c := range p.counts {
		re += c.reencrypted
		pl += c.plain
		bad += c.undecryptable + c.ambiguous
	}
	return
}

// planRekey 逐个值判断怎么处理（不写库）。
func planRekey(cells []Cell, rk *crypto.Rekeyer) *rekeyPlan {
	p := &rekeyPlan{counts: map[string]*rekeyCounts{}}
	for _, c := range cells {
		key := c.Table + "." + c.Column
		cnt := p.counts[key]
		if cnt == nil {
			cnt = &rekeyCounts{undecryptableRefs: map[string]bool{}, ambiguousRefs: map[string]bool{}}
			p.counts[key] = cnt
			p.order = append(p.order, key)
		}
		nv, kind := rk.Convert(c.Value)
		switch kind {
		case crypto.RekeyReencrypted, crypto.RekeyPlain:
			if c.MaxLen > 0 && len(nv) > c.MaxLen {
				cnt.tooLong++
				continue
			}
			if kind == crypto.RekeyReencrypted {
				cnt.reencrypted++
			} else {
				cnt.plain++
			}
			if nv != c.Value {
				p.updates = append(p.updates, cellUpdate{cell: c, newValue: nv})
			}
		case crypto.RekeyAlreadyNew:
			cnt.alreadyNew++
		case crypto.RekeyAmbiguous:
			cnt.ambiguous++
			cnt.ambiguousRefs[c.Ref] = true
		case crypto.RekeyUndecryptable:
			cnt.undecryptable++
			cnt.undecryptableRefs[c.Ref] = true
		}
	}
	return p
}

// rekeyer 构造换密钥器：新密钥 = dbtool 当前生效的密钥（取法与服务一致）；旧密钥依次尝试：
//  1. OLD_MIRAGE_SECRET_KEY（缺省 OLD_SECRET_KEY）、OLD_MIRAGE_CIPHER_MODE（缺省 ECB）、OLD_MIRAGE_CIPHER_IV：显式给出的旧密钥
//     （之前的部署单独配置过密钥时用）；
//  2. SECRET_KEY（与当前生效的 MIRAGE_SECRET_KEY 不同时）；
//  3. 公开缺省密钥（之前的部署什么都没配时用的就是它）。
func (e *Env) rekeyer() (rk *crypto.Rekeyer, tried []string, err error) {
	cands, _, _ := e.secretCandidates()
	// 第一个候选必须是“当前生效的密钥”：它无效（太短等）时 secretCandidates 会跳过它，剩下的第一个是别的密钥，绝不能拿来当新密钥写库
	if len(cands) == 0 || cands[0].label != "当前生效的密钥" {
		return nil, nil, usagef("当前生效的密钥无效（长度需不少于 32 个字符）。请检查环境变量 SECRET_KEY / MIRAGE_SECRET_KEY / MIRAGE_CIPHER_MODE / MIRAGE_CIPHER_IV（与 dbtool.env 一致）")
	}
	var olds []*crypto.Mirage
	if k := firstNonEmpty(e.Getenv("OLD_MIRAGE_SECRET_KEY"), e.Getenv("OLD_SECRET_KEY")); k != "" {
		mode := e.Getenv("OLD_MIRAGE_CIPHER_MODE")
		if mode == "" {
			mode = "ECB"
		}
		iv := e.Getenv("OLD_MIRAGE_CIPHER_IV")
		if iv == "" {
			iv = "1234567890abcdef"
		}
		m, merr := crypto.NewMirage(k, mode, iv)
		if merr != nil {
			return nil, nil, usagef("OLD_ 旧密钥配置无效：%v", merr)
		}
		olds = append(olds, m)
		tried = append(tried, "OLD_* 显式给出的旧密钥")
	}
	for _, c := range cands[1:] {
		olds = append(olds, c.m)
		tried = append(tried, c.label)
	}
	return crypto.NewRekeyer(cands[0].m, olds...), tried, nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// RunReencrypt 执行 reencrypt 并打印结果。
func (e *Env) RunReencrypt(ctx context.Context, st Store, tgt Target, o ReencryptOptions) int {
	rk, tried, kerr := e.rekeyer()
	if kerr != nil {
		return e.exitOf(kerr)
	}
	cells, err := st.EncryptedCells(ctx)
	if err != nil {
		e.err("错误：读取库失败：%s\n", RedactMsg(err.Error()))
		return ExitFailed
	}
	plan := planRekey(cells, rk)

	e.out("密文重加密（%s；不输出任何密钥、密文或明文值）\n", map[bool]string{true: "写入模式", false: "默认只演练"}[o.Apply])
	e.out("目标库：%s @ %s\n", tgt.DB, tgt.HostPort())
	publicEffective := e.effectiveKeyIsPublic()
	if publicEffective {
		e.out("⚠ 当前生效的密钥是公开的缺省值：用它加密等于没加密。先用 dbt-admin gen-secrets 生成并配置自己的 MIRAGE_SECRET_KEY（SECRET_KEY 也应换成随机值），再运行本命令。\n")
	}
	if rk.SameKey() {
		e.out("提示：没有找到与当前生效密钥不同的旧密钥可以尝试：只会把明文值按当前密钥加密，不会改动已有密文。\n" +
			"      之前用另一把密钥加密过时，用环境变量 OLD_MIRAGE_SECRET_KEY（以及 OLD_MIRAGE_CIPHER_MODE / OLD_MIRAGE_CIPHER_IV）给出。\n")
	} else {
		e.out("会尝试的旧密钥：%s\n", strings.Join(tried, "、"))
	}
	e.out("\n")
	if len(plan.order) == 0 {
		e.out("含加密列的表里没有任何需要处理的值（表不存在或都是空值）。\n")
		return ExitOK
	}
	e.out("%-34s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", "表.列", "将重加密", "将加密明文", "已是当前密钥", "解不开(原样保留)", "不明确", "超列宽保留明文", "")
	for _, k := range plan.order {
		c := plan.counts[k]
		e.out("%-34s\t%d\t%d\t%d\t%d\t%d\t%d\n", k, c.reencrypted, c.plain, c.alreadyNew, c.undecryptable, c.ambiguous, c.tooLong)
	}
	e.out("\n")
	for _, k := range plan.order {
		c := plan.counts[k]
		if names := sortedKeys(c.undecryptableRefs); len(names) > 0 {
			e.out("解不开、将原样保留（%s，%d 项）：%s\n", k, len(names), joinLimit(names, 20))
		}
		if names := sortedKeys(c.ambiguousRefs); len(names) > 0 {
			e.out("新旧两把密钥都解得开、不处理（%s，%d 项）：%s\n", k, len(names), joinLimit(names, 20))
		}
	}
	re, pl, bad := plan.total()
	if bad > 0 {
		e.out("解不开的值不会被改动；管理员在对应页面（系统配置 / 实例管理 / SSH 隧道 …）重新保存即可按当前密钥加密。\n")
	}
	if len(plan.updates) == 0 {
		e.out("\n结论：没有需要改的值。\n")
		return ExitOK
	}
	e.out("\n将改写 %d 个值（重加密 %d、明文加密 %d）。\n", len(plan.updates), re, pl)
	if !o.Apply {
		e.out("这是演练：没有写任何东西。确认无误后加 --apply（并加 --yes 或在终端里确认）执行。\n")
		e.out("写入前请先备份这些表（mysqldump 的密码用 -p 交互输入，不要写在命令行）：\n  mysqldump -h %s -P %s -u %s -p %s %s > secrets-backup.sql\n",
			tgt.Host, tgt.Port, tgt.User, tgt.DB, strings.Join(rekeyTableNames(), " "))
		return ExitOK
	}

	if publicEffective {
		e.err("错误：当前生效的密钥是公开的缺省值，拒绝把数据加密到这把密钥上；请先配置自己的密钥（见上面的提示）\n")
		return ExitUsage
	}
	ok, cerr := e.confirm(o.Yes, fmt.Sprintf("即将在 %s 上改写 %d 个加密值（只改加密列；建议已备份 %s）。", tgt, len(plan.updates), strings.Join(rekeyTableNames(), "、")))
	if cerr != nil {
		e.err("错误：%v\n", cerr)
		return ExitUsage
	}
	if !ok {
		e.err("已取消，没有写入任何东西\n")
		return ExitFailed
	}
	done, skipped := 0, 0
	txErr := st.InTx(ctx, func(tx Tx) error {
		done, skipped = 0, 0
		for _, u := range plan.updates {
			changed, err := tx.ReplaceCell(u.cell.Table, u.cell.Column, u.cell.ID, u.cell.Value, u.newValue)
			if err != nil {
				return err
			}
			if changed {
				done++
			} else {
				skipped++
			}
		}
		aerr := tx.InsertAudit(AuditEntry{
			UserName: e.actor(), UserDisplay: e.actor(), Action: "密文重加密",
			ExtraInfo: fmt.Sprintf("dbt-admin reencrypt：改写 %d 个加密值（重加密 %d、明文加密 %d），解不开原样保留 %d；未记录任何密钥、密文或明文值", done, re, pl, bad),
			At:        e.Now(),
		})
		if aerr != nil && !errors.Is(aerr, ErrTableMissing) {
			return aerr
		}
		return nil
	})
	if txErr != nil {
		var me *mysql.MySQLError
		if errors.As(txErr, &me) {
			e.err("错误：写入失败，已整体回滚，没有任何改动（MySQL 错误号 %d；常见原因：账号没有 UPDATE 权限，或换密钥后两行的唯一键值相同）\n", me.Number)
		} else {
			e.err("错误：写入失败，已整体回滚，没有任何改动：%s\n", RedactMsg(txErr.Error()))
		}
		return ExitFailed
	}
	e.out("\n已改写 %d 个值", done)
	if skipped > 0 {
		e.out("；另有 %d 个在读取之后被别人改过，未动（再运行一次即可）", skipped)
	}
	e.out("。\n")

	// 复查：再读一遍，看还有多少“旧密钥解得开”的值（应为 0）
	if after, err := st.EncryptedCells(ctx); err == nil {
		p2 := planRekey(after, rk)
		re2, pl2, bad2 := p2.total()
		e.out("复查：仍可重加密 %d、仍是明文 %d、解不开 %d。\n", re2, pl2, bad2)
		if re2+pl2 > 0 {
			e.out("还有值没有换完（可能是超列宽，或读取后又被改过）：可再运行一次；超列宽的保留明文，不影响使用。\n")
		}
	}
	e.out("建议再运行 dbt-admin check-secrets 核对；dbtool 服务不需要重启（配置与实例凭据每次使用时读库）。\n")
	return ExitOK
}

func rekeyTableNames() []string {
	out := make([]string, 0, len(RekeyColumns))
	for _, s := range RekeyColumns {
		out = append(out, s.Table)
	}
	sort.Strings(out)
	return out
}

// effectiveKeyIsPublic 判断 dbtool 当前生效的 mirage 密钥是否就是公开缺省值（取法与服务一致）。
func (e *Env) effectiveKeyIsPublic() bool {
	k := firstNonEmpty(e.Getenv("MIRAGE_SECRET_KEY"), e.Getenv("SECRET_KEY"))
	return k == "" || k == config.DefaultSecretKey()
}
