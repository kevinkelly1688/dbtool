// @author: kevinkelly
// @license: Apache Licence
// @file: check_test.go
// @time: 2026/10/28

package mongo

import (
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestCheckAccept(t *testing.T) {
	cases := []struct {
		in    string
		kind  Kind
		colls string // 逗号连接的 Collections；"" 表示 db 级
	}{
		{`db.users.find()`, KindFind, "users"},
		{`db.users.find({})`, KindFind, "users"},
		{`db.users.find({age: {$gt: 18}}, {name: 1, _id: 0})`, KindFind, "users"},
		{`db.users.find({age: {$gt: 18}}).sort({age: -1}).limit(10).skip(5)`, KindFind, "users"},
		{`db.users.find().skip(5).limit(10).sort({age: -1}).pretty()`, KindFind, "users"},
		{`db.users.find({a: 1}).toArray()`, KindFind, "users"},
		{`db.users.findOne({_id: ObjectId("507f1f77bcf86cd799439011")})`, KindFind, "users"},
		{`db.users.find({name: /^zh/i, created: {$gte: ISODate("2024-01-01")}})`, KindFind, "users"},
		{`db.users.find({$or: [{a: 1}, {b: {$in: [1, 2, 3]}}]})`, KindFind, "users"},
		{`db.users.find({$expr: {$gt: ["$a", "$b"]}})`, KindFind, "users"},
		{`db.users.find({$text: {$search: "hello"}}, {score: {$meta: "textScore"}})`, KindFind, "users"},
		{`db.users.find({loc: {$near: {$geometry: {type: "Point", coordinates: [1, 2]}, $maxDistance: 100}}})`, KindFind, "users"},
		{`db.users.find({$jsonSchema: {bsonType: "object"}})`, KindFind, "users"},
		{`db.users.find({a: {$regex: "^x", $options: "i"}}).sort({$natural: -1})`, KindFind, "users"},
		{`db.users.find({note: "db.users.drop()", s: "a;b", w: "$where", o: "$out"})`, KindFind, "users"},
		{`db.users.find({a: 1}).maxTimeMS(1000)`, KindFind, "users"},
		{`db.users.find({a: 1}).batchSize(10).allowDiskUse()`, KindFind, "users"},
		{`db.users.find({a: 1}).count()`, KindCount, "users"},
		{`db.users.find({a: 1}).limit(5).count()`, KindCount, "users"},
		{`db.users.countDocuments({a: 1})`, KindCount, "users"},
		{`db.users.countDocuments()`, KindCount, "users"},
		{`db.users.count({a: 1})`, KindCount, "users"},
		{`db.users.estimatedDocumentCount()`, KindEstimatedCount, "users"},
		{`db.users.distinct("city")`, KindDistinct, "users"},
		{`db.users.distinct("city", {a: 1})`, KindDistinct, "users"},
		{`db.users.aggregate([])`, KindAggregate, "users"},
		{`db.users.aggregate([{$match: {a: 1}}, {$group: {_id: "$city", n: {$sum: 1}}}, {$sort: {n: -1}}, {$limit: 10}])`, KindAggregate, "users"},
		{`db.users.aggregate([{$project: {a: 1}}, {$unwind: "$tags"}, {$addFields: {x: {$add: [1, 2]}}}, {$set: {y: 1}}, {$unset: "z"}, {$count: "n"}])`, KindAggregate, "users"},
		{`db.users.aggregate([{$replaceRoot: {newRoot: "$a"}}, {$replaceWith: "$b"}, {$sortByCount: "$c"}, {$sample: {size: 3}}, {$skip: 1}])`, KindAggregate, "users"},
		{`db.users.aggregate([{$bucket: {groupBy: "$a", boundaries: [0, 10], default: "o"}}, {$bucketAuto: {groupBy: "$a", buckets: 3}}])`, KindAggregate, "users"},
		{`db.users.aggregate([{$setWindowFields: {sortBy: {a: 1}, output: {s: {$sum: "$a", window: {documents: ["unbounded", "current"]}}}}}, {$fill: {output: {a: {method: "linear"}}}}, {$densify: {field: "a", range: {step: 1, bounds: "full"}}}, {$redact: "$$KEEP"}])`, KindAggregate, "users"},
		{`db.users.aggregate([{$lookup: {from: "orders", localField: "_id", foreignField: "uid", as: "o"}}])`, KindAggregate, "orders,users"},
		{`db.users.aggregate([{$lookup: {from: "orders", let: {u: "$_id"}, pipeline: [{$match: {$expr: {$eq: ["$uid", "$$u"]}}}, {$lookup: {from: "items", localField: "i", foreignField: "_id", as: "it"}}], as: "o"}}])`, KindAggregate, "items,orders,users"},
		{`db.users.aggregate([{$facet: {a: [{$match: {x: 1}}], b: [{$unionWith: "logs"}, {$count: "n"}]}}])`, KindAggregate, "logs,users"},
		{`db.users.aggregate([{$unionWith: {coll: "archive", pipeline: [{$match: {a: 1}}]}}])`, KindAggregate, "archive,users"},
		{`db.users.aggregate([{$graphLookup: {from: "emp", startWith: "$m", connectFromField: "m", connectToField: "_id", as: "chain", maxDepth: 3}}])`, KindAggregate, "emp,users"},
		{`db.users.aggregate([{$match: {a: 1}}], {allowDiskUse: true, cursor: {batchSize: 10}})`, KindAggregate, "users"},
		{`db.users.aggregate({$match: {a: 1}}, {$limit: 3})`, KindAggregate, "users"},
		{`db.users.aggregate([{$match: {a: 1}}]).toArray()`, KindAggregate, "users"},
		{`db.getCollectionNames()`, KindListCollections, ""},
		{`db.stats()`, KindDBStats, ""},
		{`db.users.getIndexes()`, KindListIndexes, "users"},
		{`db.users.stats()`, KindCollStats, "users"},
		{`db.getCollection("my-coll").find()`, KindFind, "my-coll"},
		{`db["my-coll"].find({})`, KindFind, "my-coll"},
		{`db.app.logs.find({})`, KindFind, "app.logs"},
		{`db.users.find({a: 1});`, KindFind, "users"},
		{"db.users.find({\n  // 注释\n  a: 1\n}) // 末尾注释", KindFind, "users"},
		{`db.users.find({$and: [{a: {$exists: true}}, {b: {$type: "string"}}]})`, KindFind, "users"},
	}
	for _, c := range cases {
		ck, err := CheckUserQuery(c.in)
		if err != nil {
			t.Errorf("%q 应放行，却被拒绝：%v", c.in, err)
			continue
		}
		if ck.Kind != c.kind {
			t.Errorf("%q: kind=%s want %s", c.in, ck.Kind, c.kind)
		}
		if got := strings.Join(ck.Collections, ","); got != c.colls {
			t.Errorf("%q: Collections=%q want %q", c.in, got, c.colls)
		}
		if ck.DBLevel != (c.colls == "") {
			t.Errorf("%q: DBLevel=%v", c.in, ck.DBLevel)
		}
		if ck.Statement == "" {
			t.Errorf("%q: Statement 为空", c.in)
		}
		// 规范化后的语句必须能再次通过检查（幂等）
		if _, err := CheckUserQuery(ck.Statement); err != nil {
			t.Errorf("%q: 规范化语句 %q 再检查失败：%v", c.in, ck.Statement, err)
		}
	}
}

func TestCheckReject(t *testing.T) {
	cases := []struct{ in, want string }{
		{``, "语句为空"},
		{`   `, "语句为空"},
		// 写 / 管理
		{`db.a.insertOne({a: 1})`, "只读"},
		{`db.a.insert({a: 1})`, "只读"},
		{`db.a.updateMany({}, {$set: {a: 1}})`, "只读"},
		{`db.a.update({}, {$set: {a: 1}})`, "只读"},
		{`db.a.deleteMany({})`, "只读"},
		{`db.a.remove({})`, "只读"},
		{`db.a.drop()`, "只读"},
		{`db.dropDatabase()`, "只读"},
		{`db.a.createIndex({a: 1})`, "只读"},
		{`db.a.dropIndex("a_1")`, "只读"},
		{`db.a.renameCollection("b")`, "只读"},
		{`db.a.bulkWrite([])`, "只读"},
		{`db.a.findAndModify({})`, "只读"},
		{`db.a.findOneAndUpdate({}, {$set: {a: 1}})`, "只读"},
		{`db.a.findOneAndDelete({})`, "只读"},
		{`db.a.save({})`, "只读"},
		{`db.a.mapReduce("x", "y", {})`, "只读"},
		{`db.createCollection("x")`, "只读"},
		{`db.createUser({user: "a"})`, "只读"},
		{`db.a.replaceOne({}, {})`, "只读"},
		{`db.setProfilingLevel(2)`, "只读"},
		// 命令 / 执行
		{`db.runCommand({ping: 1})`, "不在允许的方法清单"},
		{`db.adminCommand({listDatabases: 1})`, "不在允许的方法清单"},
		{`db.eval("1")`, "不在允许的方法清单"},
		{`db.serverStatus()`, "不在允许的方法清单"},
		{`db.currentOp()`, "不在允许的方法清单"},
		{`db.getSiblingDB("admin").a.find()`, "库下拉"},
		{`db.getUsers()`, "不在允许的方法清单"},
		{`db.a.watch()`, "不在允许的方法清单"},
		{`db.a.find().explain()`, "explain"},
		{`db.a.explain().find()`, "explain"},
		{`db.version()`, "不支持 db.version()"},
		{`db.a.help()`, "不支持 help"},
		{`db.a.validate()`, "只读"},
		{`eval("1")`, "必须以 db. 开头"},
		{`load("x.js")`, "必须以 db. 开头"},
		{`sleep(1000)`, "必须以 db. 开头"},
		{`while (true) {}`, "必须以 db. 开头"},
		{`rs.status()`, "副本集"},
		{`sh.status()`, "副本集"},
		{`use admin`, "库下拉"},
		{`show dbs`, "必须以 db. 开头"},
		{`db.a.find({x: sleep(1000)})`, "不支持的函数 / 构造函数 sleep"},
		{`db.a.find().forEach(function(d) { print(d) })`, "JavaScript"},
		{`db.a.find().forEach(printjson)`, "无法识别的标识符 printjson"},
		{`db.a.find().map(function(d) { return d })`, "JavaScript"},
		// JavaScript 执行
		{`db.a.find({$where: "this.a > 1"})`, "$where"},
		{`db.a.find({"$where": "sleep(1000)"})`, "$where"},
		{`db.a.find({$where: function() { return true }})`, "JavaScript"},
		{`db.a.find({"$where": {"$code": "1"}})`, "$where"},
		{`db.a.find({a: {"$code": "1"}})`, "JavaScript"},
		{`db.a.find({a: {"$code": "1", "$scope": {}}})`, "JavaScript"},
		{`db.a.find({$expr: {$function: {body: "function(){}", args: [], lang: "js"}}})`, "$function"},
		{`db.a.find({$or: [{x: 1}, {$and: [{$where: "1"}]}]})`, "$where"},
		{`db.a.find({a: {$elemMatch: {$where: "1"}}})`, "$where"},
		{`db.a.find({"$where": "1"})`, "$where"},
		{`db.a.find({"\u0024where": "1"})`, "$where"},
		{`db.a.find({"\u0024function": {}})`, "$function"},
		{`db.a.find({a: 1}, {x: {$function: {}}})`, "$function"},
		{`db.a.find({a: 1}).sort({$where: 1})`, "$where"},
		{`db.a.countDocuments({$where: "1"})`, "$where"},
		{`db.a.distinct("x", {$where: "1"})`, "$where"},
		{`db.a.find({$_internalSchemaMinItems: 1})`, "内部操作符"},
		{`db.a.aggregate([{$group: {_id: null, x: {$accumulator: {init: "function(){}", accumulate: "function(){}", accumulateArgs: [], merge: "function(){}", lang: "js"}}}}])`, "$accumulator"},
		{`db.a.aggregate([{$addFields: {x: {$function: {body: "function(){}", args: [], lang: "js"}}}}])`, "$function"},
		{`db.a.aggregate([{$match: {$expr: {$function: {}}}}])`, "$function"},
		// aggregate stage
		{`db.a.aggregate([{$out: "x"}])`, "$out"},
		{`db.a.aggregate([{$merge: {into: "x"}}])`, "$merge"},
		{`db.a.aggregate([{$currentOp: {}}])`, "$currentOp"},
		{`db.a.aggregate([{$listSessions: {}}])`, "$listSessions"},
		{`db.a.aggregate([{$listLocalSessions: {}}])`, "$listLocalSessions"},
		{`db.a.aggregate([{$collStats: {}}])`, "$collStats"},
		{`db.a.aggregate([{$indexStats: {}}])`, "$indexStats"},
		{`db.a.aggregate([{$planCacheStats: {}}])`, "$planCacheStats"},
		{`db.a.aggregate([{$changeStream: {}}])`, "$changeStream"},
		{`db.a.aggregate([{$search: {}}])`, "$search"},
		{`db.a.aggregate([{$documents: []}])`, "$documents"},
		{`db.a.aggregate([{$geoNear: {}}])`, "$geoNear"},
		{`db.a.aggregate([{$foo: 1}])`, "不在允许的清单"},
		{`db.a.aggregate([{$_internalSplitPipeline: {}}])`, "不在允许的清单"},
		{`db.a.aggregate([{$match: {}, $limit: 1}])`, "只含一个"},
		{`db.a.aggregate([{}])`, "只含一个"},
		{`db.a.aggregate([1])`, "只含一个"},
		{`db.a.aggregate([{$match: {$_internalFoo: 1}}])`, "内部操作符"},
		{`db.a.aggregate({$match: {}}, {$out: "x"})`, "$out"},
		{`db.a.aggregate("x")`, "pipeline 数组"},
		{`db.a.aggregate()`, "pipeline 数组"},
		{`db.a.aggregate([], {explain: true})`, "不支持"},
		{`db.a.aggregate([], {hint: "x"})`, "不支持"},
		{`db.a.aggregate([]).explain()`, "explain"},
		{`db.a.aggregate([]).limit(1)`, "toArray"},
		{`db.a.aggregate([{$lookup: {from: "b", as: "x", pipeline: [{$out: "z"}]}}])`, "$out"},
		{`db.a.aggregate([{$lookup: {from: {db: "x", coll: "y"}, as: "z"}}])`, "同库"},
		{`db.a.aggregate([{$lookup: {from: "system.users", localField: "a", foreignField: "b", as: "z"}}])`, "系统集合"},
		{`db.a.aggregate([{$lookup: {as: "x", pipeline: []}}])`, "必须指定 from"},
		{`db.a.aggregate([{$lookup: {from: "b", as: "x", foo: 1}}])`, "不支持选项"},
		{`db.a.aggregate([{$facet: {x: [{$merge: {into: "x"}}]}}])`, "$merge"},
		{`db.a.aggregate([{$facet: {x: 1}}])`, "pipeline 必须是数组"},
		{`db.a.aggregate([{$facet: [1]}])`, "$facet 的参数必须是文档"},
		{`db.a.aggregate([{$unionWith: {coll: "b", pipeline: [{$out: "x"}]}}])`, "$out"},
		{`db.a.aggregate([{$unionWith: {pipeline: []}}])`, "必须指定 coll"},
		{`db.a.aggregate([{$unionWith: "system.views"}])`, "系统集合"},
		{`db.a.aggregate([{$unionWith: {coll: {db: "x", coll: "y"}}}])`, "同库"},
		{`db.a.aggregate([{$unionWith: 1}])`, "$unionWith 的参数"},
		{`db.a.aggregate([{$graphLookup: {from: {x: 1}}}])`, "同库"},
		{`db.a.aggregate([{$graphLookup: {as: "x"}}])`, "必须指定 from"},
		{`db.a.aggregate([{$graphLookup: {from: "b", maxDepth: -1}}])`, "maxDepth"},
		{`db.a.aggregate([{$graphLookup: {from: "b", evil: 1}}])`, "不支持选项"},
		{"db.a.aggregate([" + strings.Repeat("{$match: {}},", 51) + "])", "stage 数超过"},
		{`db.a.aggregate([{$facet: {a: [{$facet: {a: [{$facet: {a: [{$facet: {a: [{$facet: {a: [{$facet: {a: []}}]}}]}}]}}]}}]}}])`, "嵌套层数超过"},
		// 多语句
		{`db.a.find(); db.b.find()`, "多条语句"},
		{`db.a.find({}); db.b.drop()`, "多条语句"},
		{"db.a.find()\ndb.b.find()", "多条语句"},
		{`db.a.find() db.b.find()`, "多条语句"},
		// 集合名
		{`db.system.users.find()`, "系统集合"},
		{`db.system.profile.find()`, "系统集合"},
		{`db.getCollection("system.views").find()`, "系统集合"},
		{`db.getCollection("a$b").find()`, "集合名不合法"},
		{`db.getCollection("").find()`, "集合名不能为空"},
		{`db.getCollection("a..b").find()`, "集合名不合法"},
		// 参数形态
		{`db.a.distinct()`, "字段名字符串"},
		{`db.a.distinct(1)`, "字段名字符串"},
		{`db.a.distinct("")`, "字段名字符串"},
		{`db.a.find({}, {}, {})`, "最多两个参数"},
		{`db.a.find([])`, "必须是文档"},
		{`db.a.find("x")`, "必须是文档"},
		{`db.a.find({}, 1)`, "必须是文档"},
		{`db.a.find().limit(-1)`, "非负整数"},
		{`db.a.find().limit("5")`, "非负整数"},
		{`db.a.find().skip(1.5)`, "非负整数"},
		{`db.a.find().limit()`, "非负整数"},
		{`db.a.find().sort()`, "sort()"},
		{`db.a.find().sort(1)`, "必须是文档"},
		{`db.a.find().limit(1).limit(2)`, "重复"},
		{`db.a.find().count().limit(3)`, "必须放在链的最后"},
		{`db.a.find().hint({a: 1})`, "不支持"},
		{`db.a.find().collation({locale: "en"})`, "不支持"},
		{`db.a.find().foo()`, "不支持的游标方法"},
		{`db.a.findOne({}).foo()`, "不能再接方法"},
		{`db.a.countDocuments({}, {limit: 1})`, "只支持一个过滤条件"},
		{`db.a.countDocuments().limit(1)`, "不能再接方法"},
		{`db.a.estimatedDocumentCount(1)`, "不接受参数"},
		{`db.a.getIndexes(1)`, "不接受参数"},
		{`db.a.stats().foo()`, "不能再接方法"},
		{`db.getCollectionNames().foo()`, "不能再接方法"},
		{`db.getCollectionNames(1)`, "不接受参数"},
		{`db.a.distinct("x").foo()`, "不能再接方法"},
		{`db.a.find().limit(1).limit`, "缺少括号"},
	}
	for _, c := range cases {
		ck, err := CheckUserQuery(c.in)
		if err == nil {
			t.Errorf("%q 应当被拒绝，却放行了：%s", c.in, ck.Statement)
			continue
		}
		var ue *UserError
		if !asUserError(err, &ue) {
			t.Errorf("%q: 错误类型应为 *UserError：%T", c.in, err)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: 错误 %q 应包含 %q", c.in, err.Error(), c.want)
		}
	}
}

func TestCheckTooLong(t *testing.T) {
	long := `db.a.find({x: "` + strings.Repeat("a", maxStatementBytes) + `"})`
	if _, err := CheckUserQuery(long); err == nil || !strings.Contains(err.Error(), "过长") {
		t.Fatalf("超长语句应被拒绝：%v", err)
	}
}

func TestCheckRewrite(t *testing.T) {
	// limit 缺省 / 超限
	ck, err := CheckUserQuery(`db.a.find({x: 1})`, CheckOptions{MaxRows: 100})
	if err != nil {
		t.Fatal(err)
	}
	if ck.Limit != 0 || len(ck.Notices) != 1 || !strings.Contains(ck.Notices[0], "最多返回 100 条") {
		t.Errorf("缺省 limit 提示不对：%+v", ck.Notices)
	}
	ck, _ = CheckUserQuery(`db.a.find().limit(5000)`, CheckOptions{MaxRows: 100})
	if ck.Limit != 100 || !strings.Contains(strings.Join(ck.Notices, ";"), "收窄为 100") {
		t.Errorf("超限 limit 应收窄：limit=%d notices=%v", ck.Limit, ck.Notices)
	}
	ck, _ = CheckUserQuery(`db.a.find().limit(5)`, CheckOptions{MaxRows: 100})
	if ck.Limit != 5 || len(ck.Notices) != 0 {
		t.Errorf("未超限不应改写：limit=%d notices=%v", ck.Limit, ck.Notices)
	}
	ck, _ = CheckUserQuery(`db.a.findOne({})`, CheckOptions{MaxRows: 100})
	if ck.Limit != 1 {
		t.Errorf("findOne 应 limit 1：%d", ck.Limit)
	}
	// $graphLookup 深度
	ck, _ = CheckUserQuery(`db.a.aggregate([{$graphLookup: {from: "b", startWith: "$x", connectFromField: "x", connectToField: "y", as: "z"}}])`)
	gl := ck.Pipeline[0][0].Value.(bson.D)
	if v, ok := lookup(gl, "maxDepth"); !ok || i64(v) != GraphLookupMaxDepth {
		t.Errorf("$graphLookup 应补 maxDepth：%v", gl)
	}
	ck, _ = CheckUserQuery(`db.a.aggregate([{$graphLookup: {from: "b", maxDepth: 1000}}])`)
	gl = ck.Pipeline[0][0].Value.(bson.D)
	if v, _ := lookup(gl, "maxDepth"); i64(v) != GraphLookupMaxDepth || len(ck.Notices) == 0 {
		t.Errorf("$graphLookup 超大 maxDepth 应收窄：%v %v", gl, ck.Notices)
	}
	ck, _ = CheckUserQuery(`db.a.aggregate([{$graphLookup: {from: "b", maxDepth: 3}}])`)
	gl = ck.Pipeline[0][0].Value.(bson.D)
	if v, _ := lookup(gl, "maxDepth"); i64(v) != 3 {
		t.Errorf("合法 maxDepth 不应改动：%v", gl)
	}
	// 嵌套 pipeline 里的 $graphLookup 同样被改写
	ck, _ = CheckUserQuery(`db.a.aggregate([{$facet: {x: [{$graphLookup: {from: "b"}}]}}])`)
	fx := ck.Pipeline[0][0].Value.(bson.D)
	inner := fx[0].Value.(bson.A)[0].(bson.D)[0].Value.(bson.D)
	if _, ok := lookup(inner, "maxDepth"); !ok {
		t.Errorf("$facet 内的 $graphLookup 应补 maxDepth：%v", inner)
	}
	// 被忽略的选项给提示
	ck, _ = CheckUserQuery(`db.a.aggregate([], {allowDiskUse: true})`)
	if !strings.Contains(strings.Join(ck.Notices, ";"), "allowDiskUse 已忽略") {
		t.Errorf("allowDiskUse 应提示忽略：%v", ck.Notices)
	}
	ck, _ = CheckUserQuery(`db.a.find({}).sort({x: 1}).limit(3).count()`)
	if ck.Kind != KindCount || ck.Limit != 0 || ck.Sort != nil || len(ck.Notices) == 0 {
		t.Errorf("count() 应忽略 sort/limit：%+v", ck)
	}
}

func TestCheckStatementRender(t *testing.T) {
	cases := []struct{ in, want string }{
		{`db.a.find({x: 1}).sort({y: -1}).limit(5)`, `db.a.find({"x":1}).sort({"y":-1}).limit(5)`},
		{`db['my-c'].find({}, {n: 1})`, `db.getCollection("my-c").find({}, {"n":1})`},
		{`db.a.find()`, `db.a.find()`},
		{`db.a.count({x: 1})`, `db.a.countDocuments({"x":1})`},
		{`db.a.countDocuments()`, `db.a.countDocuments({})`},
		{`db.a.distinct("c", {x: 1})`, `db.a.distinct("c", {"x":1})`},
		{`db.a.aggregate({$match: {x: 1}}, {$limit: 2})`, `db.a.aggregate([{"$match":{"x":1}}, {"$limit":2}])`},
		{`db.getCollection("find").find()`, `db.getCollection("find").find()`},
		{`db.a.find({_id: ObjectId("507f1f77bcf86cd799439011")})`, `db.a.find({"_id":{"$oid":"507f1f77bcf86cd799439011"}})`},
	}
	for _, c := range cases {
		ck, err := CheckUserQuery(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if ck.Statement != c.want {
			t.Errorf("%q\n  got  %s\n  want %s", c.in, ck.Statement, c.want)
		}
	}
}

func TestValidDBName(t *testing.T) {
	for _, ok := range []string{"app", "my-db", "db_1", "测试库"} {
		if err := ValidDBName(ok); err != nil {
			t.Errorf("%q 应合法：%v", ok, err)
		}
	}
	for _, bad := range []string{"", "admin", "local", "config", "a.b", "a/b", "a b", "a$b", `a"b`, strings.Repeat("x", 64), "a\x00b"} {
		if err := ValidDBName(bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
}
