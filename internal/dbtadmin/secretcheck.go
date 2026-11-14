// @author: kevinkelly
// @license: Apache Licence
// @file: secretcheck.go
// @time: 2026/10/28

package dbtadmin

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"dbtool/internal/config"
	"dbtool/internal/crypto"
)

// check-secrets：只读自检。判断库里“看起来是密文”的值是用哪把密钥加密的——
// 典型场景：库里的实例账号 / 密码是用另一把密钥（如公开缺省 SECRET_KEY）加密的，
// 而新系统部署时生成了另一把密钥，于是服务读出来的是乱码（用户登录不受影响：密码哈希是 PBKDF2，不用这把密钥）。
//
// 安全：只输出分类计数以及配置项名 / 实例名（不是秘密），不输出任何密钥、密文或明文值。

// 自检的列。
const (
	ColSettingValue     = "dbt_setting.svalue"
	ColInstanceUser     = "dbt_sql_instance.user"
	ColInstancePassword = "dbt_sql_instance.password"
)

// SecretRow 是自检读到的一个值。Ref 是配置项名或实例名；Value 是原值（密文或明文），只能分类，不得输出。
type SecretRow struct {
	Column string
	Ref    string
	Value  string
}

// secretKeyCand 是一把候选密钥。
type secretKeyCand struct {
	label string
	m     *crypto.Mirage
}

