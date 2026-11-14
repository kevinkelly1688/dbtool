// @author: kevinkelly
// @license: Apache Licence
// @file: http_test.go
// @time: 2026/10/28

package duty

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"dbtool/internal/btnperm"
	"dbtool/internal/model"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

// fakeButtons 是测试用的按钮权限检查器：按用户 ID 授予按钮。
type fakeButtons struct{ grants map[uint]map[string]bool }

func (f fakeButtons) HasButton(uid uint, code string) (bool, error) { return f.grants[uid][code], nil }
func (f fakeButtons) AllowedButtons(uid uint) (map[string]bool, error) {
	out := map[string]bool{}
	for c, ok := range f.grants[uid] {
		out[c] = ok
	}
	return out, nil
}
func (f fakeButtons) Setting() (btnperm.Setting, error) { return btnperm.DefaultSetting(), nil }

type httpEnv struct {
	*servertest.Env
	f      *fixture
	audits *[]*model.AuditEntry
}

// newHTTPEnv 起一个挂了值班表模块的测试服务；grants 非空时按用户 ID 授予按钮（root 是超级用户，bob 的 ID 是 2）。
func newHTTPEnv(t *testing.T, grants map[uint]map[string]bool) *httpEnv {
	t.Helper()
	f := newFixture(t)
	var audits []*model.AuditEntry
	env := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){
		func(s *server.Server, rt server.Routes) {
			if grants != nil {
				s.SetButtonChecker(fakeButtons{grants})
			}
			Mount(s, rt, f.svc, func(e *model.AuditEntry) error { audits = append(audits, e); return nil })
		},
	}})
	return &httpEnv{Env: env, f: f, audits: &audits}
}

func dataOf(t *testing.T, body string, out any) {
	t.Helper()
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("响应不是 JSON：%v\n%s", err, body)
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		t.Fatalf("data 解码失败：%v\n%s", err, body)
	}
}

func TestHTTPAccessControl(t *testing.T) {
	e := newHTTPEnv(t, nil)
	if w := e.Anonymous("GET", "/api/v1/duty/now"); w.Code != http.StatusUnauthorized {
		t.Errorf("未登录取 now 应 401：%d", w.Code)
	}
	if w := e.Anonymous("GET", "/settings/duty"); w.Code != http.StatusFound {
		t.Errorf("未登录页面应跳转登录：%d", w.Code)
	}
	bob := e.Login("bob")
	// 登录即可看顶栏的值班信息
	w := bob.Do("GET", "/api/v1/duty/now", nil)
	var now NowView
	dataOf(t, w.Body.String(), &now)
	if w.Code != 200 || now.Enabled {
		t.Errorf("没有排班时 now 应 200 且 enabled=false：%d %+v", w.Code, now)
	}
	// 管理页与写操作：没有按钮权限一律 403
	if w := bob.Do("GET", "/api/v1/duty/overview", nil); w.Code != 403 {
		t.Errorf("普通用户看概览应 403：%d", w.Code)
	}
	if code, _ := bob.Page("/settings/duty"); code != 403 {
		t.Errorf("普通用户打开管理页应 403：%d", code)
	}
	body := map[string]any{"name": "白班", "username": "zhangsan", "start": "08:00", "end": "19:00", "weekdays": []int{1}}
	if w := bob.Do("POST", "/api/v1/duty/shifts", body); w.Code != 403 {
		t.Errorf("普通用户新建班次应 403：%d", w.Code)
	}
	if w := bob.Do("POST", "/api/v1/duty/overrides", map[string]any{"username": "zhangsan", "start": "2026-10-22 11:00", "end": "2026-10-22 12:00"}); w.Code != 403 {
		t.Errorf("普通用户新建调班应 403：%d", w.Code)
	}
	list, _ := e.f.store.ListShifts()
	if len(list) != 0 {
		t.Errorf("被拒绝的请求不能写入数据：%+v", list)
	}
	root := e.Login("root")
	code, page := root.Page("/settings/duty")
	if code != 200 || !strings.Contains(page, `id="duty"`) || !strings.Contains(page, "/static/js/duty/main.js") || !strings.Contains(page, `data-edit="1"`) || !strings.Contains(page, `data-swap="1"`) {
		t.Errorf("超级用户打开管理页：%d", code)
	}
	if !strings.Contains(page, `id="duty-shift-new"`) || !strings.Contains(page, `id="duty-swap-new"`) {
		t.Error("超级用户应看到新建按钮")
	}
}

