// @author: kevinkelly
// @license: Apache Licence
// @file: logs.go
// @time: 2026/10/28

package partman

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"dbtool/internal/i18n"
)

// 全局“维护日志”（全部配置 × 来源筛选）。
//
// 来源、操作人、状态不需要新列也不需要旁路表：dbt_partman_log 不加列，但本包写入的 exec_result 每一项本来就带
// source（manual / auto）、operator、status（见 logItem），而外部程序写的项没有这些字段 —— 所以：
//   - 来源：项里有 source=manual / auto 取之，否则一律视为外部程序（external）；
//   - 操作人：取项里的 operator；外部程序的日志没有操作人，显示为空；
//   - 状态：本包写的项有 status（success / failed / skipped），全成功 = success，有失败且也有成功 = partial，否则 failed；
//     外部程序的项没有 status 时，只要有 error 字段就算 failed，否则为 unknown（不推定成功）。
// 按来源筛选在 SQL 里对 exec_result 做 LIKE（本包写的 JSON 是紧凑格式 "source":"manual"），展示的来源以解析结果为准。

const (
	SourceManual   = "manual"
	SourceAuto     = "auto"
	SourceExternal = "external"

	LogSuccess = "success"
	LogPartial = "partial"
	LogFailed  = "failed"
	LogUnknown = "unknown"
)

func validSource(s string) bool {
	return s == "" || s == SourceManual || s == SourceAuto || s == SourceExternal
}

// summarizeLog 从日志项里归纳来源、操作人与状态。
func summarizeLog(items []map[string]any) (source, operator, status string) {
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return strings.TrimSpace(s) }
	source = SourceExternal
	var ok, bad, withStatus int
	for _, m := range items {
		if src := str(m, "source"); src == SourceManual || src == SourceAuto {
			source = src
		}
		if operator == "" {
			operator = str(m, "operator")
		}
		switch st := strings.ToLower(str(m, "status")); st {
		case "success", "ok":
			ok++
			withStatus++
		case "failed", "skipped", "error":
			bad++
			withStatus++
		default:
			if str(m, "error") != "" {
				bad++
				withStatus++
			}
		}
	}
	switch {
	case withStatus == 0:
		status = LogUnknown
	case bad == 0:
		status = LogSuccess
	case ok == 0:
		status = LogFailed
	default:
		status = LogPartial
	}
	return source, operator, status
}

// GlobalLogQuery 是全局维护日志的筛选条件。
type GlobalLogQuery struct {
	ConfigID uint   // 配置的主键 ID，0 = 全部配置
	Source   string // manual / auto / external，空 = 全部
	// System 为 true 时只看“系统记录”（定时轮次摘要、推送测试；cnf_id=0），需要 change_partmanconfig。
	System bool
	Offset int
	Limit  int
}

// GlobalLogView 是全局日志的一行：日志本身 + 它所属配置的信息。
type GlobalLogView struct {
	LogView
	ConfigID     uint   `json:"config_id"`
	CnfID        int64  `json:"cnf_id"`
	InstanceName string `json:"instance_name"`
	Table        string `json:"table"`
	Scope        string `json:"scope"` // config：某个配置的维护日志；system：定时轮次摘要 / 推送测试（cnf_id=0）
}

// LogConfigOption 是筛选下拉里的一项（调用者可见的配置）。
type LogConfigOption struct {
	ID           uint   `json:"id"`
	CnfID        int64  `json:"cnf_id"`
	InstanceName string `json:"instance_name"`
	Table        string `json:"table"`
}

