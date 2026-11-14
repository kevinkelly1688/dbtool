// @author: kevinkelly
// @license: Apache Licence
// @file: parser_test.go
// @time: 2026/10/28

package mongo

import (
	"strings"
	"testing"
)

func firstArgJS(t *testing.T, text string) string {
	t.Helper()
	st, err := parseStatement(text)
	if err != nil {
		t.Fatalf("parse %q: %v", text, err)
	}
	if len(st.calls) == 0 || len(st.calls[0].args) == 0 {
		t.Fatalf("parse %q: 没有参数", text)
	}
	return st.calls[0].args[0].js
}

func TestParseValues(t *testing.T) {
	cases := []struct{ in, want string }{
		{`db.a.find({x: 1})`, `{"x":1}`},
		{`db.a.find({'x': 'it\'s', "y": "a\"b"})`, `{"x":"it's","y":"a\"b"}`},
		{`db.a.find({_id: ObjectId("507F1F77BCF86CD799439011")})`, `{"_id":{"$oid":"507f1f77bcf86cd799439011"}}`},
		{`db.a.find({t: ISODate("2024-01-02T03:04:05Z")})`, `{"t":{"$date":{"$numberLong":"1704164645000"}}}`},
		{`db.a.find({t: ISODate("2024-01-02T03:04:05+08:00")})`, `{"t":{"$date":{"$numberLong":"1704135845000"}}}`},
		{`db.a.find({t: ISODate("2024-01-02")})`, `{"t":{"$date":{"$numberLong":"1704153600000"}}}`},
		{`db.a.find({t: new Date("2024-01-02T03:04:05.123Z")})`, `{"t":{"$date":{"$numberLong":"1704164645123"}}}`},
		{`db.a.find({t: new Date(0)})`, `{"t":{"$date":{"$numberLong":"0"}}}`},
		{`db.a.find({n: NumberLong(5)})`, `{"n":{"$numberLong":"5"}}`},
		{`db.a.find({n: NumberLong("9007199254740993")})`, `{"n":{"$numberLong":"9007199254740993"}}`},
		{`db.a.find({n: new NumberInt(7)})`, `{"n":{"$numberInt":"7"}}`},
		{`db.a.find({n: NumberDecimal("1.50")})`, `{"n":{"$numberDecimal":"1.50"}}`},
		{`db.a.find({n: NumberDouble(2)})`, `{"n":{"$numberDouble":"2"}}`},
		{`db.a.find({a: /ab+c\/d/i})`, `{"a":{"$regularExpression":{"pattern":"ab+c\\/d","options":"i"}}}`},
		{`db.a.find({a: /x[/]y/gi})`, `{"a":{"$regularExpression":{"pattern":"x[/]y","options":"i"}}}`},
		{`db.a.find({a: RegExp("^a", "i")})`, `{"a":{"$regularExpression":{"pattern":"^a","options":"i"}}}`},
		{`db.a.find({a: [1, 2, ], })`, `{"a":[1,2]}`},
		{"db.a.find({a: 1 /* c */, // x\n b: 2})", `{"a":1,"b":2}`},
		{`db.a.find({a: -5, b: 1.5, c: 1e3, d: 0x10, e: .5})`, `{"a":-5,"b":{"$numberDouble":"1.5"},"c":{"$numberDouble":"1000"},"d":16,"e":{"$numberDouble":"0.5"}}`},
		{`db.a.find({a: NaN, b: Infinity, c: -Infinity})`, `{"a":{"$numberDouble":"NaN"},"b":{"$numberDouble":"Infinity"},"c":{"$numberDouble":"-Infinity"}}`},
		{`db.a.find({"$gt": 5, $lt: 9})`, `{"$gt":5,"$lt":9}`},
		{`db.a.find({1: 'a'})`, `{"1":"a"}`},
		{`db.a.find({x: '中\x41\n'})`, `{"x":"中A\n"}`},
		{`db.a.find({a: true, b: false, c: null, d: undefined})`, `{"a":true,"b":false,"c":null,"d":null}`},
		{`db.a.find({"_id": {"$oid": "507f1f77bcf86cd799439011"}})`, `{"_id":{"$oid":"507f1f77bcf86cd799439011"}}`},
		{`db.a.find({b: BinData(0, "AQID")})`, `{"b":{"$binary":{"base64":"AQID","subType":"00"}}}`},
		{`db.a.find({b: HexData(0, "010203")})`, `{"b":{"$binary":{"base64":"AQID","subType":"00"}}}`},
		{`db.a.find({u: UUID("123e4567-e89b-12d3-a456-426614174000")})`, `{"u":{"$binary":{"base64":"Ej5FZ+ibEtOkVkJmFBdAAA==","subType":"04"}}}`},
		{`db.a.find({t: Timestamp(1, 2)})`, `{"t":{"$timestamp":{"t":1,"i":2}}}`},
		{`db.a.find({m: MinKey(), n: MaxKey()})`, `{"m":{"$minKey":1},"n":{"$maxKey":1}}`},
		{`db.a.find({"a": {"$in": [ObjectId("507f1f77bcf86cd799439011"), 3]}})`, `{"a":{"$in":[{"$oid":"507f1f77bcf86cd799439011"},3]}}`},
		{`db.a.find({a: "x;y db.b.drop()"})`, `{"a":"x;y db.b.drop()"}`},
		{`db.a.find({a: 9223372036854775807})`, `{"a":9223372036854775807}`},
		{`db.a.find({a: 9223372036854775808})`, `{"a":{"$numberDouble":"9.223372036854776e+18"}}`},
	}
	for _, c := range cases {
		if got := firstArgJS(t, c.in); got != c.want {
			t.Errorf("%s\n  got  %s\n  want %s", c.in, got, c.want)
		}
	}
}

