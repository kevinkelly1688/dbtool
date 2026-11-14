// @author: kevinkelly
// @license: Apache Licence
// @file: spill_test.go
// @time: 2026/10/28

package binlog

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func spillFilesOf(t *testing.T, svc *Service) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(svc.spillDir, "*.spill"))
	return m
}

// 结果超过转存阈值就放磁盘：分页、下载的内容和全在内存时完全一样（回滚结果倒序），结果清理后临时文件被删掉。
func TestSpilledResultMatchesInMemoryResult(t *testing.T) {
	run := func(spillAt int) (*Service, *JobView, []Stmt, string) {
		svc, _, _ := newSvc(t, newFakeSource(scenario(), t))
		svc.now = func() time.Time { return fixedNow }
		svc.spillAt = spillAt
		v, err := svc.Start(context.Background(), root, req(ModeRollback))
		if err != nil {
			t.Fatal(err)
		}
		done := waitDone(t, svc, root, v.ID)
		if done.Status != StatusDone || done.Matched != 3 {
			t.Fatalf("spillAt=%d: %+v", spillAt, done)
		}
		page, err := svc.Rows(root, v.ID, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, body, err := svc.Download(root, v.ID)
		if err != nil {
			t.Fatal(err)
		}
		return svc, done, page.Rows, string(body)
	}
	memSvc, _, memRows, memBody := run(spillThreshold)
	if n := len(spillFilesOf(t, memSvc)); n != 0 {
		t.Fatalf("小结果不该落盘: %d 个临时文件", n)
	}
	diskSvc, done, diskRows, diskBody := run(300) // 第 2 条语句之后就超过阈值
	files := spillFilesOf(t, diskSvc)
	if len(files) != 1 {
		t.Fatalf("大结果应落盘: %v", files)
	}
	if fi, _ := os.Stat(files[0]); fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("结果临时文件含业务数据，权限应是 0600: %v", fi.Mode())
	}
	if diskBody != memBody {
		t.Errorf("落盘与内存的下载文件应逐字一致:\n%s\n----\n%s", diskBody, memBody)
	}
	if len(diskRows) != len(memRows) {
		t.Fatalf("分页条数: %d vs %d", len(diskRows), len(memRows))
	}
	for i := range memRows {
		if diskRows[i] != memRows[i] || diskRows[i].Seq != i+1 {
			t.Errorf("第 %d 条: %+v vs %+v", i, diskRows[i], memRows[i])
		}
	}
	// 分页取中间一段（倒序）
	p, _ := diskSvc.Rows(root, done.ID, 1, 1)
	if p.Total != 3 || len(p.Rows) != 1 || p.Rows[0] != memRows[1] {
		t.Errorf("分页: %+v", p)
	}

	// 结果过期后清理：临时文件删掉，再读返回“已过期”
	diskSvc.now = func() time.Time { return fixedNow.Add(jobTTL + time.Minute) }
	diskSvc.gc()
	if n := len(spillFilesOf(t, diskSvc)); n != 0 {
		t.Errorf("过期清理后临时文件应被删掉: %d", n)
	}
	if _, err := diskSvc.Rows(root, done.ID, 0, 0); err == nil {
		t.Error("过期的任务读不到结果")
	}
}

// 取消 / 失败的任务不保存结果：已经写出去的临时文件也要删掉。
func TestCanceledJobLeavesNoSpillFile(t *testing.T) {
	src := newFakeSource(scenario(), t)
	src.block = make(chan struct{}) // 读完事件后一直等，让任务停在“运行中”
	svc, _, _ := newSvc(t, src)
	svc.spillAt = 300
	v, err := svc.Start(context.Background(), root, req(ModeSQL))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(spillFilesOf(t, svc)) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(spillFilesOf(t, svc)) != 1 {
		t.Fatal("运行中的任务应已经把结果写到临时文件")
	}
	if err := svc.Cancel(root, v.ID); err != nil {
		t.Fatal(err)
	}
	waitDone(t, svc, root, v.ID)
	deadline = time.Now().Add(5 * time.Second)
	for len(spillFilesOf(t, svc)) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(spillFilesOf(t, svc)); n != 0 {
		t.Errorf("取消后临时文件应被删掉: %d", n)
	}
}

// 进程被杀时遗留的临时文件：启动时清掉超过两小时的，更新的可能属于别的正在运行的进程，不动。
func TestCleanSpillRemovesOnlyOrphans(t *testing.T) {
	svc, _, _ := newSvc(t, newFakeSource(scenario(), t))
	svc.now = func() time.Time { return fixedNow }
	mk := func(name string, age time.Duration) string {
		p := filepath.Join(svc.spillDir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, fixedNow.Add(-age), fixedNow.Add(-age)); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old, recent, other := mk("binlog-1.spill", 3*time.Hour), mk("binlog-2.spill", 10*time.Minute), mk("keep.txt", 5*time.Hour)
	svc.CleanSpill()
	for p, wantGone := range map[string]bool{old: true, recent: false, other: false} {
		_, err := os.Stat(p)
		if gone := os.IsNotExist(err); gone != wantGone {
			t.Errorf("%s: 已删除=%v，期望 %v", filepath.Base(p), gone, wantGone)
		}
	}
}
