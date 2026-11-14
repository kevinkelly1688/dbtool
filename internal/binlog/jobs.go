// @author: kevinkelly
// @license: Apache Licence
// @file: jobs.go
// @time: 2026/10/28

package binlog

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"dbtool/internal/i18n"
)

// timeoutUnit 是超时时间（秒）的单位；测试里调小。
var timeoutUnit = time.Second

// Job 是一个后台解析任务。任务只保存在内存里，结束 30 分钟后清理；服务重启即丢失。
type Job struct {
	mu       sync.Mutex
	ID       string
	Mode     string
	UserID   uint
	Username string
	InstID   uint
	InstName string
	Created  time.Time
	Finished time.Time
	Status   string
	Err      string
	ErrCode  string
	Req      Req
	Range    string
	Limits   Limits
	Warnings []string
	Trunc    string

	prog   progress
	res    *scanResult
	cancel context.CancelFunc
}

func (j *Job) active() bool { return j.Status == StatusQueued || j.Status == StatusRunning }

func tstr(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04:05")
}

// truncText 是还原模式（以及回滚模式没法分段时）结果没读完的说明。r 用来补充读到了哪里，可以为 nil。
func truncText(reason string, l Limits, r *scanResult) string {
	switch reason {
	case "rows":
		return fmt.Sprintf("结果达到 %d 条的上限，后面的变更没有列出。请缩小范围（结束位置 / 结束时间 / 库表 / 语句类型），或调大“最多条数”。", l.MaxRows)
	case "bytes":
		return fmt.Sprintf("结果体积达到 %d MB 的上限，后面的变更没有列出。请缩小范围，或调大“结果体积上限”。", l.MaxBytes>>20)
	case "scan":
		return fmt.Sprintf("扫描的事件数达到 %d 的上限，任务提前结束。请缩小起止范围（用起止位置 / 时间 / 库表过滤）。", l.MaxScan)
	case "timeout":
		return fmt.Sprintf("任务运行超过 %d 秒被终止，已列出超时前解析到的部分。请缩小范围，或调大“超时时间”。", l.TimeoutSec)
	case "stall":
		file, pos := "", uint32(0)
		if r != nil {
			file, pos = r.LastFile, r.LastPos
		}
		return fmt.Sprintf("读取 binlog 时目标库超过 %d 秒没有返回事件，只读到 %s:%d，还没到结束位置，后面的变更没有列出。请稍后重试，或缩小范围。", int(defaultIdle/time.Second), file, pos)
	}
	return reason
}

// segReason 是回滚分段的原因短语，拼进 segmentText / unsegmentableText。
func segReason(reason string, l Limits) string {
	switch reason {
	case "rows":
		return fmt.Sprintf("结果达到 %d 条的上限", l.MaxRows)
	case "bytes":
		return fmt.Sprintf("结果体积达到 %d MB 的上限", l.MaxBytes>>20)
	case "scan":
		return fmt.Sprintf("扫描的事件数达到 %d 的上限", l.MaxScan)
	case "timeout":
		return fmt.Sprintf("任务运行超过 %d 秒", l.TimeoutSec)
	case "stall":
		return fmt.Sprintf("目标库超过 %d 秒没有返回事件", int(defaultIdle/time.Second))
	}
	return reason
}

// segmentText 是回滚分段的说明：本段回滚 SQL 完整可用，后面的变更从续接位置继续。
func segmentText(reason string, l Limits, next Resume) string {
	return fmt.Sprintf("%s，本段在事务边界结束，续接位置 %s:%d，后面还有。点“继续下一段”接着生成；回滚时要先执行最后一段，最后执行第 1 段。",
		segReason(reason, l), next.File, next.Pos)
}

// unsegmentableText：一个完整的事务都没读完就到了上限，没法在事务边界分段。
func unsegmentableText(reason string, l Limits) string {
	return fmt.Sprintf("%s，而且范围里的第一个事务还没读完，无法在事务边界分段；回滚 SQL 必须完整，所以本次不给出结果。请调整范围或上限后重试（上限的最大值在“平台设置 › 系统配置 › 日志解析”调整）。",
		segReason(reason, l))
}

func newJobID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- 任务仓库 ----

type jobStore struct {
	mu   sync.Mutex
	jobs map[string]*Job
}

