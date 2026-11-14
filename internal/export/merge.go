// @author: kevinkelly
// @license: Apache Licence
// @file: merge.go
// @time: 2026/10/28

package export

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/xuri/excelize/v2"
)

// 合并导出：批量导出把多个实例的结果按顺序写进同一个文件，内容就是导出的数据本身（不加实例名称列）。
// 表头（CSV 首行、XML 的字段列表、Excel 首行）只写一次，列以第一个写入的实例为准；列对不上的实例整个不写（返回 *ColumnsError）。
// 只有一个实例时，合并文件与单实例导出的文件逐字节相同。

// MergedFileName 是合并导出的文件名（压缩包里 <工单号>/ 目录下）。
func MergedFileName(workflowID uint, format string) string {
	return fmt.Sprintf("merged_%d.%s", workflowID, format)
}

// ColumnsError：这个实例返回的列与合并文件里的列不一致。
type ColumnsError struct{ Want, Got []string }

func (e *ColumnsError) Error() string {
	return fmt.Sprintf("返回的列与合并文件不一致（合并文件的列：%s；这个实例的列：%s）", strings.Join(e.Want, ", "), strings.Join(e.Got, ", "))
}

// LimitError：合并后的行数超过了 Excel 工作表的上限。
type LimitError struct{ Max int }

func (e *LimitError) Error() string {
	return fmt.Sprintf("合并后的行数超过 Excel 的上限（%d 行），请改用 CSV 或缩小范围", e.Max)
}

// Recoverable 判断错误是不是“这个实例写不进去、但文件没有被破坏”（列不一致、Excel 行数超限）；其它错误是写文件失败，不能再继续写。
func Recoverable(err error) bool {
	var ce *ColumnsError
	var le *LimitError
	return errors.As(err, &ce) || errors.As(err, &le)
}

var errBadMerged = errors.New("合并文件的内容格式不对，无法追加")

// SameColumns 判断两个实例返回的列是否一致：个数相同、按顺序逐个同名（忽略大小写）。
func SameColumns(a, b []string) bool { return sameColumns(a, b) }

