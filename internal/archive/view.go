// @author: kevinkelly
// @license: Apache Licence
// @file: view.go
// @time: 2026/10/28

package archive

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"dbtool/internal/archive/engine"
	"dbtool/internal/audit"
	"dbtool/internal/model"
)

// ---- 对外 JSON 结构 ----

type pauseView struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
	At     string `json:"at"`
}

type lastRunView struct {
	ID        uint   `json:"id"`
	Result    string `json:"result"`
	Trigger   string `json:"trigger"`
	StartedAt string `json:"started_at"`
}

type runningView struct {
	RunID  uint   `json:"run_id"`
	State  string `json:"state"` // running | paused | waiting_load
	Pause  bool   `json:"can_pause"`
	Resume bool   `json:"can_resume"`
	Stop   bool   `json:"can_stop"`
}

type approvalSummary struct {
	AuditID      uint   `json:"audit_id"`
	Status       string `json:"status"` // waiting | passed | rejected | aborted
	CurrentLevel int    `json:"current_level"`
	LevelCount   int    `json:"level_count"`
	PendingText  string `json:"pending_text"`
}

type allowView struct {
	Edit    bool `json:"edit"`
	Check   bool `json:"check"`
	Approve bool `json:"approve"`
	Reject  bool `json:"reject"`
	Run     bool `json:"run"`
	Pause   bool `json:"pause"`
	Resume  bool `json:"resume"`
	Disable bool `json:"disable"`
	Delete  bool `json:"delete"`
}

// TaskView 是任务的对外形态。
type TaskView struct {
	ID                uint               `json:"id"`
	Name              string             `json:"name"`
	Status            string             `json:"status"`
	ResourceGroupID   uint               `json:"resource_group_id"`
	ResourceGroupName string             `json:"resource_group_name"`
	InstanceID        uint               `json:"instance_id"`
	InstanceName      string             `json:"instance_name"`
	DB                string             `json:"db"`
	Table             string             `json:"table"`
	TimeColumn        string             `json:"time_column"`
	TimeUnit          string             `json:"time_unit"`
	RetentionDays     int                `json:"retention_days"`
	ExtraWhere        string             `json:"extra_where"`
	Mode              string             `json:"mode"`
	PurgeReason       string             `json:"purge_reason"`
	DestTable         *engine.TableDest  `json:"dest_table"`
	DestFile          *engine.FileDest   `json:"dest_file"`
	Params            engine.Params      `json:"params"`
	Schedule          Schedule           `json:"schedule"`
	Where             string             `json:"where"`
	SQL               *engine.SQLPreview `json:"sql,omitempty"`
	ScheduleText      string             `json:"schedule_text"`
	WindowText        string             `json:"window_text"`
	NextRunAt         string             `json:"next_run_at"`
	Pause             *pauseView         `json:"pause"`
	Checkpoint        []string           `json:"checkpoint"`
	Fingerprint       string             `json:"fingerprint"`
	CumRows           int64              `json:"cum_rows"`
	CumRuns           int                `json:"cum_runs"`
	LastRun           *lastRunView       `json:"last_run"`
	Running           *runningView       `json:"running"`
	Creator           string             `json:"creator"`
	CreatedAt         string             `json:"created_at"`
	UpdatedAt         string             `json:"updated_at"`
	Version           uint               `json:"version"`
	Approval          *approvalSummary   `json:"approval"`
	RejectReason      string             `json:"reject_reason"`
	Allow             allowView          `json:"allow"`
}

func fmtT(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(timeLayout)
}

func fmtTP(t *time.Time) string {
	if t == nil {
		return ""
	}
	return fmtT(*t)
}

// viewer 在一次请求内缓存名称、审批、最近运行等查询，批量构造 TaskView。
type viewer struct {
	s     *Service
	c     Caller
	pol   Policy
	insts map[uint]string
	rgs   map[uint]string
	runs  map[uint]*Run
	auds  map[uint]*model.WorkflowAudit
	apOK  map[[2]uint]bool // [audit_id, 0] → 可审批
}