// GlobalLogs 列出调用者可见实例上全部配置的维护日志（最新在前）。
func (s *Service) GlobalLogs(c Caller, q GlobalLogQuery) ([]GlobalLogView, int64, []LogConfigOption, error) {
	if !validSource(q.Source) {
		return nil, 0, nil, badReq("来源只能是 manual、auto、external")
	}
	insts, err := s.visible(c)
	if err != nil {
		return nil, 0, nil, wrap(err)
	}
	opts := []LogConfigOption{}
	if len(insts) == 0 {
		return []GlobalLogView{}, 0, opts, nil
	}
	nameByInst := make(map[uint]string, len(insts))
	ids := make([]uint, 0, len(insts))
	for _, in := range insts {
		nameByInst[in.ID] = in.InstanceName
		ids = append(ids, in.ID)
	}
	cfgs, _, err := s.repo.ListConfigs(ConfigFilter{InstanceIDs: ids})
	if err != nil {
		return nil, 0, nil, err
	}
	byCnf := make(map[int64]Config, len(cfgs))
	for _, cf := range cfgs {
		byCnf[cf.CnfID] = cf
		opts = append(opts, LogConfigOption{ID: cf.ID, CnfID: cf.CnfID, InstanceName: nameByInst[cf.InstanceID], Table: cf.DBName + "." + cf.TbName})
	}
	sort.Slice(opts, func(i, j int) bool { return opts[i].ID < opts[j].ID })
	var cnfIDs []int64
	for _, cf := range cfgs {
		if q.ConfigID == 0 || cf.ID == q.ConfigID {
			cnfIDs = append(cnfIDs, cf.CnfID)
		}
	}
	if q.ConfigID != 0 && len(cnfIDs) == 0 {
		return nil, 0, nil, newErr(404, "not_found", "分区配置不存在")
	}
	if q.Limit <= 0 || q.Limit > 100 {
		q.Limit = 20
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	// 系统记录（cnf_id=0）：只给有 change_partmanconfig 的人；只看系统记录时不要配置的日志。
	withSystem := c.has(permChange) && q.ConfigID == 0
	if q.System {
		if !c.has(permChange) {
			return nil, 0, nil, newErr(403, "forbidden", "没有权限查看系统记录（需要 %s）", permChange)
		}
		cnfIDs, withSystem = nil, true
	}
	if withSystem {
		cnfIDs = append(cnfIDs, 0)
	}
	if len(cnfIDs) == 0 {
		return []GlobalLogView{}, 0, opts, nil
	}
	rows, total, err := s.repo.ListLogsByCnf(cnfIDs, q.Source, q.Offset, q.Limit)
	if err != nil {
		return nil, 0, nil, err
	}
	out := make([]GlobalLogView, 0, len(rows))
	for _, r := range rows {
		if r.CnfID == 0 {
			lv := parseLog(r)
			if lines := visibleTrace(c, r.ExecResult, byCnf); lines != nil {
				lv.OutputLines = len(lines)
			}
			lv.Round = visibleRound(c, lv.Round, byCnf)
			out = append(out, GlobalLogView{LogView: lv, Scope: "system", Table: "（定时维护轮次 / 推送测试）"})
			continue
		}
		cf := byCnf[r.CnfID]
		out = append(out, GlobalLogView{LogView: parseLog(r), ConfigID: cf.ID, CnfID: r.CnfID, InstanceName: nameByInst[cf.InstanceID], Table: cf.DBName + "." + cf.TbName, Scope: "config"})
	}
	return out, total, opts, nil
}

// visibleTrace 取一条日志的运行输出，并按调用者可见范围过滤：属于某个配置（c 不为 0）的行，只有该配置在调用者可见范围内才保留；
// 超级用户全部可见。没有 trace 返回 nil。
func visibleTrace(c Caller, raw string, visible map[int64]Config) []traceLine {
	lines := extractTrace(raw)
	if lines == nil || c.IsSuperuser {
		return lines
	}
	out := make([]traceLine, 0, len(lines))
	for _, l := range lines {
		if l.C != 0 {
			if _, ok := visible[l.C]; !ok {
				continue
			}
		}
		out = append(out, l)
	}
	return out
}

// visibleRound 按可见范围过滤定时轮次摘要里的配置项（超级用户全部可见），总数不变。
func visibleRound(c Caller, r *RoundSummary, visible map[int64]Config) *RoundSummary {
	if r == nil || c.IsSuperuser {
		return r
	}
	cp := *r
	cp.Items = make([]RoundItem, 0, len(r.Items))
	for _, it := range r.Items {
		if _, ok := visible[it.CnfID]; ok {
			cp.Items = append(cp.Items, it)
		}
	}
	return &cp
}

// LogDetail 是单条日志的明细：与列表项相同，另带完整运行输出。
type LogDetail struct {
	GlobalLogView
	Output *OutputView `json:"output"`
}

// logAccess 校验调用者能看这条日志，返回日志与（可见范围内的）配置表。
func (s *Service) logAccess(c Caller, logID uint) (*Log, map[int64]Config, *Config, error) {
	lg, err := s.repo.GetLog(logID)
	if err != nil {
		return nil, nil, nil, err
	}
	if lg == nil {
		return nil, nil, nil, newErr(404, "not_found", "日志不存在")
	}
	insts, err := s.visible(c)
	if err != nil {
		return nil, nil, nil, wrap(err)
	}
	ids := make([]uint, 0, len(insts))
	for _, in := range insts {
		ids = append(ids, in.ID)
	}
	byCnf := map[int64]Config{}
	if len(ids) > 0 {
		cfgs, _, err := s.repo.ListConfigs(ConfigFilter{InstanceIDs: ids})
		if err != nil {
			return nil, nil, nil, err
		}
		for _, cf := range cfgs {
			byCnf[cf.CnfID] = cf
		}
	}
	if lg.CnfID == 0 {
		if !c.has(permChange) {
			return nil, nil, nil, newErr(403, "forbidden", "没有权限查看系统记录（需要 %s）", permChange)
		}
		return lg, byCnf, nil, nil
	}
	cf, ok := byCnf[lg.CnfID]
	if !ok { // 不存在或不在可见范围：统一 404，不暴露存在性
		return nil, nil, nil, newErr(404, "not_found", "日志不存在")
	}
	return lg, byCnf, &cf, nil
}

// LogDetail 返回单条日志（含完整运行输出）。
func (s *Service) LogDetail(c Caller, logID uint) (*LogDetail, error) {
	lg, byCnf, cf, err := s.logAccess(c, logID)
	if err != nil {
		return nil, err
	}
	d := &LogDetail{GlobalLogView: GlobalLogView{LogView: parseLog(*lg), CnfID: lg.CnfID, Scope: "system", Table: "（定时维护轮次 / 推送测试）"}}
	if cf != nil {
		d.ConfigID, d.Scope, d.Table = cf.ID, "config", cf.DBName+"."+cf.TbName
		if insts, err := s.visible(c); err == nil {
			for _, in := range insts {
				if in.ID == cf.InstanceID {
					d.InstanceName = in.InstanceName
				}
			}
		}
	}
	lines := visibleTrace(c, lg.ExecResult, byCnf)
	d.Output = outputView(lines)
	d.OutputLines = len(d.Output.Lines)
	d.Round = visibleRound(c, d.Round, byCnf)
	return d, nil
}

// LogOutput 返回单条日志运行输出的纯文本（下载用）与建议的文件名。
func (s *Service) LogOutput(c Caller, logID uint) (filename, text string, err error) {
	lg, byCnf, _, err := s.logAccess(c, logID)
	if err != nil {
		return "", "", err
	}
	lines := visibleTrace(c, lg.ExecResult, byCnf)
	who := strconv.FormatInt(lg.CnfID, 10)
	if lg.CnfID == 0 {
		who = "system"
	}
	filename = fmt.Sprintf("partman_%s_%s.log", who, lg.CreateTime.Format("20060102_150405"))
	if len(lines) == 0 {
		return filename, i18n.T(c.Lang, "（这条日志没有运行输出：它早于运行输出功能，或由外部程序写入。语句与结果见日志明细。）") + "\n", nil
	}
	if c.Lang != "" && c.Lang != i18n.ZH { // 导出文件里的消息也按请求语言翻译（日志里存的是中文原文）
		for i := range lines {
			lines[i].M = tpText(c.Lang, lines[i].M)
		}
	}
	return filename, renderOutput(lines), nil
}
