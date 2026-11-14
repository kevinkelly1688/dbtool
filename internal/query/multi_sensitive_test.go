// @author: kevinkelly
// @license: Apache Licence
// @file: multi_sensitive_test.go
// @time: 2026/10/28

package query

import (
	"context"
	"reflect"
	"testing"

	"dbtool/internal/settings"
)

// 非 MySQL 类型的库表树也要标出命中脱敏规则的字段（表名旁的 🛡 数字），和 MySQL 一致。

func sensOf(m *DBMeta) map[string][]string {
	out := map[string][]string{}
	for _, tb := range m.Tables {
		out[tb.Name] = tb.Sensitive
	}
	return out
}

func TestMultiDBMetaSensitive(t *testing.T) {
	ctx := context.Background()
	t.Run("PostgreSQL：规则点名的表有标记，其它表是空数组", func(t *testing.T) {
		e := newMultiEnv(t)
		e.repo.cfg[settings.KeyQueryMaskEnabled] = "true"
		e.repo.maskCols = append(e.repo.maskCols, keepCfg("shop", "public.orders", "name", 1, 0), keepCfg("other", "public.orders", "id", 1, 0))
		m, err := e.svc.DBMeta(ctx, dev, pgID, "shop")
		if err != nil {
			t.Fatal(err)
		}
		want := map[string][]string{"public.orders": {"name"}, "public.v_orders": {}, "reporting.daily": {}}
		if got := sensOf(m); !reflect.DeepEqual(got, want) || m.SensitiveTotal != 1 || m.Unmasked {
			t.Errorf("sens=%v total=%d unmasked=%v", got, m.SensitiveTotal, m.Unmasked)
		}
	})
	t.Run("PostgreSQL：表名只写了不带 schema 的名字也能命中", func(t *testing.T) {
		e := newMultiEnv(t)
		e.repo.cfg[settings.KeyQueryMaskEnabled] = "true"
		e.repo.maskCols = append(e.repo.maskCols, keepCfg("shop", "orders", "id", 1, 0))
		m, _ := e.svc.DBMeta(ctx, dev, pgID, "shop")
		if got := sensOf(m)["public.orders"]; !reflect.DeepEqual(got, []string{"id"}) {
			t.Errorf("sens=%v", got)
		}
	})
	t.Run("库表都是 * 的通配规则：每张表按字段名命中", func(t *testing.T) {
		e := newMultiEnv(t)
		e.repo.cfg[settings.KeyQueryMaskEnabled] = "true"
		e.repo.maskCols = append(e.repo.maskCols, keepCfg("*", "*", "id", 1, 0))
		m, _ := e.svc.DBMeta(ctx, dev, pgID, "shop")
		want := map[string][]string{"public.orders": {"id"}, "public.v_orders": {"id"}, "reporting.daily": {"id"}}
		if got := sensOf(m); !reflect.DeepEqual(got, want) || m.SensitiveTotal != 3 {
			t.Errorf("sens=%v total=%d", got, m.SensitiveTotal)
		}
	})
	t.Run("MongoDB：按集合采样出的字段命中", func(t *testing.T) {
		e := newMultiEnv(t)
		e.repo.cfg[settings.KeyQueryMaskEnabled] = "true"
		e.repo.maskCols = append(e.repo.maskCols, keepCfg("app", "users", "name", 1, 0))
		m, err := e.svc.DBMeta(ctx, admin, mongoID, "app")
		if err != nil {
			t.Fatal(err)
		}
		want := map[string][]string{"users": {"name"}, "orders": {}}
		if got := sensOf(m); !reflect.DeepEqual(got, want) || m.SensitiveTotal != 1 {
			t.Errorf("sens=%v total=%d", got, m.SensitiveTotal)
		}
	})
	t.Run("ClickHouse", func(t *testing.T) {
		e := newMultiEnv(t)
		e.repo.cfg[settings.KeyQueryMaskEnabled] = "true"
		e.repo.maskCols = append(e.repo.maskCols, keepCfg("app", "events", "dt", 1, 0))
		m, err := e.svc.DBMeta(ctx, admin, chID, "app")
		if err != nil {
			t.Fatal(err)
		}
		if got := sensOf(m)["events"]; !reflect.DeepEqual(got, []string{"dt"}) || m.SensitiveTotal != 1 {
			t.Errorf("sens=%v total=%d", sensOf(m), m.SensitiveTotal)
		}
	})
	t.Run("系统脱敏总开关关闭：不标", func(t *testing.T) {
		e := newMultiEnv(t)
		e.repo.maskCols = append(e.repo.maskCols, keepCfg("shop", "public.orders", "name", 1, 0))
		m, _ := e.svc.DBMeta(ctx, dev, pgID, "shop")
		if m.SensitiveTotal != 0 || !m.Unmasked {
			t.Errorf("total=%d unmasked=%v", m.SensitiveTotal, m.Unmasked)
		}
	})
	t.Run("没有任何规则：不逐表取列", func(t *testing.T) {
		e := newMultiEnv(t)
		e.repo.cfg[settings.KeyQueryMaskEnabled] = "true"
		m, _ := e.svc.DBMeta(ctx, dev, pgID, "shop")
		if m.SensitiveTotal != 0 {
			t.Errorf("total=%d", m.SensitiveTotal)
		}
		if e.pg.Has("attidentity") {
			t.Errorf("没有规则时不该逐表取列: %v", e.pg.Statements())
		}
	})
}
