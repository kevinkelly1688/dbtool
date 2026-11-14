// @author: kevinkelly
// @license: Apache Licence
// @file: check.go
// @time: 2026/10/28

package mongo

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// 在线查询的静态检查：解析 mongo shell 风格的只读子集，校验并按限量改写，返回涉及的集合。纯函数，不连服务端。
//
// 默认拒绝：方法、aggregate stage 都是白名单；表达式 / 过滤条件里出现任何能执行 JavaScript 或写入的操作符一律拒绝。

// Kind 是语句的种类。
type Kind string

const (
	KindFind            Kind = "find"
	KindCount           Kind = "count"     // countDocuments / count()
	KindEstimatedCount  Kind = "estimated" // estimatedDocumentCount
	KindDistinct        Kind = "distinct"
	KindAggregate       Kind = "aggregate"
	KindListCollections Kind = "listCollections" // db.getCollectionNames()
	KindDBStats         Kind = "dbStats"         // db.stats()
	KindListIndexes     Kind = "listIndexes"     // coll.getIndexes()
	KindCollStats       Kind = "collStats"       // coll.stats()
)

// 限量常量。
const (
	DefaultMaxRows      = 1000    // QueryLimits.MaxRows 为 0 时
	HardMaxBytes        = 4 << 20 // 一次查询结果（原始 BSON）总量上限
	DefaultMaxCellBytes = 64 << 10
	MaxPipelineStages   = 50 // 单个 pipeline（含嵌套）的 stage 数上限
	MaxNestedPipelines  = 4  // $facet / $lookup / $unionWith 的嵌套层数
	GraphLookupMaxDepth = 10 // $graphLookup 的 maxDepth 上限（缺省补这个值）
	MaxColumns          = 200
)

// CheckOptions 是静态检查的选项。
type CheckOptions struct {
	// MaxRows > 0 时，检查阶段就把缺省 / 超限的 limit 收窄到它并给出提示（RunUser 的 QueryLimits.MaxRows 仍会再强制一次）。
	MaxRows int
}

// Checked 是通过检查的语句。
type Checked struct {
	Kind Kind
	// Statement 是规范化后的语句文本（Extended JSON），用于查询日志与“实际执行的语句”展示。
	Statement string
	// Collection 是语句作用的集合；db.getCollectionNames() / db.stats() 为空。
	Collection string
	// Collections 是语句涉及的全部集合（主集合 + $lookup / $graphLookup / $unionWith 的目标），去重、排序，供上层做库表级权限判断。
	// db 级语句为空，此时 DBLevel 为 true（只需要库级权限）。
	Collections []string
	DBLevel     bool
	// Notices 是静态改写 / 忽略产生的提示。
	Notices []string

	Filter     bson.D
	Projection bson.D
	Sort       bson.D
	Skip       int64
	Limit      int64 // 用户指定的 limit（0=未指定）；已按 CheckOptions.MaxRows 收窄
	Field      string
	Pipeline   []bson.D

	maxRows int
}

func (c *Checked) notice(format string, a ...any) {
	n := fmt.Sprintf(format, a...)
	for _, e := range c.Notices {
		if e == n {
			return
		}
	}
	c.Notices = append(c.Notices, n)
}

