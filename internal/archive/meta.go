// @author: kevinkelly
// @license: Apache Licence
// @file: meta.go
// @time: 2026/10/28

package archive

import (
	"dbtool/internal/audit"
	"dbtool/internal/query"
)

type enumOpt struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type metaMe struct {
	Username string `json:"username"`
	Display  string `json:"display"`
	IsSuper  bool   `json:"is_super"`
}

type metaEnums struct {
	Modes      []enumOpt `json:"modes"`
	TaskStatus []string  `json:"task_status"`
	RunResult  []string  `json:"run_result"`
	Triggers   []string  `json:"triggers"`
	StoreTypes []enumOpt `json:"store_types"`
	Formats    []string  `json:"file_formats"`
}

// Meta 是 GET /meta 的结果。表未建时只有 ready=false 与 me。
type Meta struct {
	Ready  bool        `json:"ready"`
	Me     metaMe      `json:"me"`
	Limits *limitsView `json:"limits,omitempty"`
	Policy *Policy     `json:"policy,omitempty"`
	Enums  *metaEnums  `json:"enums,omitempty"`
}

func (s *Service) Meta(c Caller) Meta {
	m := Meta{Me: metaMe{Username: c.Username, Display: c.Display, IsSuper: c.IsSuperuser}}
	if !s.repo.Ready() {
		return m
	}
	m.Ready = true
	l := limits
	m.Limits = &l
	p := s.loadPolicy()
	m.Policy = &p
	m.Enums = &metaEnums{
		Modes:      []enumOpt{{"move", "归档并删除"}, {"copy", "仅备份"}, {"purge", "仅清理"}},
		TaskStatus: []string{StDraft, StPending, StRejected, StEnabled, StPaused, StDisabled},
		RunResult:  []string{ResRunning, ResPaused, "success", "window_end", "load_abort", "limit_reached", "failed", "verify_failed", "stopped", "structure_changed", ResInterrupted, ResSkipped},
		Triggers:   []string{TrigSchedule, TrigManual, TrigRestore, TrigDryrun},
		StoreTypes: []enumOpt{{StoreLocal, "本地目录"}, {StoreNFS, "NFS 挂载目录"}, {StoreS3, "S3 兼容（含 MinIO）"}, {StoreOSS, "阿里云 OSS（S3 兼容端点）"}},
		Formats:    []string{"csv", "sql"},
	}
	return m
}

// PendingItem 是一张等待某人审批的归档申请（首页待办用）。
type PendingItem struct {
	ID    uint
	Name  string
	Sub   string
	Table string
}

// PendingReviewList 返回当前等待 a 审批的数据归档申请（最多 limit 张用于展示）与总数；规则与任务列表“待我审批”一致。
func (s *Service) PendingReviewList(a audit.Actor, limit int) ([]PendingItem, int, error) {
	if !s.repo.Ready() {
		return nil, 0, nil
	}
	c := Caller{Principal: query.Principal{ID: a.ID, Username: a.Username, Display: a.Display, IsSuperuser: a.IsSuperuser}}
	all, ids, err := s.scope(c)
	if err != nil {
		return nil, 0, err
	}
	tasks, _, err := s.repo.ListTasks(TaskFilter{AllGroups: all, ResourceGroupIDs: ids, Statuses: []string{StPending}, Limit: 300})
	if err != nil {
		return nil, 0, err
	}
	v := s.newViewer(c)
	var out []PendingItem
	total := 0
	for i := range tasks {
		t := &tasks[i]
		if _, ok := v.approvalOf(t); !ok {
			continue
		}
		total++
		if len(out) < limit {
			who := t.CreatorDisplay
			if who == "" {
				who = t.Creator
			}
			out = append(out, PendingItem{ID: t.ID, Name: t.Name, Table: t.DBName + "." + t.TblName,
				Sub: v.insts[t.InstanceID] + " / " + t.DBName + "." + t.TblName + " · " + modeLabel(t.Mode) + " · " + who + " · " + t.UpdatedAt.Format("01-02 15:04")})
		}
	}
	return out, total, nil
}

// PendingReview 返回当前等待 a 审批的数据归档申请数，供侧栏角标使用（与任务列表“待我审批”同一套规则）。
func (s *Service) PendingReview(a audit.Actor) (int, error) {
	_, n, err := s.PendingReviewList(a, 0)
	return n, err
}