func TestHTTPCRUDAndAudit(t *testing.T) {
	e := newHTTPEnv(t, nil)
	root := e.Login("root")
	create := func(name, user, start, end string, days []int) (int, string, uint) {
		w := root.Do("POST", "/api/v1/duty/shifts", map[string]any{"name": name, "username": user, "start": start, "end": end, "weekdays": days, "contact": "8001"})
		var out struct{ ID uint }
		if w.Code == 200 {
			dataOf(t, w.Body.String(), &out)
		}
		return w.Code, w.Body.String(), out.ID
	}
	all := []int{0, 1, 2, 3, 4, 5, 6}
	code, body, dayID := create("白班", "zhangsan", "08:00", "19:00", all)
	if code != 200 || dayID == 0 {
		t.Fatalf("新建：%d %s", code, body)
	}
	if code, _, _ := create("晚班", "lisi", "19:00", "24:00", all); code != 200 {
		t.Fatal("晚班")
	}
	if code, _, _ := create("夜班", "zhaowu", "00:00", "08:00", all); code != 200 {
		t.Fatal("夜班")
	}
	// 校验失败：400 + 中文原因；重叠：409
	code, body, _ = create("", "zhangsan", "08:00", "19:00", all)
	if code != 400 || !strings.Contains(body, "班次名称") {
		t.Errorf("名称为空：%d %s", code, body)
	}
	code, body, _ = create("加班", "wangwu", "18:00", "20:00", []int{1})
	if code != 409 || !strings.Contains(body, "与班次「白班」时间重叠") || !strings.Contains(body, `"code":"conflict"`) {
		t.Errorf("重叠：%d %s", code, body)
	}
	// 概览
	var ov Overview
	root.GetJSON("/api/v1/duty/overview", &ov)
	if !ov.Ready || len(ov.Shifts) != 3 || len(ov.Gaps) != 0 || len(ov.Days) != 7 {
		t.Errorf("概览：%+v", ov)
	}
	// 修改：换人
	w := root.Do("PUT", "/api/v1/duty/shifts/"+itoa(dayID), map[string]any{"name": "白班", "username": "wangwu", "start": "08:00", "end": "19:00", "weekdays": all})
	if w.Code != 200 {
		t.Fatalf("修改：%d %s", w.Code, w.Body.String())
	}
	if w := root.Do("PUT", "/api/v1/duty/shifts/999", map[string]any{"name": "x", "username": "wangwu", "start": "08:00", "end": "19:00", "weekdays": all}); w.Code != 404 {
		t.Errorf("修改不存在的班次应 404：%d", w.Code)
	}
	for _, bad := range []string{"abc", "12abc", "0", "-1", "1.5"} { // 路径里的 id 不是正整数：按“不存在”处理，不会取到别的记录
		if w := root.Do("PUT", "/api/v1/duty/shifts/"+bad, map[string]any{"name": "x"}); w.Code != 404 || !strings.Contains(w.Body.String(), "班次不存在") {
			t.Errorf("班次 id %q 应 404：%d %s", bad, w.Code, w.Body.String())
		}
	}
	if w := root.Do("POST", "/api/v1/duty/overrides/12abc/cancel", map[string]any{"confirm": true}); w.Code != 404 || !strings.Contains(w.Body.String(), "调班不存在") {
		t.Errorf("调班 id 不是正整数应 404：%d %s", w.Code, w.Body.String())
	}
	// 删除要二次确认
	if w := root.Do("POST", "/api/v1/duty/shifts/"+itoa(dayID)+"/delete", map[string]any{}); w.Code != 400 || !strings.Contains(w.Body.String(), "need_confirm") {
		t.Errorf("删除没带 confirm 应 400：%d %s", w.Code, w.Body.String())
	}
	if w := root.Do("POST", "/api/v1/duty/shifts/"+itoa(dayID)+"/delete", map[string]any{"confirm": true}); w.Code != 200 {
		t.Errorf("删除：%d %s", w.Code, w.Body.String())
	}
	// 调班
	w = root.Do("POST", "/api/v1/duty/overrides", map[string]any{"username": "wangwu", "start": "2026-10-22 20:00", "end": "2026-10-22 22:00", "reason": "请假"})
	var ovr struct{ ID uint }
	dataOf(t, w.Body.String(), &ovr)
	if w.Code != 200 || ovr.ID == 0 {
		t.Fatalf("新建调班：%d %s", w.Code, w.Body.String())
	}
	if w := root.Do("POST", "/api/v1/duty/overrides/"+itoa(ovr.ID)+"/cancel", map[string]any{}); w.Code != 400 {
		t.Errorf("撤销没带 confirm 应 400：%d", w.Code)
	}
	if w := root.Do("POST", "/api/v1/duty/overrides/"+itoa(ovr.ID)+"/cancel", map[string]any{"confirm": true}); w.Code != 200 {
		t.Errorf("撤销：%d %s", w.Code, w.Body.String())
	}
	if w := root.Do("POST", "/api/v1/duty/overrides/"+itoa(ovr.ID)+"/cancel", map[string]any{"confirm": true}); w.Code != 409 {
		t.Errorf("重复撤销应 409：%d", w.Code)
	}
	if w := root.Do("POST", "/api/v1/duty/shifts", `{"name":`); w.Code != 400 {
		t.Errorf("请求体不是合法 JSON 应 400：%d", w.Code)
	}

	// 每次成功的写操作都有审计；失败的没有
	var actions []string
	for _, a := range *e.audits {
		actions = append(actions, a.Action)
		if a.UserName != "root" || a.ExtraInfo == "" {
			t.Errorf("审计缺少操作人或内容：%+v", a)
		}
	}
	want := []string{"值班-新增班次", "值班-新增班次", "值班-新增班次", "值班-修改班次", "值班-删除班次", "值班-新增调班", "值班-撤销调班"}
	if strings.Join(actions, ",") != strings.Join(want, ",") {
		t.Errorf("审计动作 = %v，期望 %v", actions, want)
	}
	if got := (*e.audits)[3].ExtraInfo; !strings.Contains(got, "值班人 zhangsan") || !strings.Contains(got, "值班人 wangwu") || !strings.Contains(got, "->") {
		t.Errorf("修改审计应写清前后：%s", got)
	}
}