// 拒绝原因表。
var (
	writeMethods = setOf("insert", "insertOne", "insertMany", "update", "updateOne", "updateMany", "replaceOne", "remove", "deleteOne", "deleteMany",
		"save", "findAndModify", "findOneAndUpdate", "findOneAndReplace", "findOneAndDelete", "bulkWrite", "drop", "dropIndex", "dropIndexes",
		"createIndex", "createIndexes", "ensureIndex", "reIndex", "renameCollection", "createCollection", "dropDatabase", "createView", "convertToCapped",
		"compact", "validate", "mapReduce", "initializeUnorderedBulkOp", "initializeOrderedBulkOp", "copyTo", "cloneCollection", "dropCollection",
		"createUser", "dropUser", "updateUser", "grantRolesToUser", "revokeRolesFromUser", "createRole", "dropRole", "setProfilingLevel",
		"fsyncLock", "fsyncUnlock", "shutdownServer", "killOp", "killCursors", "setLogLevel", "setSlaveOk", "setSecondaryOk", "setReadPref")
	execMethods = setOf("runCommand", "adminCommand", "eval", "getSiblingDB", "getMongo", "auth", "logout", "getProfilingStatus", "currentOp", "serverStatus",
		"hostInfo", "getUsers", "getRoles", "getReplicationInfo", "printShardingStatus", "watch", "explain", "getPlanCache", "getShardDistribution")
	cursorNoop = setOf("pretty", "toArray", "batchSize", "noCursorTimeout", "allowDiskUse")
)

// 不允许在 pipeline 里出现的 stage 及原因（不在这里也不在白名单里的 stage 走通用提示）。
var deniedStages = map[string]string{
	"$out":                         "会写入集合",
	"$merge":                       "会写入集合",
	"$collStats":                   "请改用 db.<集合>.stats()",
	"$indexStats":                  "请改用 db.<集合>.getIndexes()",
	"$planCacheStats":              "泄露执行计划缓存",
	"$currentOp":                   "会列出服务端正在执行的操作（含其他人的语句）",
	"$listSessions":                "会列出服务端会话",
	"$listLocalSessions":           "会列出服务端会话",
	"$listSampledQueries":          "会读取其他人采样的查询",
	"$listSearchIndexes":           "属于管理操作",
	"$changeStream":                "是长连接监听，会占住游标",
	"$changeStreamSplitLargeEvent": "是长连接监听，会占住游标",
	"$search":                      "Atlas Search 不支持",
	"$searchMeta":                  "Atlas Search 不支持",
	"$vectorSearch":                "Atlas Search 不支持",
	"$documents":                   "不支持无集合的 aggregate",
	"$geoNear":                     "暂不支持",
	"$queryStats":                  "会读取其他人的查询统计",
	"$shardedDataDistribution":     "属于管理操作",
}

var allowedStages = setOf("$match", "$project", "$group", "$sort", "$limit", "$skip", "$unwind", "$count", "$addFields", "$set", "$unset",
	"$replaceRoot", "$replaceWith", "$lookup", "$facet", "$bucket", "$bucketAuto", "$sortByCount", "$sample", "$densify", "$fill",
	"$setWindowFields", "$redact", "$graphLookup", "$unionWith")

// 任何位置（过滤条件 / 投影 / 表达式 / 嵌套 pipeline）出现这些操作符键都拒绝。
var deniedOperators = map[string]string{
	"$where":       "会在服务端执行 JavaScript",
	"$function":    "会在服务端执行 JavaScript",
	"$accumulator": "会在服务端执行 JavaScript",
	"$out":         "会写入集合",
	"$merge":       "会写入集合",
	"$eval":        "会在服务端执行 JavaScript",
}

