// @author: kevinkelly
// @license: Apache Licence
// @file: jobs.go
// @time: 2026/10/28

package sqlopt

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"dbtool/internal/audit"
)

// 任务与条目状态。
const (
	StatusQueued   = "queued"
	StatusRunning  = "running"
	StatusDone     = "done"
	StatusFailed   = "failed"
	StatusCanceled = "canceled"
	StatusWaiting  = "waiting" // 条目：排队中
)

// 任务种类。
const (
	KindAnalyze  = "analyze"
	KindOptimize = "optimize"
)

// Item 是任务里的一条 SQL（分析任务）或唯一的一次工具运行（优化任务）。
type Item struct {
	Index  int
	ID     string
	SQL    string
	Stats  *LogStats
	FP     string
	FPID   string
	Type   string
	Tables []string

	Status string
	Err    string
	Report *Report
	Result *OptimizeResult
	size   int
}

// Job 是一个后台任务。任务只保存在内存里，结束 30 分钟后清理；服务重启即丢失（页面会提示重新提交）。
type Job struct {
	mu       sync.Mutex
	ID       string
	Kind     string
	Title    string
	UserID   uint
	Username string
	Created  time.Time
	Finished time.Time
	Status   string
	Err      string
	Notice   string
	Items    []*Item
	// 以下仅分析任务有：用于“最近报告”列表展示。
	File     string // 上传的文件名（页面传入，仅展示）
	Format   string // sql / mapper / slowlog
	InstName string
	DBName   string
	bytes    int
	cancel   context.CancelFunc
	summary  *Summary
}

// JobView / ItemView / ItemDetail 是返回给页面的结构。
type JobView struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Title      string     `json:"title"`
	Status     string     `json:"status"`
	Total      int        `json:"total"`
	Done       int        `json:"done"`
	Failed     int        `json:"failed"`
	CreatedAt  string     `json:"created_at"`
	FinishedAt string     `json:"finished_at,omitempty"`
	Error      string     `json:"error,omitempty"`
	Notice     string     `json:"notice,omitempty"`
	Items      []ItemView `json:"items"`
	Summary    *Summary   `json:"summary,omitempty"`
	File       string     `json:"file,omitempty"`
	Format     string     `json:"format,omitempty"`
	Instance   string     `json:"instance,omitempty"`
	DB         string     `json:"db,omitempty"`
	ExpiresAt  string     `json:"expires_at,omitempty"` // 结束后的清理时间（结束时间 + 保留期）
	KeepMin    int        `json:"keep_minutes"`         // 结束后保留多少分钟（页面提示用）
}

type ItemView struct {
	Index    int       `json:"index"`
	ID       string    `json:"sql_id"`
	SQL      string    `json:"sql"`
	Type     string    `json:"type,omitempty"`
	FPID     string    `json:"fp_id,omitempty"`
	Status   string    `json:"status"`
	Error    string    `json:"error,omitempty"`
	Level    string    `json:"level,omitempty"` // 最高的建议级别
	Findings int       `json:"findings"`
	Score    *int      `json:"score,omitempty"`
	Stats    *LogStats `json:"stats,omitempty"`
	Tables   []string  `json:"tables,omitempty"`
}

type ItemDetail struct {
	ItemView
	FullSQL     string          `json:"full_sql"`
	Fingerprint string          `json:"fingerprint,omitempty"`
	Report      *Report         `json:"report,omitempty"`
	Result      *OptimizeResult `json:"result,omitempty"`
}

const itemSQLPreview = 240

func (it *Item) view() ItemView {
	v := ItemView{Index: it.Index, ID: it.ID, SQL: truncRunes(it.SQL, itemSQLPreview), Type: it.Type, FPID: it.FPID,
		Status: it.Status, Error: it.Err, Stats: it.Stats, Tables: it.Tables}
	if it.Report != nil {
		v.Findings = len(it.Report.Findings)
		v.Level = it.Report.Highest
		sc := it.Report.Score
		v.Score = &sc
	}
	return v
}

