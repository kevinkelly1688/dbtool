// @author: kevinkelly
// @license: Apache Licence
// @file: state.go
// @time: 2026/10/28

package giconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// snapshot 是某一时刻的“磁盘文件 + 版本状态”。
type snapshot struct {
	path, source string
	disk         []byte
	diskInfo     FileInfo
	diskErr      error
	latest       *Version
	effective    *Version // 最近一个 applied
	pending      *Version // 比 effective 更新的 draft（已写入磁盘、尚未重启生效）
}

func (s *Service) snapshot() (*snapshot, error) {
	sn := &snapshot{}
	var err error
	sn.path, sn.source, err = s.configPath()
	if err != nil {
		sn.diskErr = err
	}
	if sn.path != "" && sn.diskErr == nil {
		sn.disk, sn.diskInfo, sn.diskErr = s.files.Read(sn.path)
	}
	if !s.repo.TableReady() {
		return sn, nil
	}
	if sn.latest, err = s.repo.LatestVersion(); err != nil {
		return nil, newErr(500, "load_failed", "读取版本历史失败：%v", err)
	}
	if sn.effective, err = s.repo.LatestByStatus(StatusApplied); err != nil {
		return nil, newErr(500, "load_failed", "读取版本历史失败：%v", err)
	}
	if d, err := s.repo.LatestByStatus(StatusDraft); err != nil {
		return nil, newErr(500, "load_failed", "读取版本历史失败：%v", err)
	} else if d != nil && (sn.effective == nil || d.Version > sn.effective.Version) {
		sn.pending = d
	}
	return sn, nil
}

// expected 是磁盘上应该有的版本：有待生效的草稿就是草稿，否则是当前生效版本。
func (sn *snapshot) expected() *Version {
	if sn.pending != nil {
		return sn.pending
	}
	return sn.effective
}

func (sn *snapshot) latestNum() int {
	if sn.latest == nil {
		return 0
	}
	return sn.latest.Version
}

// Drift 描述磁盘文件与版本记录是否一致。
type Drift struct {
	Kind    string `json:"kind"` // none / modified / missing / unmanaged
	Message string `json:"message"`
}

func (sn *snapshot) drift() Drift {
	exp := sn.expected()
	switch {
	case sn.diskErr != nil:
		return Drift{Kind: "error", Message: fmt.Sprintf("读取配置文件失败：%s", sn.diskErr.Error())}
	case sn.path == "":
		return Drift{Kind: "none", Message: "还没有配置配置文件路径"}
	case exp == nil && !sn.diskInfo.Exists:
		return Drift{Kind: "none", Message: "配置文件还不存在，第一次保存时会创建"}
	case exp == nil:
		return Drift{Kind: "unmanaged", Message: "磁盘上的配置文件还没有纳入版本管理；第一次保存时会以它为对比基线，并留一份备份"}
	case !sn.diskInfo.Exists:
		return Drift{Kind: "missing", Message: fmt.Sprintf("磁盘上的配置文件不存在（应为 #%d）", exp.Version)}
	case sha256hex(sn.disk) == exp.ContentSHA256:
		return Drift{Kind: "none", Message: fmt.Sprintf("磁盘文件与 #%d 一致", exp.Version)}
	}
	return Drift{Kind: "modified", Message: fmt.Sprintf("磁盘文件与 #%d 不一致：有人绕过本页面直接改过它", exp.Version)}
}

// baseline 是编辑时的对比基线：纳入版本管理后是期望版本的内容，否则是磁盘内容。
func (sn *snapshot) baseline() string {
	if exp := sn.expected(); exp != nil {
		return exp.Content
	}
	return string(sn.disk)
}

// VersionMeta 是版本列表里的一行。
type VersionMeta struct {
	Version       int        `json:"version"`
	CreatedAt     time.Time  `json:"created_at"`
	Operator      string     `json:"operator"`
	ChangeSummary string     `json:"change_summary"`
	Status        string     `json:"status"`
	Label         string     `json:"label"` // 显示用状态：当前生效 / 待重启生效 / 已被替换 / 重启失败 …
	Current       bool       `json:"current"`
	AppliedAt     *time.Time `json:"applied_at,omitempty"`
	Error         string     `json:"error,omitempty"`
}

