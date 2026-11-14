// @author: kevinkelly
// @license: Apache Licence
// @file: viewexp.go
// @time: 2026/10/28

package query

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/masking"
	"dbtool/internal/tsql"
)

// 视图展开（数据脱敏）：配置写在基表字段上时，经过视图改名、套表达式、套另一个视图之后，结果列的名字和语句文本里都看不出
// 它来自敏感字段，来源信息也只到视图本身（见 internal/masking/viewexp.go）。这里负责取视图定义：
//
//  1. 从语句里找出表位置上的对象名，到目标库的视图目录里按名字查（engine.ViewDefiner）；
//  2. 查到的视图定义里又用到的对象名继续查，直到没有新的视图（嵌套有层数上限）；
//  3. 把定义交给保守匹配（masking.ConservativeHitsViews），来源是视图的结果列也要再按它补一遍。
//
// 取不到的视图（没有权限、加密、定义看不懂、目录查询失败、嵌套太深）按脱敏失败处理：返回 *viewDefError，
// 由调用方按 query.block_on_mask_error 决定阻断还是放行（放行时其余的列已经按配置脱敏，只有经过这些视图的列可能漏）。
// 引擎没有视图目录（engine.ErrViewDefsUnsupported）或不提供视图定义（没有实现 engine.ViewDefiner）时不展开，也不算失败。

const (
	maxViewDepth = 8   // 视图嵌套的最大层数
	maxViewRefs  = 300 // 一条语句连同视图定义里最多查多少个对象名
	maxViewCount = 100 // 最多展开多少个视图
	viewRefChunk = 100 // 一次目录查询最多带多少个对象名

	viewDefsTimeout = 20 * time.Second
)

// viewDefError 表示视图定义没有读到或没有读全：语句里用到的某些视图没有展开，经过它们的结果列可能没有脱敏。
type viewDefError struct {
	Views  []string // 定义读不到（没有权限、已加密）或看不懂的视图
	Reason string   // 其它原因：目录查询失败、嵌套太深、引用的对象太多等
}

func (e *viewDefError) Error() string {
	var parts []string
	if len(e.Views) > 0 {
		parts = append(parts, "视图定义读不到或看不懂："+strings.Join(e.Views, ", ")) // i18n-ignore 只进日志
	}
	if e.Reason != "" {
		parts = append(parts, e.Reason)
	}
	return strings.Join(parts, "; ")
}

// 放行未（完整）脱敏的数据时给用户的提示（query.block_on_mask_error 关闭）。
const (
	viewUnreadableNotice = "视图 %s 的定义读不到或看不懂（没有权限、已加密等），经过这些视图的结果列可能没有脱敏（系统设置 query.block_on_mask_error 为关闭状态）。"
	viewLookupNotice     = "读取视图定义失败，经过视图的结果列可能没有脱敏（系统设置 query.block_on_mask_error 为关闭状态）。"
)

func (e *viewDefError) notice() string {
	var parts []string
	if len(e.Views) > 0 {
		parts = append(parts, fmt.Sprintf(viewUnreadableNotice, strings.Join(e.Views, ", ")))
	}
	if e.Reason != "" {
		parts = append(parts, viewLookupNotice)
	}
	return joinNotice(parts...)
}

// viewCatalogWarned 记录已经警告过“没有视图定义目录”的引擎类型。
var viewCatalogWarned sync.Map

// viewExpansion 是展开出来的视图：交给保守匹配的查询体（bodies），以及目录里找到的全部视图（views，含读不到定义的，给来源标记用）。
type viewExpansion struct {
	bodies []masking.ViewBody
	views  []engine.ViewDef
}

func (x *viewExpansion) bodiesOf() []masking.ViewBody {
	if x == nil {
		return nil
	}
	return x.bodies
}

// isView 判断（库 / 架构, 表）是不是展开过的视图。goInception 给的来源表名不一定带库名，库名对不上时只比名字。
func (x *viewExpansion) isView(schema, table string) bool {
	if x == nil {
		return false
	}
	if i := strings.LastIndex(table, "."); i >= 0 {
		table = table[i+1:]
	}
	for _, v := range x.views {
		if strings.EqualFold(v.Name, table) && (schema == "" || v.Schema == "" || strings.EqualFold(v.Schema, schema)) {
			return true
		}
	}
	return false
}