func (j *Job) counts() (done, failed int) {
	for _, it := range j.Items {
		switch it.Status {
		case StatusDone:
			done++
		case StatusFailed:
			done++
			failed++
		}
	}
	return
}

func tstr(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04:05")
}

func (j *Job) active() bool { return j.Status == StatusQueued || j.Status == StatusRunning }

func (j *Job) view(withSummary bool) *JobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	done, failed := j.counts()
	v := &JobView{ID: j.ID, Kind: j.Kind, Title: j.Title, Status: j.Status, Total: len(j.Items), Done: done, Failed: failed,
		CreatedAt: tstr(j.Created), FinishedAt: tstr(j.Finished), Error: j.Err, Notice: j.Notice, Items: make([]ItemView, 0, len(j.Items)),
		File: j.File, Format: j.Format, Instance: j.InstName, DB: j.DBName, KeepMin: int(jobTTL / time.Minute)}
	if !j.active() && !j.Finished.IsZero() {
		v.ExpiresAt = tstr(j.Finished.Add(jobTTL))
	}
	for _, it := range j.Items {
		iv := it.view()
		if !j.active() && (iv.Status == StatusWaiting || iv.Status == StatusRunning) {
			iv.Status = StatusCanceled // 已取消的任务：后台协程还没来得及收尾时，条目也按已取消显示
		}
		v.Items = append(v.Items, iv)
	}
	if withSummary && !j.active() && j.Kind == KindAnalyze {
		if j.summary == nil {
			j.summary = summarize(j.Items)
		}
		v.Summary = j.summary
	}
	return v
}

// ---- 汇总统计 ----

// FPStat 是同一指纹的语句汇总。
type FPStat struct {
	FPID         string   `json:"fp_id"`
	Fingerprint  string   `json:"fingerprint"`
	Type         string   `json:"type"`
	Count        int      `json:"count"`
	SQLIDs       []string `json:"sql_ids"`
	Level        string   `json:"level,omitempty"`
	TotalTime    float64  `json:"total_time,omitempty"`
	MaxTime      float64  `json:"max_time,omitempty"`
	RowsExamined int64    `json:"rows_examined,omitempty"`
}

// Summary 是分析任务的汇总：按类型、按最高建议级别、按指纹、按表的统计。
type Summary struct {
	Total    int            `json:"total"`
	Unique   int            `json:"unique"`
	ByType   map[string]int `json:"by_type"`
	ByLevel  map[string]int `json:"by_level"`
	TopFP    []FPStat       `json:"top_fingerprints"`
	Tables   []TableStat    `json:"tables"`
	HasStats bool           `json:"has_stats"` // 来自慢日志，有耗时统计
	AvgScore int            `json:"avg_score"` // 已出报告的语句的平均评分（没有报告时为 0）
	Scored   int            `json:"scored"`    // 参与平均评分的语句数
}

type TableStat struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

const topN = 20

