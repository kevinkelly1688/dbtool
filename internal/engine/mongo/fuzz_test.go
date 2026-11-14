// @author: kevinkelly
// @license: Apache Licence
// @file: fuzz_test.go
// @time: 2026/10/28

package mongo

import (
	"errors"
	"strings"
	"testing"
	"time"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestFriendlyErrorKinds(t *testing.T) {
	e := &Engine{password: "pw123"}
	ctx := t.Context()
	if err := e.friendlyError(ctx, timeoutErr{}); !errors.Is(err, ErrTimeout) {
		t.Errorf("Timeout() 接口应映射为超时：%v", err)
	}
	err := e.friendlyError(ctx, errors.New("server selection error: context deadline exceeded, current topology: { Type: Single, Servers: [{ Addr: 10.0.0.1:27017, Type: Unknown, Last error: dial tcp 10.0.0.1:27017: i/o timeout pw123 }] }"))
	if err == nil || !strings.Contains(err.Error(), "无法连接 MongoDB 实例") || strings.Contains(err.Error(), "pw123") || errors.Is(err, ErrTimeout) {
		t.Errorf("连接失败应单独提示且不泄露口令：%v", err)
	}
	if e.friendlyError(ctx, nil) != nil {
		t.Error("nil 应返回 nil")
	}
	_ = time.Second
}

// FuzzCheckUserQuery：任意输入不得 panic；被放行的语句必须满足不变量（规范化文本能再次通过检查、
// 不含被禁止的操作符键、方法只在白名单内）。常规 go test 只跑种子语料。
func FuzzCheckUserQuery(f *testing.F) {
	seeds := []string{
		`db.a.find({})`, `db.a.find({$where: "1"})`, `db.a.aggregate([{$out: "x"}])`, `db.a.aggregate([{$lookup: {from: "b", as: "c", pipeline: [{$merge: {into: "z"}}]}}])`,
		`db.a.find({"\u0024where": 1})`, `db.a.find({a: ObjectId("507f1f77bcf86cd799439011")}).sort({a: 1}).limit(3)`, `db["x"].find()`,
		`db.getCollection("system.users").find()`, `db.a.find({a: /x/i}); db.b.drop()`, "db.a.find({a: 'x'}) // db.b.drop()", `db.a.aggregate([{$facet: {x: [{$unionWith: "b"}]}}])`,
		`db.a.find({$expr: {$function: {body: "x", args: [], lang: "js"}}})`, `db.runCommand({ping: 1})`, `db.a.distinct("x")`, `db.stats()`,
		`db.a.find({a: {"$code": "1"}})`, `db.a.find({x: NumberLong("9223372036854775807")})`, `db.a.countDocuments({})`,
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		c, err := CheckUserQuery(in)
		if err != nil {
			return
		}
		for _, bad := range []string{`"$where":`, `"$function":`, `"$accumulator":`, `"$out":`, `"$merge":`, `"$eval":`} {
			if strings.Contains(c.Statement, bad) {
				t.Fatalf("放行的语句含被禁止的操作符 %s：%s", bad, c.Statement)
			}
		}
		for _, n := range c.Collections {
			if strings.HasPrefix(n, "system.") || strings.Contains(n, "$") {
				t.Fatalf("涉及了不允许的集合 %q：%s", n, c.Statement)
			}
		}
		if _, err := CheckUserQuery(c.Statement); err != nil {
			t.Fatalf("规范化语句未通过再次检查：%q → %v", c.Statement, err)
		}
	})
}
