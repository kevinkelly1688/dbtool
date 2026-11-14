// @author: kevinkelly
// @license: Apache Licence
// @file: data_test.go
// @time: 2026/10/28

package bootstrapsql

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"dbtool/internal/btnperm"
)

// ---- 002 的内存模型 ----

// 002 写入的五张基础表：只写源表名，新表名一律经 targetName 得到（不在测试里再写一遍 dbt_content_type 之类的字面量）。
// 注意内容类型表（dbt_content_type）里的“数据值”（app_label / model）不是表名，与这里无关。
var (
	tblContentType      = targetName("django_content_type")
	tblPermission       = targetName("auth_permission")
	tblGroup            = targetName("auth_group")
	tblGroupPermissions = targetName("auth_group_permissions")
	tblInstanceTag      = targetName("sql_instance_tag")
)

type ctRow struct{ App, Model string }

type permRow struct {
	ID       int
	Name     string
	CT       int
	Codename string
}

type dataset struct {
	CTs        map[int]ctRow
	Perms      map[int]permRow
	Groups     map[int]string // id -> name
	GroupPerms map[int][]int  // group id -> permission ids
	Tags       map[string]string
	TagIDs     map[int]string
}

func (d *dataset) appOf(permID int) string { return d.CTs[d.Perms[permID].CT].App }

// permKey 返回 "app:codename"。
func (d *dataset) permKey(permID int) string {
	return d.appOf(permID) + ":" + d.Perms[permID].Codename
}

// has 判断某权限码（含应用）是否存在于 auth_permission 数据里。
func (d *dataset) has(app, codename string) bool {
	for id := range d.Perms {
		if d.Perms[id].Codename == codename && d.appOf(id) == app {
			return true
		}
	}
	return false
}

func (d *dataset) groupID(name string) int {
	for id, n := range d.Groups {
		if n == name {
			return id
		}
	}
	return -1
}

// groupHolds 返回组持有的 "app:codename" 集合。
func (d *dataset) groupHolds(name string) map[string]bool {
	out := map[string]bool{}
	for _, pid := range d.GroupPerms[d.groupID(name)] {
		out[d.permKey(pid)] = true
	}
	return out
}

func loadDataset(t testing.TB) *dataset {
	t.Helper()
	all := loadInserts(t)
	d := &dataset{CTs: map[int]ctRow{}, Perms: map[int]permRow{}, Groups: map[int]string{}, GroupPerms: map[int][]int{},
		Tags: map[string]string{}, TagIDs: map[int]string{}}
	for _, ins := range insertsOf(all, tblContentType) {
		for _, r := range ins.Rows {
			id := ins.intCol(t, r, "id")
			if _, dup := d.CTs[id]; dup {
				t.Errorf("%s：id %d 重复", tblContentType, id)
			}
			d.CTs[id] = ctRow{App: ins.col(r, "app_label").S, Model: ins.col(r, "model").S}
		}
	}
	for _, ins := range insertsOf(all, tblPermission) {
		for _, r := range ins.Rows {
			id := ins.intCol(t, r, "id")
			if _, dup := d.Perms[id]; dup {
				t.Errorf("%s：id %d 重复", tblPermission, id)
			}
			d.Perms[id] = permRow{ID: id, Name: ins.col(r, "name").S, CT: ins.intCol(t, r, "content_type_id"), Codename: ins.col(r, "codename").S}
		}
	}
	for _, ins := range insertsOf(all, tblGroup) {
		for _, r := range ins.Rows {
			id := ins.intCol(t, r, "id")
			if _, dup := d.Groups[id]; dup {
				t.Errorf("%s：id %d 重复", tblGroup, id)
			}
			d.Groups[id] = ins.col(r, "name").S
		}
	}
	for _, ins := range insertsOf(all, tblGroupPermissions) {
		for _, r := range ins.Rows {
			g, p := ins.intCol(t, r, "group_id"), ins.intCol(t, r, "permission_id")
			d.GroupPerms[g] = append(d.GroupPerms[g], p)
		}
	}
	for _, ins := range insertsOf(all, tblInstanceTag) {
		for _, r := range ins.Rows {
			id := ins.intCol(t, r, "id")
			code := ins.col(r, "tag_code").S
			d.Tags[code] = ins.col(r, "tag_name").S
			d.TagIDs[id] = code
		}
	}
	return d
}