func setOf(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var reservedCollNames = setOf("find", "findOne", "count", "countDocuments", "aggregate", "distinct", "getIndexes", "stats", "estimatedDocumentCount",
	"getCollection", "getCollectionNames", "getName", "getDB", "help", "length", "toString", "constructor", "valueOf", "hasOwnProperty")

// systemDBs 是不允许在线查询的库。
var systemDBs = map[string]bool{"admin": true, "local": true, "config": true}

// ValidDBName 校验库名（也用于拒绝系统库）。
func ValidDBName(name string) error {
	if name == "" {
		return uerr("请先选择数据库")
	}
	if len(name) > 63 || strings.ContainsAny(name, "/\\. \"$*<>:|?\x00") {
		return uerr("库名不合法：%q", name)
	}
	if systemDBs[name] {
		return uerr("在线查询不允许访问系统库 %s", name)
	}
	return nil
}

// validCollName 校验集合名；system.* 集合（system.users / system.profile / system.views ...）一律拒绝。
func validCollName(name string) error {
	switch {
	case name == "":
		return uerr("集合名不能为空")
	case len(name) > 255 || strings.ContainsAny(name, "$\x00"):
		return uerr("集合名不合法：%q", name)
	case strings.HasPrefix(name, "system."):
		return uerr("不允许访问系统集合 %s", name)
	case strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") || strings.Contains(name, ".."):
		return uerr("集合名不合法：%q", name)
	}
	return nil
}

// CheckUserQuery 对语句做静态检查与改写（纯静态，不连服务端）。拒绝时返回 *UserError（上层按 bad_query 处理）。
func CheckUserQuery(text string, opts ...CheckOptions) (*Checked, error) {
	var opt CheckOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, uerr("语句为空")
	}
	if len(text) > maxStatementBytes {
		return nil, uerr("语句过长（上限 %d KB）", maxStatementBytes>>10)
	}
	st, err := parseStatement(text)
	if err != nil {
		return nil, err
	}
	c := &Checked{maxRows: opt.MaxRows}
	if st.hasColl {
		if err := validCollName(st.coll); err != nil {
			return nil, err
		}
		c.Collection = st.coll
	}
	if len(st.calls) == 0 {
		return nil, uerr("缺少方法调用，例如 db.%s.find({})", orDefault(st.coll, "<集合>"))
	}
	head := st.calls[0]
	if !st.hasColl {
		err = c.dbLevel(head, st.calls[1:])
	} else {
		err = c.collLevel(head, st.calls[1:])
	}
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	if c.Collection != "" {
		set[c.Collection] = true
	}
	for _, n := range c.Collections {
		set[n] = true
	}
	c.Collections = c.Collections[:0]
	for n := range set {
		c.Collections = append(c.Collections, n)
	}
	sort.Strings(c.Collections)
	c.DBLevel = len(c.Collections) == 0
	c.Statement = c.render()
	return c, nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func methodDenied(c call) error {
	switch {
	case writeMethods[c.name]:
		return uerr("%s 是写操作 / 管理操作，在线查询只读，已禁止", c.name)
	case execMethods[c.name]:
		hint := ""
		switch c.name {
		case "getSiblingDB":
			hint = "；切换库请用页面顶部的库下拉"
		case "explain":
			hint = "（explain 会实际执行查询）"
		}
		return uerr("%s 不在允许的方法清单里，已禁止%s", c.name, hint)
	}
	return nil
}

const supportedHint = "支持：find / findOne / countDocuments / count / estimatedDocumentCount / distinct / aggregate / getIndexes / stats，以及 db.getCollectionNames() / db.stats()"

func (c *Checked) dbLevel(head call, chain []call) error {
	if err := methodDenied(head); err != nil {
		return err
	}
	if len(chain) > 0 {
		return uerr("db.%s() 后面不能再接方法", head.name)
	}
	switch head.name {
	case "getCollectionNames":
		if len(head.args) > 0 {
			return uerr("getCollectionNames 不接受参数")
		}
		c.Kind = KindListCollections
	case "stats":
		if len(head.args) > 0 {
			if len(head.args) != 1 || !head.args[0].hasNum {
				return uerr("db.stats() 的参数应为数字（scale）")
			}
			c.notice("db.stats() 的 scale 参数已忽略，统一按字节返回")
		}
		c.Kind = KindDBStats
	default:
		return uerr("不支持 db.%s()。%s", head.name, supportedHint)
	}
	return nil
}

func (c *Checked) collLevel(head call, chain []call) error {
	if err := methodDenied(head); err != nil {
		return err
	}
	switch head.name {
	case "find", "findOne":
		return c.find(head, chain)
	case "countDocuments", "count":
		if len(chain) > 0 {
			return uerr("%s() 后面不能再接方法", head.name)
		}
		if len(head.args) > 1 {
			if head.name == "countDocuments" {
				return uerr("countDocuments 只支持一个过滤条件参数（limit / skip 等选项请改用 aggregate 的 $skip / $limit / $count）")
			}
			return uerr("count 只支持一个过滤条件参数")
		}
		c.Kind = KindCount
		if len(head.args) == 1 {
			d, err := c.doc(head.args[0], "过滤条件")
			if err != nil {
				return err
			}
			c.Filter = d
		}
	case "estimatedDocumentCount":
		if len(chain) > 0 {
			return uerr("estimatedDocumentCount() 后面不能再接方法")
		}
		if len(head.args) > 0 {
			return uerr("estimatedDocumentCount 不接受参数")
		}
		c.Kind = KindEstimatedCount
	case "distinct":
		return c.distinct(head, chain)
	case "aggregate":
		return c.aggregate(head, chain)
	case "getIndexes":
		if len(chain) > 0 || len(head.args) > 0 {
			return uerr("getIndexes() 不接受参数，也不能再接方法")
		}
		c.Kind = KindListIndexes
	case "stats":
		if len(chain) > 0 {
			return uerr("stats() 后面不能再接方法")
		}
		if len(head.args) > 0 {
			c.notice("stats() 的参数已忽略，统一按字节返回")
		}
		c.Kind = KindCollStats
	default:
		return uerr("不支持 %s。%s", head.name, supportedHint)
	}
	return nil
}

// doc 解码一个文档参数并检查其中的操作符。
func (c *Checked) doc(n node, what string) (bson.D, error) {
	if n.kind != nObject {
		return nil, uerr("%s必须是文档，例如 {\"status\": \"ok\"}", what)
	}
	var d bson.D
	if err := bson.UnmarshalExtJSON([]byte(n.js), false, &d); err != nil {
		return nil, uerr("%s不是合法的 MongoDB Extended JSON：%s", what, trimErr(err))
	}
	if d == nil {
		d = bson.D{}
	}
	if err := walkValue(d, 0); err != nil {
		return nil, err
	}
	return d, nil
}

func trimErr(err error) string {
	s := err.Error()
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return s
}

func (c *Checked) intArg(m call, _ string) (int64, error) {
	if len(m.args) != 1 || !m.args[0].hasNum || !m.args[0].isInt || m.args[0].i64 < 0 || m.args[0].i64 > 1<<40 {
		return 0, uerr("%s() 的参数必须是非负整数", m.name)
	}
	return m.args[0].i64, nil
}

func (c *Checked) find(head call, chain []call) error {
	if len(head.args) > 2 {
		return uerr("%s 最多两个参数（过滤条件, 投影）", head.name)
	}
	c.Kind = KindFind
	if len(head.args) >= 1 {
		d, err := c.doc(head.args[0], "过滤条件")
		if err != nil {
			return err
		}
		c.Filter = d
	}
	if len(head.args) == 2 {
		d, err := c.doc(head.args[1], "投影")
		if err != nil {
			return err
		}
		c.Projection = d
	}
	if head.name == "findOne" {
		if len(chain) > 0 {
			return uerr("findOne() 后面不能再接方法")
		}
		c.Limit = 1
		return nil
	}
	seen := map[string]bool{}
	for i, m := range chain {
		if err := methodDenied(m); err != nil {
			return err
		}
		if seen[m.name] && m.name != "pretty" && m.name != "toArray" {
			return uerr("游标方法 %s 重复出现", m.name)
		}
		seen[m.name] = true
		switch m.name {
		case "sort":
			if len(m.args) != 1 {
				return uerr("sort() 需要一个文档参数，例如 sort({\"_id\": -1})")
			}
			d, err := c.doc(m.args[0], "sort 的参数")
			if err != nil {
				return err
			}
			c.Sort = d
		case "limit":
			n, err := c.intArg(m, "limit")
			if err != nil {
				return err
			}
			c.Limit = n
		case "skip":
			n, err := c.intArg(m, "skip")
			if err != nil {
				return err
			}
			c.Skip = n
		case "count", "size", "itcount":
			if i != len(chain)-1 {
				return uerr("%s() 必须放在链的最后", m.name)
			}
			c.Kind = KindCount
			if c.Sort != nil || c.Skip > 0 || c.Limit > 0 {
				c.notice("%s() 统计满足过滤条件的全部文档，链上的 sort / skip / limit 被忽略", m.name)
			}
			c.Sort, c.Skip, c.Limit, c.Projection = nil, 0, 0, nil
		case "maxTimeMS":
			c.notice("maxTimeMS 已忽略，执行时间上限由系统统一设置")
		case "hint", "collation", "comment", "min", "max", "returnKey", "showRecordId", "readPref", "readConcern", "tailable", "addOption", "next", "forEach", "map":
			return uerr("游标方法 %s 不支持", m.name)
		default:
			if cursorNoop[m.name] {
				if m.name == "allowDiskUse" {
					c.notice("allowDiskUse 已忽略：在线查询不允许使用磁盘临时文件")
				}
				continue
			}
			return uerr("不支持的游标方法 %s（支持 sort / limit / skip / count / pretty / toArray）", m.name)
		}
	}
	if c.Kind == KindFind {
		c.clampLimit()
	}
	return nil
}

// clampLimit 缺省补 limit / 收窄超限的 limit（仅当 CheckOptions.MaxRows 给出时）。
func (c *Checked) clampLimit() {
	if c.maxRows <= 0 {
		return
	}
	switch {
	case c.Limit == 0:
		c.notice("未指定 limit，结果最多返回 %d 条（可用 skip / limit 分页）", c.maxRows)
	case c.Limit > int64(c.maxRows):
		c.notice("limit %d 超过上限，已收窄为 %d", c.Limit, c.maxRows)
		c.Limit = int64(c.maxRows)
	}
}

func (c *Checked) distinct(head call, chain []call) error {
	if len(chain) > 0 {
		return uerr("distinct() 后面不能再接方法")
	}
	if len(head.args) < 1 || len(head.args) > 2 || head.args[0].kind != nString || head.args[0].str == "" {
		return uerr("distinct 的第一个参数必须是字段名字符串，例如 distinct(\"status\", {...})")
	}
	if strings.ContainsRune(head.args[0].str, 0) {
		return uerr("字段名不合法")
	}
	c.Kind = KindDistinct
	c.Field = head.args[0].str
	if len(head.args) == 2 {
		d, err := c.doc(head.args[1], "过滤条件")
		if err != nil {
			return err
		}
		c.Filter = d
	}
	return nil
}

var allowedAggOptions = setOf("allowDiskUse", "batchSize", "cursor", "maxTimeMS")

func (c *Checked) aggregate(head call, chain []call) error {
	for _, m := range chain {
		if err := methodDenied(m); err != nil {
			return err
		}
		if !cursorNoop[m.name] {
			return uerr("aggregate() 后面只能接 toArray() / pretty()，不支持 %s", m.name)
		}
	}
	c.Kind = KindAggregate
	args := head.args
	if len(args) == 0 {
		return uerr("aggregate 需要 pipeline 数组，例如 aggregate([{\"$match\": {...}}])")
	}
	var pipeNode node
	if args[0].kind == nArray {
		pipeNode = args[0]
		if len(args) > 2 {
			return uerr("aggregate 最多两个参数（pipeline, 选项）")
		}
		if len(args) == 2 {
			if args[1].kind != nObject {
				return uerr("aggregate 的第二个参数（选项）必须是文档")
			}
			var o bson.D
			if err := bson.UnmarshalExtJSON([]byte(args[1].js), false, &o); err != nil {
				return uerr("aggregate 选项不是合法的 Extended JSON：%s", trimErr(err))
			}
			for _, e := range o {
				if !allowedAggOptions[e.Key] {
					return uerr("aggregate 选项 %s 不支持（只接受 allowDiskUse / batchSize / cursor / maxTimeMS，其中 allowDiskUse 与 maxTimeMS 会被忽略）", e.Key)
				}
				switch e.Key {
				case "allowDiskUse":
					c.notice("allowDiskUse 已忽略：在线查询不允许使用磁盘临时文件")
				case "maxTimeMS":
					c.notice("maxTimeMS 已忽略，执行时间上限由系统统一设置")
				}
			}
		}
	} else { // 旧写法 aggregate({stage1}, {stage2}, ...)
		var sb strings.Builder
		sb.WriteByte('[')
		for i, a := range args {
			if a.kind != nObject {
				return uerr("aggregate 的参数必须是 pipeline 数组，或依次列出的 stage 文档")
			}
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(a.js)
		}
		sb.WriteByte(']')
		pipeNode = node{js: sb.String(), kind: nArray}
	}
	var wrap bson.D
	if err := bson.UnmarshalExtJSON([]byte(`{"p":`+pipeNode.js+`}`), false, &wrap); err != nil || len(wrap) != 1 {
		return uerr("pipeline 不是合法的 MongoDB Extended JSON：%s", trimErrNil(err))
	}
	arr, ok := wrap[0].Value.(bson.A)
	if !ok {
		return uerr("pipeline 必须是数组")
	}
	p, err := c.pipeline(arr, 0)
	if err != nil {
		return err
	}
	c.Pipeline = p
	return nil
}

func trimErrNil(err error) string {
	if err == nil {
		return "格式错误"
	}
	return trimErr(err)
}

// pipeline 检查并（必要时）改写一个 pipeline，同时登记涉及的集合。
func (c *Checked) pipeline(arr bson.A, depth int) ([]bson.D, error) {
	if depth > MaxNestedPipelines {
		return nil, uerr("pipeline 嵌套层数超过 %d 层", MaxNestedPipelines)
	}
	if len(arr) > MaxPipelineStages {
		return nil, uerr("pipeline 的 stage 数超过 %d 个", MaxPipelineStages)
	}
	out := make([]bson.D, 0, len(arr))
	for i, s := range arr {
		sd, ok := s.(bson.D)
		if !ok || len(sd) != 1 {
			return nil, uerr("pipeline 第 %d 个 stage 必须是只含一个 $stage 键的文档", i+1)
		}
		name := sd[0].Key
		if why, bad := deniedStages[name]; bad {
			return nil, uerr("pipeline 第 %d 个 stage %s 已禁止：%s", i+1, name, why)
		}
		if !allowedStages[name] {
			return nil, uerr("pipeline 第 %d 个 stage %s 不在允许的清单里（允许：%s）", i+1, name, allowedStageList())
		}
		if err := walkValue(sd[0].Value, 0); err != nil {
			return nil, err
		}
		stage, err := c.stage(sd, depth)
		if err != nil {
			return nil, err
		}
		out = append(out, stage)
	}
	return out, nil
}

var stageList = func() string {
	names := make([]string, 0, len(allowedStages))
	for n := range allowedStages {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, " ")
}()

func allowedStageList() string { return stageList }

func (c *Checked) subPipeline(v any, what string, depth int) ([]bson.D, error) {
	arr, ok := v.(bson.A)
	if !ok {
		return nil, uerr("%s 的 pipeline 必须是数组", what)
	}
	return c.pipeline(arr, depth+1)
}

func asDoc(v any) (bson.D, bool) { d, ok := v.(bson.D); return d, ok }

func (c *Checked) fromColl(v any, stage string) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", uerr("%s 的 from 必须是同库的集合名字符串（不支持跨库 / 对象写法）", stage)
	}
	if err := validCollName(s); err != nil {
		return "", uerr("%s 的 from：%s", stage, err.Error())
	}
	c.Collections = append(c.Collections, s)
	return s, nil
}