func sameColumns(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

// MergeWriter 把多个实例的结果依次写进同一个 io.Writer。
type MergeWriter struct {
	w      io.Writer
	format string
	cols   []string // 结果列（以第一次 Add 为准）
	added  bool
	data   int // 已写入的数据行数
	objs   int // JSON：已写入的对象个数
	xrow   int // Excel：已占用的行数（含表头）
	xlsx   *excelize.File
	sw     *excelize.StreamWriter
	b      strings.Builder
}

// NewMergeWriter 创建合并写入器。
func NewMergeWriter(w io.Writer, format string) (*MergeWriter, error) {
	if !ValidFormat(format) {
		return nil, fmt.Errorf("不支持的导出格式：%s", format)
	}
	return &MergeWriter{w: w, format: format}, nil
}

// Columns 返回结果列；还没有成功 Add 过返回 nil。
func (m *MergeWriter) Columns() []string { return m.cols }

// Rows 返回已写入的数据行数（不含表头）。
func (m *MergeWriter) Rows() int { return m.data }

func (m *MergeWriter) flushBuf(force bool) error {
	if m.b.Len() == 0 || (!force && m.b.Len() <= 1<<20) {
		return nil
	}
	_, err := io.WriteString(m.w, m.b.String())
	m.b.Reset()
	return err
}

// Add 写入一个实例的结果。列与第一个实例不一致返回 *ColumnsError，Excel 行数会超限返回 *LimitError，这两种情况都不写任何内容；
// 其它错误是写文件失败，之后不要再用这个写入器。
func (m *MergeWriter) Add(r Result) error {
	first := !m.added
	if first {
		m.cols = append([]string(nil), r.Columns...)
	} else if !sameColumns(m.cols, r.Columns) {
		return &ColumnsError{Want: m.cols, Got: r.Columns}
	}
	if m.format == XLSX {
		need := m.xrow + len(r.Rows)
		if first {
			need++ // 表头
		}
		if need > MaxXLSXRows {
			return &LimitError{Max: MaxXLSXRows}
		}
	}
	var err error
	switch m.format {
	case CSV:
		err = m.addCSV(first, r)
	case JSON:
		err = m.addJSON(r)
	case XML:
		err = m.addXML(first, r)
	case XLSX:
		err = m.addXLSX(first, r)
	case SQL:
		m.sqlRows(r)
	}
	if err != nil {
		return err
	}
	m.added = true
	m.data += len(r.Rows)
	return m.flushBuf(true)
}

// Close 写出文件尾（JSON 的 ]、XML 的收尾标签、Excel 的整个工作簿）。一次都没有成功 Add 过时什么也不写。
func (m *MergeWriter) Close() error {
	if !m.added {
		return nil
	}
	switch m.format {
	case JSON:
		if m.objs == 0 {
			m.b.WriteString("[]")
		} else {
			m.b.WriteString("\n]")
		}
	case XML:
		m.b.WriteString(xmlFooter)
	case XLSX:
		defer m.xlsx.Close()
		if err := m.sw.Flush(); err != nil {
			return err
		}
		return m.xlsx.Write(m.w)
	}
	return m.flushBuf(true)
}

// ---- CSV ----

func csvQuote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// csvCells 把一行数据转成 CSV 单元格文本（NULL 写成 null，与单文件导出一致）。
func csvCells(dst []string, row []any) []string {
	for _, v := range row {
		if v == nil {
			dst = append(dst, "null")
		} else {
			dst = append(dst, text(v))
		}
	}
	return dst
}

func (m *MergeWriter) csvLine(cells []string) {
	for i, c := range cells {
		if i > 0 {
			m.b.WriteByte(',')
		}
		m.b.WriteString(csvQuote(c))
	}
	m.b.WriteString("\r\n")
}

func (m *MergeWriter) addCSV(first bool, r Result) error {
	if first && len(m.cols) > 0 {
		m.csvLine(m.cols)
	}
	cells := make([]string, 0, len(m.cols))
	for _, row := range r.Rows {
		m.csvLine(csvCells(cells[:0], row))
		if err := m.flushBuf(false); err != nil {
			return err
		}
	}
	return nil
}

// ---- JSON ----

func jsonEnc(v any) []byte {
	var o bytes.Buffer
	e := json.NewEncoder(&o)
	e.SetEscapeHTML(false)
	_ = e.Encode(jsonValue(v))
	return bytes.TrimRight(o.Bytes(), "\n")
}

func (m *MergeWriter) jsonObject(row []any) {
	if m.objs == 0 {
		m.b.WriteString("[\n")
	} else {
		m.b.WriteString(",\n")
	}
	m.objs++
	m.b.WriteString("  {\n")
	for j, col := range m.cols {
		var v any
		if j < len(row) {
			v = row[j]
		}
		if j > 0 {
			m.b.WriteString(",\n")
		}
		fmt.Fprintf(&m.b, "    %s: %s", jsonEnc(col), jsonEnc(v))
	}
	m.b.WriteString("\n  }")
}

func (m *MergeWriter) addJSON(r Result) error {
	for _, row := range r.Rows {
		m.jsonObject(row)
		if err := m.flushBuf(false); err != nil {
			return err
		}
	}
	return nil
}

// ---- XML ----

const xmlFooter = "</data></tabledata>"

func (m *MergeWriter) xmlHeader() {
	m.b.WriteString("<?xml version='1.0' encoding='utf-8'?>\n<tabledata><fields>")
	for _, c := range m.cols {
		m.b.WriteString("<field>")
		m.b.WriteString(esc(c))
		m.b.WriteString("</field>")
	}
	m.b.WriteString("</fields><data>")
}

func (m *MergeWriter) xmlRow(id int, row []any) {
	fmt.Fprintf(&m.b, `<row id="%d">`, id)
	for j, v := range row {
		s := "(null)"
		if v != nil {
			s = text(v)
		}
		fmt.Fprintf(&m.b, "<column-%d>%s</column-%d>", j+1, esc(s), j+1)
	}
	m.b.WriteString("</row>")
}

func (m *MergeWriter) addXML(first bool, r Result) error {
	if first {
		m.xmlHeader()
	}
	for i, row := range r.Rows {
		m.xmlRow(m.data+i+1, row)
		if err := m.flushBuf(false); err != nil {
			return err
		}
	}
	return nil
}

// ---- XLSX ----

func xlsxCell(row int) string { c, _ := excelize.CoordinatesToCellName(1, row); return c }

func (m *MergeWriter) addXLSX(first bool, r Result) error {
	if first {
		m.xlsx = excelize.NewFile()
		sw, err := m.xlsx.NewStreamWriter("Sheet1")
		if err != nil {
			m.xlsx.Close()
			m.xlsx = nil
			return err
		}
		m.sw = sw
		head := make([]any, len(m.cols))
		for i, c := range m.cols {
			head[i] = c
		}
		m.xrow = 1
		if err := m.sw.SetRow(xlsxCell(m.xrow), head); err != nil {
			return err
		}
	}
	for _, row := range r.Rows {
		vals := make([]any, len(row))
		for j, v := range row {
			vals[j] = text(v)
		}
		m.xrow++
		if err := m.sw.SetRow(xlsxCell(m.xrow), vals); err != nil {
			return err
		}
	}
	return nil
}

// ---- SQL ----

func (m *MergeWriter) sqlRows(r Result) {
	cols := strings.Join(m.cols, ", ")
	mysql := r.Dialect == "mysql"
	for _, row := range r.Rows {
		vals := make([]string, len(row))
		for i, v := range row {
			vals[i] = sqlLiteral(v, mysql)
		}
		fmt.Fprintf(&m.b, "INSERT INTO your_table_name (%s) VALUES (%s);\n", cols, strings.Join(vals, ", "))
	}
}

// ---- 追加（重试失败实例）----

// AppendMerged 把一个实例的结果追加到已有的合并文件末尾（批量导出里重试失败的实例）。
// 先核对已有文件的列，不一致返回 *ColumnsError，Excel 行数会超限返回 *LimitError，这两种情况文件保持原样。
// 文件不存在或是空的，等同于新建（写表头）。
func AppendMerged(path, format string, r Result) error {
	st, err := os.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err != nil || st.Size() == 0 {
		return createMerged(path, format, r)
	}
	switch format {
	case CSV:
		return appendCSV(path, r)
	case SQL:
		return appendSQL(path, r)
	case JSON:
		return appendJSON(path, r)
	case XML:
		return appendXML(path, r)
	case XLSX:
		return appendXLSX(path, r)
	}
	return fmt.Errorf("不支持的导出格式：%s", format)
}

func createMerged(path, format string, r Result) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	mw, err := NewMergeWriter(f, format)
	if err == nil {
		if err = mw.Add(r); err == nil {
			err = mw.Close()
		}
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// appendOnly 把 build 生成的内容追加到文件末尾。
func appendOnly(path string, build func(m *MergeWriter)) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	m := &MergeWriter{}
	build(m)
	_, err = io.WriteString(f, m.b.String())
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ensureTrailingNewline 保证文件以换行结尾，追加的内容才不会接在最后一行后面。
func ensureTrailingNewline(path, nl string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return err
	}
	var last [1]byte
	if _, err := f.ReadAt(last[:], st.Size()-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	_, err = f.WriteAt([]byte(nl), st.Size())
	return err
}

func appendCSV(path string, r Result) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	head, err := csv.NewReader(f).Read()
	f.Close()
	if err != nil {
		return fmt.Errorf("读取合并文件的表头失败：%w", err)
	}
	if !sameColumns(head, r.Columns) {
		return &ColumnsError{Want: head, Got: r.Columns}
	}
	if err := ensureTrailingNewline(path, "\r\n"); err != nil {
		return err
	}
	return appendOnly(path, func(m *MergeWriter) {
		cells := make([]string, 0, len(r.Columns))
		for _, row := range r.Rows {
			m.csvLine(csvCells(cells[:0], row))
		}
	})
}

var sqlHeadRe = regexp.MustCompile(`^INSERT INTO your_table_name \((.*?)\) VALUES \(`)

func appendSQL(path string, r Result) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	line, err := bufio.NewReader(f).ReadString('\n')
	f.Close()
	if err != nil && err != io.EOF {
		return err
	}
	mt := sqlHeadRe.FindStringSubmatch(line)
	if mt == nil {
		return errBadMerged
	}
	if !strings.EqualFold(mt[1], strings.Join(r.Columns, ", ")) {
		return &ColumnsError{Want: strings.Split(mt[1], ", "), Got: r.Columns}
	}
	if err := ensureTrailingNewline(path, "\n"); err != nil {
		return err
	}
	return appendOnly(path, func(m *MergeWriter) {
		m.cols = r.Columns
		m.sqlRows(r)
	})
}