// expandViews 读出 sql 用到的视图的定义。返回的 *viewExpansion 在出错时也尽量带上已经读到的部分（可能为 nil）；
// 错误是 *viewDefError。引擎不提供视图定义时返回 nil, nil。
func (s *Service) expandViews(ctx context.Context, e engine.Engine, dbType, db, sql string) (*viewExpansion, error) {
	vd, ok := e.(engine.ViewDefiner)
	if !ok {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, viewDefsTimeout)
	defer cancel()
	d := dialectOf(dbType)
	x := &viewExpansion{}
	verr := &viewDefError{}
	seen := map[string]bool{}       // 已经查过的对象名
	registered := map[string]bool{} // 已经登记的视图
	refs := masking.TableNames(sql, d)
	total := 0
walk:
	for depth := 0; len(refs) > 0; depth++ {
		var todo []engine.ViewRef
		for _, r := range refs {
			k := viewKey(dbType, r.Catalog, r.Schema, r.Name)
			if !seen[k] {
				seen[k] = true
				todo = append(todo, engine.ViewRef{Catalog: r.Catalog, Schema: r.Schema, Name: r.Name})
			}
		}
		if len(todo) == 0 {
			break
		}
		if depth >= maxViewDepth {
			verr.Reason = fmt.Sprintf("视图嵌套超过 %d 层", maxViewDepth) // i18n-ignore 只进日志
			break
		}
		if total += len(todo); total > maxViewRefs {
			verr.Reason = fmt.Sprintf("语句和视图定义里引用的对象超过 %d 个", maxViewRefs) // i18n-ignore 只进日志
			break
		}
		var next []masking.NameRef
		for i := 0; i < len(todo); i += viewRefChunk {
			defs, err := vd.ViewDefs(ctx, db, todo[i:min(i+viewRefChunk, len(todo))])
			if err != nil {
				if errors.Is(err, engine.ErrViewDefsUnsupported) {
					if _, dup := viewCatalogWarned.LoadOrStore(dbType, struct{}{}); !dup { // 每次脱敏的查询都会走到这里，每种引擎只警告一次
						slog.Warn("目标库没有可查询的视图定义目录，数据脱敏不展开视图", "dbType", dbType, "err", err)
					}
					break walk // 不算失败；但前面已经发现的读不到定义的视图仍要报
				}
				slog.Error("读取视图定义失败", "dbType", dbType, "db", db, "err", err)
				verr.Reason = "读取视图定义失败：" + err.Error() // i18n-ignore 只进日志
				break walk
			}
			for _, def := range defs {
				dk := viewKey(dbType, def.Catalog, def.Schema, def.Name)
				if registered[dk] {
					continue // 同一个视图被不同写法（带库名 / 不带）查到了两次
				}
				registered[dk] = true
				seen[dk] = true                // 别的视图定义里带库名写的同一个视图不用再查
				x.views = append(x.views, def) // 读不到定义的视图也要登记：来源是它的列不能按“普通表的列”精确匹配了事
				label := viewLabel(def)
				body, err := viewBodyOf(dbType, def)
				if err == nil {
					err = masking.CheckViewBody(body.Body, d)
				}
				if err != nil {
					slog.Warn("视图定义读不到或看不懂，数据脱敏无法展开它", "view", label, "err", err)
					verr.Views = append(verr.Views, label)
					continue
				}
				if len(x.bodies) >= maxViewCount {
					verr.Reason = fmt.Sprintf("语句涉及的视图超过 %d 个", maxViewCount) // i18n-ignore 只进日志
					break walk
				}
				x.bodies = append(x.bodies, body)
				// 视图定义里没写库名 / 架构的名字，按这个视图自己所在的位置解析（PostgreSQL 按搜索路径，不补）。
				nested := masking.TableNames(body.Body, d)
				for j := range nested {
					switch {
					case isMSSQL(dbType):
						if nested[j].Catalog == "" {
							nested[j].Catalog = def.Catalog
						}
					case dbType != dbTypePG:
						if nested[j].Schema == "" {
							nested[j].Schema = def.Schema
						}
					}
				}
				next = append(next, nested...)
			}
		}
		refs = next
	}
	if len(verr.Views) > 0 || verr.Reason != "" {
		return x.orNil(), verr
	}
	return x.orNil(), nil
}

