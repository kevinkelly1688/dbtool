// @author: kevinkelly
// @license: Apache Licence
// @file: undecryptable_test.go
// @time: 2026/10/28

package store

import (
	"reflect"
	"testing"

	"dbtool/internal/crypto"
	"dbtool/internal/model"
)

// 实例的账号或密码是密文、但当前密钥解不开：列出实例名（去重、升序）；明文和解得开的不列。
func TestUndecryptableInstanceNames(t *testing.T) {
	cur, err := crypto.NewMirage("current-key-for-tests-0123456789abcdefghij", "ECB", "")
	if err != nil {
		t.Fatal(err)
	}
	old, err := crypto.NewMirage("another-old-key-for-tests-0123456789abcdefgh", "ECB", "")
	if err != nil {
		t.Fatal(err)
	}
	rows := []model.Instance{
		{InstanceName: "ok", User: cur.Encrypt("u"), Password: cur.Encrypt("p")},
		{InstanceName: "plain", User: "root", Password: "short"}, // 明文：读取时兼容
		{InstanceName: "zzz-imported", User: old.Encrypt("u"), Password: old.Encrypt("p")},
		{InstanceName: "aaa-pw-only", User: cur.Encrypt("u"), Password: old.Encrypt("p")},
		{InstanceName: "zzz-imported", User: old.Encrypt("u2"), Password: old.Encrypt("p2")}, // 重名只列一次
		{InstanceName: "empty"},
	}
	got := undecryptableInstanceNames(cur, rows)
	want := []string{"aaa-pw-only", "zzz-imported"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