func (c *Checked) stage(sd bson.D, depth int) (bson.D, error) {
	name, val := sd[0].Key, sd[0].Value
	switch name {
	case "$facet":
		d, ok := asDoc(val)
		if !ok {
			return nil, uerr("$facet 的参数必须是文档（名称 → pipeline）")
		}
		nd := make(bson.D, 0, len(d))
		for _, e := range d {
			p, err := c.subPipeline(e.Value, "$facet."+e.Key, depth)
			if err != nil {
				return nil, err
			}
			nd = append(nd, bson.E{Key: e.Key, Value: toA(p)})
		}
		return bson.D{{Key: name, Value: nd}}, nil
	case "$lookup":
		d, ok := asDoc(val)
		if !ok {
			return nil, uerr("$lookup 的参数必须是文档")
		}
		nd := make(bson.D, 0, len(d))
		hasFrom := false
		for _, e := range d {
			switch e.Key {
			case "from":
				hasFrom = true
				if _, err := c.fromColl(e.Value, "$lookup"); err != nil {
					return nil, err
				}
				nd = append(nd, e)
			case "pipeline":
				p, err := c.subPipeline(e.Value, "$lookup", depth)
				if err != nil {
					return nil, err
				}
				nd = append(nd, bson.E{Key: e.Key, Value: toA(p)})
			case "localField", "foreignField", "as", "let":
				nd = append(nd, e)
			default:
				return nil, uerr("$lookup 不支持选项 %s", e.Key)
			}
		}
		if !hasFrom {
			return nil, uerr("$lookup 必须指定 from（同库集合名）")
		}
		return bson.D{{Key: name, Value: nd}}, nil
	case "$graphLookup":
		d, ok := asDoc(val)
		if !ok {
			return nil, uerr("$graphLookup 的参数必须是文档")
		}
		nd := make(bson.D, 0, len(d)+1)
		hasFrom, hasDepth := false, false
		for _, e := range d {
			switch e.Key {
			case "from":
				hasFrom = true
				if _, err := c.fromColl(e.Value, "$graphLookup"); err != nil {
					return nil, err
				}
				nd = append(nd, e)
			case "maxDepth":
				hasDepth = true
				n, ok := numToInt(e.Value)
				if !ok || n < 0 {
					return nil, uerr("$graphLookup 的 maxDepth 必须是非负整数")
				}
				if n > GraphLookupMaxDepth {
					c.notice("$graphLookup 的 maxDepth %d 超过上限，已改为 %d", n, GraphLookupMaxDepth)
					n = GraphLookupMaxDepth
				}
				nd = append(nd, bson.E{Key: e.Key, Value: n})
			case "startWith", "connectFromField", "connectToField", "as", "depthField", "restrictSearchWithMatch":
				nd = append(nd, e)
			default:
				return nil, uerr("$graphLookup 不支持选项 %s", e.Key)
			}
		}
		if !hasFrom {
			return nil, uerr("$graphLookup 必须指定 from（同库集合名）")
		}
		if !hasDepth {
			nd = append(nd, bson.E{Key: "maxDepth", Value: int64(GraphLookupMaxDepth)})
			c.notice("$graphLookup 未指定 maxDepth，已补 %d（递归深度上限）", GraphLookupMaxDepth)
		}
		return bson.D{{Key: name, Value: nd}}, nil
	case "$unionWith":
		switch v := val.(type) {
		case string:
			if _, err := c.fromColl(v, "$unionWith"); err != nil {
				return nil, err
			}
			return sd, nil
		case bson.D:
			nd := make(bson.D, 0, len(v))
			hasColl := false
			for _, e := range v {
				switch e.Key {
				case "coll":
					hasColl = true
					if _, err := c.fromColl(e.Value, "$unionWith"); err != nil {
						return nil, err
					}
					nd = append(nd, e)
				case "pipeline":
					p, err := c.subPipeline(e.Value, "$unionWith", depth)
					if err != nil {
						return nil, err
					}
					nd = append(nd, bson.E{Key: e.Key, Value: toA(p)})
				default:
					return nil, uerr("$unionWith 不支持选项 %s", e.Key)
				}
			}
			if !hasColl {
				return nil, uerr("$unionWith 必须指定 coll（同库集合名；不支持 $documents）")
			}
			return bson.D{{Key: name, Value: nd}}, nil
		}
		return nil, uerr("$unionWith 的参数必须是集合名字符串或 {coll, pipeline} 文档")
	}
	return sd, nil
}