// viewKey 是对象名去重用的键。MySQL 系、SQL Server 的名字按不区分大小写处理；PostgreSQL（加引号的名字）、ClickHouse、Oracle、达梦区分大小写。
func viewKey(dbType string, parts ...string) string {
	k := strings.Join(parts, string(rune(0)))
	if dbType == dbTypePG || dbType == dbTypeCH || dbType == dbTypeOracle || dbType == dbTypeDM {
		return k
	}
	return strings.ToLower(k)
}

func (x *viewExpansion) orNil() *viewExpansion {
	if x == nil || len(x.views) == 0 {
		return nil
	}
	return x
}

func viewLabel(def engine.ViewDef) string {
	var parts []string
	for _, p := range []string{def.Catalog, def.Schema, def.Name} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ".")
}

// viewBodyOf 取出视图定义里的查询体。SQL Server 存的是完整的 CREATE VIEW … AS …，其余引擎只有 SELECT。
func viewBodyOf(dbType string, def engine.ViewDef) (masking.ViewBody, error) {
	text := strings.TrimSpace(def.Definition)
	if text == "" {
		return masking.ViewBody{}, errors.New("定义为空（没有权限或已加密）") // i18n-ignore 只进日志
	}
	if isMSSQL(dbType) {
		return mssqlViewBody(text)
	}
	return masking.ViewBody{Body: strings.TrimRight(text, "; \t\r\n"), Positional: def.ColumnNames}, nil
}

// mssqlViewBody 去掉 CREATE [OR ALTER] VIEW 名字 [(列清单)] [WITH 选项] AS 这个头，列清单（有的话）一并取出。
func mssqlViewBody(text string) (masking.ViewBody, error) {
	sig := tsql.Significant(tsql.Tokenize(text))
	isWord := func(t tsql.Token, w string) bool { return t.Kind == tsql.Word && strings.EqualFold(t.Text, w) }
	isPunct := func(t tsql.Token, s string) bool { return t.Kind == tsql.Punct && t.Text == s }
	i := 0
	for i < len(sig) && !isWord(sig[i], "view") {
		i++
	}
	if i >= len(sig) {
		return masking.ViewBody{}, errors.New("认不出 CREATE VIEW 头") // i18n-ignore 只进日志
	}
	i++
	for i < len(sig) && (sig[i].Kind == tsql.Word || sig[i].Kind == tsql.Ident) { // 视图名：名字(.名字)*
		i++
		if i < len(sig) && isPunct(sig[i], ".") {
			i++
			continue
		}
		break
	}
	var cols []string
	if i < len(sig) && isPunct(sig[i], "(") {
		depth := 0
		for ; i < len(sig); i++ {
			t := sig[i]
			if t.Kind == tsql.Punct {
				switch t.Text {
				case "(":
					depth++
				case ")":
					depth--
				}
				if depth == 0 {
					i++
					break
				}
				continue
			}
			if depth == 1 && (t.Kind == tsql.Word || t.Kind == tsql.Ident) {
				cols = append(cols, tsql.Unquote(t.Text))
			}
		}
	}
	for i < len(sig) && !isWord(sig[i], "as") { // 到 AS 之间是 WITH ENCRYPTION, SCHEMABINDING … 之类的选项
		i++
	}
	if i >= len(sig) {
		return masking.ViewBody{}, errors.New("认不出 CREATE VIEW 头里的 AS") // i18n-ignore 只进日志
	}
	body := strings.TrimRight(strings.TrimSpace(text[sig[i].Start+len(sig[i].Text):]), "; \t\r\n")
	return masking.ViewBody{Body: body, Columns: cols}, nil
}