// ---- 002 的形态：幂等、只写基础表、不含敏感内容 ----

func TestDataInsertsAreIdempotentAndWhitelisted(t *testing.T) {
	allowed := map[string]bool{
		tblContentType: true, tblPermission: true, tblGroup: true,
		tblGroupPermissions: true, tblInstanceTag: true,
	}
	secretCol := regexp.MustCompile(`(?i)pass|secret|token|key|user$|host|email|phone`)
	n := 0
	for _, ins := range loadInserts(t) {
		n++
		if !allowed[ins.Table] {
			t.Errorf("第 %d 行：002 不应写表 %s（只允许 %v）", ins.Line, ins.Table, keys(allowed))
		}
		if !ins.Ignore && !strings.Contains(strings.ToUpper(ins.OnDup), "=") {
			t.Errorf("第 %d 行：%s 不是幂等形式（需要 INSERT IGNORE 或 ON DUPLICATE KEY UPDATE col = col）：%s", ins.Line, ins.Table, ins.RawPreview)
		}
		if ins.OnDup != "" {
			// 空操作：每个赋值都必须是 `col` = `col`
			for _, a := range strings.Split(ins.OnDup, ",") {
				kv := strings.SplitN(a, "=", 2)
				if len(kv) != 2 || strings.Trim(strings.TrimSpace(kv[0]), "`") != strings.Trim(strings.TrimSpace(kv[1]), "`") {
					t.Errorf("第 %d 行：ON DUPLICATE KEY UPDATE 必须是空操作（col = col），实际 %q", ins.Line, a)
				}
			}
		}
		for _, c := range ins.Cols {
			if secretCol.MatchString(c) && c != "permission_id" && c != "content_type_id" {
				t.Errorf("第 %d 行：%s 的列 %s 看起来是敏感列", ins.Line, ins.Table, c)
			}
		}
		if len(ins.Rows) == 0 {
			t.Errorf("第 %d 行：%s 没有任何数据行", ins.Line, ins.Table)
		}
	}
	if n < 5 {
		t.Errorf("INSERT 语句只有 %d 条", n)
	}
	// 不含用户 / 密码 / 密钥 / 实例连接信息的痕迹：写入的表已经限定在白名单里，这里再扫一遍取值
	for _, ins := range loadInserts(t) {
		for _, r := range ins.Rows {
			for _, v := range r {
				if v.IsStr && regexp.MustCompile(`(?i)pbkdf2_|^\$2[aby]\$|AKIA[0-9A-Z]{8,}|-----BEGIN`).MatchString(v.S) {
					t.Errorf("第 %d 行：%s 的取值像密码哈希 / 密钥：%q", ins.Line, ins.Table, v.S)
				}
			}
		}
	}
}