// jsonKeys 读出合并 JSON 文件第一个对象的键（也就是列名）。文件里还没有对象时返回 nil。
func jsonKeys(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(bufio.NewReader(f))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		return nil, errBadMerged
	}
	if !dec.More() {
		return nil, nil
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errBadMerged
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return nil, err
		}
		ks, _ := k.(string)
		keys = append(keys, ks)
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

func appendJSON(path string, r Result) error {
	keys, err := jsonKeys(path)
	if err != nil {
		return err
	}
	if len(keys) == 0 { // 还没有任何一行（"[]"）：按新文件重写
		return createMerged(path, JSON, r)
	}
	if !sameColumns(keys, r.Columns) {
		return &ColumnsError{Want: keys, Got: r.Columns}
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	// 文件以 "…}\n]" 结尾：找到最后一个 ] 前面的那个 }，在它后面截断再接新对象
	n := int64(256)
	if st.Size() < n {
		n = st.Size()
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, st.Size()-n); err != nil {
		return err
	}
	trimmed := bytes.TrimRight(buf, " \t\r\n")
	if len(trimmed) == 0 || trimmed[len(trimmed)-1] != ']' {
		return errBadMerged
	}
	trimmed = bytes.TrimRight(trimmed[:len(trimmed)-1], " \t\r\n")
	if len(trimmed) == 0 || trimmed[len(trimmed)-1] != '}' {
		return errBadMerged
	}
	cut := st.Size() - n + int64(len(trimmed))
	if err := f.Truncate(cut); err != nil {
		return err
	}
	m := &MergeWriter{cols: r.Columns, objs: 1}
	for _, row := range r.Rows {
		m.jsonObject(row)
	}
	m.b.WriteString("\n]")
	_, err = f.WriteAt([]byte(m.b.String()), cut)
	return err
}

var xmlFieldRe = regexp.MustCompile(`<field>(.*?)</field>`)

func appendXML(path string, r Result) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	head := make([]byte, 1<<20)
	nr, _ := io.ReadFull(f, head)
	head = head[:nr]
	end := bytes.Index(head, []byte("</fields>"))
	if end < 0 {
		f.Close()
		return errBadMerged
	}
	var fields []string
	for _, mt := range xmlFieldRe.FindAllSubmatch(head[:end], -1) {
		fields = append(fields, html.UnescapeString(string(mt[1])))
	}
	if !sameColumns(fields, r.Columns) {
		f.Close()
		return &ColumnsError{Want: fields, Got: r.Columns}
	}
	// 已有的行数：数 <row id=" 出现的次数（流式读，不把整个文件读进内存）
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return err
	}
	count, err := countOccurrences(f, []byte(`<row id="`))
	f.Close()
	if err != nil {
		return err
	}
	g, err := os.OpenFile(path, os.O_RDWR, 0o640)
	if err != nil {
		return err
	}
	defer g.Close()
	st, err := g.Stat()
	if err != nil {
		return err
	}
	if st.Size() < int64(len(xmlFooter)) {
		return errBadMerged
	}
	tailBuf := make([]byte, len(xmlFooter))
	if _, err := g.ReadAt(tailBuf, st.Size()-int64(len(xmlFooter))); err != nil || string(tailBuf) != xmlFooter {
		return errBadMerged
	}
	cut := st.Size() - int64(len(xmlFooter))
	if err := g.Truncate(cut); err != nil {
		return err
	}
	m := &MergeWriter{}
	for i, row := range r.Rows {
		m.xmlRow(count+i+1, row)
	}
	m.b.WriteString(xmlFooter)
	_, err = g.WriteAt([]byte(m.b.String()), cut)
	return err
}