func summarize(items []*Item) *Summary {
	s := &Summary{Total: len(items), ByType: map[string]int{}, ByLevel: map[string]int{}, TopFP: []FPStat{}, Tables: []TableStat{}} // 空列表序列化成 []，页面直接遍历它们
	scoreSum := 0
	byFP := map[string]*FPStat{}
	var order []string
	tbl := map[string]int{}
	for _, it := range items {
		if it.FP == "" {
			continue
		}
		s.ByType[it.Type]++
		if it.Report != nil {
			scoreSum += it.Report.Score
			s.Scored++
		}
		if it.Report != nil {
			if it.Report.Highest != "" {
				s.ByLevel[it.Report.Highest]++
			} else {
				s.ByLevel["none"]++
			}
		}
		st := byFP[it.FPID]
		if st == nil {
			st = &FPStat{FPID: it.FPID, Fingerprint: truncRunes(it.FP, 400), Type: it.Type}
			byFP[it.FPID] = st
			order = append(order, it.FPID)
		}
		st.Count++
		if len(st.SQLIDs) < 20 {
			st.SQLIDs = append(st.SQLIDs, it.ID)
		}
		if it.Report != nil && levelRank[it.Report.Highest] > levelRank[st.Level] {
			st.Level = it.Report.Highest
		}
		if it.Stats != nil {
			s.HasStats = true
			st.TotalTime += it.Stats.QueryTime
			if it.Stats.QueryTime > st.MaxTime {
				st.MaxTime = it.Stats.QueryTime
			}
			st.RowsExamined += it.Stats.RowsExamined
		}
		for _, t := range it.Tables {
			tbl[t]++
		}
	}
	s.Unique = len(byFP)
	if s.Scored > 0 {
		s.AvgScore = (scoreSum + s.Scored/2) / s.Scored
	}
	for _, id := range order {
		s.TopFP = append(s.TopFP, *byFP[id])
	}
	sort.SliceStable(s.TopFP, func(a, b int) bool {
		x, y := s.TopFP[a], s.TopFP[b]
		if s.HasStats && x.TotalTime != y.TotalTime {
			return x.TotalTime > y.TotalTime
		}
		return x.Count > y.Count
	})
	if len(s.TopFP) > topN {
		s.TopFP = s.TopFP[:topN]
	}
	for n, c := range tbl {
		s.Tables = append(s.Tables, TableStat{n, c})
	}
	sort.Slice(s.Tables, func(a, b int) bool {
		if s.Tables[a].Count != s.Tables[b].Count {
			return s.Tables[a].Count > s.Tables[b].Count
		}
		return s.Tables[a].Name < s.Tables[b].Name
	})
	if len(s.Tables) > topN {
		s.Tables = s.Tables[:topN]
	}
	return s
}

// ---- 任务仓库 ----

type jobStore struct {
	mu   sync.Mutex
	jobs map[string]*Job
}

func newJobStore() *jobStore { return &jobStore{jobs: map[string]*Job{}} }

func newJobID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// gc 删除结束超过 TTL 的任务。调用方持有 st.mu。
func (st *jobStore) gc(now time.Time) {
	for id, j := range st.jobs {
		j.mu.Lock()
		old := !j.active() && !j.Finished.IsZero() && now.Sub(j.Finished) > jobTTL
		j.mu.Unlock()
		if old {
			delete(st.jobs, id)
		}
	}
}

// add 登记任务，同时检查并发上限：每人同时最多 2 个、全局最多 16 个运行中的任务，保存的任务总数不超过上限。
func (st *jobStore) add(j *Job, now time.Time) error {
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
	if mine >= maxActivePerUsr {
		return newErr(http.StatusTooManyRequests, "too_many_jobs", "你已有 2 个分析任务在运行，请等它们结束或先取消一个。")
	}
	if all >= maxActiveAll {
		return newErr(http.StatusTooManyRequests, "busy", "系统当前分析任务较多，请稍后再试。")
	}
	if len(st.jobs) >= maxStoredJobs {
		// 淘汰最早结束的
		var oldest *Job
		var oldID string
		for id, o := range st.jobs {
			o.mu.Lock()
			fin, f := !o.active(), o.Finished
			o.mu.Unlock()
			if fin && (oldest == nil || f.Before(oldest.Finished)) {
				oldest, oldID = o, id
			}
		}
		if oldest == nil {
			return newErr(http.StatusTooManyRequests, "busy", "系统当前分析任务较多，请稍后再试。")
		}
		delete(st.jobs, oldID)
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

// launch 在后台运行任务。run 返回非 nil 表示整个任务失败。任务 panic 不会拖垮服务。
func (s *Service) launch(j *Job, run func(ctx context.Context, j *Job) error) {
	ctx, cancel := context.WithTimeout(s.rootCtx(), jobMaxRun)
	j.mu.Lock()
	j.cancel = cancel
	j.mu.Unlock()
	go func() {
		defer cancel()
		j.mu.Lock()
		if j.Status == StatusQueued { // 排队期间可能已被取消
			j.Status = StatusRunning
		}
		j.mu.Unlock()
		var runErr error
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("SQL 优化任务异常", "job", j.ID, "panic", r, "stack", string(debug.Stack()))
					runErr = newErr(http.StatusInternalServerError, "internal", "任务运行时发生内部错误")
				}
			}()
			runErr = run(ctx, j)
		}()
		j.mu.Lock()
		defer j.mu.Unlock()
		j.Finished = s.now()
		canceled := j.Status == StatusCanceled || ctx.Err() != nil
		for _, it := range j.Items {
			if it.Status == StatusWaiting || it.Status == StatusRunning {
				it.Status = StatusCanceled
			}
		}
		switch {
		case j.Status == StatusCanceled:
		case runErr != nil:
			j.Status, j.Err = StatusFailed, runErr.Error()
		case canceled:
			j.Status = StatusCanceled
			if j.Err == "" {
				j.Err = "任务超时或已取消"
			}
		default:
			j.Status = StatusDone
		}
	}()
}

