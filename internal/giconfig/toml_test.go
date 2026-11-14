// @author: kevinkelly
// @license: Apache Licence
// @file: toml_test.go
// @time: 2026/10/28

package giconfig

import (
	"strings"
	"testing"
)

func TestValidateOK(t *testing.T) {
	r := Validate(baseConf)
	if !r.OK() || r.Warnings != 0 {
		t.Fatalf("合法配置不应有错误或警告：%+v", r.Issues)
	}
	// 关闭 WHERE 检查属于风险项，应给警告（带行号）而不是报错
	r = Validate("# 注释\n[inc]\ncheck_dml_where = false\n")
	if !r.OK() || r.Warnings != 1 || r.Issues[0].Key != "inc.check_dml_where" || r.Issues[0].Line != 3 {
		t.Fatalf("%+v", r.Issues)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{"空文件", "  \n", "不能为空"},
		{"语法错误", "[inc\nlang = 1", "语法错误"},
		{"重复键", "[inc]\nlang = \"zh-CN\"\nlang = \"en-US\"\n", "语法错误"},
		{"布尔写成字符串", "[inc]\ncheck_primary_key = \"true\"\n", "应为布尔值"},
		{"整数写成小数", "[inc]\nmax_keys = 3.5\n", "应为整数"},
		{"整数越界", "[inc]\nmax_keys = 1000\n", "超出范围"},
		{"负数", "[inc]\nmax_update_rows = -1\n", "超出范围"},
		{"端口越界", "[inc]\nbackup_port = 70000\n", "超出范围"},
		{"枚举非法", "[inc]\nlang = \"fr-FR\"\n", "不合法"},
		{"日志级别非法", "[log]\nlevel = \"loud\"\n", "不合法"},
		{"字符串写成数字", "[osc]\nosc_bin_dir = 5\n", "应为字符串"},
		{"含 NUL", "[inc]\x00\n", "NUL"},
		{"占位符残留", "[inc]\nbackup_password = \"********\"\n", "占位符"},
	}
	for _, c := range cases {
		r := Validate(c.src)
		if r.OK() {
			t.Errorf("%s：应该报错", c.name)
			continue
		}
		if !strings.Contains(firstError(r), c.want) {
			t.Errorf("%s：错误信息 %q 不含 %q", c.name, firstError(r), c.want)
		}
	}
	big := "[inc]\n# " + strings.Repeat("x", MaxContentBytes) + "\n"
	if r := Validate(big); r.OK() {
		t.Error("超大文件应拒绝")
	}
}

func TestValidateWarnings(t *testing.T) {
	// 顶层键必须出现在任何段之前才算顶层
	src := "top_level_unknown = 2\n[inc]\nenable_drop_table = true\nmystery_param = 1\nmax_update_rows = 5000000\n[weird]\nx = 1\n"
	r := Validate(src)
	if !r.OK() {
		t.Fatalf("只有警告时应视为通过：%+v", r.Issues)
	}
	want := []string{"DROP TABLE", "未知参数 mystery_param", "max_update_rows", "未知的配置段 [weird]", "顶层参数 top_level_unknown"}
	var all strings.Builder
	for _, i := range r.Issues {
		all.WriteString(i.Message + "\n")
		if i.Level != LevelWarn {
			t.Errorf("应为警告：%+v", i)
		}
	}
	for _, w := range want {
		if !strings.Contains(all.String(), w) {
			t.Errorf("缺少警告 %q，实际：\n%s", w, all.String())
		}
	}
}

func TestValidateAcceptsIntForFloat(t *testing.T) {
	if r := Validate("[osc]\nosc_chunk_time = 1\nosc_chunk_size_limit = 4.0\n"); !r.OK() || r.Warnings != 0 {
		t.Fatalf("%+v", r.Issues)
	}
}

func TestSetKeyReplaceKeepsComments(t *testing.T) {
	src := "# 头部注释\n[inc]\ncheck_dml_where = false   # 行尾注释\nmax_keys = 16\n\n[log]\nlevel = \"info\"\n"
	out, err := SetKey(src, "inc", "check_dml_where", true)
	if err != nil {
		t.Fatal(err)
	}
	want := "# 头部注释\n[inc]\ncheck_dml_where = true   # 行尾注释\nmax_keys = 16\n\n[log]\nlevel = \"info\"\n"
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
	out, _ = SetKey(out, "log", "level", "debug")
	if !strings.Contains(out, "level = \"debug\"\n") {
		t.Fatal(out)
	}
	out, _ = SetKey(out, "inc", "max_keys", float64(20))
	if !strings.Contains(out, "max_keys = 20\n") {
		t.Fatal(out)
	}
}