// 002 的每条 INSERT 的目标表都在 001 里，且列名都是该表的列（不是旧名、拼错的名字）。
func TestDataInsertsTargetTablesAndColumnsExistIn001(t *testing.T) {
	cols := map[string]map[string]bool{}
	colRe := regexp.MustCompile("(?m)^\\s+`([^`]+)` ")
	for _, d := range loadSchema(t) {
		cols[d.Name] = map[string]bool{}
		for _, m := range colRe.FindAllStringSubmatch(d.Body, -1) {
			cols[d.Name][m[1]] = true
		}
	}
	for _, ins := range loadInserts(t) {
		c, ok := cols[ins.Table]
		if !ok {
			t.Errorf("第 %d 行：INSERT 的目标表 %s 不在 001 里（旧名？）", ins.Line, ins.Table)
			continue
		}
		if !strings.HasPrefix(ins.Table, tablePrefix) {
			t.Errorf("第 %d 行：INSERT 的目标表 %s 没有 %s 前缀", ins.Line, ins.Table, tablePrefix)
		}
		for _, col := range ins.Cols {
			if !c[col] {
				t.Errorf("第 %d 行：%s 没有列 %s", ins.Line, ins.Table, col)
			}
		}
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- 内容类型与权限的完整性 ----

// 002 只放代码里有判断点的权限码（精简后不再带 Django 自带的 admin / auth / sessions / django_q 模型权限，
// 也不再带每个模型的 add / change / delete / view 四件套）。因此这里不再要求“每个内容类型恰好 4 个默认权限”，
// 而是要求：引用关系完整、不重复、每个内容类型都有权限在用；“每个权限码都有判断点”见 refs_test.go。
func TestContentTypesAndPermissionsAreConsistent(t *testing.T) {
	d := loadDataset(t)
	if len(d.CTs) == 0 || len(d.Perms) < 40 {
		t.Fatalf("数据量过少：内容类型 %d，权限 %d", len(d.CTs), len(d.Perms))
	}
	seen := map[string]int{}
	for id, c := range d.CTs {
		k := c.App + "." + c.Model
		if prev, ok := seen[k]; ok {
			t.Errorf("内容类型 %s 重复：id %d 与 %d", k, prev, id)
		}
		seen[k] = id
		if c.App == "" || c.Model == "" || c.Model != strings.ToLower(c.Model) {
			t.Errorf("内容类型 id=%d (%q,%q) 不合法", id, c.App, c.Model)
		}
	}
	pk := map[string]int{}
	perCT := map[int]int{} // 内容类型 -> 权限个数
	for id, p := range d.Perms {
		ct, ok := d.CTs[p.CT]
		if !ok {
			t.Errorf("权限 id=%d (%s) 引用了不存在的内容类型 %d", id, p.Codename, p.CT)
			continue
		}
		k := ct.App + "." + ct.Model + ":" + p.Codename
		if prev, ok := pk[k]; ok {
			t.Errorf("权限 %s 重复：id %d 与 %d", k, prev, id)
		}
		pk[k] = id
		perCT[p.CT]++
		if p.Name == "" || len(p.Name) > 255 || len(p.Codename) > 100 || p.Codename == "" {
			t.Errorf("权限 id=%d name/codename 为空或超长", id)
		}
		if ct.App != "sql" && ct.App != "sql_perf" {
			t.Errorf("权限 %s 在 %s 应用下：002 只放 sql / sql_perf 应用的业务权限", p.Codename, ct.App)
		}
	}
	for id, c := range d.CTs {
		if perCT[id] == 0 {
			t.Errorf("内容类型 %s.%s 下没有任何权限：用不到的内容类型不要放进 002", c.App, c.Model)
		}
	}
	t.Logf("内容类型 %d 个；权限 %d 个", len(d.CTs), len(d.Perms))
}

// ---- 内置权限组 ----

var builtinNameRe = regexp.MustCompile(`''([A-Za-z0-9_]+)''`)

func TestBuiltinGroupsMatchProject(t *testing.T) {
	d := loadDataset(t)
	var got []string
	for _, n := range d.Groups {
		got = append(got, n)
	}
	sort.Strings(got)

	// 150_builtin_groups.sql 里的内置组名单
	text := readFile(t, "deploy/sql/150_builtin_groups.sql")
	m := regexp.MustCompile(`name IN \(([^)]*)\)`).FindStringSubmatch(text)
	if m == nil {
		t.Fatal("150_builtin_groups.sql 里找不到 name IN (…)")
	}
	var want []string
	for _, x := range builtinNameRe.FindAllStringSubmatch(m[1], -1) {
		want = append(want, x[1])
	}
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("002 的权限组 %v 与 150_builtin_groups.sql 的内置组 %v 不一致", got, want)
	}

	// 与 btnperm 预设（去掉非内置的“审计”）一致
	var presets []string
	for _, p := range btnperm.PresetNames {
		if p != "审计" {
			presets = append(presets, p)
		}
	}
	sort.Strings(presets)
	if strings.Join(got, ",") != strings.Join(presets, ",") {
		t.Errorf("002 的权限组 %v 与 btnperm 预设 %v 不一致", got, presets)
	}
	// 内置组的 id 是固定值
	wantID := map[string]int{"Default": 1, "RD": 2, "DBA": 3, "TL": 4, "QA": 5}
	for id, n := range d.Groups {
		if wantID[n] != id {
			t.Errorf("权限组 %s 的 id=%d，期望 %d", n, id, wantID[n])
		}
	}
	// 不应存在 PM 组
	if d.groupID("PM") != -1 {
		t.Error("不应创建 PM 组：dbtool 里它叫 TL")
	}
}

func TestGroupPermissionsReferenceExistingRows(t *testing.T) {
	d := loadDataset(t)
	for g, pids := range d.GroupPerms {
		if _, ok := d.Groups[g]; !ok {
			t.Errorf("auth_group_permissions 引用了不存在的权限组 %d", g)
		}
		seen := map[int]bool{}
		for _, p := range pids {
			if _, ok := d.Perms[p]; !ok {
				t.Errorf("组 %s 引用了不存在的权限 ID %d", d.Groups[g], p)
			}
			if seen[p] {
				t.Errorf("组 %s 重复授予权限 %d", d.Groups[g], p)
			}
			seen[p] = true
			// 内置组只授 sql / sql_perf 应用的业务权限，不授内部应用（admin / auth / sessions / django_q）的权限
			if app := d.appOf(p); app != "sql" && app != "sql_perf" {
				t.Errorf("组 %s 被授予了 %s 应用的权限 %s", d.Groups[g], app, d.Perms[p].Codename)
			}
		}
		if len(pids) == 0 {
			t.Errorf("组 %s 没有任何授权", d.Groups[g])
		}
	}
	for id, n := range d.Groups {
		if len(d.GroupPerms[id]) == 0 {
			t.Errorf("组 %s 没有 auth_group_permissions", n)
		}
	}
}

// ---- 与 btnperm 登记表 / 预设的交叉检查 ----

// knownAbsentLegacy 是“登记表里出现、但 002 里没有的原权限码”的显式白名单（原因必须写明）。
// 目前为空：binlog_del 与 inspection_plan_manage 都已写进 002（见 docs/bootstrap-design.md）。
var knownAbsentLegacy = map[string]string{}

func TestEveryRegistryLegacyCodeExistsInPermissionData(t *testing.T) {
	d := loadDataset(t)
	var missing []string
	for _, code := range btnperm.LegacyCodes() {
		app := btnperm.LegacyApp(code)
		if d.has(app, code) {
			if why, ok := knownAbsentLegacy[code]; ok {
				t.Errorf("%s 已存在于 002，请从 knownAbsentLegacy 删掉（%s）", code, why)
			}
			continue
		}
		if _, ok := knownAbsentLegacy[code]; ok {
			continue
		}
		missing = append(missing, app+":"+code)
	}
	if len(missing) > 0 {
		t.Errorf("btnperm 登记表的 Legacy 权限码在 002 的 auth_permission 里找不到：%v", missing)
	}
	t.Logf("登记表共 %d 个 Legacy 权限码，全部在 002 里（白名单 %d 个）", len(btnperm.LegacyCodes()), len(knownAbsentLegacy))
	// 登记表之外：权限所在应用必须与 LegacyApp 一致（menu_perf / perf_manage 在 sql_perf，其余在 sql）
	for _, code := range []string{"menu_perf", "perf_manage"} {
		if !d.has("sql_perf", code) || d.has("sql", code) {
			t.Errorf("%s 应只在 sql_perf 应用下", code)
		}
	}
}

// effectiveButtons 计算种子数据经 perm migrate 迁移之后，某组成员（非超级用户、无单用户覆盖）能拿到的按钮：
// 固定开放的基础按钮（Open），加上 Legacy 里任一码被该组持有的按钮。
func effectiveButtons(held map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, c := range btnperm.GrantableCodes() {
		b, _ := btnperm.Lookup(c)
		if b.Open {
			out[c] = true
			continue
		}
		for _, l := range b.Legacy {
			if held[btnperm.LegacyApp(l)+":"+l] {
				out[c] = true
				break
			}
		}
	}
	return out
}

func TestGroupPermissionsCoverBtnpermPresets(t *testing.T) {
	d := loadDataset(t)
	for _, name := range []string{"DBA", "TL", "RD", "QA", "Default"} {
		held := d.groupHolds(name)
		preset := btnperm.Preset(name)
		if len(preset) == 0 {
			t.Fatalf("预设 %s 为空", name)
		}
		eff := effectiveButtons(held)
		inPreset := map[string]bool{}
		var missing, viaButtonGrantOnly []string
		for _, c := range preset {
			inPreset[c] = true
			if eff[c] {
				continue
			}
			b, _ := btnperm.Lookup(c)
			if len(b.Legacy) == 0 { // 没有原权限码：迁移给不了，只能经按钮授权获得
				viaButtonGrantOnly = append(viaButtonGrantOnly, c)
				continue
			}
			missing = append(missing, c+"（需要 "+strings.Join(b.Legacy, " 或 ")+"）")
		}
		if len(missing) > 0 {
			t.Errorf("组 %s：迁移后预设按钮拿不到 %d 个：%v", name, len(missing), missing)
		}
		// 页面访问码：预设里有该页面的任一按钮，组就必须持有页面的菜单码之一
		for _, c := range preset {
			p, _, ok := btnperm.PageOf(c)
			if !ok || len(p.Menu) == 0 {
				continue
			}
			okMenu := false
			for _, m := range p.Menu {
				if held[btnperm.LegacyApp(m)+":"+m] {
					okMenu = true
				}
			}
			if !okMenu {
				t.Errorf("组 %s：预设含页面 %s 的按钮 %s，但组没有该页面的任何菜单码 %v", name, p.Key, c, p.Menu)
			}
		}
		var extra []string
		for c := range eff {
			if !inPreset[c] {
				extra = append(extra, c)
			}
		}
		sort.Strings(extra)
		sort.Strings(viaButtonGrantOnly)
		t.Logf("组 %-7s 持有 %2d 个权限码；预设 %3d 个按钮：迁移后经种子权限可达 %d 个，仅能经按钮授权获得 %d 个 %v，缺口 %d 个；超出预设的按钮 %d 个",
			name, len(held), len(preset), len(preset)-len(viaButtonGrantOnly)-len(missing), len(viaButtonGrantOnly), viaButtonGrantOnly, len(missing), len(extra))
		if len(extra) > 0 {
			t.Logf("  %s 迁移后超出预设的按钮（权限码的粒度比按钮粗，或来自基线授权）：%v", name, extra)
		}
	}
}

// ---- 实例标签 ----

func TestInstanceTagsCoverLogicDependencies(t *testing.T) {
	d := loadDataset(t)
	for _, code := range []string{"can_write", "can_read", "mysql_inspection", "mssql_inspection"} {
		if _, ok := d.Tags[code]; !ok {
			t.Errorf("缺少实例标签 %s", code)
		}
	}
	if len(d.Tags) != 4 {
		t.Errorf("实例标签应为 4 个，实际 %d：%v", len(d.Tags), d.Tags)
	}
	// read_view 是 can_read 的旧同义标签，不再预置（老库里已有的行照常识别）
	if _, ok := d.Tags["read_view"]; ok {
		t.Error("不应再预置 read_view 标签")
	}
	// 标签名唯一（sql_instance_tag.tag_name 有唯一索引）、不超过 20 个字符
	names := map[string]string{}
	for code, name := range d.Tags {
		if prev, ok := names[name]; ok {
			t.Errorf("标签名 %q 重复：%s 与 %s", name, prev, code)
		}
		names[name] = code
		if n := len([]rune(name)); n == 0 || n > 20 {
			t.Errorf("标签 %s 的名称长度 %d 不合法", code, n)
		}
		if len(code) > 20 {
			t.Errorf("标签码 %s 超过 20 个字符", code)
		}
	}
}