func itoa(n uint) string { return strconv.FormatUint(uint64(n), 10) }

func TestHTTPButtonGrants(t *testing.T) {
	e := newHTTPEnv(t, map[uint]map[string]bool{2: {BtnView: true}})
	bob := e.Login("bob")
	if w := bob.Do("GET", "/api/v1/duty/overview", nil); w.Code != 200 {
		t.Errorf("授予 duty.view 后应能看概览：%d", w.Code)
	}
	code, page := bob.Page("/settings/duty")
	if code != 200 || !strings.Contains(page, `data-edit=""`) || !strings.Contains(page, `data-swap=""`) || strings.Contains(page, `id="duty-shift-new"`) {
		t.Errorf("只读用户的管理页：%d 不应有新建按钮", code)
	}
	body := map[string]any{"name": "白班", "username": "zhangsan", "start": "08:00", "end": "19:00", "weekdays": []int{1}}
	if w := bob.Do("POST", "/api/v1/duty/shifts", body); w.Code != 403 {
		t.Errorf("只有 duty.view 不能新建班次：%d", w.Code)
	}

	e2 := newHTTPEnv(t, map[uint]map[string]bool{2: {BtnView: true, BtnEdit: true}})
	bob = e2.Login("bob")
	if w := bob.Do("POST", "/api/v1/duty/shifts", body); w.Code != 200 {
		t.Errorf("授予 duty.edit 后应能新建班次：%d %s", w.Code, w.Body.String())
	}
	swap := map[string]any{"username": "wangwu", "start": "2026-10-22 11:00", "end": "2026-10-22 12:00"}
	if w := bob.Do("POST", "/api/v1/duty/overrides", swap); w.Code != 403 {
		t.Errorf("没有 duty.swap 不能新建调班：%d", w.Code)
	}

	e3 := newHTTPEnv(t, map[uint]map[string]bool{2: {BtnView: true, BtnSwap: true}})
	bob = e3.Login("bob")
	if w := bob.Do("POST", "/api/v1/duty/overrides", swap); w.Code != 200 {
		t.Errorf("授予 duty.swap 后应能新建调班：%d %s", w.Code, w.Body.String())
	}
	if w := bob.Do("POST", "/api/v1/duty/shifts", body); w.Code != 403 {
		t.Errorf("只有 duty.swap 不能新建班次：%d", w.Code)
	}
}

