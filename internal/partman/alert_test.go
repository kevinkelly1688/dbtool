// @author: kevinkelly
// @license: Apache Licence
// @file: alert_test.go
// @time: 2026/10/28

package partman

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"dbtool/internal/model"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

// ---- 假仓库：在 fakeRepo 之上补上 AlertRepo ----

type alertRepoFake struct {
	*fakeRepo
	rows map[int64]AlertSetting
	logs []AlertLog
	down bool // 模拟扩展表没建
}

func newAlertRepo(r *fakeRepo) *alertRepoFake {
	return &alertRepoFake{fakeRepo: r, rows: map[int64]AlertSetting{}}
}

func (a *alertRepoFake) AlertReady() bool { return !a.down }

func (a *alertRepoFake) ListAlertSettings(ids []int64) (map[int64]AlertSetting, error) {
	if a.down {
		return nil, ErrAlertNotReady
	}
	out := map[int64]AlertSetting{}
	for _, id := range ids {
		if r, ok := a.rows[id]; ok {
			out[id] = r
		}
	}
	return out, nil
}

func (a *alertRepoFake) SetAlertThreshold(id int64, thr *int) error {
	if a.down {
		return ErrAlertNotReady
	}
	r := a.rows[id]
	r.CnfID, r.Threshold = id, thr
	a.rows[id] = r
	return nil
}

func (a *alertRepoFake) SetAlertSilence(id int64, until *time.Time) error {
	if a.down {
		return ErrAlertNotReady
	}
	r := a.rows[id]
	r.CnfID, r.SilenceUntil = id, until
	a.rows[id] = r
	return nil
}

func (a *alertRepoFake) SaveAlertState(id int64, alerting bool, last *time.Time) error {
	if a.down {
		return ErrAlertNotReady
	}
	r := a.rows[id]
	r.CnfID, r.Alerting = id, alerting
	if last != nil {
		r.LastAlertAt = last
	}
	a.rows[id] = r
	return nil
}

func (a *alertRepoFake) InsertAlertLog(l *AlertLog) error {
	if a.down {
		return ErrAlertNotReady
	}
	l.ID = uint(len(a.logs) + 1)
	a.logs = append(a.logs, *l)
	return nil
}

func (a *alertRepoFake) ListAlertLogs(ids []int64, offset, limit int) ([]AlertLog, int64, error) {
	if a.down {
		return nil, 0, ErrAlertNotReady
	}
	in := map[int64]bool{}
	for _, id := range ids {
		in[id] = true
	}
	var all []AlertLog
	for i := len(a.logs) - 1; i >= 0; i-- { // 新的在前
		if in[a.logs[i].CnfID] {
			all = append(all, a.logs[i])
		}
	}
	total := int64(len(all))
	if offset >= len(all) {
		return nil, total, nil
	}
	all = all[offset:]
	if len(all) > limit {
		all = all[:limit]
	}
	return all, total, nil
}

func (a *alertRepoFake) DeleteAlertData(id int64) error {
	if a.down {
		return ErrAlertNotReady
	}
	delete(a.rows, id)
	var keep []AlertLog
	for _, l := range a.logs {
		if l.CnfID != id {
			keep = append(keep, l)
		}
	}
	a.logs = keep
	return nil
}

// newAlertEnv 返回带告警存储的测试环境（与 e.repo 共用同一份配置 / 通道数据）。
func newAlertEnv(t *testing.T) (*env, *alertRepoFake) {
	t.Helper()
	e := newEnv()
	ar := newAlertRepo(e.repo)
	e.svc = NewService(ar, e.src, e.access, e.tgt.dialer())
	e.svc.now = func() time.Time { return fixedNow }
	return e, ar
}

// aheadTable 返回一张“还剩 n 个可用分区”的按天分区表：先有一个已过去的分区，再有 n 个上界在今天之后的（含当前）。
func aheadTable(now time.Time, n int) *TableState {
	d := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, cst)
	bs := []string{d.Format("2006-01-02")}
	for i := 1; i <= n; i++ {
		bs = append(bs, d.AddDate(0, 0, i).Format("2006-01-02"))
	}
	return dailyTable(bs...)
}

func intp(n int) *int { return &n }

func timep(t time.Time) *time.Time { return &t }

// ---- 可用分区数 ----

