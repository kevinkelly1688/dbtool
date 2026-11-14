// @author: kevinkelly
// @license: Apache Licence
// @file: testbin_test.go
// @time: 2026/10/28

package binlog

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"testing"

	"github.com/go-mysql-org/go-mysql/replication"
)

// 测试用的 binlog 原始事件构造器：按 MySQL 5.5 的格式（无校验和）手工拼出事件字节，再交给 go-mysql 的解析器解码，
// 与真实服务端发来的字节走同一条解码路径。

const (
	tTiny     = 1
	tLong     = 3
	tLongLong = 8
	tVarchar  = 15
)

type binBuilder struct {
	pos      uint32
	ts       uint32
	raws     [][]byte
	starts   []uint32 // 每个事件的起始位置（与 raws 对应）
	tableID  uint64
	curTable *tbl
}

type tbl struct {
	schema, name string
	types        []byte
	metas        []uint16 // 仅 VARCHAR 用到：最大长度
}

func newBin(ts uint32) *binBuilder { return &binBuilder{pos: 4, ts: ts, tableID: 100} }

func le(n int, v uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, v)
	return b[:n]
}

func lenenc(n int) []byte {
	if n < 251 {
		return []byte{byte(n)}
	}
	return append([]byte{0xfc}, le(2, uint64(n))...)
}

// add 追加一个事件，返回它的起止位置。
func (b *binBuilder) add(typ replication.EventType, body []byte, ts uint32) (start, end uint32) {
	size := uint32(19 + len(body))
	start, end = b.pos, b.pos+size
	h := make([]byte, 0, 19)
	h = append(h, le(4, uint64(ts))...)
	h = append(h, byte(typ))
	h = append(h, le(4, 1)...)
	h = append(h, le(4, uint64(size))...)
	h = append(h, le(4, uint64(end))...)
	h = append(h, le(2, 0)...)
	b.raws = append(b.raws, append(h, body...))
	b.starts = append(b.starts, start)
	b.pos = end
	return start, end
}

func (b *binBuilder) fde() {
	body := le(2, 4)
	ver := make([]byte, 50)
	copy(ver, "5.5.0-fake")
	body = append(body, ver...)
	body = append(body, le(4, uint64(b.ts))...)
	body = append(body, 19)
	post := make([]byte, 40)
	for i := range post {
		post[i] = 10
	}
	post[replication.TABLE_MAP_EVENT-1] = 8
	body = append(body, post...)
	b.add(replication.FORMAT_DESCRIPTION_EVENT, body, b.ts)
}

func (b *binBuilder) query(schema, q string, ts uint32) (uint32, uint32) {
	body := le(4, 1)
	body = append(body, le(4, 0)...)
	body = append(body, byte(len(schema)))
	body = append(body, le(2, 0)...)
	body = append(body, le(2, 0)...)
	body = append(body, schema...)
	body = append(body, 0)
	body = append(body, q...)
	return b.add(replication.QUERY_EVENT, body, ts)
}

func (b *binBuilder) begin(ts uint32)  { b.query("", "BEGIN", ts) }
func (b *binBuilder) commit(ts uint32) { b.add(replication.XID_EVENT, le(8, 7), ts) }

func (b *binBuilder) tableMap(t *tbl, ts uint32) {
	b.tableID++
	b.curTable = t
	body := le(6, b.tableID)
	body = append(body, le(2, 0)...)
	body = append(body, byte(len(t.schema)))
	body = append(body, t.schema...)
	body = append(body, 0, byte(len(t.name)))
	body = append(body, t.name...)
	body = append(body, 0)
	body = append(body, lenenc(len(t.types))...)
	body = append(body, t.types...)
	var meta []byte
	mi := 0
	for _, ty := range t.types {
		if ty == tVarchar {
			meta = append(meta, le(2, uint64(t.metas[mi]))...)
			mi++
		}
	}
	body = append(body, lenenc(len(meta))...)
	body = append(body, meta...)
	body = append(body, make([]byte, (len(t.types)+7)/8)...) // 全部 NOT NULL；是否可空对解码没有影响
	b.add(replication.TABLE_MAP_EVENT, body, ts)
}

// image 把一行编码成行镜像。present 为 nil 表示所有列都在；nil 值是 NULL。
func (t *tbl) image(vals []any, present []bool) []byte {
	n := 0
	for i := range t.types {
		if present == nil || present[i] {
			n++
		}
	}
	nullBits := make([]byte, (n+7)/8)
	var data []byte
	k := 0
	vi := 0
	for i, ty := range t.types {
		if present != nil && !present[i] {
			continue
		}
		v := vals[i]
		if v == nil {
			nullBits[k/8] |= 1 << (uint(k) % 8)
		} else {
			switch ty {
			case tTiny:
				data = append(data, byte(v.(int)))
			case tLong:
				data = append(data, le(4, uint64(uint32(v.(int))))...)
			case tLongLong:
				data = append(data, le(8, uint64(v.(int64)))...)
			case tVarchar:
				s := v.(string)
				if t.metas[vi] < 256 {
					data = append(data, byte(len(s)))
				} else {
					data = append(data, le(2, uint64(len(s)))...)
				}
				data = append(data, s...)
			}
		}
		if ty == tVarchar {
			vi++
		}
		k++
	}
	return append(nullBits, data...)
}