func TestSetKeyInsertAndNewSection(t *testing.T) {
	src := "[inc]\nlang = \"zh-CN\"\n\n[log]\nlevel = \"info\"\n"
	out, err := SetKey(src, "inc", "max_keys", float64(16))
	if err != nil {
		t.Fatal(err)
	}
	if want := "[inc]\nlang = \"zh-CN\"\nmax_keys = 16\n\n[log]\nlevel = \"info\"\n"; out != want {
		t.Fatalf("插入到段尾：\n%s", out)
	}
	out, _ = SetKey(out, "osc", "osc_on", true)
	if !strings.HasSuffix(out, "\n[osc]\nosc_on = true\n") {
		t.Fatalf("新建段：\n%s", out)
	}
	// 顶层键：放在第一个段之前
	out, _ = SetKey("[inc]\nlang = \"zh-CN\"\n", "", "port", float64(4000))
	if !strings.HasPrefix(out, "port = 4000\n\n[inc]") {
		t.Fatalf("顶层键：\n%s", out)
	}
	// 空文件
	out, _ = SetKey("", "inc", "lang", "en-US")
	if out != "[inc]\nlang = \"en-US\"\n" {
		t.Fatalf("空文件：%q", out)
	}
	// 结果必须仍是合法 TOML
	if r := Validate(out); !r.OK() {
		t.Fatal(r.Issues)
	}
}

func TestSetKeyTypeAndNameChecks(t *testing.T) {
	if _, err := SetKey("", "inc", "check_primary_key", "yes"); err == nil {
		t.Error("布尔参数传字符串应报错")
	}
	if _, err := SetKey("", "inc", "max_keys", 3.5); err == nil {
		t.Error("整数参数传小数应报错")
	}
	if _, err := SetKey("", "inc", "bad key\n= 1", true); err == nil {
		t.Error("非法参数名应报错")
	}
	if _, err := SetKey("", "in c", "x", true); err == nil {
		t.Error("非法段名应报错")
	}
	// 字符串里的特殊字符必须被转义，不能注入新的行
	out, err := SetKey("[inc]\n", "inc", "backup_host", "a\"b\nc = 1")
	if err != nil {
		t.Fatal(err)
	}
	vals, err := ParseValues(out)
	if err != nil || vals["inc.backup_host"] != "a\"b\nc = 1" || len(vals) != 1 {
		t.Fatalf("转义失败：%q %v %v", out, vals, err)
	}
	// 浮点参数整数值应写成 1.0
	out, _ = SetKey("[osc]\n", "osc", "osc_chunk_time", float64(1))
	if !strings.Contains(out, "osc_chunk_time = 1.0\n") {
		t.Fatal(out)
	}
}

func TestSetKeyMultilineValue(t *testing.T) {
	src := "[inc]\nmust_have_columns = \"\"\"\nid\ncreated_at\"\"\"  # 注释\nmax_keys = 8\n"
	out, err := SetKey(src, "inc", "must_have_columns", "id")
	if err != nil {
		t.Fatal(err)
	}
	if out != "[inc]\nmust_have_columns = \"id\"  # 注释\nmax_keys = 8\n" {
		t.Fatalf("多行值替换：%q", out)
	}
	// 多行数组不应让后面的键丢失
	src = "[security]\nlist = [\n  \"a\", # x\n  \"b\"\n]\nssl-ca = \"/x\"\n"
	es, _ := parseEntries(splitLines(src))
	if len(es) != 2 || es[0].Key != "list" || es[1].Key != "ssl-ca" || es[0].EndLine != 4 {
		t.Fatalf("多行数组解析：%+v", es)
	}
}

func TestMaskAndUnmask(t *testing.T) {
	m := MaskContent(baseConf)
	if strings.Contains(m, "s3cret") || !strings.Contains(m, `backup_password = "********"`) {
		t.Fatalf("脱敏失败：\n%s", m)
	}
	if !strings.Contains(m, "check_dml_limit = false") {
		t.Fatal("其他行不应改动")
	}
	// 未改动：占位符还原为原值，整体与原文件一致
	back, err := Unmask(m, baseConf)
	if err != nil || back != baseConf {
		t.Fatalf("还原失败：%v\n%s", err, back)
	}
	// 用户在源文件里直接写了新密码：不是占位符，原样保留
	changed := strings.Replace(m, `"********"`, `"newpw"`, 1)
	back, err = Unmask(changed, baseConf)
	if err != nil || !strings.Contains(back, `backup_password = "newpw"`) {
		t.Fatalf("%v %s", err, back)
	}
	// 没有原值可还原：必须报错而不是写入占位符
	if _, err := Unmask(m, "[inc]\nlang = \"zh-CN\"\n"); err == nil || !strings.Contains(err.Error(), "backup_password") {
		t.Fatalf("应报错：%v", err)
	}
	// 空密码不脱敏
	if got := MaskContent("[inc]\nbackup_password = \"\"\n"); !strings.Contains(got, `backup_password = ""`) {
		t.Fatal(got)
	}
	// 基线里的原值带转义也能还原
	base := "[inc]\nbackup_password = \"a\\\"b\"\n"
	m2 := MaskContent(base)
	if back, err := Unmask(m2, base); err != nil || back != base {
		t.Fatalf("%v %q", err, back)
	}
}