func TestAvailability(t *testing.T) {
	cfg := Config{ColType: ColDatetime, Timezone: "Asia/Shanghai"}
	withMax := dailyTable("2026-10-07", "2026-10-08")
	withMax.Partitions = append(withMax.Partitions, rp("pmax", "MAXVALUE"))
	listed := okTable()
	listed.Method = "LIST"
	wrongType := okTable()
	wrongType.ColumnType = "int"
	badBound := okTable()
	badBound.Partitions[1].Desc = "'garbage'"
	sub := okTable()
	sub.Subpartitioned = true
	doris := &TableState{Exists: true, IsBase: true, ColumnType: "datetime", Method: "RANGE", Dialect: DialectDoris}
	for _, d := range []int{5, 6, 7, 8} { // 上界 10-06 / 10-07 / 10-08 / 10-09：现在 10-06 12:00，之后的有 3 个
		lo, hi := time.Date(2026, 10, d, 0, 0, 0, 0, cst), time.Date(2026, 10, d+1, 0, 0, 0, 0, cst)
		doris.Partitions = append(doris.Partitions, rp("p"+lo.Format("20060102"), dorisRange("DATETIME", lo.Format(dtLayout), hi.Format(dtLayout))))
	}
	dorisBad := &TableState{Exists: true, IsBase: true, ColumnType: "datetime", Method: "RANGE", Dialect: DialectDoris, Partitions: []RawPartition{rp("p1", "garbage")}}

	cases := []struct {
		name    string
		cfg     Config
		st      *TableState
		known   bool
		count   int
		max     bool
		endDate string
	}{
		{"维护正常：当前 + 3 个预创建", cfg, okTable(), true, 4, false, "2026-10-10"},
		{"只剩 2 个", cfg, dailyTable("2026-10-06", "2026-10-07", "2026-10-08"), true, 2, false, "2026-10-08"},
		{"已用尽（上界都已过去）", cfg, dailyTable("2026-10-05", "2026-10-06"), true, 0, false, ""},
		{"MAXVALUE 不计入，但要标出", cfg, withMax, true, 2, true, "2026-10-08"},
		{"没有分区", cfg, &TableState{Exists: true, IsBase: true}, false, 0, false, ""},
		{"表不存在", cfg, &TableState{}, false, 0, false, ""},
		{"LIST 分区不适用", cfg, listed, false, 0, false, ""},
		{"带子分区不适用", cfg, sub, false, 0, false, ""},
		{"字段类型与配置对不上", cfg, wrongType, false, 0, false, ""},
		{"边界解析不了", cfg, badBound, false, 0, false, ""},
		{"时区留空又没读到会话时区", Config{ColType: ColDatetime}, okTable(), false, 0, false, ""},
		{"Doris：按 Range 的上界数", cfg, doris, true, 3, false, "2026-10-09"},
		{"Doris：读不到上界就不判断", cfg, dorisBad, false, 0, false, ""},
	}
	for _, c := range cases {
		ai := availability(c.cfg, *c.st, fixedNow)
		if ai.Known != c.known || ai.Count != c.count || ai.MaxValue != c.max {
			t.Errorf("%s: %+v", c.name, ai)
			continue
		}
		if c.endDate != "" && ai.End.In(cst).Format("2006-01-02") != c.endDate {
			t.Errorf("%s: 预计用尽 %v，期望 %s", c.name, ai.End, c.endDate)
		}
		if c.endDate == "" && !ai.End.IsZero() {
			t.Errorf("%s: 没有可用分区时不应有预计用尽时间: %v", c.name, ai.End)
		}
	}
	// 时区留空时用目标库的会话偏移
	st := okTable()
	st.SessionOffset, st.HasSessionOffset = 8*3600, true
	if ai := availability(Config{ColType: ColDatetime}, *st, fixedNow); !ai.Known || ai.Count != 4 {
		t.Errorf("跟随目标库时区: %+v", ai)
	}
}

func TestAlertLevelOf(t *testing.T) {
	on := AlertSettings{Enabled: true, MinAvailable: 3, RepeatHours: 24, NotifyRecover: true}
	future, past := fixedNow.Add(time.Hour), fixedNow.Add(-time.Hour)
	cases := []struct {
		name  string
		set   AlertSettings
		per   AlertSetting
		avail *int
		level string
		thr   int
	}{
		{"不足", on, AlertSetting{}, intp(2), levelAlert, 3},
		{"等于阈值不告警", on, AlertSetting{}, intp(3), levelOK, 3},
		{"已用尽", on, AlertSetting{}, intp(0), levelAlert, 3},
		{"本表阈值更大", on, AlertSetting{Threshold: intp(12)}, intp(11), levelAlert, 12},
		{"本表阈值更小", on, AlertSetting{Threshold: intp(2)}, intp(2), levelOK, 2},
		{"本表阈值 0 不告警", on, AlertSetting{Threshold: intp(0)}, intp(0), levelOff, 0},
		{"全局关闭", AlertSettings{MinAvailable: 3}, AlertSetting{}, intp(0), levelOff, 3},
		{"无法判断", on, AlertSetting{}, nil, levelNA, 3},
		{"静默中", on, AlertSetting{SilenceUntil: timep(future)}, intp(1), levelSilent, 3},
		{"静默已过期", on, AlertSetting{SilenceUntil: timep(past)}, intp(1), levelAlert, 3},
		{"静默期内但已恢复", on, AlertSetting{SilenceUntil: timep(future)}, intp(4), levelOK, 3},
	}
	for _, c := range cases {
		if lv, thr := alertLevelOf(c.set, c.per, c.avail, fixedNow); lv != c.level || thr != c.thr {
			t.Errorf("%s: %s/%d，期望 %s/%d", c.name, lv, thr, c.level, c.thr)
		}
	}
}