// opens 判断用这把密钥解 v 是否得到“像样的”文本：mirage 在密钥错误时有约 0.4% 的概率碰巧通过填充校验，
// 所以除了解得开，还要求结果是合法 UTF-8 且没有控制字符。
func (c secretKeyCand) opens(v string) bool {
	out := c.m.Decrypt(v)
	if out == v { // Decrypt 解不开时原样返回
		return false
	}
	if !utf8.ValidString(out) {
		return false
	}
	for _, r := range out {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// secretCandidates 按与 dbtool 相同的规则得出当前生效的密钥，再加上可能的旧密钥：
//  1. 当前生效：MIRAGE_SECRET_KEY，没设用 SECRET_KEY，都没设用缺省值（服务也是这样取）；
//  2. SECRET_KEY（与 1 不同时）：之前的部署可能没单独配 mirage，而是直接用 SECRET_KEY；
//  3. 公开缺省值：之前的部署什么都没配时用的就是它。
//
// 模式与 IV 沿用 MIRAGE_CIPHER_MODE / MIRAGE_CIPHER_IV（缺省 ECB / 缺省 IV）。
func (e *Env) secretCandidates() (cands []secretKeyCand, source string, publicSecret bool) {
	def := config.DefaultSecretKey()
	secret := e.Getenv("SECRET_KEY")
	secretSet := secret != ""
	if !secretSet {
		secret = def
	}
	mirage := e.Getenv("MIRAGE_SECRET_KEY")
	switch {
	case mirage != "":
		source = "MIRAGE_SECRET_KEY"
	case secretSet:
		source, mirage = "SECRET_KEY（未单独设置 MIRAGE_SECRET_KEY）", secret
	default:
		source, mirage = "未设置任何密钥，服务会使用公开的缺省值", secret
	}
	mode, iv := e.Getenv("MIRAGE_CIPHER_MODE"), e.Getenv("MIRAGE_CIPHER_IV")
	if iv == "" {
		iv = "1234567890abcdef"
	}
	seen := map[string]bool{}
	add := func(label, key string) {
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		if m, err := crypto.NewMirage(key, mode, iv); err == nil {
			cands = append(cands, secretKeyCand{label: label, m: m})
		}
	}
	add("当前生效的密钥", mirage)
	secretLabel := "SECRET_KEY"
	if secret == def {
		secretLabel = "SECRET_KEY（与公开缺省值相同）"
	}
	add(secretLabel, secret)
	add("公开缺省密钥", def)
	return cands, source, secret == def
}

type secretStat struct {
	total, plain, current, unknown int
	other                          map[string]int // 候选标签 → 个数（只有那把候选密钥解得开）
}

// RunCheckSecrets 执行自检并打印结果。有任何值当前密钥解不开时返回 ExitFailed，方便脚本判断。
func (e *Env) RunCheckSecrets(ctx context.Context, st Store, tgt Target) int {
	rows, err := st.SecretRows(ctx)
	if err != nil {
		e.err("错误：读取库失败：%v\n", err)
		return ExitFailed
	}
	cands, source, publicSecret := e.secretCandidates()
	if len(cands) == 0 {
		e.err("错误：没有可用的密钥（长度需不少于 32 个字符）。请检查环境变量 SECRET_KEY / MIRAGE_SECRET_KEY / MIRAGE_CIPHER_MODE / MIRAGE_CIPHER_IV\n")
		return ExitUsage
	}

	order := []string{ColSettingValue, ColInstanceUser, ColInstancePassword}
	stats := map[string]*secretStat{}
	for _, c := range order {
		stats[c] = &secretStat{other: map[string]int{}}
	}
	// 需要关注的项（只记名字）：label → 名字集合；"" 表示谁都解不开
	needs := map[string]map[string]bool{}
	note := func(label, ref string) {
		if needs[label] == nil {
			needs[label] = map[string]bool{}
		}
		needs[label][ref] = true
	}
	for _, r := range rows {
		s := stats[r.Column]
		if s == nil || r.Value == "" {
			continue
		}
		s.total++
		if !crypto.LooksEncrypted(r.Value) {
			s.plain++
			continue
		}
		hit := -1
		for i, c := range cands {
			if c.opens(r.Value) {
				hit = i
				break
			}
		}
		switch {
		case hit == 0:
			s.current++
		case hit > 0:
			s.other[cands[hit].label]++
			note(cands[hit].label, r.Ref)
		default:
			s.unknown++
			note("", r.Ref)
		}
	}

	e.out("密钥自检（只读；不输出任何密钥、密文或明文值）\n")
	e.out("目标库：%s @ %s\n", tgt.DB, tgt.HostPort())
	e.out("当前生效的加密密钥来自：%s\n", source)
	if publicSecret {
		e.out("⚠ SECRET_KEY 是公开的缺省值：任何知道它的人都能伪造会话 Cookie / JWT，以超级用户身份登录。上线前应换成随机值（dbt-admin gen-secrets 生成）；\n" +
			"  换 SECRET_KEY 会让所有已登录会话失效，并使 SSO / LDAP 登录方式的“已测试通过”标记失效，需要在“登录方式”页重新测试。这与数据能不能解开是两件事。\n")
	}
	e.out("\n")
	e.out("%-28s\t%s\t%s\t%s\t%s\t%s\n", "检查对象", "合计", "明文", "当前密钥可解", "仅旧密钥可解", "都解不开")
	var bad int
	for _, c := range order {
		s := stats[c]
		oth := 0
		for _, n := range s.other {
			oth += n
		}
		bad += oth + s.unknown
		e.out("%-28s\t%d\t%d\t%d\t%d\t%d\n", c, s.total, s.plain, s.current, oth, s.unknown)
	}
	e.out("\n")
	if bad == 0 {
		e.out("结论：库里看起来是密文的值，当前密钥都能解开（或本来就是明文），密钥与数据一致。\n")
		return ExitOK
	}

	labels := make([]string, 0, len(needs))
	for l := range needs {
		if l != "" {
			labels = append(labels, l)
		}
	}
	sort.Strings(labels)
	for _, l := range labels {
		names := sortedKeys(needs[l])
		e.out("只有「%s」解得开（%d 项）：%s\n", l, len(names), joinLimit(names, 20))
	}
	if names := sortedKeys(needs[""]); len(names) > 0 {
		e.out("现有候选密钥都解不开（%d 项）：%s\n", len(names), joinLimit(names, 20))
	}
	e.out("\n结论：当前密钥与这些值当初的加密密钥不一致，服务读出来是乱码（实例连不上、开关类配置项等于没开）。\n")
	if len(labels) > 0 {
		e.out("  · 用户登录不受影响：密码哈希是 PBKDF2，不依赖这把密钥。\n")
		e.out("  · 修法：只在 dbtool.env 里把 MIRAGE_SECRET_KEY 设成「%s」对应的值，SECRET_KEY 保持不变（它还签发会话 / JWT，不能是公开值），然后重启 dbtool。\n", strings.Join(labels, " / "))
		if def := labelDefault(labels); def {
			e.out("    「公开缺省密钥」的值见 internal/config/config.go 的 defaultSecretKey；不要把它贴进聊天、工单或 Git。\n")
		}
		e.out("  · 或者保持当前密钥不变，用 dbt-admin reencrypt 把这些值原地换成当前密钥（先演练看统计；写入前备份；可重复运行）；解不开的值原样保留，" +
			"超级用户也可以在“平台设置 › 系统配置”页或实例管理页重新保存。\n")
		cur := 0
		for _, c := range order {
			cur += stats[c].current
		}
		if cur > 0 {
			e.out("  · 注意：已有 %d 项是用当前密钥保存的（混合状态）。把 MIRAGE_SECRET_KEY 换成旧密钥后，这些新保存的值会变成解不开——先记下它们（重新录入），或改用 dbt-admin reencrypt 把旧值换成当前密钥（保持当前密钥不变，不会让这些新保存的值失效）。\n", cur)
		}
	}
	if len(needs[""]) > 0 {
		e.out("  · 都解不开的项：之前的部署可能单独配置过 SECRET_KEY / MIRAGE_SECRET_KEY，或改过 MIRAGE_CIPHER_MODE / MIRAGE_CIPHER_IV。请用之前部署时 dbtool.env 里的值（写入 dbtool.env 的 SECRET_KEY / MIRAGE_*）再试。\n")
	}
	return ExitFailed
}

func labelDefault(labels []string) bool {
	for _, l := range labels {
		if strings.Contains(l, "缺省") {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func joinLimit(names []string, n int) string {
	if len(names) <= n {
		return strings.Join(names, "、")
	}
	return fmt.Sprintf("%s 等共 %d 项", strings.Join(names[:n], "、"), len(names))
}
