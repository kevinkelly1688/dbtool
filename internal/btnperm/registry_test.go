// @author: kevinkelly
// @license: Apache Licence
// @file: registry_test.go
// @time: 2026/10/28

package btnperm

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 早期版本里服务端实际用到的原权限码（迁移用）。登记表里的 Legacy 必须都在这里面：写错码会在这里被发现。
var knownDjangoCodes = map[string]bool{
	// SQL 上线 / 导出 / 查询 / 权限申请
	"menu_sqlworkflow": true, "menu_sqlanalyze": true, "menu_query": true, "menu_sqladvisor": true, "menu_slowquery": true,
	"menu_queryapplylist": true, "menu_sqlexportworkflow": true, "menu_sqlexportbatchworkflow": true,
	"sql_submit": true, "sqlbatch_submit": true, "sql_review": true, "sql_execute": true, "sql_execute_for_resource_group": true,
	"sql_analyze": true, "query_submit": true, "query_all_instances": true, "query_resource_group_instance": true,
	"query_applypriv": true, "query_review": true, "query_mgtpriv": true, "sqlexport_submit": true, "sqlexportbatch_submit": true,
	"offline_download": true, "audit_user": true,
	"optimize_sqltuning": true, "optimize_soar": true,
	// 分区维护
	"menu_partman_list": true, "add_partmanconfig": true, "change_partmanconfig": true, "delete_partmanconfig": true,
	// 数据归档
	"menu_archive": true, "archive_apply": true, "archive_mgt": true,
	// 实例管理
	"menu_instance_list": true, "menu_dbdiagnostic": true, "menu_database": true, "menu_instance_account": true, "menu_param": true,
	"add_instance": true, "change_instance": true, "delete_instance": true, "binlog_del": true,
	"process_view": true, "process_kill": true, "tablespace_view": true, "trx_view": true, "trxandlocks_view": true,
	"instance_account_manage": true, "param_view": true, "param_edit": true, "add_paramtemplate": true, "change_paramtemplate": true,
	// 巡检 / 性能工单（app_label 为 sql_perf）
	"menu_mssqlinspection": true, "inspection_plan_manage": true, "menu_perf": true, "perf_manage": true,
	// Binlog 解析（My2SQL 菜单）
	"menu_my2sql": true,
}

func TestRegistryValid(t *testing.T) {
	if err := Validate(); err != nil {
		t.Fatal(err)
	}
	total, grantable, danger := Counts()
	if total < 80 || grantable == 0 || danger == 0 || grantable > total {
		t.Fatalf("按钮数量不合常理: %d/%d/%d", total, grantable, danger)
	}
}

func TestRegistryNoDuplicateAndKinds(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Codes() {
		if seen[c] {
			t.Fatalf("重复码 %s", c)
		}
		seen[c] = true
		b, _ := Lookup(c)
		if b.Kind.Label() == string(b.Kind) {
			t.Errorf("%s 的类型 %q 无效", c, b.Kind)
		}
	}
}

func TestLegacyCodesAreKnown(t *testing.T) {
	for _, c := range LegacyCodes() {
		if !knownDjangoCodes[c] {
			t.Errorf("登记表里的原权限码 %q 不在已知清单里（写错了？）", c)
		}
	}
	for c := range knownDjangoCodes {
		if len(ButtonsForLegacy(c)) == 0 {
			t.Errorf("早期版本用到的原权限码 %q 没有任何按钮映射：迁移时会被丢掉", c)
		}
	}
}