func TestAlertSettingsDefaultsAndRanges(t *testing.T) {
	e := newEnv()
	if got := e.svc.alertSettings(); got != (AlertSettings{Enabled: true, MinAvailable: 3, RepeatHours: 24, NotifyRecover: true}) {
		t.Fatalf("没配置时取默认值: %+v", got)
	}
	e.src.cfg[cfgAlertEnabled], e.src.cfg[cfgAlertMin], e.src.cfg[cfgAlertRepeat], e.src.cfg[cfgAlertRecover] = "false", "5", "0", "false"
	if got := e.svc.alertSettings(); got != (AlertSettings{Enabled: false, MinAvailable: 5, RepeatHours: 0, NotifyRecover: false}) {
		t.Fatalf("按配置: %+v", got)
	}
	for _, bad := range []string{"0", "101", "abc", ""} { // 阈值要在 1~100，非法回落到默认
		e.src.cfg[cfgAlertMin] = bad
		if got := e.svc.alertSettings().MinAvailable; got != 3 {
			t.Errorf("阈值 %q 应回落到 3: %d", bad, got)
		}
	}
	for _, bad := range []string{"-1", "721", "x"} { // 重复间隔要在 0~720
		e.src.cfg[cfgAlertRepeat] = bad
		if got := e.svc.alertSettings().RepeatHours; got != 24 {
			t.Errorf("重复间隔 %q 应回落到 24: %d", bad, got)
		}
	}
}

// ---- 状态里带可用分区数 ----

func TestStatusCarriesAvailability(t *testing.T) {
	e := newEnv()
	cfg := e.addConfig(goodInput())
	e.tgt.tables["log.access_log"] = dailyTable("2026-10-06", "2026-10-07", "2026-10-08")
	v, err := e.svc.Status(ctx, root, cfg.ID, true)
	if err != nil || v.Available == nil || *v.Available != 2 || v.AvailableEnd != "2026-10-08 00:00:00" || v.AvailableSecs != 36*3600 || v.HasMaxValue {
		t.Fatalf("可用分区: %+v %v", v, err)
	}
	// 计划失败（边界顺序不对）的表照样数得出来，状态仍是异常
	bad := dailyTable("2026-10-08", "2026-10-07", "2026-10-09")
	e.tgt.tables["log.access_log"] = bad
	if v, err = e.svc.Status(ctx, root, cfg.ID, true); err != nil || v.State != StateError || v.Available == nil || *v.Available != 3 {
		t.Fatalf("计划失败时仍应有可用分区数: %+v %v", v, err)
	}
	// 已用尽：Available 是 0（不是 nil）
	e.tgt.tables["log.access_log"] = dailyTable("2026-10-05", "2026-10-06")
	if v, _ = e.svc.Status(ctx, root, cfg.ID, true); v.Available == nil || *v.Available != 0 || v.AvailableEnd != "" {
		t.Fatalf("已用尽: %+v", v)
	}
	b, _ := json.Marshal(v)
	if !strings.Contains(string(b), `"available":0`) {
		t.Fatalf("0 也要输出: %s", b)
	}
	// 读不到目标表：没有 available
	e.tgt.inspectEr = errAny("i/o timeout")
	if v, _ = e.svc.Status(ctx, root, cfg.ID, true); v.State != StateUnknown || v.Available != nil {
		t.Fatalf("读不到时不应有可用分区数: %+v", v)
	}
}

// ---- 每张表的设置 ----