func (s *Service) meta(v Version, sn *snapshot) VersionMeta {
	m := VersionMeta{Version: v.Version, CreatedAt: v.CreatedAt, Operator: v.Operator, ChangeSummary: v.ChangeSummary,
		Status: v.Status, AppliedAt: v.AppliedAt, Error: v.Error}
	switch v.Status {
	case StatusApplied:
		if sn.effective != nil && sn.effective.Version == v.Version {
			m.Label, m.Current = "当前生效", true
		} else {
			m.Label = "已被替换"
		}
	case StatusDraft:
		if sn.pending != nil && sn.pending.Version == v.Version {
			m.Label = "已保存，待重启生效"
		} else {
			m.Label = "已被替换"
		}
	case StatusFailed:
		m.Label = "重启失败"
	case StatusRolledBack:
		m.Label = "重启失败，已自动恢复上一版本"
	}
	return m
}

// RestartRecord 是最近一次重启的结果（存在 dbt_gi_setting 里）。
type RestartRecord struct {
	Version    int       `json:"version"` // 0 表示只重启、没有换配置
	OK         bool      `json:"ok"`
	Result     string    `json:"result"` // success / rolledback / failed
	Message    string    `json:"message"`
	Operator   string    `json:"operator"`
	At         time.Time `json:"at"`
	DurationMS int64     `json:"duration_ms"`
}

func (s *Service) lastRestart() *RestartRecord {
	raw, err := s.repo.LoadState("last_restart")
	if err != nil || raw == "" {
		return nil
	}
	var r RestartRecord
	if json.Unmarshal([]byte(raw), &r) != nil {
		return nil
	}
	return &r
}

// FileView 是配置文件的展示形式（密码已脱敏）。
type FileView struct {
	Path       string    `json:"path"`
	Source     string    `json:"source"` // env / config / none
	Exists     bool      `json:"exists"`
	ModTime    time.Time `json:"mod_time"`
	Mode       string    `json:"mode,omitempty"`
	ReadError  string    `json:"read_error,omitempty"`
	Drift      Drift     `json:"drift"`
	Editor     string    `json:"editor"`      // 编辑框初始内容（已脱敏）
	EditorFrom string    `json:"editor_from"` // 内容来源说明
}

// Overview 是页面首屏需要的全部数据。
type Overview struct {
	TableReady bool           `json:"table_ready"`
	Conn       ConnView       `json:"conn"`
	Backup     ConnView       `json:"backup"`
	File       FileView       `json:"file"`
	Effective  *VersionMeta   `json:"effective,omitempty"`
	Pending    *VersionMeta   `json:"pending,omitempty"`
	LatestNum  int            `json:"latest_version"` // 乐观并发：保存时带回来
	Versions   []VersionMeta  `json:"versions"`
	Deploy     DeployView     `json:"deploy"`
	Catalog    []FormGroup    `json:"catalog"`
	LastResult *RestartRecord `json:"last_restart,omitempty"`
	Job        *JobView       `json:"job,omitempty"`
}

const editorTemplate = "# goInception 配置文件\n# 常用参数可以在左侧表单里改，其余参数直接在这里编辑。\n\n[inc]\n" // i18n-ignore 写进配置文件的注释，不翻译

// Overview 汇总页面首屏数据。
func (s *Service) Overview() (*Overview, error) {
	o := &Overview{TableReady: s.repo.TableReady(), Catalog: FormGroups(), Versions: []VersionMeta{}}
	o.Conn, o.Backup = connView(s.loadConn("main")), connView(s.loadConn("backup"))
	d, err := s.loadDeploy()
	if err != nil {
		return nil, newErr(500, "load_failed", "读取部署设置失败：%v", err)
	}
	o.Deploy = s.deployView(d)
	sn, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	fv := FileView{Path: sn.path, Source: sn.source, Exists: sn.diskInfo.Exists, ModTime: sn.diskInfo.ModTime, Drift: sn.drift()}
	if sn.diskInfo.Exists {
		fv.Mode = fmt.Sprintf("%04o", uint32(sn.diskInfo.Mode))
	}
	if sn.diskErr != nil {
		fv.ReadError = sn.diskErr.Error()
	}
	switch {
	case sn.pending != nil:
		fv.Editor, fv.EditorFrom = MaskContent(sn.pending.Content), fmt.Sprintf("已保存、待重启生效的 #%d", sn.pending.Version)
	case sn.diskInfo.Exists:
		fv.Editor, fv.EditorFrom = MaskContent(string(sn.disk)), "磁盘上的当前文件"
	case sn.effective != nil:
		fv.Editor, fv.EditorFrom = MaskContent(sn.effective.Content), fmt.Sprintf("当前生效的 #%d", sn.effective.Version)
	default:
		fv.Editor, fv.EditorFrom = editorTemplate, "空白模板"
	}
	o.File = fv
	o.LatestNum = sn.latestNum()
	if sn.effective != nil {
		m := s.meta(*sn.effective, sn)
		o.Effective = &m
	}
	if sn.pending != nil {
		m := s.meta(*sn.pending, sn)
		o.Pending = &m
	}
	if o.TableReady {
		list, err := s.repo.ListVersions(keepVersions)
		if err != nil {
			return nil, newErr(500, "load_failed", "读取版本历史失败：%v", err)
		}
		for _, v := range list {
			o.Versions = append(o.Versions, s.meta(v, sn))
		}
		o.LastResult = s.lastRestart()
	}
	o.Job = s.JobView()
	return o, nil
}