// countOccurrences 流式统计 pat 在 r 里出现的次数；块边界上被截断的 pat 也能数到。
func countOccurrences(r io.Reader, pat []byte) (int, error) {
	buf := make([]byte, 1<<20)
	carry, total := 0, 0
	for {
		n, err := r.Read(buf[carry:])
		data := buf[:carry+n]
		total += bytes.Count(data, pat)
		// 末尾 len(pat)-1 个字节留到下一块（比 pat 短，里面不可能有整段 pat，所以不会重复计数）
		keep := len(pat) - 1
		if keep > len(data) {
			keep = len(data)
		}
		copy(buf, data[len(data)-keep:])
		carry = keep
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return 0, err
		}
	}
}

func appendXLSX(path string, r Result) error {
	tmp := path + ".tmp"
	if err := rewriteXLSX(path, tmp, r); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// rewriteXLSX 把 src 里的行原样复制到 dst，再接上新实例的行（Excel 文件没法原地追加）。
func rewriteXLSX(src, dst string, r Result) error {
	in, err := excelize.OpenFile(src)
	if err != nil {
		return fmt.Errorf("打开合并文件失败：%w", err)
	}
	defer in.Close()
	rows, err := in.Rows("Sheet1")
	if err != nil {
		return err
	}
	defer rows.Close()
	out := excelize.NewFile()
	defer out.Close()
	sw, err := out.NewStreamWriter("Sheet1")
	if err != nil {
		return err
	}
	n := 0
	for rows.Next() {
		cells, err := rows.Columns()
		if err != nil {
			return err
		}
		n++
		if n == 1 {
			head := cells
			for len(head) < len(r.Columns) { // Excel 读出的行会丢掉末尾的空单元格
				head = append(head, "")
			}
			if !sameColumns(head, r.Columns) {
				return &ColumnsError{Want: cells, Got: r.Columns}
			}
		}
		vals := make([]any, len(cells))
		for i, c := range cells {
			vals[i] = c
		}
		if err := sw.SetRow(xlsxCell(n), vals); err != nil {
			return err
		}
	}
	if err := rows.Error(); err != nil {
		return err
	}
	if n+len(r.Rows) > MaxXLSXRows {
		return &LimitError{Max: MaxXLSXRows}
	}
	for _, row := range r.Rows {
		vals := make([]any, len(row))
		for j, v := range row {
			vals[j] = text(v)
		}
		n++
		if err := sw.SetRow(xlsxCell(n), vals); err != nil {
			return err
		}
	}
	if err := sw.Flush(); err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	err = out.Write(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