// 顶栏：有值班信息时每个页面都带芯片，没有时一点痕迹都没有；管理入口只给有权限的人。
func TestHTTPTopbarChipAndMenu(t *testing.T) {
	e := newHTTPEnv(t, nil)
	root, bob := e.Login("root"), e.Login("bob")
	if _, page := root.Page("/help"); strings.Contains(page, "tb-duty") || strings.Contains(page, "shell/duty.js") {
		t.Error("没有排班时页面不应出现值班芯片和脚本")
	}
	// 一个 24 小时的班：不管现在几点都有人值班
	if _, err := e.f.svc.CreateShift("root", ShiftInput{Name: "全天", Username: "zhangsan", Start: "00:00", End: "00:00", Weekdays: allDays(), Contact: "分机 8001"}); err != nil {
		t.Fatal(err)
	}
	code, page := root.Page("/help")
	if code != 200 {
		t.Fatal(code)
	}
	for _, want := range []string{`id="tb-duty"`, `id="td-btn"`, `class="td-btn on"`, `<b class="td-name">张三</b>`, `00:00–24:00`, `data-manage="/settings/duty"`,
		`data-duty="{&#34;enabled&#34;:true`, `/static/js/shell/duty.js`, `href="/settings/duty"`} {
		if !strings.Contains(page, want) {
			t.Errorf("超级用户的页面应含 %s", want)
		}
	}
	// 芯片在时钟前面
	if i, j := strings.Index(page, `id="tb-duty"`), strings.Index(page, `id="tb-clock"`); i < 0 || j < 0 || i > j {
		t.Errorf("值班芯片应在时钟前面：%d %d", i, j)
	}
	_, page = bob.Page("/help")
	if !strings.Contains(page, `id="tb-duty"`) || !strings.Contains(page, `<b class="td-name">张三</b>`) {
		t.Error("普通用户也能看到值班芯片")
	}
	if strings.Contains(page, "data-manage") || strings.Contains(page, `href="/settings/duty"`) {
		t.Error("没有 duty.view 的用户不应看到管理入口")
	}
	// 当前时段没人（值班人账号停用）：芯片还在，显示“暂无”
	for i := range e.f.dir {
		if e.f.dir[i].Username == "zhangsan" {
			e.f.dir[i].Active = false
		}
	}
	e.f.svc.Invalidate()
	_, page = root.Page("/help")
	if !strings.Contains(page, `class="td-btn none"`) || !strings.Contains(page, `<b class="td-name">暂无</b>`) {
		t.Errorf("值班人账号停用：应显示暂无")
	}
	// 排班都删掉：芯片消失
	list, _ := e.f.store.ListShifts()
	for _, s := range list {
		_, _ = e.f.svc.DeleteShift(s.ID)
	}
	if _, page := root.Page("/help"); strings.Contains(page, "tb-duty") {
		t.Error("没有任何班次后芯片应消失")
	}
}