func TestParseParamsForForm(t *testing.T) {
	s := &Service{}
	p := s.ParseParams(baseConf)
	if !p.Report.OK() {
		t.Fatal(p.Report.Issues)
	}
	if p.Values["inc.check_primary_key"] != true || p.Values["inc.max_update_rows"] != int64(5000) || p.Values["log.level"] != "info" {
		t.Fatalf("%v", p.Values)
	}
	if _, leaked := p.Values["inc.backup_password"]; leaked {
		t.Fatal("密码类参数不能出现在 values 里")
	}
	if len(p.SecretSet) != 1 || p.SecretSet[0] != "inc.backup_password" {
		t.Fatalf("SecretSet = %v", p.SecretSet)
	}
	bad := s.ParseParams("[inc\n")
	if bad.Report.OK() || len(bad.Values) != 0 {
		t.Fatal("语法错误时不应返回参数")
	}
}

func TestPatchContent(t *testing.T) {
	s := &Service{}
	out, err := s.PatchContent(baseConf, []Change{{"inc", "check_dml_limit", true}, {"inc", "max_update_rows", float64(10000)}, {"osc", "osc_on", true}})
	if err != nil {
		t.Fatal(err)
	}
	vals, _ := ParseValues(out)
	if vals["inc.check_dml_limit"] != true || vals["inc.max_update_rows"] != int64(10000) || vals["osc.osc_on"] != true {
		t.Fatalf("%v", vals)
	}
	if _, err := s.PatchContent(baseConf, []Change{{"inc", "backup_password", "x"}}); err == nil {
		t.Fatal("密码类参数不允许通过表单写入")
	}
	if _, err := s.PatchContent(baseConf, []Change{{"inc", "max_keys", "abc"}}); err == nil {
		t.Fatal("非法整数应报错")
	}
}

func TestSummarize(t *testing.T) {
	newConf := strings.Replace(baseConf, "check_dml_limit = false", "check_dml_limit = true", 1)
	newConf = strings.Replace(newConf, "max_update_rows = 5000", "max_update_rows = 10000", 1)
	got := Summarize(baseConf, newConf)
	if got != "check_dml_limit: false → true；max_update_rows: 5000 → 10000" {
		t.Fatalf("摘要 = %q", got)
	}
	pw := strings.Replace(baseConf, "s3cret", "other", 1)
	if got := Summarize(baseConf, pw); got != "backup_password: 已修改" {
		t.Fatalf("密码变更摘要 = %q（不能泄露值）", got)
	}
	if got := Summarize(baseConf, baseConf+"# 注释\n"); got != "仅注释或排版变更" {
		t.Fatal(got)
	}
	if got := Summarize(baseConf, "[inc\n"); got != "配置文件内容变更" {
		t.Fatal(got)
	}
	if got := Summarize("[inc]\nlang = \"zh-CN\"\n", "[inc]\nlang = \"zh-CN\"\nmax_keys = 3\n"); !strings.Contains(got, "新增 3") {
		t.Fatal(got)
	}
}

func TestFormGroupsMatchCatalog(t *testing.T) {
	groups := FormGroups()
	if len(groups) != 4 {
		t.Fatalf("分组数 = %d", len(groups))
	}
	for _, g := range groups {
		for _, it := range g.Items {
			if lookupParam(it.Section, it.Key) == nil {
				t.Errorf("表单项 %s.%s 不在参数目录里", it.Section, it.Key)
			}
			if it.Kind == "" {
				t.Errorf("表单项 %s.%s 缺少类型", it.Section, it.Key)
			}
		}
	}
}