func toA(p []bson.D) bson.A {
	a := make(bson.A, len(p))
	for i, d := range p {
		a[i] = d
	}
	return a
}

func numToInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		if n == math.Trunc(n) && math.Abs(n) < 1<<53 {
			return int64(n), true
		}
	}
	return 0, false
}

// walkValue 递归检查文档 / 数组里的每个键：任何位置出现被禁止的操作符（$where / $function / $accumulator / $out / $merge ...）都拒绝，
// 以 "$_" 开头的内部操作符（$_internalXxx）也一律拒绝。
func walkValue(v any, depth int) error {
	if depth > 100 {
		return uerr("文档嵌套层数过深")
	}
	switch x := v.(type) {
	case bson.D:
		for _, e := range x {
			if why, bad := deniedOperators[e.Key]; bad {
				return uerr("%s 已禁止：%s", e.Key, why)
			}
			if strings.HasPrefix(e.Key, "$_") {
				return uerr("内部操作符 %s 已禁止", e.Key)
			}
			if err := walkValue(e.Value, depth+1); err != nil {
				return err
			}
		}
	case bson.A:
		for _, e := range x {
			if err := walkValue(e, depth+1); err != nil {
				return err
			}
		}
	case bson.JavaScript, bson.CodeWithScope:
		return uerr("不允许在查询里使用 JavaScript 代码")
	}
	return nil
}

