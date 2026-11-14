// @author: kevinkelly
// @license: Apache Licence
// @file: audit_events_test.go
// @time: 2026/10/28

package server

import (
	"strconv"
	"testing"
	"time"
)

// 去重表：窗口内只放行一次，窗口过后再放行；条目数有上限，不会无限增长。
func TestDenyDedupWindowAndCap(t *testing.T) {
	var d denyDedup
	t0 := time.Date(2026, 10, 9, 10, 0, 0, 0, time.Local)
	if !d.first("a", t0) || d.first("a", t0.Add(59*time.Second)) {
		t.Fatal("窗口内第二次不应放行")
	}
	if !d.first("b", t0.Add(10*time.Second)) {
		t.Fatal("不同 key 各自放行")
	}
	if !d.first("a", t0.Add(61*time.Second)) {
		t.Fatal("窗口过后应再放行")
	}
	// 上限：塞满之后继续塞，表的大小不超过上限
	now := t0.Add(10 * time.Minute)
	for i := 0; i < denyMaxKey*3; i++ {
		d.first("k"+strconv.Itoa(i), now)
		if len(d.seen) > denyMaxKey {
			t.Fatalf("去重表超过上限: %d", len(d.seen))
		}
	}
	// 过期的先被清掉，而不是整体清空
	d2 := denyDedup{}
	for i := 0; i < denyMaxKey; i++ {
		d2.first("old"+strconv.Itoa(i), t0)
	}
	if !d2.first("fresh", t0.Add(2*time.Minute)) {
		t.Fatal("过期条目满了之后新 key 应放行")
	}
	if len(d2.seen) != 1 {
		t.Errorf("过期条目应被清理: %d", len(d2.seen))
	}
}