func bitmap(n int, present []bool) []byte {
	bm := make([]byte, (n+7)/8)
	for i := 0; i < n; i++ {
		if present == nil || present[i] {
			bm[i/8] |= 1 << (uint(i) % 8)
		}
	}
	return bm
}

// rows 追加一个行事件。update 时 images 依次是 前、后、前、后…；present1/present2 是前 / 后镜像的列位图（nil 表示全部）。
func (b *binBuilder) rows(typ replication.EventType, ts uint32, present1, present2 []bool, rows ...[]any) (uint32, uint32) {
	t := b.curTable
	body := le(6, b.tableID)
	body = append(body, le(2, 1)...) // STMT_END
	body = append(body, le(2, 2)...) // v2：没有 extra data
	body = append(body, lenenc(len(t.types))...)
	body = append(body, bitmap(len(t.types), present1)...)
	if typ == replication.UPDATE_ROWS_EVENTv2 {
		body = append(body, bitmap(len(t.types), present2)...)
	}
	for i, r := range rows {
		pr := present1
		if typ == replication.UPDATE_ROWS_EVENTv2 && i%2 == 1 {
			pr = present2
		}
		body = append(body, t.image(r, pr)...)
	}
	return b.add(typ, body, ts)
}

func (b *binBuilder) insert(ts uint32, rows ...[]any) (uint32, uint32) {
	return b.rows(replication.WRITE_ROWS_EVENTv2, ts, nil, nil, rows...)
}
func (b *binBuilder) update(ts uint32, before, after []any) (uint32, uint32) {
	return b.rows(replication.UPDATE_ROWS_EVENTv2, ts, nil, nil, before, after)
}
func (b *binBuilder) delete(ts uint32, rows ...[]any) (uint32, uint32) {
	return b.rows(replication.DELETE_ROWS_EVENTv2, ts, nil, nil, rows...)
}

// events 把原始字节依次交给解析器解码，得到与真实流一致的事件对象。
func (b *binBuilder) events(tb testing.TB) []*replication.BinlogEvent {
	tb.Helper()
	p := replication.NewBinlogParser()
	var out []*replication.BinlogEvent
	for _, raw := range b.raws {
		ev, err := p.Parse(raw)
		if err != nil {
			tb.Fatalf("解码事件失败: %v", err)
		}
		out = append(out, ev)
	}
	return out
}

// rotate 生成开头的伪 Rotate 事件（真实服务端拉取时的第一个事件，LogPos 为 0）。
func rotateRaw(name string) []byte {
	body := append(le(8, 4), name...)
	h := make([]byte, 0, 19)
	h = append(h, le(4, 0)...)
	h = append(h, byte(replication.ROTATE_EVENT))
	h = append(h, le(4, 1)...)
	h = append(h, le(4, uint64(19+len(body)))...)
	h = append(h, le(4, 0)...)
	h = append(h, le(2, 0)...)
	return append(h, body...)
}

func rotateEvent(tb testing.TB, name string) *replication.BinlogEvent {
	tb.Helper()
	ev, err := replication.NewBinlogParser().Parse(rotateRaw(name))
	if err != nil {
		tb.Fatal(err)
	}
	return ev
}

// memStream 是内存里的事件流。
type memStream struct {
	evs    []*replication.BinlogEvent
	closed bool
}

func (m *memStream) Next(ctx context.Context) (*replication.BinlogEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(m.evs) == 0 {
		return nil, io.EOF
	}
	ev := m.evs[0]
	m.evs = m.evs[1:]
	return ev, nil
}
func (m *memStream) Close() error { m.closed = true; return nil }

// fileBytes 拼出完整的 binlog 文件内容（魔数 + 事件）。
func (b *binBuilder) fileBytes() []byte {
	return append([]byte{0xfe, 'b', 'i', 'n'}, bytes.Join(b.raws, nil)...)
}

// ---- 假元数据 ----

type fakeMeta struct {
	cols    map[string][]colMeta // "schema.table"（小写）
	err     error
	queries int
}

func (f *fakeMeta) Columns(_ context.Context, schema, table string) ([]colMeta, error) {
	f.queries++
	if f.err != nil {
		return nil, f.err
	}
	return f.cols[schema+"."+table], nil
}

// orders: id INT PK, name VARCHAR(50), qty TINYINT UNSIGNED, note VARCHAR(50)
var ordersTbl = &tbl{schema: "shop", name: "orders", types: []byte{tLong, tVarchar, tTiny, tVarchar}, metas: []uint16{200, 200}}

var ordersCols = []colMeta{
	{Name: "id", DataType: "int", ColumnType: "int", PK: true},
	{Name: "name", DataType: "varchar", ColumnType: "varchar(50)"},
	{Name: "qty", DataType: "tinyint", ColumnType: "tinyint unsigned", Unsigned: true},
	{Name: "note", DataType: "varchar", ColumnType: "varchar(50)"},
}

func newFakeMeta() *fakeMeta {
	return &fakeMeta{cols: map[string][]colMeta{"shop.orders": ordersCols}}
}
