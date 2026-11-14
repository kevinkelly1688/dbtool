// @author: kevinkelly
// @license: Apache Licence
// @file: batch_test.go
// @time: 2026/10/28

package instmgr

import (
	"strings"
	"testing"
)

// 批量停用 / 启用：逐个走单个实例的路径；已经是目标状态的算成功（无需操作）；看不到的实例单独报错。
func TestBatchDisableAndEnable(t *testing.T) {
	e, x := newExtEnv()
	res, err := e.svc.BatchDisable(root, []uint{1, 2, 2, 99}, true) // 重复 id 去重；99 不存在
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 3 || res.Done != 2 || res.Failed != 1 {
		t.Fatalf("应处理 3 个（去重后），成功 2 失败 1: %+v", res)
	}
	if !x.exts[1].Disabled || !x.exts[2].Disabled {
		t.Fatalf("1、2 号应已停用: %+v", x.exts)
	}
	if !strings.Contains(res.Items[2].Message, "不存在") {
		t.Errorf("不存在的实例应报原因: %+v", res.Items[2])
	}
	if a := e.repo.lastAudit(); a.Action != "批量停用实例" || !strings.Contains(a.ExtraInfo, "prod-mysql-01") {
		t.Errorf("审计: %+v", a)
	}
	// 再停用一次：已经是停用状态，算成功并提示无需操作
	res, _ = e.svc.BatchDisable(root, []uint{1}, true)
	if res.Done != 1 || !strings.Contains(res.Items[0].Message, "无需操作") {
		t.Errorf("已停用的应跳过: %+v", res)
	}
	// 启用
	res, _ = e.svc.BatchDisable(root, []uint{1, 2}, false)
	if res.Done != 2 || x.exts[1].Disabled || x.exts[2].Disabled {
		t.Errorf("批量启用: %+v %+v", res, x.exts)
	}
	// 没有权限看到的实例（bob 的资源组没关联 2 号）
	res, _ = e.svc.BatchDisable(bob, []uint{2}, true)
	if res.Failed != 1 || !strings.Contains(res.Items[0].Message, "未关联") {
		t.Errorf("越权的实例不能被批量操作: %+v", res)
	}
	// 参数校验
	_, err = e.svc.BatchDisable(root, nil, true)
	wantErr(t, err, 400, "请先选择")
	many := make([]uint, maxBatch+1)
	for i := range many {
		many[i] = uint(i + 1)
	}
	_, err = e.svc.BatchDisable(root, many, true)
	wantErr(t, err, 400, "最多")
	// 旁路表没建：整批拒绝
	x.instReady = false
	_, err = e.svc.BatchDisable(root, []uint{1}, true)
	wantErr(t, err, 409, "070_instance_ext.sql")
}

// 批量设置环境：逐个检查可见范围；已经是目标环境的算成功（无需操作）；看不到的实例单独报错；旁路表没建整批拒绝。
func TestBatchSetEnv(t *testing.T) {
	e, x := newExtEnv()
	res, err := e.svc.BatchSetEnv(root, []uint{1, 2, 2, 99}, "prod") // 重复 id 去重；99 不存在
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 3 || res.Done != 2 || res.Failed != 1 {
		t.Fatalf("应处理 3 个（去重后），成功 2 失败 1: %+v", res)
	}
	if x.exts[1].Env != "prod" || x.exts[2].Env != "prod" {
		t.Fatalf("1、2 号应为生产: %+v", x.exts)
	}
	if !strings.Contains(res.Items[2].Message, "不存在") {
		t.Errorf("不存在的实例应报原因: %+v", res.Items[2])
	}
	if a := e.repo.lastAudit(); a.Action != "批量设置环境" || !strings.Contains(a.ExtraInfo, "生产") || !strings.Contains(a.ExtraInfo, "prod-mysql-01") {
		t.Errorf("审计: %+v", a)
	}
	// 再设一次：已经是目标环境，算成功并提示无需操作
	res, _ = e.svc.BatchSetEnv(root, []uint{1}, "prod")
	if res.Done != 1 || !strings.Contains(res.Items[0].Message, "无需操作") {
		t.Errorf("已是目标环境的应跳过: %+v", res)
	}
	// 改成测试、再清除（未设置）
	if res, _ = e.svc.BatchSetEnv(root, []uint{1, 2}, "test"); res.Done != 2 || x.exts[1].Env != "test" {
		t.Errorf("批量设为测试: %+v %+v", res, x.exts)
	}
	if res, _ = e.svc.BatchSetEnv(root, []uint{1, 2}, ""); res.Done != 2 || x.exts[1].Env != "" || x.exts[2].Env != "" {
		t.Errorf("批量清除环境: %+v %+v", res, x.exts)
	}
	// 没有权限看到的实例（bob 的资源组没关联 2 号）
	res, _ = e.svc.BatchSetEnv(bob, []uint{2}, "prod")
	if res.Failed != 1 || x.exts[2].Env != "" {
		t.Errorf("越权的实例不能被批量设置: %+v %+v", res, x.exts)
	}
	// 参数校验
	_, err = e.svc.BatchSetEnv(root, nil, "prod")
	wantErr(t, err, 400, "请先选择")
	_, err = e.svc.BatchSetEnv(root, []uint{1}, "staging")
	wantErr(t, err, 400, "环境")
	// 旁路表没建：整批拒绝
	x.instReady = false
	_, err = e.svc.BatchSetEnv(root, []uint{1}, "prod")
	wantErr(t, err, 409, "070_instance_ext.sql")
}