func newJobStore() *jobStore { return &jobStore{jobs: map[string]*Job{}} }

// gc 删除结束超过 TTL 的任务，并清掉它们的结果（含临时文件）。调用方持有 st.mu。
func (st *jobStore) gc(now time.Time) {
	for id, j := range st.jobs {
		j.mu.Lock()
		old := !j.active() && !j.Finished.IsZero() && now.Sub(j.Finished) > jobTTL
		j.mu.Unlock()
		if old {
			delete(st.jobs, id)
			j.closeResult()
		}
	}
}

// closeResult 清理任务的结果：删掉临时文件、释放内存。可重复调用；正在读取（分页 / 下载）的会等读完再删。
func (j *Job) closeResult() {
	j.mu.Lock()
	res := j.res
	j.mu.Unlock()
	if res != nil {
		res.Stmts.Close()
	}
}

// reserveLocked 返回任务占用的内存与磁盘预留。运行中的任务按体积上限预留：结果在内存里最多放 spillThreshold，超过就整个转存到磁盘；
// 已结束的按实际占用。调用方持有 j.mu（新登记的任务还没有别人能碰到，可以不持有）。
func (j *Job) reserveLocked() (mem int, disk int64) {
	if j.active() {
		m := j.Limits.MaxBytes
		if m > spillThreshold {
			return spillThreshold, int64(m)
		}
		return m, 0
	}
	if j.res == nil {
		return 0, 0
	}
	return j.res.Stmts.MemBytes(), j.res.Stmts.DiskBytes()
}

// storeBudget 是所有任务结果占用的磁盘预算：至少 minStoreBytes；体积硬上限调大后，至少容得下两份满额的结果。
// 内存里的结果（每个任务最多 spillThreshold）合计不超过 minStoreBytes，不随上限放大。
func storeBudget(hardMB int) int { return max(minStoreBytes, 2*(hardMB<<20)) }

// add 登记任务并检查并发上限：每人同时最多 2 个、全局最多 6 个运行中的任务；保存的任务数有上限，结果占用的内存不能超过 minStoreBytes、
// 磁盘不能超过 diskBudget。占用按“预留”算：运行中的任务按各自的体积上限（Limits.MaxBytes）预留，已结束的按实际体积，新任务也按自己的上限预留。
// 放不下时淘汰最早结束的任务，再放不下（都在运行）就拒绝。
func (st *jobStore) add(j *Job, now time.Time, diskBudget int) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.gc(now)
	mine, all := 0, 0
	for _, o := range st.jobs {
		o.mu.Lock()
		if o.active() {
			all++
			if o.UserID == j.UserID {
				mine++
			}
		}
		o.mu.Unlock()
	}
	if mine >= maxActivePerUser {
		return newErr(http.StatusTooManyRequests, "too_many_jobs", fmt.Sprintf("你已有 %d 个解析任务在运行，请等它们结束或先取消一个。", maxActivePerUser))
	}
	if all >= maxActiveAll {
		return newErr(http.StatusTooManyRequests, "busy", "系统当前的 Binlog 解析任务较多，请稍后再试。")
	}
	for {
		mem, disk := j.reserveLocked() // 新任务最多再占自己体积上限的结果空间
		var oldest *Job
		var oldID string
		for id, o := range st.jobs {
			o.mu.Lock()
			m, d := o.reserveLocked()
			fin, f := !o.active(), o.Finished
			o.mu.Unlock()
			mem += m
			disk += d
			if fin && (oldest == nil || f.Before(oldest.Finished)) {
				oldest, oldID = o, id
			}
		}
		if len(st.jobs) < maxStoredJobs && mem <= minStoreBytes && disk <= int64(diskBudget) {
			break
		}
		if oldest == nil {
			return newErr(http.StatusTooManyRequests, "busy", "系统当前保存的解析结果较多，请稍后再试。")
		}
		delete(st.jobs, oldID)
		oldest.closeResult()
	}
	st.jobs[j.ID] = j
	return nil
}

func (st *jobStore) get(id string) *Job {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.jobs[id]
}

// ---- 运行 ----