func TestAlertThresholdAndSilence(t *testing.T) {
	e, ar := newAlertEnv(t)
	cfg := e.addConfig(goodInput())
	viewer, editor := user(permMenu), user(permMenu, permChange)
	if _, err := e.svc.SetAlertThreshold(viewer, cfg.ID, intp(3)); code(err) != "forbidden" {
		t.Fatalf("没有编辑权限: %v", err)
	}
	if _, err := e.svc.SilenceAlert(viewer, cfg.ID, 24); code(err) != "forbidden" {
		t.Fatalf("没有编辑权限: %v", err)
	}
	for _, bad := range []int{-1, 101} {
		if _, err := e.svc.SetAlertThreshold(editor, cfg.ID, intp(bad)); code(err) != "bad_request" {
			t.Errorf("阈值 %d 应拒绝: %v", bad, err)
		}
	}
	v, err := e.svc.SetAlertThreshold(editor, cfg.ID, intp(5))
	if err != nil || v.Threshold == nil || *v.Threshold != 5 || ar.rows[cfg.CnfID].Threshold == nil {
		t.Fatalf("设置阈值: %+v %v", v, err)
	}
	if v, err = e.svc.SetAlertThreshold(editor, cfg.ID, intp(0)); err != nil || v.Threshold == nil || *v.Threshold != 0 {
		t.Fatalf("0 = 不告警要原样保存: %+v %v", v, err)
	}
	if v, err = e.svc.SetAlertThreshold(editor, cfg.ID, nil); err != nil || v.Threshold != nil {
		t.Fatalf("nil = 跟随全局: %+v %v", v, err)
	}
	if v, err = e.svc.SilenceAlert(editor, cfg.ID, 24); err != nil || v.SilenceUntil != fixedNow.Add(24*time.Hour).Format(timeLayout) {
		t.Fatalf("静默 24 小时: %+v %v", v, err)
	}
	if v, err = e.svc.SilenceAlert(editor, cfg.ID, 0); err != nil || v.SilenceUntil != "" {
		t.Fatalf("取消静默: %+v %v", v, err)
	}
	for _, bad := range []int{-1, maxSilenceHours + 1} {
		if _, err := e.svc.SilenceAlert(editor, cfg.ID, bad); code(err) != "bad_request" {
			t.Errorf("静默 %d 小时应拒绝: %v", bad, err)
		}
	}
	if _, err := e.svc.SetAlertThreshold(editor, 999, intp(3)); code(err) != "not_found" {
		t.Errorf("配置不存在: %v", err)
	}
	acts := strings.Join(e.repo.auditActions(), ",")
	if !strings.Contains(acts, "分区维护-告警阈值") || !strings.Contains(acts, "分区维护-告警静默") {
		t.Errorf("审计: %s", acts)
	}
	ar.down = true // 扩展表没建：给出执行 280 脚本的提示
	if _, err := e.svc.SetAlertThreshold(editor, cfg.ID, intp(3)); code(err) != "alert_table_missing" {
		t.Errorf("扩展表没建: %v", err)
	}
	// 没有 AlertRepo 的仓库同样如此
	plain := newEnv()
	c2 := plain.addConfig(goodInput())
	if _, err := plain.svc.SilenceAlert(editor, c2.ID, 1); code(err) != "alert_table_missing" {
		t.Errorf("仓库不支持: %v", err)
	}
}

