// @author: kevinkelly
// @license: Apache Licence
// @file: multiengine_test.go
// @time: 2026/10/28

package privapply_test

import (
	"context"
	"testing"

	"dbtool/internal/model"
	"dbtool/internal/privapply"
)

// PostgreSQL / MongoDB / ClickHouse / Oracle / 达梦实例：出现在申请 / 直接授予的实例列表里，库级与表级权限都可以申请（不同于 Redis）。
// PostgreSQL 的表名是 schema.table。
func TestMultiEngineInstancesSupportDBAndTableLevel(t *testing.T) {
	s, r, _ := setup()
	for id, u := range r.Users { // 直接授予要求被授予人是启用的用户
		u.IsActive = true
		r.Users[id] = u
	}
	cases := []struct {
		id     uint
		name   string
		dbType string
		db     string
		table  string
	}{
		{5, "pg-01", "pgsql", "shop", "public.orders"},
		{6, "mongo-01", "mongo", "app", "users"},
		{7, "ch-01", "clickhouse", "events_db", "events"},
		{8, "ora-01", "oracle", "HR", "EMPLOYEES"},
		{9, "dm-01", "dm", "SALES", "ORDERS"},
	}
	for _, c := range cases {
		r.Instances = append(r.Instances, model.Instance{ID: c.id, InstanceName: c.name, DBType: c.dbType, Tags: []model.InstanceTag{{TagCode: "can_read", Active: true}}})
		r.InstRGs[c.id] = []uint{1}
	}
	insts, err := s.ApplyInstances(dev)
	if err != nil {
		t.Fatal(err)
	}
	found := map[uint]bool{}
	for _, i := range insts {
		found[i.ID] = true
	}
	for _, c := range cases {
		if !found[c.id] {
			t.Errorf("%s 应出现在可申请的实例里", c.dbType)
		}
	}
	for _, c := range cases {
		tb := privapply.CreateReq{InstanceID: c.id, GroupName: "研发组", PrivType: 2, DBName: c.db, TableList: []string{c.table}, ValidDate: tomorrow(), LimitNum: 100}
		ap, err := s.Create(context.Background(), dev, tb)
		if err != nil || ap.TableList != c.table || ap.DBList != c.db {
			t.Errorf("%s 表级权限申请: %v %+v", c.dbType, err, ap)
		}
		db := privapply.CreateReq{InstanceID: c.id, GroupName: "研发组", PrivType: 1, DBList: []string{c.db + "_2"}, ValidDate: tomorrow(), LimitNum: 100}
		if ap, err = s.Create(context.Background(), dev, db); err != nil || ap.DBList != c.db+"_2" {
			t.Errorf("%s 库级权限申请: %v %+v", c.dbType, err, ap)
		}
		if _, err := s.GrantPrivilege(context.Background(), root, privapply.GrantReq{UserName: "dev", InstanceID: c.id, PrivType: 2, DBName: c.db, TableList: []string{c.table}, ValidDate: tomorrow(), LimitNum: 10, Reason: "测试授予权限"}); err != nil {
			t.Errorf("%s 直接授予表权限: %v", c.dbType, err)
		}
	}
}