// ---- 规范化文本 ----

func extJSON(v any) string {
	b, err := bson.MarshalExtJSON(v, false, false)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func (c *Checked) collExpr() string {
	if identRe.MatchString(c.Collection) && !reservedCollNames[c.Collection] {
		return "db." + c.Collection
	}
	return "db.getCollection(" + strconv.Quote(c.Collection) + ")"
}

func (c *Checked) render() string {
	switch c.Kind {
	case KindListCollections:
		return "db.getCollectionNames()"
	case KindDBStats:
		return "db.stats()"
	}
	base := c.collExpr()
	switch c.Kind {
	case KindListIndexes:
		return base + ".getIndexes()"
	case KindCollStats:
		return base + ".stats()"
	case KindEstimatedCount:
		return base + ".estimatedDocumentCount()"
	case KindCount:
		if c.Filter == nil {
			return base + ".countDocuments({})"
		}
		return base + ".countDocuments(" + extJSON(c.Filter) + ")"
	case KindDistinct:
		s := base + ".distinct(" + strconv.Quote(c.Field)
		if c.Filter != nil {
			s += ", " + extJSON(c.Filter)
		}
		return s + ")"
	case KindAggregate:
		parts := make([]string, len(c.Pipeline))
		for i, st := range c.Pipeline {
			parts[i] = extJSON(st)
		}
		return base + ".aggregate([" + strings.Join(parts, ", ") + "])"
	}
	s := base + ".find("
	if c.Filter != nil || c.Projection != nil {
		if c.Filter == nil {
			s += "{}"
		} else {
			s += extJSON(c.Filter)
		}
		if c.Projection != nil {
			s += ", " + extJSON(c.Projection)
		}
	}
	s += ")"
	if c.Sort != nil {
		s += ".sort(" + extJSON(c.Sort) + ")"
	}
	if c.Skip > 0 {
		s += ".skip(" + strconv.FormatInt(c.Skip, 10) + ")"
	}
	if c.Limit > 0 {
		s += ".limit(" + strconv.FormatInt(c.Limit, 10) + ")"
	}
	return s
}