func (s *Service) newViewer(c Caller) *viewer {
	v := &viewer{s: s, c: c, pol: s.loadPolicy(), insts: map[uint]string{}, rgs: map[uint]string{}, runs: map[uint]*Run{}, auds: map[uint]*model.WorkflowAudit{}, apOK: map[[2]uint]bool{}}
	if list, err := s.instances(); err == nil {
		for _, in := range list {
			v.insts[in.ID] = in.InstanceName
		}
	}
	return v
}

func (v *viewer) rgName(id uint) string {
	if n, ok := v.rgs[id]; ok {
		return n
	}
	n := ""
	if g, err := v.s.repo.ResourceGroupByID(id); err == nil && g != nil {
		n = g.GroupName
	}
	v.rgs[id] = n
	return n
}

func (v *viewer) lastRun(t *Task) *Run {
	if t.LastRunID == 0 {
		return nil
	}
	if r, ok := v.runs[t.LastRunID]; ok {
		return r
	}
	r, _ := v.s.repo.GetRun(t.LastRunID)
	v.runs[t.LastRunID] = r
	return r
}

func (v *viewer) audit(t *Task) *model.WorkflowAudit {
	if t.AuditID == 0 {
		return nil
	}
	if a, ok := v.auds[t.AuditID]; ok {
		return a
	}
	a, _ := v.s.repo.AuditByWorkflow(audit.TypeArchive, t.ID, false)
	v.auds[t.AuditID] = a
	return a
}

func auditStatusText(s int) string {
	switch s {
	case audit.StatusWaiting:
		return "waiting"
	case audit.StatusPassed:
		return "passed"
	case audit.StatusRejected:
		return "rejected"
	case audit.StatusAborted:
		return "aborted"
	}
	return ""
}