// setItem 更新条目状态（持锁）。
func (j *Job) setItem(it *Item, status, errMsg string) {
	j.mu.Lock()
	it.Status, it.Err = status, errMsg
	j.mu.Unlock()
}

// finishItem 保存条目结果，并控制单个任务保存的结果总量。
func (j *Job) finishItem(it *Item, rep *Report, res *OptimizeResult) {
	size := 0
	if rep != nil {
		if b, err := json.Marshal(rep); err == nil {
			size = len(b)
		}
	}
	if res != nil {
		if b, err := json.Marshal(res); err == nil {
			size = len(b)
		}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.bytes+size > MaxJobBytes {
		// 超出任务结果总量上限：只保留建议，丢掉体积大的执行计划与外部工具报告
		if rep != nil {
			rep.Plan, rep.Soar = nil, ""
			rep.Notes = append(rep.Notes, "任务结果过大，已省略执行计划与 SOAR 报告")
			size = 4 << 10
		} else {
			res = nil
			it.Err = "任务结果过大，已省略"
			size = 0
		}
	}
	j.bytes += size
	it.Report, it.Result, it.size = rep, res, size
	it.Status = StatusDone
	if it.Err != "" {
		it.Status = StatusFailed
	}
}

func (s *Service) newJob(actor audit.Actor, kind, title string) *Job {
	return &Job{ID: newJobID(), Kind: kind, Title: title, UserID: actor.ID, Username: actor.Username, Created: s.now(), Status: StatusQueued}
}

// Job 返回任务视图。只有创建者和超级用户能看。
func (s *Service) Job(actor audit.Actor, id string) (*JobView, error) {
	j, err := s.ownedJob(actor, id)
	if err != nil {
		return nil, err
	}
	return j.view(true), nil
}

func (s *Service) ownedJob(actor audit.Actor, id string) (*Job, error) {
	j := s.jobs.get(id)
	if j == nil || (j.UserID != actor.ID && !actor.IsSuperuser) {
		return nil, newErr(http.StatusNotFound, "not_found", "任务不存在或已过期（任务只保留 30 分钟，服务重启后也会丢失）")
	}
	return j, nil
}

// JobItem 返回一个条目的完整结果。
func (s *Service) JobItem(actor audit.Actor, id string, index int) (*ItemDetail, error) {
	j, err := s.ownedJob(actor, id)
	if err != nil {
		return nil, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if index < 0 || index >= len(j.Items) {
		return nil, newErr(http.StatusNotFound, "not_found", "条目不存在")
	}
	it := j.Items[index]
	iv := it.view()
	if !j.active() && (iv.Status == StatusWaiting || iv.Status == StatusRunning) {
		iv.Status = StatusCanceled
	}
	return &ItemDetail{ItemView: iv, FullSQL: it.SQL, Fingerprint: it.FP, Report: it.Report, Result: it.Result}, nil
}

// CancelJob 取消任务（运行中的外部工具会被终止）。已结束的任务返回 409。
func (s *Service) CancelJob(actor audit.Actor, id string) error {
	j, err := s.ownedJob(actor, id)
	if err != nil {
		return err
	}
	j.mu.Lock()
	if !j.active() {
		j.mu.Unlock()
		return newErr(http.StatusConflict, "conflict", "任务已经结束")
	}
	j.Status = StatusCanceled
	j.Err = "已取消"
	if j.Finished.IsZero() {
		j.Finished = s.now()
	}
	cancel, owner, title, kind := j.cancel, j.Username, j.Title, j.Kind
	j.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	action := "SQL优化-取消任务"
	if kind == KindAnalyze {
		action = "SQL分析-取消"
	}
	s.record(actor, action, fmt.Sprintf("任务:%s 提交人:%s 标题:%s", j.ID, owner, title))
	return nil
}

// JobBrief 是“最近报告”列表里的一行。
type JobBrief struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	File       string `json:"file,omitempty"`
	Format     string `json:"format,omitempty"`
	Instance   string `json:"instance,omitempty"`
	DB         string `json:"db,omitempty"`
	Owner      string `json:"owner"`
	Mine       bool   `json:"mine"`
	Status     string `json:"status"`
	Total      int    `json:"total"`
	Done       int    `json:"done"`
	Failed     int    `json:"failed"`
	CreatedAt  string `json:"created_at"`
	FinishedAt string `json:"finished_at,omitempty"`
	ExpiresAt  string `json:"expires_at,omitempty"`
	RemainSec  int    `json:"remain_seconds"` // 距离清理还有多少秒；进行中的任务为 -1（结束后才开始计时）
	CanCancel  bool   `json:"can_cancel"`
}

// maxRecentJobs 是“最近报告”最多返回多少条。
const maxRecentJobs = 30

// ListJobs 返回当前仍保留的分析任务（新的在前）：普通用户只能看到自己的，超级用户能看到所有人的。
// 保留期与任务本身的清理策略一致（结束后 jobTTL）。
func (s *Service) ListJobs(actor audit.Actor) []JobBrief {
	now := s.now()
	s.jobs.mu.Lock()
	s.jobs.gc(now)
	all := make([]*Job, 0, len(s.jobs.jobs))
	for _, j := range s.jobs.jobs {
		all = append(all, j)
	}
	s.jobs.mu.Unlock()

	out := make([]JobBrief, 0, len(all))
	var created []time.Time
	for _, j := range all {
		j.mu.Lock()
		if j.Kind != KindAnalyze || (j.UserID != actor.ID && !actor.IsSuperuser) {
			j.mu.Unlock()
			continue
		}
		done, failed := j.counts()
		b := JobBrief{ID: j.ID, Title: j.Title, File: j.File, Format: j.Format, Instance: j.InstName, DB: j.DBName,
			Owner: j.Username, Mine: j.UserID == actor.ID, Status: j.Status, Total: len(j.Items), Done: done, Failed: failed,
			CreatedAt: tstr(j.Created), FinishedAt: tstr(j.Finished), RemainSec: -1, CanCancel: j.active()}
		if !j.active() && !j.Finished.IsZero() {
			exp := j.Finished.Add(jobTTL)
			b.ExpiresAt = tstr(exp)
			if b.RemainSec = int(exp.Sub(now) / time.Second); b.RemainSec < 0 {
				b.RemainSec = 0
			}
		}
		created = append(created, j.Created)
		out = append(out, b)
		j.mu.Unlock()
	}
	idx := make([]int, len(out))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return created[idx[a]].After(created[idx[b]]) })
	sorted := make([]JobBrief, 0, len(out))
	for _, i := range idx {
		sorted = append(sorted, out[i])
		if len(sorted) >= maxRecentJobs {
			break
		}
	}
	return sorted
}