// 批量开启 / 关闭慢日志采集：逐个走单个实例的路径；已经是目标状态的算成功；只支持在线查询的类型报错且不写开关；
// 看不到的实例单独报错；与实例级停用互不影响。
func TestBatchCollect(t *testing.T) {
	e, x := newExtEnv()
	e.repo.addInstance(6, "pg-01", "pgsql")
	e.repo.instances[2].IsEnable = true // 2 号本来就在采集

	res, err := e.svc.BatchCollect(root, []uint{1, 2, 2, 6, 99}, true) // 重复 id 去重；6 号只支持在线查询；99 不存在
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 4 || res.Done != 2 || res.Failed != 2 {
		t.Fatalf("应处理 4 个（去重后），成功 2（1 开启、2 已开启）失败 2: %+v", res)
	}
	if !e.repo.instances[1].IsEnable || !e.repo.instances[2].IsEnable || e.repo.instances[6].IsEnable {
		t.Fatalf("1、2 应在采集，6 不应被写开关: %v %v %v", e.repo.instances[1].IsEnable, e.repo.instances[2].IsEnable, e.repo.instances[6].IsEnable)
	}
	if !strings.Contains(res.Items[1].Message, "无需操作") {
		t.Errorf("已开启的应跳过: %+v", res.Items[1])
	}
	if !strings.Contains(res.Items[2].Message, "暂不支持") || !strings.Contains(res.Items[3].Message, "不存在") {
		t.Errorf("不支持的类型 / 不存在的实例应说明原因: %+v", res.Items)
	}
	if a := e.repo.lastAudit(); a.Action != "批量开启慢日志采集" || !strings.Contains(a.ExtraInfo, "prod-mysql-01") {
		t.Errorf("应写批量审计: %+v", a)
	}
	if x.exts[1].Disabled {
		t.Errorf("采集开关不应改动实例级停用状态: %+v", x.exts)
	}

	// 关闭
	res, _ = e.svc.BatchCollect(root, []uint{1, 2}, false)
	if res.Done != 2 || e.repo.instances[1].IsEnable || e.repo.instances[2].IsEnable {
		t.Errorf("批量关闭采集: %+v", res)
	}
	if e.repo.lastAudit().Action != "批量关闭慢日志采集" {
		t.Errorf("审计: %+v", e.repo.lastAudit())
	}
	// 再关一次：已经是关闭状态
	res, _ = e.svc.BatchCollect(root, []uint{1}, false)
	if res.Done != 1 || !strings.Contains(res.Items[0].Message, "无需操作") {
		t.Errorf("已关闭的应跳过: %+v", res)
	}
	// 没有权限看到的实例（bob 的资源组没关联 2 号）
	res, _ = e.svc.BatchCollect(bob, []uint{2}, true)
	if res.Failed != 1 || !strings.Contains(res.Items[0].Message, "未关联") || e.repo.instances[2].IsEnable {
		t.Errorf("越权的实例不能被批量改采集开关: %+v", res)
	}
	// 参数校验
	_, err = e.svc.BatchCollect(root, nil, true)
	wantErr(t, err, 400, "请先选择")
}

// 批量删除：先 dry_run 预检（不删任何东西）；真正删除要 confirm；被引用的删不掉并说明原因，其余照常删除。
func TestBatchDelete(t *testing.T) {
	e, _ := newExtEnv()
	e.repo.refs[2] = []Ref{{Label: "SQL 工单", Count: 3, Blocking: true}, {Label: "脱敏字段配置", Count: 1}}

	pre, err := e.svc.BatchDelete(root, []uint{1, 2}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !pre.DryRun || pre.Done != 1 || pre.Failed != 1 || len(e.repo.deleted) != 0 {
		t.Fatalf("dry_run 不应删除任何东西: %+v deleted=%v", pre, e.repo.deleted)
	}
	if !pre.Items[0].OK || pre.Items[1].OK || !strings.Contains(pre.Items[1].Message, "SQL 工单 3 条") {
		t.Errorf("预检结果: %+v", pre.Items)
	}
	if strings.Contains(pre.Items[1].Message, "脱敏字段配置") {
		t.Errorf("非阻断的引用不该算原因: %q", pre.Items[1].Message)
	}

	// 没确认
	_, err = e.svc.BatchDelete(root, []uint{1}, false, false)
	wantErr(t, err, 400, "确认")
	if len(e.repo.deleted) != 0 {
		t.Fatal("没确认不应删除")
	}

	// 真正删除：1 号能删，2 号被引用
	res, err := e.svc.BatchDelete(root, []uint{1, 2}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Done != 1 || res.Failed != 1 || len(e.repo.deleted) != 1 || e.repo.deleted[0] != 1 {
		t.Fatalf("只应删除 1 号: %+v deleted=%v", res, e.repo.deleted)
	}
	if !strings.Contains(res.Items[1].Message, "仍被引用") {
		t.Errorf("被引用的应说明原因: %+v", res.Items[1])
	}
	if a := e.repo.lastAudit(); a.Action != "批量删除实例" {
		t.Errorf("应写批量审计: %+v", a)
	}
	// 越权
	res, _ = e.svc.BatchDelete(bob, []uint{2}, true, false)
	if res.Failed != 1 || len(e.repo.deleted) != 1 || !strings.Contains(res.Items[0].Message, "未关联") {
		t.Errorf("越权的实例不能被批量删除: %+v", res)
	}
}