func splitGroupIDs(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// approvalOf 生成审批概要，并判断调用者能否审批。
func (v *viewer) approvalOf(t *Task) (*approvalSummary, bool) {
	au := v.audit(t)
	if au == nil {
		return nil, false
	}
	groups := splitGroupIDs(au.AuditAuthGroups)
	sum := &approvalSummary{AuditID: au.AuditID, Status: auditStatusText(au.CurrentStatus), LevelCount: len(groups)}
	canApprove := false
	if au.CurrentStatus == audit.StatusWaiting {
		for i, g := range groups {
			if g == au.CurrentAudit {
				sum.CurrentLevel = i
			}
		}
		names := v.s.levelUsers(au, sum.CurrentLevel, au.CreateUser)
		gname := v.s.groupNameByID(groups, sum.CurrentLevel)
		who := "本级无其他可审批人，仅超级用户可审"
		if len(names) > 0 {
			who = strings.Join(names, "、")
		}
		sum.PendingText = fmt.Sprintf("第 %d/%d 级 · 待%s（%s）", sum.CurrentLevel+1, len(groups), gname, who)
		key := [2]uint{au.AuditID, v.c.ID}
		ok, seen := v.apOK[key]
		if !seen {
			ok = au.CreateUser != v.c.Username && audit.CanReview(v.s.repo, au, v.c.actor())
			v.apOK[key] = ok
		}
		canApprove = ok
	} else {
		sum.CurrentLevel = len(groups)
	}
	return sum, canApprove
}

func itoa(n int) string { return strconv.Itoa(n) }

// view 构造单个任务的对外形态。withSQL 控制是否带“将执行的 SQL”。
func (v *viewer) view(t *Task, withSQL bool) TaskView {
	s := v.s
	c := t.cfg()
	tv := TaskView{ID: t.ID, Name: t.Name, Status: t.Status, ResourceGroupID: t.ResourceGroupID, ResourceGroupName: v.rgName(t.ResourceGroupID),
		InstanceID: t.InstanceID, InstanceName: v.insts[t.InstanceID], DB: t.DBName, Table: t.TblName, TimeColumn: t.TimeColumn, TimeUnit: t.TimeUnit,
		RetentionDays: t.RetentionDays, ExtraWhere: t.ExtraWhere, Mode: t.Mode, PurgeReason: t.PurgeReason, DestTable: c.DestTable, DestFile: c.DestFile,
		Params: c.Params, Schedule: c.Schedule, ScheduleText: scheduleTextL(v.c.Lang, c.Schedule), WindowText: windowTextL(v.c.Lang, c.Params.Window),
		Checkpoint: t.checkpoint(), Fingerprint: t.Fingerprint, CumRows: t.CumRows, CumRuns: t.CumRuns, Creator: t.Creator,
		CreatedAt: fmtT(t.CreatedAt), UpdatedAt: fmtT(t.UpdatedAt), Version: t.Version, RejectReason: t.RejectReason}
	if tv.Checkpoint == nil {
		tv.Checkpoint = []string{}
	}
	spec := specOf(t, 0)
	if w, err := engine.BuildWhere(spec); err == nil {
		tv.Where = w
	}
	if withSQL {
		prev := engine.BuildSQLPreview(spec, nil, nil, s.now())
		tv.SQL = &prev
	}
	if t.Status == StEnabled {
		if t.NextRunAt != nil {
			tv.NextRunAt = fmtT(*t.NextRunAt)
		} else if n := nextRun(c.Schedule, s.now()); !n.IsZero() {
			tv.NextRunAt = fmtT(n)
		}
	}
	if t.Status == StPaused {
		tv.Pause = &pauseView{Kind: t.PauseKind, Reason: t.PauseReason, At: fmtTP(t.PauseAt)}
	}
	if r := v.lastRun(t); r != nil {
		tv.LastRun = &lastRunView{ID: r.ID, Result: r.Result, Trigger: r.Trigger, StartedAt: fmtT(r.StartedAt)}
	}
	live := s.liveOfTask(t.ID)
	if live != nil {
		st := live.state()
		tv.Running = &runningView{RunID: live.runID, State: st, Pause: st != "paused" && v.c.has("archive.pause"), Resume: st == "paused" && v.c.has("archive.pause"), Stop: v.c.has("archive.stop")}
	}
	sum, canApprove := v.approvalOf(t)
	tv.Approval = sum
	running := live != nil
	tv.Allow = allowView{
		Edit:    v.c.has("archive.apply") && !running,
		Check:   v.c.has("archive.dryrun"),
		Approve: t.Status == StPending && canApprove,
		Reject:  t.Status == StPending && canApprove,
		Run:     t.Status == StEnabled && !running && v.c.has("archive.run"),
		Pause:   t.Status == StEnabled && !running && v.c.has("archive.pause"),
		Resume:  t.Status == StPaused && t.PauseKind != PauseStructure && v.c.has("archive.pause"),
		Disable: (t.Status == StEnabled || t.Status == StPaused || t.Status == StPending) && !running && v.c.has("archive.delete"),
		Delete:  (t.Status == StDraft || t.Status == StRejected || t.Status == StDisabled) && !running && v.c.has("archive.delete"),
	}
	return tv
}

// levelUsers 返回审批流第 level 级的可审批用户名（权限组成员 ∩ 资源组成员，去掉申请人）。
func (s *Service) levelUsers(au *model.WorkflowAudit, level int, exclude string) []string {
	groups := splitGroupIDs(au.AuditAuthGroups)
	if level < 0 || level >= len(groups) {
		return nil
	}
	name := s.groupNameByID(groups, level)
	us, err := s.repo.AuthGroupUsers([]string{name}, au.GroupID)
	if err != nil {
		return nil
	}
	var out []string
	for _, u := range us {
		if u.Username != exclude {
			out = append(out, u.Username)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Service) groupNameByID(groups []string, level int) string {
	if level < 0 || level >= len(groups) {
		return ""
	}
	n := 0
	for _, ch := range groups[level] {
		if ch < '0' || ch > '9' {
			return groups[level]
		}
		n = n*10 + int(ch-'0')
	}
	if g, err := s.repo.AuthGroup(uint(n)); err == nil && g != nil {
		return g.Name
	}
	return groups[level]
}