func TestParseStatementShape(t *testing.T) {
	type want struct {
		coll    string
		hasColl bool
		calls   []string
	}
	cases := []struct {
		in string
		w  want
	}{
		{`db.users.find()`, want{"users", true, []string{"find"}}},
		{`db.users.find().limit(5).sort({x: 1})`, want{"users", true, []string{"find", "limit", "sort"}}},
		{`db.a.b.c.find({})`, want{"a.b.c", true, []string{"find"}}},
		{`db.getCollection("a-b").find({})`, want{"a-b", true, []string{"find"}}},
		{`db["a-b"].find({})`, want{"a-b", true, []string{"find"}}},
		{`db['a'].b.find({})`, want{"a.b", true, []string{"find"}}},
		{`db.stats()`, want{"", false, []string{"stats"}}},
		{`db.getCollectionNames()`, want{"", false, []string{"getCollectionNames"}}},
		{`db.users.find();`, want{"users", true, []string{"find"}}},
		{`db.users.find();;`, want{"users", true, []string{"find"}}},
		{"  // 注释\n db.users.find() // 行尾注释", want{"users", true, []string{"find"}}},
		{`db.users.aggregate([]).toArray()`, want{"users", true, []string{"aggregate", "toArray"}}},
	}
	for _, c := range cases {
		st, err := parseStatement(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if st.coll != c.w.coll || st.hasColl != c.w.hasColl {
			t.Errorf("%q: coll=%q has=%v", c.in, st.coll, st.hasColl)
		}
		var names []string
		for _, cl := range st.calls {
			names = append(names, cl.name)
		}
		if strings.Join(names, ",") != strings.Join(c.w.calls, ",") {
			t.Errorf("%q: calls=%v want %v", c.in, names, c.w.calls)
		}
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct{ in, want string }{
		{`db.a.find({x: })`, "第 1 行第 15 列"},
		{"db.a.find({\n  x: 1,\n  y: foo\n})", "第 3 行第 6 列"},
		{`db.a.find({x: "abc})`, "字符串没有结束"},
		{`db.a.find({x: foo})`, "无法识别的标识符 foo"},
		{`db.a.find({x: Foo(1)})`, "不支持的函数"},
		{`db.a.find({x: ObjectId("zz")})`, "24 位十六进制"},
		{`db.a.find({x: ObjectId()})`, "参数个数"},
		{`db.a.find({x: /abc})`, "正则表达式没有结束"},
		{`db.a.find({x: /abc/q})`, "不支持的正则选项"},
		{`db.a.find({x: ISODate("not a date")})`, "无法识别的日期"},
		{`db.a.find({x: NumberLong("abc")})`, "64 位整数"},
		{`db.a.find({x: NumberInt(3000000000)})`, "32 位整数"},
		{`db.a.find({x: function() { return 1 }})`, "JavaScript"},
		{`db.a.find({x: this.y})`, "JavaScript"},
		{`db.a.find(`, "此处应为一个值"},
		{`db.a.find({x: 1}`, "参数之间应为"},
		{`db.a.find({x 1})`, "此处应为“:”"},
		{`db.a.find({x: 1} y)`, "参数之间应为"},
		{`db.a.find() foo`, "多余内容"},
		{`db.a.find(); db.b.find()`, "多条语句"},
		{"db.a.find()\ndb.b.find()", "多条语句"},
		{`db.a.find().limit`, "缺少括号"},
		{`db.a`, "此处应为“.”"},
		{`use foo`, "切换库请用页面顶部的库下拉"},
		{`show collections`, "db.getCollectionNames()"},
		{`rs.status()`, "副本集"},
		{`find({})`, "必须以 db. 开头"},
		{`db.a.find({x: 1 # })`, "无法识别的字符"},
		{`db.a.find({x: /* 没结束 1})`, "块注释"},
		{"db.a.find({x: 'a\nb'})", "不能直接换行"},
		{`db.a.find({x: '\q'})`, "不支持的转义"},
		{`db.a.find({x: 0xZZ})`, ""},
		{`db.getCollection(1).find()`, "getCollection 需要一个字符串参数"},
		{`db.getSiblingDB("x").a.find()`, "切换库请用页面顶部的库下拉"},
		{`db.a.find({x: -foo})`, "“-”后面应为数字"},
		{`db.a.find({` + strings.Repeat("a:{", 70) + strings.Repeat("}", 70) + `})`, "嵌套层数超过"},
		{`db.a.find({x: 'bad\u12'})`, "\\u 转义"},
	}
	for _, c := range cases {
		_, err := parseStatement(c.in)
		if err == nil {
			t.Errorf("%q: 应当失败", c.in)
			continue
		}
		var ue *UserError
		if !asUserError(err, &ue) {
			t.Errorf("%q: 错误类型应为 *UserError: %T", c.in, err)
		}
		if c.want != "" && !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: 错误 %q 应包含 %q", c.in, err.Error(), c.want)
		}
	}
}

func asUserError(err error, target **UserError) bool {
	ue, ok := err.(*UserError)
	if ok {
		*target = ue
	}
	return ok
}
