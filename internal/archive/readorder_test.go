// @author: kevinkelly
// @license: Apache Licence
// @file: readorder_test.go
// @time: 2026/10/28

package archive

import (
	"strings"
	"testing"

	"dbtool/internal/archive/engine"
	"dbtool/internal/notify"
)

// 归档并删除不再使用断点：升级前遗留的旧断点被清掉；单次上限中断后下一次运行直接接着取剩下的行。
func TestMoveTaskDoesNotKeepCheckpointAndResumesByDeletion(t *testing.T) {
	e := newEnv(t)
	in := e.baseInput("不用断点")
	in.Params.MaxRows = 100 // 300 行的表，单次只处理 100 行
	in = e.checked(e.rd, in)
	r, err := e.svc.CreateTask(e.rd, in)
	if err != nil {
		t.Fatal(err)
	}
	id := r.Task.ID
	if _, err := e.svc.Approve(e.tl, id, "同意"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Approve(e.dba, id, "同意"); err != nil {
		t.Fatal(err)
	}
	stale, _ := e.repo.GetTask(id, false)
	stale.Checkpoint = `["250"]` // 升级前遗留的旧断点：不再有用，且必须清掉
	if err := e.repo.SaveTask(stale); err != nil {
		t.Fatal(err)
	}

	run1 := e.runNow(id)
	e.wait()
	d1, _ := e.svc.GetRun(e.dba, run1)
	if d1.Result != engine.ResultLimitReached || d1.Deleted != 100 || e.h.Src.Len() != 200 {
		t.Fatalf("第 1 次应到单次上限就停：%+v 剩 %d", d1.RunView, e.h.Src.Len())
	}
	if cur, _ := e.repo.GetTask(id, false); cur.Checkpoint != "" {
		t.Errorf("没有断点，旧断点要清掉：%q", cur.Checkpoint)
	}
	if tv, _ := e.svc.GetTask(e.dba, id); len(tv.Checkpoint) != 0 {
		t.Errorf("任务视图不应有断点：%v", tv.Checkpoint)
	}
	// 读取不排序、不带断点（旧断点 250 若还在用，会漏掉前面的行）
	for _, rq := range e.h.Src.ReadReqs {
		if !rq.Unordered || len(rq.After) != 0 {
			t.Fatalf("读取请求应不排序、没有断点：%+v", rq)
		}
	}
	// 第 2 次接着取剩下的行（已删除的不会再被读到）
	run2 := e.runNow(id)
	e.wait()
	d2, _ := e.svc.GetRun(e.dba, run2)
	if d2.Result != engine.ResultLimitReached || d2.Deleted != 100 || e.h.Src.Len() != 100 {
		t.Fatalf("第 2 次：%+v 剩 %d", d2.RunView, e.h.Src.Len())
	}
	// 恢复范围按实际处理的最小 / 最大主键记录，恢复仍可用
	if len(d2.MinPK) != 1 || len(d2.MaxPK) != 1 || d2.MinPK[0] != "101" || d2.MaxPK[0] != "200" {
		t.Errorf("第 2 次的主键范围：%v ~ %v", d2.MinPK, d2.MaxPK)
	}
	if !d2.CanRestore {
		t.Error("应可恢复")
	}
	// 预览不再有 ORDER BY 与断点
	tv, _ := e.svc.GetTask(e.dba, id)
	if tv.SQL == nil || strings.Contains(tv.SQL.Select, "ORDER BY") || !strings.Contains(tv.SQL.Select, "不排序") {
		t.Errorf("任务详情里的 SQL：%+v", tv.SQL)
	}
}

// 仅备份不删数据，仍靠断点翻页：断点照常保存。
func TestCopyTaskKeepsCheckpoint(t *testing.T) {
	e := newEnv(t)
	in := e.baseInput("仅备份断点")
	in.Mode = "copy"
	in.Params.MaxRows = 100
	in = e.checked(e.rd, in)
	r, err := e.svc.CreateTask(e.rd, in)
	if err != nil {
		t.Fatal(err)
	}
	id := r.Task.ID
	if _, err := e.svc.Approve(e.tl, id, "同意"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Approve(e.dba, id, "同意"); err != nil {
		t.Fatal(err)
	}
	e.runNow(id)
	e.wait()
	cur, _ := e.repo.GetTask(id, false)
	if cur.Checkpoint != `["100"]` || e.h.Src.Len() != 300 {
		t.Errorf("仅备份应保留断点：%q 源表 %d 行", cur.Checkpoint, e.h.Src.Len())
	}
	if first := e.h.Src.ReadReqs[0]; first.Unordered {
		t.Errorf("仅备份应按主键排序读取：%+v", first)
	}
}

// 通知正文里的“来源”要写实例名称，不能只有一个 #ID；查不到名称时才退回 #ID。
func TestNotificationShowsInstanceName(t *testing.T) {
	e := newEnv(t)
	task := e.enabled("通知里的实例名")
	e.runNow(task.ID)
	e.wait()
	e.notif.mu.Lock()
	evs := append([]notify.Event(nil), e.notif.evs...)
	e.notif.mu.Unlock()
	if len(evs) == 0 {
		t.Fatal("应有审批 / 运行通知")
	}
	for _, ev := range evs {
		if !strings.Contains(ev.Content, "任务：通知里的实例名\n来源：mysql-prod-m1\n库表：shop.orders\n模式：") || strings.Contains(ev.Content, "实例 #") {
			t.Errorf("通知 %s 的来源应写实例名称：\n%s", ev.Name, ev.Content)
		}
	}
	// 实例不在平台里（被删除等）时退回 #ID，不至于空着
	missing := &Task{Name: "t", DBName: "d", TblName: "x", InstanceID: 999, Mode: "move"}
	if got := taskContent(missing, e.svc.instNameOf(999)); !strings.Contains(got, "来源：实例 #999\n库表：d.x\n") {
		t.Errorf("查不到实例名称时：%s", got)
	}
}