// 调班期间芯片带“调班”标记，时间段写调班自己的起止。
func TestHTTPChipShowsOverride(t *testing.T) {
	e := newHTTPEnv(t, nil)
	if _, err := e.f.svc.CreateShift("root", ShiftInput{Name: "全天", Username: "zhangsan", Start: "00:00", End: "00:00", Weekdays: allDays()}); err != nil {
		t.Fatal(err)
	}
	// 页面渲染用的是真实时间，测试服务的“现在”是固定的 2026-10-22，所以这里直接取指定时刻的芯片数据来验证。
	if _, err := e.f.svc.CreateOverride("root", OverrideInput{Username: "wangwu", Start: "2026-10-22 11:00", End: "2026-10-22 13:00", Reason: "请假"}); err != nil {
		t.Fatal(err)
	}
	v := e.f.svc.Now(at(0, 11, 30))
	if v.Cur == nil || v.Cur.Kind != KindOverride || v.Cur.Display != "王五" {
		t.Fatalf("调班期间：%+v", v.Cur)
	}
	chip := chipProvider{e.f.svc}.Chip(at(0, 11, 30))
	if chip == nil || !chip.Override || chip.Name != "王五" || chip.Range != "11:00–13:00" || chip.State != "on" {
		t.Errorf("芯片：%+v", chip)
	}
	if !strings.Contains(chip.JSON, `"kind":"override"`) {
		t.Errorf("快照 JSON：%s", chip.JSON)
	}
}

// 停用 / 启用走单独的接口（只改开关）；修改带版本，别人改过就 409；审计里写清联系方式和备注。
func TestHTTPEnableShiftAndVersion(t *testing.T) {
	e := newHTTPEnv(t, nil)
	root := e.Login("root")
	all := []int{0, 1, 2, 3, 4, 5, 6}
	w := root.Do("POST", "/api/v1/duty/shifts", map[string]any{"name": "白班", "username": "zhangsan", "start": "08:00", "end": "19:00", "weekdays": all, "contact": "分机 8001", "remark": "主班"})
	var created struct{ ID uint }
	dataOf(t, w.Body.String(), &created)
	if w.Code != 200 || created.ID == 0 {
		t.Fatalf("新建：%d %s", w.Code, w.Body.String())
	}
	base := "/api/v1/duty/shifts/" + itoa(created.ID)
	if got := (*e.audits)[0].ExtraInfo; !strings.Contains(got, "联系方式 分机 8001") || !strings.Contains(got, "备注：主班") {
		t.Errorf("新增审计应含联系方式和备注：%s", got)
	}

	// 停用、重复停用（不重复写审计）、启用
	if w := root.Do("POST", base+"/enable", map[string]any{"enabled": false}); w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("停用：%d %s", w.Code, w.Body.String())
	}
	if w := root.Do("POST", base+"/enable", map[string]any{"enabled": false}); w.Code != 200 {
		t.Errorf("重复停用：%d", w.Code)
	}
	if w := root.Do("POST", base+"/enable", map[string]any{"enabled": true}); w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":true`) {
		t.Fatalf("启用：%d %s", w.Code, w.Body.String())
	}
	var actions []string
	for _, a := range *e.audits {
		actions = append(actions, a.Action)
	}
	if strings.Join(actions, ",") != "值班-新增班次,值班-停用班次,值班-启用班次" {
		t.Errorf("审计动作 = %v", actions)
	}
	if got := (*e.audits)[1].ExtraInfo; !strings.Contains(got, "班次「白班」") || !strings.Contains(got, "已停用") {
		t.Errorf("停用审计：%s", got)
	}
	// 请求有问题
	if w := root.Do("POST", base+"/enable", map[string]any{}); w.Code != 400 {
		t.Errorf("没带 enabled 应 400：%d", w.Code)
	}
	for _, id := range []string{"999", "abc", "0"} {
		if w := root.Do("POST", "/api/v1/duty/shifts/"+id+"/enable", map[string]any{"enabled": true}); w.Code != 404 || !strings.Contains(w.Body.String(), "班次不存在") {
			t.Errorf("班次 %s 应 404：%d %s", id, w.Code, w.Body.String())
		}
	}

	// 版本：A、B 同时打开编辑框，B 先保存，A 拿旧版本保存被拒
	var ov Overview
	root.GetJSON("/api/v1/duty/overview", &ov)
	if len(ov.Shifts) != 1 || ov.Shifts[0].Version == "" {
		t.Fatalf("概览里的班次应带版本：%+v", ov.Shifts)
	}
	ver := ov.Shifts[0].Version
	put := func(contact, version string) (int, string) {
		w := root.Do("PUT", base, map[string]any{"name": "白班", "username": "zhangsan", "start": "08:00", "end": "19:00", "weekdays": all, "contact": contact, "version": version})
		return w.Code, w.Body.String()
	}
	if code, body := put("分机 1", ver); code != 200 {
		t.Fatalf("B 保存：%d %s", code, body)
	}
	code, body := put("分机 2", ver)
	if code != 409 || !strings.Contains(body, "这个班次刚被别人修改过") || !strings.Contains(body, `"code":"conflict"`) {
		t.Errorf("A 用旧版本保存应 409：%d %s", code, body)
	}
	if sh, _ := e.f.store.GetShift(created.ID); sh.Contact != "分机 1" {
		t.Errorf("B 的修改不能被盖掉：%+v", sh)
	}
	if got := (*e.audits)[len(*e.audits)-1]; got.Action != "值班-修改班次" || !strings.Contains(got.ExtraInfo, "联系方式 分机 1") {
		t.Errorf("最后一条审计应是 B 的修改：%+v", got)
	}
}