// launch 在后台运行任务；sess 由本函数负责关闭。任务 panic 不会拖垮服务。
func (s *Service) launch(j *Job, sess Session, p *plan, secrets []string) {
	ctx, cancel := context.WithTimeout(s.rootCtx(), time.Duration(p.limits.TimeoutSec)*timeoutUnit)
	j.mu.Lock()
	j.cancel = cancel
	j.mu.Unlock()
	go func() {
		defer cancel()
		defer sess.Close()
		j.mu.Lock()
		if j.Status == StatusQueued {
			j.Status = StatusRunning
		}
		j.mu.Unlock()
		var res *scanResult
		var runErr error
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("Binlog 解析任务异常", "job", j.ID, "panic", r, "stack", string(debug.Stack()))
					runErr = newErr(http.StatusInternalServerError, "internal", "任务运行时发生内部错误")
				}
			}()
			res, runErr = s.run(ctx, j, sess, p, secrets)
		}()
		s.finish(ctx, j, p, res, runErr)
	}()
}

func (s *Service) run(ctx context.Context, j *Job, sess Session, p *plan, secrets []string) (*scanResult, error) {
	select { // 同时从目标实例拉取 binlog 的任务数有上限
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	st, err := sess.Stream(ctx, p.req.StartFile, p.req.StartPos, p.spec.Loc)
	if err != nil {
		return nil, dbErr(ctx, err, "拉取 binlog", secrets)
	}
	defer st.Close()
	return scan(ctx, p.spec, st, sess, &j.prog)
}

// finish 收尾：确定最终状态，保存结果。
func (s *Service) finish(ctx context.Context, j *Job, p *plan, res *scanResult, runErr error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	kept := false
	defer func() { // 没有保存的结果（失败 / 取消）立刻清掉，不留临时文件
		if !kept && res != nil {
			res.Stmts.Close()
		}
	}()
	if j.Finished.IsZero() {
		j.Finished = s.now()
	}
	if res != nil {
		j.Warnings = append(j.Warnings, res.Warnings...)
	}
	if j.Status == StatusCanceled { // 用户已取消：不保存结果
		return
	}
	rollback := p.req.Mode == ModeRollback
	fail := func(code, msg string) {
		j.Status, j.ErrCode, j.Err = StatusFailed, code, msg
	}
	switch {
	case errors.Is(runErr, context.DeadlineExceeded):
		if rollback || res == nil || res.Stmts.Len() == 0 {
			fail("timeout", fmt.Sprintf("任务运行超过 %d 秒仍未完成，已终止。请缩小范围（起止位置 / 时间 / 库表）后重试。", p.limits.TimeoutSec))
			return
		}
		res.Truncated = "timeout"
	case errors.Is(runErr, context.Canceled):
		j.Status, j.Err = StatusCanceled, "已取消"
		return
	case runErr != nil:
		var be *Error
		if errors.As(runErr, &be) {
			fail(be.Code, be.Message)
		} else {
			slog.Warn("Binlog 解析失败", "job", j.ID, "err", runErr)
			fail("internal", fmt.Sprintf("解析失败：%s", runErr.Error()))
		}
		return
	}
	if res == nil {
		fail("internal", "没有得到结果")
		return
	}
	if rollback && res.Truncated != "" {
		// 回滚模式到上限时 scan 已经在事务边界截断并给出续接位置：本段完整可用，后面的变更从续接位置继续（分段）
		if res.Next == nil {
			fail("limit_exceeded", fmt.Sprintf("%s回滚 SQL 必须完整，只回滚一部分会造成数据不一致，所以本次不给出结果。", truncText(res.Truncated, p.limits, res)))
			return
		}
		switch next := normalizeNext(p, *res.Next); {
		case next == nil: // 续接位置已经在结束位置上：其实读完了，不是分段
			res.Truncated, res.Next = "", nil
		case *next == (Resume{File: p.req.StartFile, Pos: max(p.req.StartPos, 4)}) ||
			(res.unitCut && res.Trx == 0 && res.Stmts.Len() == 0): // 一个完整的事务都没读完，没法分段（从续接位置继续只会原样再来一遍）
			fail("limit_exceeded", unsegmentableText(res.Truncated, p.limits))
			return
		default:
			res.Next = next
		}
	}
	kept = true
	j.res = res
	j.Trunc = ""
	switch {
	case res.Next != nil:
		j.Trunc = segmentText(res.Truncated, p.limits, *res.Next)
	case res.Truncated != "":
		j.Trunc = truncText(res.Truncated, p.limits, res)
	}
	j.Status = StatusDone
}

// normalizeNext 把回滚分段的续接位置规整成下一段真能开始的位置：位置在所在文件的末尾（或之后）就换成下一个文件的开头；
// 已经到了结束位置（没有下一个文件、或已超过结束文件 / 结束位置）返回 nil，表示没有后续的段。
func normalizeNext(p *plan, n Resume) *Resume {
	for i, f := range p.files {
		if f.Name != n.File {
			continue
		}
		if f.Size > 0 && int64(n.Pos) >= f.Size {
			if i+1 >= len(p.files) {
				return nil
			}
			n = Resume{File: p.files[i+1].Name, Pos: 4}
		}
		break
	}
	endFile, endPos := p.spec.EndFile, int64(p.spec.EndPos)
	if endFile == "" {
		endFile, endPos = p.spec.LastFile, p.spec.LastSize
	}
	if endFile != "" {
		if c := fileCmp(n.File, endFile); c > 0 || (c == 0 && endPos > 0 && int64(n.Pos) >= endPos) {
			return nil
		}
	}
	return &n
}

// view 生成任务视图（调用时不要持有 j.mu）。CanDownload 只反映任务状态，调用方再与“下载”按钮权限相与。
func (j *Job) view(actorID uint) *JobView {
	scanned, matched, bytes, file, pos := j.prog.get()
	j.mu.Lock()
	defer j.mu.Unlock()
	v := &JobView{ID: j.ID, Mode: j.Mode, Status: j.Status, Error: j.Err, ErrorCode: j.ErrCode, Instance: j.InstName, Owner: j.Username,
		Mine: j.UserID == actorID, CreatedAt: tstr(j.Created), FinishedAt: tstr(j.Finished), KeepMin: int(jobTTL / time.Minute),
		Range: j.Range, Scanned: scanned, Matched: matched, Bytes: bytes, File: file, Pos: pos, Limits: j.Limits,
		Warnings: append([]string{}, j.Warnings...), ByType: map[string]int{}, Truncate: j.Trunc, req: j.Req}
	v.RemainSec = -1
	if !j.active() && !j.Finished.IsZero() {
		exp := j.Finished.Add(jobTTL)
		v.ExpiresAt = tstr(exp)
		if v.RemainSec = int(time.Until(exp) / time.Second); v.RemainSec < 0 {
			v.RemainSec = 0
		}
	}
	if j.res != nil {
		v.Scanned, v.Matched, v.Bytes, v.Trx = j.res.Scanned, j.res.Stmts.Len(), j.res.Bytes, j.res.Trx
		for k, n := range j.res.ByType {
			v.ByType[k] = n
		}
		v.File, v.Pos = j.res.LastFile, j.res.LastPos
		if j.Mode == ModeRollback && j.Status == StatusDone {
			if sg := j.segmentLocked(); sg != nil && (sg.More || sg.Index > 1) {
				v.Segment = sg
			}
		}
	}
	v.CanDownload = j.Status == StatusDone && j.res != nil
	return v
}

// segmentLocked 描述回滚任务的分段情况（调用方持有 j.mu，且 j.res 不为空）。
func (j *Job) segmentLocked() *SegmentView {
	sg := &SegmentView{Index: max(j.Req.Segment, 1)}
	if n := j.res.Next; n != nil {
		sg.More, sg.NextFile, sg.NextPos, sg.Reason = true, n.File, n.Pos, j.res.Truncated
	}
	return sg
}

// ---- 结果文件 ----

func oneLineComment(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

// fileMeta 是结果文件头部要用的任务信息：在 j.mu 保护下拷出来，写文件时（可能很久）不再持锁。
type fileMeta struct {
	mode, inst, user, rangeText string
	req                         Req
	warnings                    []string
	trunc                       string
	trx                         int
	store                       *stmtStore
	segment                     *SegmentView
}

// fileMetaLocked 调用方持有 j.mu，且 j.res 不为空。
func (j *Job) fileMetaLocked() fileMeta {
	m := fileMeta{mode: j.Mode, inst: j.InstName, user: j.Username, rangeText: j.Range, req: j.Req,
		warnings: append([]string(nil), j.Warnings...), trunc: j.Trunc, trx: j.res.Trx, store: j.res.Stmts}
	if j.Mode == ModeRollback {
		if sg := j.segmentLocked(); sg.More || sg.Index > 1 {
			m.segment = sg
		}
	}
	return m
}

// writeFile 把结果写成 SQL 文件：头部说明 + 语句，同一事务的语句包在 BEGIN / COMMIT 里。
// 回滚结果按“后发生的先撤销”的顺序（倒着读存放的结果）；大结果在临时文件里，边读边写，不会把整个文件装进内存。
// lang 只影响文件头部的说明注释（范围、警告等按目录翻译），语句本身不变；中文与旧格式逐字一致。返回写出的字节数。
func writeFile(w io.Writer, m fileMeta, now time.Time, lang string) (int64, error) {
	cw := &countWriter{w: w}
	b := bufio.NewWriterSize(cw, 256<<10)
	rb := m.mode == ModeRollback
	title := i18n.T(lang, "还原 SQL")
	if rb {
		title = i18n.T(lang, "回滚 SQL")
	}
	line := func(s string) { _, _ = b.WriteString(oneLineComment(s) + "\n") }
	rangeText := m.rangeText
	if lang != "" && lang != i18n.ZH {
		rangeText = describeIn(lang, m.req)
	}
	_, _ = b.WriteString("-- ==========================================================================\n")
	line(i18n.T(lang, "-- Binlog 解析结果（{title}）", "title", title))
	line(i18n.T(lang, "-- 实例: {inst}    操作人: {user}    生成时间: {time}", "inst", oneLineComment(m.inst), "user", oneLineComment(m.user), "time", now.Format("2006-01-02 15:04:05")))
	line(i18n.T(lang, "-- 范围: {range}", "range", oneLineComment(rangeText)))
	line(i18n.T(lang, "-- 语句数: {n}    事务数: {trx}", "n", m.store.Len(), "trx", m.trx))
	if rb {
		line(i18n.T(lang, "-- 回滚语句按“后发生的先撤销”的顺序排列，请严格按文件顺序执行；执行前务必先在测试环境验证。"))
		if sg := m.segment; sg != nil {
			if sg.More {
				line(i18n.T(lang, "-- 分段回滚：这是第 {n} 段，后面还有（续接位置 {file}:{pos}）。完整回滚要按段号从大到小执行：先执行最后一段，最后执行第 1 段。", "n", sg.Index, "file", sg.NextFile, "pos", sg.NextPos))
			} else {
				line(i18n.T(lang, "-- 分段回滚：这是第 {n} 段，也是最后一段。完整回滚要按段号从大到小执行：先执行最后一段，最后执行第 1 段。", "n", sg.Index))
			}
		}
	}
	if m.trunc != "" {
		line(i18n.T(lang, "-- 注意: {text}", "text", oneLineComment(i18n.TP(lang, m.trunc))))
	}
	for _, w := range m.warnings {
		line(i18n.T(lang, "-- 警告: {text}", "text", oneLineComment(i18n.TP(lang, w))))
	}
	line(i18n.T(lang, "-- 内容可能含有业务敏感数据，请妥善保管，用后删除。"))
	_, _ = b.WriteString("-- ==========================================================================\n")
	open := 0
	each := m.store.EachForward
	if rb {
		each = m.store.EachReverse
	}
	err := each(func(st Stmt) error {
		if st.Trx != open {
			if open != 0 {
				_, _ = b.WriteString("COMMIT;\n")
			}
			if st.Trx != 0 {
				_, _ = b.WriteString("BEGIN;\n")
			}
			open = st.Trx
		}
		_, _ = b.WriteString(st.SQL)
		_ = b.WriteByte('\n')
		return cw.err // 客户端断开等写出错误：不用把剩下的全读完
	})
	if err == nil && open != 0 {
		_, _ = b.WriteString("COMMIT;\n")
	}
	if ferr := b.Flush(); err == nil {
		err = ferr
	}
	return cw.n, err
}

// countWriter 数写出的字节，并记住第一个写错误。
type countWriter struct {
	w   io.Writer
	n   int64
	err error
}

func (c *countWriter) Write(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	c.err = err
	return n, err
}