func TestAlertListOverviewAndDelete(t *testing.T) {
	e, ar := newAlertEnv(t)
	c1 := e.addConfig(goodInput())
	in2 := goodInput()
	in2.TbName = "events"
	c2 := e.addConfig(in2)
	ar.rows[c1.CnfID] = AlertSetting{CnfID: c1.CnfID, Threshold: intp(2), SilenceUntil: timep(fixedNow.Add(time.Hour))}
	ar.rows[c2.CnfID] = AlertSetting{CnfID: c2.CnfID, SilenceUntil: timep(fixedNow.Add(-time.Hour))} // 已过期的静默不显示
	rows, _, err := e.svc.List(root, ListQuery{})
	if err != nil || len(rows) != 2 {
		t.Fatalf("%v %v", rows, err)
	}
	if rows[0].AlertThreshold == nil || *rows[0].AlertThreshold != 2 || rows[0].AlertSilenceUntil == "" {
		t.Errorf("第一行应带阈值和静默期: %+v", rows[0])
	}
	if rows[1].AlertThreshold != nil || rows[1].AlertSilenceUntil != "" {
		t.Errorf("第二行没有单独设置: %+v", rows[1])
	}
	sm, err := e.svc.Summary(root) // KPI 按汇总里每项自己的阈值 / 静默状态判定“可用分区不足”
	if err != nil || len(sm.Items) != 2 || sm.Items[0].AlertThreshold == nil || *sm.Items[0].AlertThreshold != 2 || !sm.Items[0].AlertSilenced ||
		sm.Items[1].AlertThreshold != nil || sm.Items[1].AlertSilenced {
		t.Errorf("汇总: %+v %v", sm, err)
	}
	ov, err := e.svc.Overview(root)
	if err != nil || !ov.Alert.Ready || !ov.Alert.Enabled || ov.Alert.MinAvailable != 3 || ov.Alert.MaxThreshold != maxAlertThreshold {
		t.Fatalf("概览: %+v %v", ov.Alert, err)
	}
	ar.down = true // 扩展表没建：列表照常，概览标明没就绪
	if rows, _, err = e.svc.List(root, ListQuery{}); err != nil || len(rows) != 2 || rows[0].AlertThreshold != nil {
		t.Errorf("扩展表没建时列表照常: %+v %v", rows, err)
	}
	if ov, _ = e.svc.Overview(root); ov.Alert.Ready {
		t.Errorf("扩展表没建时 ready 应为 false")
	}
	ar.down = false
	ar.logs = []AlertLog{{CnfID: c1.CnfID, Kind: AlertFirst}}
	if err := e.svc.Delete(root, c1.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := ar.rows[c1.CnfID]; ok || len(ar.logs) != 0 {
		t.Errorf("删除配置要一并清理告警数据: %+v %+v", ar.rows, ar.logs)
	}
	if _, ok := ar.rows[c2.CnfID]; !ok {
		t.Errorf("别的配置的数据不能被清掉")
	}
}

// ---- 后台检查与推送 ----

func alertEnvWithHook(t *testing.T) (*env, *alertRepoFake, *hookServer, *Config) {
	t.Helper()
	e, ar := newAlertEnv(t)
	srv := newHookServer(t, 200, replyFor(chDingtalk))
	e.addHook(chDingtalk, srv, nil)
	cfg := e.addConfig(goodInput()) // 实例 1 · log.access_log
	return e, ar, srv, cfg
}

func hookText(h *hookServer) string {
	txt, _ := h.last().Body["text"].(map[string]any)
	s, _ := txt["content"].(string)
	return s
}

func TestRunAlertsDedupeRepeatAndRecover(t *testing.T) {
	e, ar, srv, cfg := alertEnvWithHook(t)
	now := fixedNow
	e.svc.now = func() time.Time { return now }
	e.tgt.tables["log.access_log"] = aheadTable(now, 2) // 可用 2 < 阈值 3

	sum := e.svc.RunAlerts(ctx)
	if !sum.Enabled || sum.Checked != 1 || sum.Alerted != 1 || srv.count() != 1 {
		t.Fatalf("首次不足推一次: %+v 推送 %d", sum, srv.count())
	}
	txt := hookText(srv)
	for _, want := range []string{"【分区告警】可用分区数不足", "实例：prod-mysql-01（线上日志）", "库表：log.access_log", "可用分区：2 个（阈值 3）", "预计用尽：2026-10-08 00:00:00", "维护开关：已开启", "待续建 2"} {
		if !strings.Contains(txt, want) {
			t.Errorf("消息缺少 %q:\n%s", want, txt)
		}
	}
	st := ar.rows[cfg.CnfID]
	if !st.Alerting || st.LastAlertAt == nil || !st.LastAlertAt.Equal(now) {
		t.Fatalf("状态应记下已告警与推送时间: %+v", st)
	}
	if len(ar.logs) != 1 || ar.logs[0].Kind != AlertFirst || !ar.logs[0].OK || ar.logs[0].Available != 2 || ar.logs[0].Threshold != 3 || ar.logs[0].Channels != chDingtalk {
		t.Fatalf("告警记录: %+v", ar.logs)
	}

	now = now.Add(3 * time.Hour) // 间隔内：不重复推
	e.tgt.tables["log.access_log"] = aheadTable(now, 2)
	if sum = e.svc.RunAlerts(ctx); sum.Repeated != 0 || sum.Alerted != 0 || srv.count() != 1 {
		t.Fatalf("间隔内不应重复推: %+v 推送 %d", sum, srv.count())
	}

	now = now.Add(22 * time.Hour) // 距上次推送 25 小时 ≥ 24：重复提醒
	e.tgt.tables["log.access_log"] = aheadTable(now, 1)
	if sum = e.svc.RunAlerts(ctx); sum.Repeated != 1 || srv.count() != 2 || !strings.Contains(hookText(srv), "【分区告警·持续】") || !strings.Contains(hookText(srv), "可用分区：1 个") {
		t.Fatalf("应重复提醒: %+v 推送 %d\n%s", sum, srv.count(), hookText(srv))
	}
	if ar.logs[1].Kind != AlertRepeat {
		t.Fatalf("记录类型: %+v", ar.logs)
	}

	e.src.cfg[cfgAlertRepeat] = "0" // 重复间隔 0 = 不重复
	now = now.Add(72 * time.Hour)
	e.tgt.tables["log.access_log"] = aheadTable(now, 1)
	if sum = e.svc.RunAlerts(ctx); sum.Repeated != 0 || srv.count() != 2 {
		t.Fatalf("间隔 0 不重复: %+v 推送 %d", sum, srv.count())
	}
	delete(e.src.cfg, cfgAlertRepeat)

	e.tgt.tables["log.access_log"] = aheadTable(now, 4) // 续建之后回到阈值以上：推“已恢复”
	if sum = e.svc.RunAlerts(ctx); sum.Recovered != 1 || srv.count() != 3 || !strings.Contains(hookText(srv), "【分区告警·已恢复】") || strings.Contains(hookText(srv), "预计用尽") {
		t.Fatalf("应推已恢复: %+v 推送 %d\n%s", sum, srv.count(), hookText(srv))
	}
	if ar.rows[cfg.CnfID].Alerting || ar.logs[2].Kind != AlertRecover {
		t.Fatalf("恢复后状态复位: %+v %+v", ar.rows[cfg.CnfID], ar.logs)
	}
	if sum = e.svc.RunAlerts(ctx); srv.count() != 3 || sum.Recovered != 0 {
		t.Fatalf("恢复之后不再推: %+v", sum)
	}

	e.tgt.tables["log.access_log"] = aheadTable(now, 2) // 再次不足：重新算“首次”
	if sum = e.svc.RunAlerts(ctx); sum.Alerted != 1 || srv.count() != 4 {
		t.Fatalf("再次不足应重新推: %+v 推送 %d", sum, srv.count())
	}
}

func TestRunAlertsSilentOffDisabledAndUnknown(t *testing.T) {
	e, ar, srv, cfg := alertEnvWithHook(t)
	e.tgt.tables["log.access_log"] = aheadTable(fixedNow, 1)

	ar.rows[cfg.CnfID] = AlertSetting{CnfID: cfg.CnfID, SilenceUntil: timep(fixedNow.Add(time.Hour))} // 静默中
	if e.svc.RunAlerts(ctx); srv.count() != 0 || ar.rows[cfg.CnfID].Alerting {
		t.Fatalf("静默期内不推: %d", srv.count())
	}
	ar.rows[cfg.CnfID] = AlertSetting{CnfID: cfg.CnfID, Threshold: intp(0), Alerting: true} // 本表阈值 0：不推，也不推“已恢复”，状态复位
	if e.svc.RunAlerts(ctx); srv.count() != 0 || ar.rows[cfg.CnfID].Alerting {
		t.Fatalf("阈值 0 不告警: %d %+v", srv.count(), ar.rows[cfg.CnfID])
	}
	ar.rows[cfg.CnfID] = AlertSetting{CnfID: cfg.CnfID, Threshold: intp(1)} // 阈值 1：可用 1 不小于 1，正常
	if e.svc.RunAlerts(ctx); srv.count() != 0 {
		t.Fatalf("等于阈值不告警: %d", srv.count())
	}
	delete(ar.rows, cfg.CnfID)

	e.src.cfg[cfgAlertEnabled] = "false" // 全局关闭：不检查
	if sum := e.svc.RunAlerts(ctx); sum.Enabled || sum.Checked != 0 || srv.count() != 0 {
		t.Fatalf("全局关闭: %+v", sum)
	}
	delete(e.src.cfg, cfgAlertEnabled)

	e.tgt.inspectEr = errAny("i/o timeout") // 读不到目标表：无法判断，不告警，状态不动
	ar.rows[cfg.CnfID] = AlertSetting{CnfID: cfg.CnfID, Alerting: true}
	if e.svc.RunAlerts(ctx); srv.count() != 0 || !ar.rows[cfg.CnfID].Alerting {
		t.Fatalf("读不到时不动: %d %+v", srv.count(), ar.rows[cfg.CnfID])
	}
	e.tgt.inspectEr = nil
	listed := okTable() // 不是 RANGE 分区：同样无法判断
	listed.Method = "LIST"
	e.tgt.tables["log.access_log"] = listed
	if e.svc.RunAlerts(ctx); srv.count() != 0 {
		t.Fatalf("LIST 分区不告警: %d", srv.count())
	}
}

func TestRunAlertsRecoverSwitchAndLock(t *testing.T) {
	e, ar, srv, cfg := alertEnvWithHook(t)
	e.tgt.tables["log.access_log"] = aheadTable(fixedNow, 4)
	ar.rows[cfg.CnfID] = AlertSetting{CnfID: cfg.CnfID, Alerting: true, LastAlertAt: timep(fixedNow.Add(-time.Hour))}
	e.src.cfg[cfgAlertRecover] = "false" // 不推“已恢复”，但状态要复位
	if sum := e.svc.RunAlerts(ctx); sum.Recovered != 0 || srv.count() != 0 || ar.rows[cfg.CnfID].Alerting {
		t.Fatalf("关了恢复通知: %+v %d %+v", sum, srv.count(), ar.rows[cfg.CnfID])
	}
	e.tgt.tables["log.access_log"] = aheadTable(fixedNow, 1)
	e.repo.lockBusy[alertLockName] = true // 另一个进程正在检查：本进程什么都不做
	if sum := e.svc.RunAlerts(ctx); sum.Checked != 0 || srv.count() != 0 {
		t.Fatalf("锁被占用: %+v", sum)
	}
	delete(e.repo.lockBusy, alertLockName)
	ar.down = true // 扩展表没建：不检查也不报错
	if sum := e.svc.RunAlerts(ctx); sum.Checked != 0 || srv.count() != 0 {
		t.Fatalf("扩展表没建: %+v", sum)
	}
	plain := newEnv() // 仓库不支持告警
	if sum := plain.svc.RunAlerts(ctx); sum.Checked != 0 {
		t.Fatalf("仓库不支持: %+v", sum)
	}
}

func TestRunAlertsChannelsMissingAndFailing(t *testing.T) {
	e, ar := newAlertEnv(t)
	cfg := e.addConfig(goodInput())
	e.tgt.tables["log.access_log"] = aheadTable(fixedNow, 1)

	sum := e.svc.RunAlerts(ctx) // 没有启用的通道：不记告警、不改状态，等有了通道再推
	if len(sum.Notes) != 1 || len(ar.logs) != 0 || ar.rows[cfg.CnfID].Alerting {
		t.Fatalf("没有通道: %+v %+v", sum, ar.logs)
	}

	srv := newHookServer(t, 500, `{"errcode":310000}`) // 通道在，但推送失败：记一条失败记录，状态不动，下一轮重试
	e.addHook(chDingtalk, srv, nil)
	if sum = e.svc.RunAlerts(ctx); sum.Failed != 1 || sum.Alerted != 0 || len(ar.logs) != 1 || ar.logs[0].OK || ar.logs[0].Message == "" || ar.rows[cfg.CnfID].Alerting {
		t.Fatalf("推送失败: %+v %+v", sum, ar.logs)
	}
	if strings.Contains(ar.logs[0].Message, secretToken) {
		t.Fatalf("失败原因里的 token 要脱敏: %s", ar.logs[0].Message)
	}
	srv.mu.Lock()
	srv.status, srv.reply = 200, replyFor(chDingtalk)
	srv.mu.Unlock()
	if sum = e.svc.RunAlerts(ctx); sum.Alerted != 1 || len(ar.logs) != 2 || !ar.logs[1].OK || !ar.rows[cfg.CnfID].Alerting {
		t.Fatalf("通道恢复后应补推: %+v %+v", sum, ar.logs)
	}
}

func TestAlertMessageBranches(t *testing.T) {
	cfg := &Config{DBName: "log", TbName: "access_log", Env: ""}
	inst := &model.Instance{InstanceName: "prod-mysql-01"}
	cases := []struct {
		name  string
		kind  string
		cfg   *Config
		v     StatusView
		avail int
		want  []string
		deny  []string
	}{
		{"已用尽，没有兜底分区", AlertFirst, cfg, StatusView{}, 0, []string{"已用尽，新数据写入会报错", "维护开关：已关闭（不会自动续建）"}, []string{"（线上）"}},
		{"已用尽，有 MAXVALUE", AlertFirst, cfg, StatusView{HasMaxValue: true}, 0, []string{"落入 MAXVALUE 分区", "REORGANIZE"}, nil},
		{"异常状态带原因", AlertRepeat, cfg, StatusView{State: StateError, Message: "边界不连续", Available: intp(2), AvailableEnd: "2026-10-08 00:00:00", AvailableSecs: 90000}, 2, []string{"当前状态：异常：边界不连续", "约 1 天 1 小时后"}, nil},
	}
	for _, c := range cases {
		_, body := alertMessage(c.kind, c.cfg, inst, c.v, c.avail, 3, false, fixedNow)
		for _, w := range c.want {
			if !strings.Contains(body, w) {
				t.Errorf("%s: 缺少 %q:\n%s", c.name, w, body)
			}
		}
		for _, d := range c.deny {
			if strings.Contains(body, d) {
				t.Errorf("%s: 不应出现 %q:\n%s", c.name, d, body)
			}
		}
	}
	on := *cfg
	on.IsEnable = true
	if _, body := alertMessage(AlertFirst, &on, inst, StatusView{}, 0, 3, true, fixedNow); !strings.Contains(body, "维护开关：已开启") || !strings.Contains(body, "本表单独设置") {
		t.Errorf("开启状态与单独阈值:\n%s", body)
	}
	for sec, want := range map[int64]string{0: "0 分钟", 59: "0 分钟", 3600: "1 小时", 5400: "1 小时 30 分钟", 86400: "1 天", 90000: "1 天 1 小时", 20 * 86400: "20 天", -5: "0 分钟"} {
		if got := humanSecs(sec); got != want {
			t.Errorf("humanSecs(%d) = %q，期望 %q", sec, got, want)
		}
	}
}

// ---- HTTP ----

func TestAlertHTTP(t *testing.T) {
	e, ar := newAlertEnv(t)
	web := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){
		func(s *server.Server, rt server.Routes) { register(s, rt, e.svc) },
	}})
	web.Repo.Perms[2] = map[string]bool{permMenu: true}
	cfg := e.addConfig(goodInput())
	e.tgt.tables["log.access_log"] = dailyTable("2026-10-06", "2026-10-07", "2026-10-08")
	rt := web.Login("root")
	bob := web.Login("bob")
	path := "/api/v1/partman/configs/" + strconv.Itoa(int(cfg.ID))

	if code, r := call(t, bob, "PUT", path+"/alert", map[string]any{"threshold": 5}); code != 403 {
		t.Fatalf("没有编辑权限应 403: %d %+v", code, r)
	}
	if code, _ := call(t, bob, "POST", path+"/alert/silence", map[string]any{"hours": 1}); code != 403 {
		t.Fatalf("静默需要编辑权限: %d", code)
	}
	code, r := call(t, rt, "PUT", path+"/alert", map[string]any{"threshold": 5})
	var v struct {
		Threshold *int   `json:"alert_threshold"`
		Silence   string `json:"alert_silence_until"`
	}
	_ = json.Unmarshal(r.Data, &v)
	if code != 200 || v.Threshold == nil || *v.Threshold != 5 {
		t.Fatalf("设置阈值: %d %+v", code, r)
	}
	if code, _ = call(t, rt, "PUT", path+"/alert", map[string]any{"threshold": 101}); code != 400 {
		t.Fatalf("越界应 400: %d", code)
	}
	if code, r = call(t, rt, "PUT", path+"/alert", map[string]any{"threshold": nil}); code != 200 || !strings.Contains(string(r.Data), `"alert_threshold":null`) {
		t.Fatalf("null 跟随全局: %d %s", code, r.Data)
	}
	if code, r = call(t, rt, "POST", path+"/alert/silence", map[string]any{"hours": 24}); code != 200 || !strings.Contains(string(r.Data), `"alert_silence_until":"2026-10-07 12:00:00"`) {
		t.Fatalf("静默: %d %s", code, r.Data)
	}
	if code, _ = call(t, rt, "POST", path+"/alert/silence", map[string]any{"hours": maxSilenceHours + 1}); code != 400 {
		t.Fatalf("静默越界应 400: %d", code)
	}
	// 列表带阈值 / 静默，概览带 alert，状态带可用分区数
	code, r = call(t, rt, "GET", "/api/v1/partman/configs", nil)
	if code != 200 || !strings.Contains(string(r.Data), `"alert_silence_until":"2026-10-07 12:00:00"`) || !strings.Contains(string(r.Data), `"alert_threshold":null`) {
		t.Fatalf("列表: %d %s", code, r.Data)
	}
	if code, r = call(t, rt, "GET", "/api/v1/partman/overview", nil); code != 200 || !strings.Contains(string(r.Data), `"alert":{"ready":true,"max_threshold":100,"max_silence_hours":2160,"enabled":true,"min_available":3,"repeat_hours":24,"notify_recover":true}`) {
		t.Fatalf("概览: %d %s", code, r.Data)
	}
	if code, r = call(t, rt, "GET", path+"/status?refresh=1", nil); code != 200 || !strings.Contains(string(r.Data), `"available":2`) || !strings.Contains(string(r.Data), `"available_end":"2026-10-08 00:00:00"`) {
		t.Fatalf("状态: %d %s", code, r.Data)
	}
	// 告警记录：新的在前，带库表名与实例名
	ar.logs = []AlertLog{{CnfID: cfg.CnfID, Kind: AlertFirst, Available: 2, Threshold: 3, Channels: chDingtalk, OK: true, CreateTime: fixedNow},
		{CnfID: cfg.CnfID, Kind: AlertRecover, Available: 4, Threshold: 3, Channels: chDingtalk, OK: true, CreateTime: fixedNow.Add(time.Hour)}}
	code, r = call(t, rt, "GET", "/api/v1/partman/alerts?limit=1", nil)
	var rows []AlertLogView
	_ = json.Unmarshal(r.Data, &rows)
	if code != 200 || r.Total != 2 || len(rows) != 1 || rows[0].Kind != AlertRecover || rows[0].DBName != "log" || rows[0].TbName != "access_log" || rows[0].Instance != "prod-mysql-01" {
		t.Fatalf("告警记录: %d %+v %+v", code, r, rows)
	}
	ar.down = true
	if code, r = call(t, rt, "GET", "/api/v1/partman/alerts", nil); code != 503 || r.Code != "alert_table_missing" {
		t.Fatalf("扩展表没建: %d %+v", code, r)
	}
}