func TestHTTPEnableShiftNeedsEditGrant(t *testing.T) {
	e := newHTTPEnv(t, map[uint]map[string]bool{2: {BtnView: true, BtnSwap: true}})
	sh, err := e.f.svc.CreateShift("root", ShiftInput{Name: "白班", Username: "zhangsan", Start: "08:00", End: "19:00", Weekdays: allDays()})
	if err != nil {
		t.Fatal(err)
	}
	bob := e.Login("bob")
	if w := bob.Do("POST", "/api/v1/duty/shifts/"+itoa(sh.ID)+"/enable", map[string]any{"enabled": false}); w.Code != 403 {
		t.Errorf("没有 duty.edit 不能停用班次：%d", w.Code)
	}
	if got, _ := e.f.store.GetShift(sh.ID); !got.Enabled {
		t.Error("被拒绝的请求不能改数据")
	}
}

// 值班人名单（全部账号的登录名和显示名）只给能新增 / 修改班次或调班的人；只读用户拿不到，名单里也没有邮箱。
func TestHTTPOverviewUsersByGrant(t *testing.T) {
	for _, c := range []struct {
		name   string
		grants map[uint]map[string]bool
		users  int
	}{
		{"只读", map[uint]map[string]bool{2: {BtnView: true}}, 0},
		{"能改班次", map[uint]map[string]bool{2: {BtnView: true, BtnEdit: true}}, 4},
		{"能调班", map[uint]map[string]bool{2: {BtnView: true, BtnSwap: true}}, 4},
	} {
		e := newHTTPEnv(t, c.grants)
		w := e.Login("bob").Do("GET", "/api/v1/duty/overview", nil)
		var ov Overview
		dataOf(t, w.Body.String(), &ov)
		if w.Code != 200 || len(ov.Users) != c.users {
			t.Errorf("%s：%d users=%d，期望 %d", c.name, w.Code, len(ov.Users), c.users)
		}
		if strings.Contains(w.Body.String(), "@") {
			t.Errorf("%s：名单里不能带邮箱：%s", c.name, w.Body.String())
		}
	}
	e := newHTTPEnv(t, nil)
	var ov Overview
	e.Login("root").GetJSON("/api/v1/duty/overview", &ov)
	if len(ov.Users) != 4 {
		t.Errorf("超级用户：users=%d", len(ov.Users))
	}
}