// Status 是“进程状态”卡片：取不到的项为空，页面显示“未知”。
type Status struct {
	State       string         `json:"state"` // running / down / unknown
	StateLabel  string         `json:"state_label"`
	Message     string         `json:"message,omitempty"`
	Version     string         `json:"version,omitempty"`
	LatencyMS   int64          `json:"latency_ms,omitempty"`
	Connections *int           `json:"connections,omitempty"`
	Running     *int           `json:"running,omitempty"`
	Tasks       []Task         `json:"tasks,omitempty"`
	TaskNote    string         `json:"task_note,omitempty"`
	Drift       Drift          `json:"drift"`
	Effective   int            `json:"effective_version,omitempty"`
	Pending     int            `json:"pending_version,omitempty"`
	LastResult  *RestartRecord `json:"last_restart,omitempty"`
	Job         *JobView       `json:"job,omitempty"`
}

// Status 探测 goInception 的运行状态。
func (s *Service) Status(ctx context.Context) (*Status, error) {
	sn, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	st := &Status{State: "unknown", StateLabel: "未知", Drift: sn.drift(), Job: s.JobView()}
	if sn.effective != nil {
		st.Effective = sn.effective.Version
	}
	if sn.pending != nil {
		st.Pending = sn.pending.Version
	}
	if s.repo.TableReady() {
		st.LastResult = s.lastRestart()
	}
	c := s.loadConn("main")
	if c.Host == "" {
		st.Message = "还没有配置 goInception 的连接信息"
		return st, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	r, err := s.prober.Ping(ctx, c)
	if err != nil {
		st.State, st.StateLabel, st.Message = "down", "无法连接", err.Error()
		return st, nil
	}
	st.State, st.StateLabel, st.Version, st.LatencyMS = "running", "运行中", r.Version, r.LatencyMS
	if r.Note != "" {
		st.Message = r.Note
	}
	t, err := s.prober.Tasks(ctx, c)
	if err == nil && t.Known {
		conns, run := t.Connections, t.Running
		st.Connections, st.Running, st.Tasks = &conns, &run, t.Items
	} else if err == nil {
		st.TaskNote = t.Note
	} else {
		st.TaskNote = err.Error()
	}
	return st, nil
}

// ---- 版本查看与对比 ----

// VersionDetail 是单个版本的详情（密码已脱敏）。
type VersionDetail struct {
	VersionMeta
	Content string `json:"content"`
	SHA256  string `json:"content_sha256"`
}

func (s *Service) GetVersion(num int) (*VersionDetail, error) {
	if err := s.needTables(); err != nil {
		return nil, err
	}
	v, err := s.repo.GetVersion(num)
	if err != nil {
		return nil, newErr(500, "load_failed", "读取版本失败：%v", err)
	}
	if v == nil {
		return nil, newErr(404, "not_found", "版本不存在（可能已超过保留数量被清理）")
	}
	sn, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	return &VersionDetail{VersionMeta: s.meta(*v, sn), Content: MaskContent(v.Content), SHA256: v.ContentSHA256}, nil
}

// DiffResponse 是版本对比结果。
type DiffResponse struct {
	From *VersionMeta `json:"from,omitempty"`
	To   VersionMeta  `json:"to"`
	Diff DiffResult   `json:"diff"`
}

// DiffVersions 对比版本 num 与 base；base 为 0 表示它的上一个版本。
func (s *Service) DiffVersions(num, base int) (*DiffResponse, error) {
	if err := s.needTables(); err != nil {
		return nil, err
	}
	to, err := s.repo.GetVersion(num)
	if err != nil {
		return nil, newErr(500, "load_failed", "读取版本失败：%v", err)
	}
	if to == nil {
		return nil, newErr(404, "not_found", "版本不存在（可能已超过保留数量被清理）")
	}
	if base == 0 {
		base = num - 1
	}
	sn, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	res := &DiffResponse{To: s.meta(*to, sn)}
	oldContent := ""
	if base > 0 {
		from, err := s.repo.GetVersion(base)
		if err != nil {
			return nil, newErr(500, "load_failed", "读取版本失败：%v", err)
		}
		if from != nil {
			m := s.meta(*from, sn)
			res.From = &m
			oldContent = from.Content
		}
	}
	res.Diff = Diff(oldContent, to.Content, 3)
	return res, nil
}

// ---- 编辑辅助（校验 / 表单同步 / 变更对比） ----

// ParamsResult 是源文件解析后给表单用的数据。
type ParamsResult struct {
	Report    Report         `json:"report"`
	Values    map[string]any `json:"values"`
	SecretSet []string       `json:"secret_set"` // 已设置（非空）的密码类参数
}

// ValidateEdit 校验编辑器里的内容。页面源文件把密码显示成占位符，这里先按保存时同样的方式把占位符
// 还原成磁盘 / 版本里的原值再校验，避免“校验”按钮对没改动的密码误报；找不到原值时（例如整份文件是新贴的）
// 仍按原文校验，占位符会照常报错。
func (s *Service) ValidateEdit(content string) Report {
	if s == nil || s.env == nil || s.repo == nil { // 没有依赖（例如只做语法校验的零值服务）：按原文校验
		return Validate(content)
	}
	if sn, err := s.snapshot(); err == nil {
		if merged, err := s.mergeSecrets(content, nil, sn); err == nil {
			return Validate(merged)
		}
	}
	return Validate(content)
}

// ParseParams 校验源文件并取出表单要显示的参数值（语法错误时 Values 为空）。
func (s *Service) ParseParams(content string) *ParamsResult {
	res := &ParamsResult{Report: s.ValidateEdit(content), Values: map[string]any{}, SecretSet: []string{}}
	vals, err := ParseValues(content)
	if err != nil {
		return res
	}
	for id, v := range vals {
		if IsSensitiveKey(id) {
			if str, ok := v.(string); ok && str != "" {
				res.SecretSet = append(res.SecretSet, id)
			}
			continue
		}
		switch v.(type) {
		case bool, int64, float64, string:
			res.Values[id] = v
		}
	}
	return res
}

// Change 是表单里的一处改动。
type Change struct {
	Section string `json:"section"`
	Key     string `json:"key"`
	Value   any    `json:"value"`
}

// PatchContent 把表单改动应用到源文件文本上（保留注释与排版）。密码类参数不走这里。
func (s *Service) PatchContent(content string, changes []Change) (string, error) {
	if len(content) > MaxContentBytes {
		return "", badReq("配置文件过大")
	}
	if len(changes) > 100 {
		return "", badReq("一次最多修改 100 个参数")
	}
	out := normalizeNewlines(content)
	for _, c := range changes {
		if IsSensitiveKey(c.Key) {
			return "", badReq("%s 是密码类参数，不能通过表单同步写入源文件", c.Key)
		}
		var err error
		if out, err = SetKey(out, c.Section, c.Key, c.Value); err != nil {
			return "", badReq("%s", err.Error())
		}
	}
	return out, nil
}

// PreviewDiff 对比“待保存的内容”与当前基线（待生效版本，或当前生效版本，或磁盘文件）。
func (s *Service) PreviewDiff(content string, secrets map[string]string) (*DiffResponse, error) {
	sn, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	merged, err := s.mergeSecrets(content, secrets, sn)
	if err != nil {
		merged = normalizeNewlines(content) // 占位符找不到原值时，仍然给出对比；保存时会明确报错
	}
	base := sn.baseline()
	res := &DiffResponse{Diff: Diff(base, merged, 3)}
	if exp := sn.expected(); exp != nil {
		m := s.meta(*exp, sn)
		res.From = &m
	}
	return res, nil
}

// mergeSecrets 规整换行，把密码占位符还原为磁盘/版本里的原值，再写入页面上新填的密码。
func (s *Service) mergeSecrets(content string, secrets map[string]string, sn *snapshot) (string, error) {
	out := normalizeNewlines(content)
	bases := []string{string(sn.disk)}
	if sn.expected() != nil {
		bases = append(bases, sn.expected().Content)
	}
	if sn.effective != nil {
		bases = append(bases, sn.effective.Content)
	}
	out, err := Unmask(out, bases...)
	if err != nil {
		return "", badReq("%s", err.Error())
	}
	for id, v := range secrets {
		section, key := "", id
		if i := strings.LastIndex(id, "."); i >= 0 {
			section, key = id[:i], id[i+1:]
		}
		if !IsSensitiveKey(key) {
			return "", badReq("%s 不是密码类参数", id)
		}
		if v == "" {
			continue
		}
		if len(v) > 200 || strings.ContainsAny(v, "\r\n\x00") {
			return "", badReq("%s 的值过长或含有换行", id)
		}
		if out, err = SetKey(out, section, key, v); err != nil {
			return "", badReq("%s", err.Error())
		}
	}
	return out, nil
}
