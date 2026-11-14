// @author: kevinkelly
// @license: Apache Licence
// @file: createtable_flow_test.go
// @time: 2026/10/31

package sqlflow_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/goinception"
	"dbtool/internal/model"
	"dbtool/internal/review"
	"dbtool/internal/sqlflow"
)

const dorisDDL = "CREATE TABLE IF NOT EXISTS t_order (id BIGINT, name VARCHAR(64)) ENGINE=OLAP UNIQUE KEY(id) DISTRIBUTED BY HASH(id) BUCKETS 8 PROPERTIES ('replication_num' = '1')"

// PolarDB-X / SelectDB / ADB / Doris 的 CREATE TABLE 工单：检查只做语法检查（不碰 goInception），执行永远是原生方式。
func TestCreateTableWorkflowIsNativeForEngineTypes(t *testing.T) {
	for _, typ := range []string{"polardb-x", "polardbx", "selectdb", "doris", "adb"} {
		e := setup(t)
		e.repo.Instances[0].DBType = typ
		e.useInception() // goInception 已配置：这些建表工单也不走它

		chk, err := e.svc.Preview(context.Background(), dev, 1, "shop", dorisDDL, false)
		if err != nil {
			t.Fatalf("%s 检查: %v", typ, err)
		}
		if chk.Source != sqlflow.SourceCreateTable || chk.ErrorCount != 0 || chk.Notice == "" || chk.SyntaxType != sqlflow.SyntaxDDL {
			t.Fatalf("%s 检查结果: source=%s errors=%d notice=%q syntax=%d", typ, chk.Source, chk.ErrorCount, chk.Notice, chk.SyntaxType)
		}
		id := e.submit(t, dev, dorisDDL)
		e.approve(t, id)
		if opts, def := e.svc.ExecutorOptions(&model.SqlWorkflow{}); def != sqlflow.ExecGoInception || len(opts) != 2 {
			t.Fatalf("其他工单不受影响：%v %q", opts, def)
		}
		// 明确要求用 goInception 会被拒绝；不指定 / 选原生才行
		if _, err := e.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto, Executor: sqlflow.ExecGoInception}); code(err) != "bad_request" || e.status(id) != model.WfReviewPass {
			t.Fatalf("%s 选 goInception 应被拒绝：%v %s", typ, err, e.status(id))
		}
		if _, err := e.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
			t.Fatalf("%s 执行: %v", typ, err)
		}
		if e.status(id) != model.WfFinish || len(e.eng.ran) != 1 || !strings.Contains(e.eng.ran[0], "CREATE TABLE") {
			t.Errorf("%s 应原生执行：%s ran=%v", typ, e.status(id), e.eng.ran)
		}
		if len(e.inc.calls) != 0 {
			t.Errorf("%s 不应调用 goInception：%v", typ, e.inc.calls)
		}
	}
}

func TestCreateTableSyntaxErrorBlocksSubmit(t *testing.T) {
	e := setup(t)
	e.repo.Instances[0].DBType = "doris"
	chk, err := e.svc.Preview(context.Background(), dev, 1, "shop", "create table t (id int", false)
	if err != nil || chk.ErrorCount != 1 || !strings.Contains(chk.Rows[0].ErrorMessage, "缺少右括号") || chk.Rows[0].StageStatus != "驳回语法错误" {
		t.Fatalf("语法错误应报在行上：%+v %v", chk.Rows, err)
	}
}

// 混了别的语句 / 其他类型的实例：仍按原来的方式（goInception）。
func TestCreateTableScopeLeavesOtherWorkflowsAlone(t *testing.T) {
	e := setup(t)
	e.repo.Instances[0].DBType = "doris"
	e.useInception()
	e.inc.checkOut = inceptionRows("create table t (id int)", "alter table t add c int")
	chk, err := e.svc.Preview(context.Background(), dev, 1, "shop", "create table t (id int); alter table t add c int", false)
	if err != nil || chk.Source != sqlflow.SourceGoInception || len(e.inc.calls) != 1 {
		t.Fatalf("混了 ALTER 仍走 goInception：%+v %v calls=%v", chk, err, e.inc.calls)
	}
	e2 := setup(t)
	e2.useInception()
	e2.inc.checkOut = inceptionRows("create table t (id int)")
	chk, err = e2.svc.Preview(context.Background(), dev, 1, "shop", "create table t (id int)", false)
	if err != nil || chk.Source != sqlflow.SourceGoInception {
		t.Fatalf("MySQL 建表仍走 goInception：%+v %v", chk, err)
	}
}

// 强制使用 goInception（REQUIRE_GOINCEPTION）时，这些建表工单照样能检查、能原生执行。
func TestCreateTableIgnoresRequireInception(t *testing.T) {
	e := setup(t)
	e.repo.Instances[0].DBType = "selectdb"
	s := sqlflow.NewService(e.repo, sqlflow.Options{Notifier: e.cap, RequireInception: true, Open: func(string, engine.Conn) (engine.Engine, error) {
		return &fakeEngine{state: e.eng}, nil
	}})
	s.SetAsync(func(f func()) { f() })
	s.SetSpawn(func(f func()) { f() })
	s.SetClock(func() time.Time { return e.now })
	s.SetInception(func(goinception.Config) sqlflow.Inception { return e.inc }, func(goinception.BackupConfig) sqlflow.BackupReader { return fakeBackup{} })
	e.svc = s
	e.useInception()
	id := e.submit(t, dev, dorisDDL)
	e.approve(t, id)
	if _, err := s.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	if e.status(id) != model.WfFinish || len(e.eng.ran) != 1 {
		t.Errorf("强制 goInception 时建表工单仍应原生执行：%s ran=%v", e.status(id), e.eng.ran)
	}
}

// 定时执行：到点原生执行，到点前可以多次改时间（重新设定覆盖旧计划）。
func TestCreateTableScheduleCanBeChangedRepeatedly(t *testing.T) {
	e := setup(t)
	e.repo.Instances[0].DBType = "doris"
	e.useInception()
	id := e.submit(t, dev, dorisDDL)
	e.approve(t, id)
	first, second, third := e.now.Add(time.Hour), e.now.Add(3*time.Hour), e.now.Add(2*time.Hour)
	for _, at := range []time.Time{first, second, third} {
		if _, err := e.svc.Schedule(dev, id, at, ""); err != nil {
			t.Fatalf("设定 %s: %v", at.Format("15:04"), err)
		}
		if sc := e.repo.Schedules[id]; e.status(id) != model.WfTimingTask || !sc.RunAt.Equal(at) || sc.Executor != sqlflow.ExecNative {
			t.Fatalf("定时计划应覆盖成 %s 并记原生执行：%s %+v", at.Format("15:04"), e.status(id), sc)
		}
	}
	// 旧数据：提交时设定的计划里记的执行方式可能是 goInception，到点也照样原生执行
	sc := e.repo.Schedules[id]
	sc.Executor = sqlflow.ExecGoInception
	e.repo.Schedules[id] = sc
	e.svc.SetClock(func() time.Time { return third.Add(time.Second) })
	if n := e.svc.RunDue(); n != 1 || e.status(id) != model.WfFinish || len(e.eng.ran) != 1 || len(e.inc.calls) != 0 {
		t.Fatalf("到点原生执行：n=%d %s ran=%v inc=%v", n, e.status(id), e.eng.ran, e.inc.calls)
	}
	rows, _ := review.ParseRows(e.repo.Contents[id].ExecuteResult)
	if len(rows) != 1 || rows[0].StageStatus != "Execute Successfully" {
		t.Errorf("执行结果: %+v", rows)
	}
}