func TestDiff(t *testing.T) {
	a := "l1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\nl9\nl10\n"
	b := "l1\nl2\nl3\nl4\nCHANGED\nl6\nl7\nl8\nl9\nl10\nl11\n"
	d := Diff(a, b, 1)
	if d.Added != 2 || d.Removed != 1 || d.Same {
		t.Fatalf("统计 = +%d -%d", d.Added, d.Removed)
	}
	var ops []string
	for _, l := range d.Lines {
		ops = append(ops, l.Op+l.Text)
	}
	got := strings.Join(ops, "|")
	if !strings.Contains(got, "-l5|+CHANGED") || !strings.Contains(got, "~") {
		t.Fatalf("diff 输出：%s", got)
	}
	if full := Diff(a, b, -1); len(full.Lines) != 12 {
		t.Fatalf("全量行数 = %d", len(full.Lines))
	}
	if !Diff(a, a, 3).Same {
		t.Fatal("相同内容应 Same")
	}
	if d := Diff("", "x\ny\n", 3); d.Added != 2 || d.Removed != 0 {
		t.Fatalf("%+v", d)
	}
	if d := Diff("x\ny\n", "", 3); d.Removed != 2 {
		t.Fatalf("%+v", d)
	}
}

func TestDiffMasksPasswords(t *testing.T) {
	newConf := strings.Replace(baseConf, "s3cret", "brand-new-pw", 1)
	d := Diff(baseConf, newConf, -1)
	var all strings.Builder
	for _, l := range d.Lines {
		all.WriteString(l.Text + "\n")
	}
	if strings.Contains(all.String(), "s3cret") || strings.Contains(all.String(), "brand-new-pw") {
		t.Fatalf("对比里泄露了密码：\n%s", all.String())
	}
	if d.Added != 1 || d.Removed != 1 || !strings.Contains(all.String(), "已修改") {
		t.Fatalf("密码变更应显示为一行修改：+%d -%d\n%s", d.Added, d.Removed, all.String())
	}
	// 密码没变就不出现在差异里
	if !Diff(baseConf, baseConf, -1).Same {
		t.Fatal()
	}
}

func TestDiffLargeFallback(t *testing.T) {
	var a, b strings.Builder
	for i := 0; i < 2500; i++ {
		a.WriteString("a" + strings.Repeat("x", i%7) + "\n")
		b.WriteString("b" + strings.Repeat("y", i%5) + "\n")
	}
	d := Diff(a.String(), b.String(), 0)
	if d.Removed != 2500 || d.Added != 2500 {
		t.Fatalf("超大文件应退化为整段替换：+%d -%d", d.Added, d.Removed)
	}
}

// 上游 goInception 最新版里真实存在的参数与段：不应有错误，也不应有“未知”警告。
func TestValidateUpstreamParams(t *testing.T) {
	src := "[inc]\nsql_safe_updates = 0\nenable_sql_statistic = true\nenable_any_statement = false\n" +
		"check_read_only = true\ncustom_keywords = [\"a\", \"b\"]\nskip_grant_table = true\n" +
		"[osc]\nosc_max_flow_ctl = -1\nosc_sleep = 0.0\nosc_lock_wait_timeout = 60\n" +
		"[ghost]\nghost_heartbeat_interval_millis = 500\nghost_approve_renamed_columns = true\n" +
		"ghost_exponential_backoff_max_interval = 64\nghost_dml_batch_size = 10\n" +
		"[log.file]\nlog-rotate = true\n[inc_level]\ner_table_must_have_pk = 2\ner_no_where_condition = 1\n"
	r := Validate("advertise-address = \"\"\nignore_sighup = true\n" + src)
	if !r.OK() || r.Warnings != 0 {
		t.Fatalf("上游已有的参数不应报错或警告：%+v", r.Issues)
	}
	// sql_safe_updates 是整数 -1 / 0 / 1，写成布尔值才是类型错误
	if r := Validate("[inc]\nsql_safe_updates = true\n"); r.OK() || !strings.Contains(firstError(r), "应为整数") {
		t.Errorf("布尔值应报类型错误：%+v", r.Issues)
	}
	if r := Validate("[inc]\nsql_safe_updates = 5\n"); r.OK() {
		t.Error("超出 -1~1 应报错")
	}
	// inc_level 的值必须是整数；数组元素必须是字符串
	if r := Validate("[inc_level]\ner_x = \"2\"\n"); r.OK() {
		t.Error("inc_level 的值写成字符串应报错")
	}
	if r := Validate("[inc]\ncustom_keywords = [1, 2]\n"); r.OK() {
		t.Error("custom_keywords 的元素必须是字符串")
	}
	// 上游没有的写法仍给警告：下划线的 advertise_address（上游是 advertise-address）、enable_not_innodb
	r = Validate("advertise_address = \"\"\n[inc]\nenable_not_innodb = true\n")
	if !r.OK() || r.Warnings != 2 {
		t.Errorf("未知参数应只给警告：%+v", r.Issues)
	}
}