func TestLegacyMappings(t *testing.T) {
	if b, ok := Lookup("sqlworkflow.execute_group"); !ok || len(b.Legacy) != 1 || b.Legacy[0] != "sql_execute_for_resource_group" {
		t.Fatalf("sqlworkflow.execute_group 的原权限码 = %v", b.Legacy)
	}
	if _, ok := Lookup("nope"); ok || Valid("nope") {
		t.Fatal("未登记码应无映射")
	}
	// 同一个原权限码映射了多个按钮：sql_execute 是 execute_own 的原码，也是 schedule 等细粒度按钮的原码
	var codes []string
	for _, b := range ButtonsForLegacy("sql_execute") {
		codes = append(codes, b.Code)
	}
	sort.Strings(codes)
	want := []string{"sqlworkflow.batch_op", "sqlworkflow.execute_own", "sqlworkflow.manual_finish", "sqlworkflow.osc", "sqlworkflow.schedule"}
	if strings.Join(codes, ",") != strings.Join(want, ",") {
		t.Fatalf("ButtonsForLegacy(sql_execute) = %v", codes)
	}
	if len(ButtonsForLegacy("not_a_code")) != 0 {
		t.Fatal("未知原权限码应为空")
	}
}

func TestEveryPageHasMenuViewAndLegacyApp(t *testing.T) {
	for _, m := range Modules() {
		for _, p := range m.Pages {
			if len(p.Menu) == 0 {
				for _, b := range p.Buttons {
					// 没有访问码（原权限码）的页面：按钮要么锁定（仅超级用户），要么是“只能显式授予”的新功能按钮（Super 且没有原权限码，如值班表）
					if !b.Locked && !(b.Super && len(b.Legacy) == 0) && p.Key != "inspect" {
						t.Errorf("页面 %s 没有访问码，按钮 %s 却可授予", p.Key, b.Code)
					}
				}
			}
		}
	}
	if LegacyApp("perf_manage") != "sql_perf" || LegacyApp("menu_perf") != "sql_perf" || LegacyApp("sql_review") != "sql" {
		t.Fatal("LegacyApp 不对")
	}
}

func TestPresets(t *testing.T) {
	for _, n := range PresetNames {
		list := Preset(n)
		if len(list) == 0 {
			t.Fatalf("预设 %s 为空", n)
		}
		for _, c := range list {
			b, ok := Lookup(c)
			if !ok || !b.Grantable() {
				t.Errorf("预设 %s 含无效或不可授予的码 %s", n, c)
			}
		}
	}
	for _, c := range presetKeep["TL"] { // 白名单里写错的码会被静默丢掉，这里显式检查
		if !Valid(c) {
			t.Errorf("预设白名单里的 %s 未登记", c)
		}
	}
	for n, keep := range presetKeep {
		for _, c := range keep {
			if !Valid(c) {
				t.Errorf("预设 %s 白名单里的 %s 未登记", n, c)
			}
		}
	}
	if Preset("nope") != nil {
		t.Fatal("未知预设应为 nil")
	}
	dba := map[string]bool{}
	for _, c := range Preset("DBA") {
		dba[c] = true
	}
	if !dba["sqlworkflow.execute_group"] || !dba["inspect.run"] || dba["audit.view"] || dba["user.edit"] {
		t.Fatal("DBA 预设应含执行、含 inspect.run 等运维按钮、不含审计")
	}
}

// 源码里每个 HasButton / RequireButtonAPI / RequireButtonPage / ActorHasButton / Btn.Can 的字面量码都必须已登记。
func TestSourceButtonLiteralsRegistered(t *testing.T) {
	re := regexp.MustCompile(`(?:HasButton|RequireButtonAPI|RequireButtonPage|ActorHasButton|\.Can|\.Show|\.Attr|\bbtn|\bbtnAny|\bguard)\(([^)]*)\)`)
	lit := regexp.MustCompile(`"([a-z_]+\.[a-z_]+)"`)
	root := filepath.Join("..", "..")
	found := 0
	_ = filepath.Walk(filepath.Join(root, "internal"), func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") || strings.Contains(p, string(filepath.Separator)+"btnperm"+string(filepath.Separator)+"registry.go") {
			return nil
		}
		raw, _ := os.ReadFile(p)
		for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
			for _, l := range lit.FindAllStringSubmatch(m[1], -1) {
				found++
				if !Valid(l[1]) {
					t.Errorf("%s 里校验了未登记的按钮码 %s", p, l[1])
				}
			}
		}
		return nil
	})
	if found < 20 {
		t.Logf("提示：只扫到 %d 处按钮码字面量", found)
	}
}
