// @author: kevinkelly
// @license: Apache Licence
// @file: redis_test.go
// @time: 2026/10/28

package privapply_test

import (
	"context"
	"strings"
	"testing"

	"dbtool/internal/model"
	"dbtool/internal/privapply"
)

// Redis 实例：能出现在申请 / 直接授予的实例列表里（SupportsQuery），但只能申请库（db 编号）级权限，不能申请表权限。
func TestRedisInstancePrivilegeRules(t *testing.T) {
	s, r, _ := setup()
	r.Instances = append(r.Instances, model.Instance{ID: 4, InstanceName: "redis-01", DBType: "redis", Tags: []model.InstanceTag{{TagCode: "can_read", Active: true}}})
	r.InstRGs[4] = []uint{1}

	insts, err := s.ApplyInstances(dev)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, i := range insts {
		found = found || i.ID == 4
	}
	if !found {
		t.Errorf("Redis 实例应出现在可申请的实例里: %+v", insts)
	}

	tableOnRedis := privapply.CreateReq{InstanceID: 4, GroupName: "研发组", PrivType: 2, DBName: "0", TableList: []string{"x"}, ValidDate: tomorrow(), LimitNum: 100}
	if _, err := s.Create(context.Background(), dev, tableOnRedis); err == nil || !strings.Contains(err.Error(), "Redis 没有表") {
		t.Errorf("Redis 不能申请表权限: %v", err)
	}
	dbOnRedis := privapply.CreateReq{InstanceID: 4, GroupName: "研发组", PrivType: 1, DBList: []string{"0", "3"}, ValidDate: tomorrow(), LimitNum: 100}
	ap, err := s.Create(context.Background(), dev, dbOnRedis)
	if err != nil || ap.DBList != "0,3" {
		t.Fatalf("Redis 库权限申请: %v %+v", err, ap)
	}
	// 直接授予同样只能库级
	if _, err := s.GrantPrivilege(context.Background(), root, privapply.GrantReq{UserName: "dev", InstanceID: 4, PrivType: 2, DBName: "0", TableList: []string{"x"}, ValidDate: tomorrow(), LimitNum: 10, Reason: "测试授予权限"}); err == nil || !strings.Contains(err.Error(), "Redis 没有表") {
		t.Errorf("Redis 直接授予表权限应拒绝: %v", err)
	}
}
