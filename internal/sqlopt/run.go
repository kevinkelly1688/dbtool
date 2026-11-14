// @author: kevinkelly
// @license: Apache Licence
// @file: run.go
// @time: 2026/10/28

package sqlopt

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/engine"
	"dbtool/internal/model"
)

// Parse 解析上传内容（给“SQL 列表”预览用，不连任何实例）。
func (s *Service) Parse(text string) ([]SQLRow, string, error) { return ParseText(text) }

// ---- SQL 分析任务 ----

type AnalyzeReq struct {
	Text       string `json:"text"`
	FileName   string `json:"file_name"`   // 上传的文件名，仅用于“最近报告”展示
	InstanceID uint   `json:"instance_id"` // 可不选：只做内置静态分析（和 SOAR 的离线分析）
	DB         string `json:"db"`
}

// StartAnalyze 创建分析任务并在后台逐条分析，立即返回任务视图（页面用任务 ID 轮询进度）。
//
// 每条 SQL：指纹/类型/表 → 内置规则 →（选了实例）表结构与执行计划 →（配置了 SOAR）SOAR 报告。
// 没配置 SOAR 不算错误，任务的 notice 里会说明。
func (s *Service) StartAnalyze(ctx context.Context, actor audit.Actor, req AnalyzeReq) (*JobView, error) {
	rows, format, err := ParseText(req.Text)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, bad("没有解析出任何 SQL，请检查文件内容")
	}
	var inst *model.Instance
	if req.InstanceID != 0 {
		if inst, err = s.instanceFor(actor, req.InstanceID); err != nil {
			return nil, err
		}
		if !toolDBTypes[inst.DBType] {
			return nil, newErr(http.StatusBadRequest, "unsupported", "SQL 体检仅支持 MySQL 系的实例（mysql / polardb / polardb-x）")
		}
		if req.DB != "" && !validDBName(req.DB) {
			return nil, bad("数据库名不合法")
		}
	}
	title := "SQL 体检"
	if inst != nil {
		title += " · " + inst.InstanceName
		if req.DB != "" {
			title += "/" + req.DB
		}
	}
	j := s.newJob(actor, KindAnalyze, title)
	j.File, j.Format, j.DBName = cleanFileName(req.FileName), format, req.DB
	if inst != nil {
		j.InstName = inst.InstanceName
	}
	var notices []string
	switch format {
	case FormatMapper:
		notices = append(notices, "已按 MyBatis Mapper XML 解析，#{} / ${} 参数已替换成占位符。")
	case FormatSlow:
		notices = append(notices, "已按 MySQL 慢日志解析，并带上每条记录的耗时、扫描行数统计。")
	}
	soar, soarErr := s.soarConfig()
	if soarErr != nil {
		notices = append(notices, fmt.Sprintf("SOAR 不可用，本次只输出内置规则分析：%s", soarErr.Error()))
	}
	if inst == nil {
		notices = append(notices, "没有选择实例：只做不依赖数据库的静态规则分析；选择实例和数据库后会读取表结构、分析执行计划并给出索引建议。")
	}
	j.Notice = strings.Join(notices, "\n")
	for i, r := range rows {
		it := &Item{Index: i, ID: r.ID, SQL: r.SQL, Stats: r.Stats, Status: StatusWaiting}
		if r.Err != "" {
			it.Status, it.Err = StatusFailed, r.Err
		} else {
			p := parseStmt(r.SQL)
			it.FP = Fingerprint(r.SQL)
			it.FPID = FingerprintID(it.FP)
			it.Type = p.kind
			it.Tables = p.tableNames()
		}
		j.Items = append(j.Items, it)
	}
	if err := s.jobs.add(j, s.now()); err != nil {
		return nil, err
	}
	instName := ""
	if inst != nil {
		instName = inst.InstanceName
	}
	s.record(actor, "SQL分析", fmt.Sprintf("任务:%s 条数:%d 实例:%s 库:%s 格式:%s", j.ID, len(rows), instName, req.DB, format))
	useSoar := soarErr == nil
	s.launch(j, func(ctx context.Context, j *Job) error {
		return s.runAnalyze(ctx, j, inst, req.DB, soar, useSoar)
	})
	return j.view(false), nil
}

func (s *Service) runAnalyze(ctx context.Context, j *Job, inst *model.Instance, db string, soar soarConf, useSoar bool) error {
	var t *target
	if inst != nil {
		rel, err := s.acquire(ctx, 0, s.sem)
		if err != nil {
			return nil
		}
		t, err = s.connect(ctx, inst, db)
		rel()
		if err != nil {
			// 连不上目标实例：降级为只做静态分析，并在任务提示里说明，而不是让整个任务失败
			j.mu.Lock()
			j.Notice += "\n无法使用所选实例，已降级为静态分析：" + err.Error()
			j.mu.Unlock()
			t = nil
		} else {
			defer t.close()
		}
	}
	for _, it := range j.Items {
		if ctx.Err() != nil {
			return nil
		}
		if it.Status == StatusFailed { // 解析阶段就已失败（如过长）
			continue
		}
		rel, err := s.acquire(ctx, 0, s.sem)
		if err != nil {
			return nil
		}
		j.setItem(it, StatusRunning, "")
		rep := s.analyzeOne(ctx, j, it, t, inst, db, soar, useSoar)
		rel()
		j.finishItem(it, rep, nil)
	}
	return nil
}

func (s *Service) analyzeOne(ctx context.Context, j *Job, it *Item, t *target, inst *model.Instance, db string, soar soarConf, useSoar bool) *Report {
	ictx, cancel := context.WithTimeout(ctx, adviseTimeout+soarTimeout)
	defer cancel()
	rep := s.buildReport(ictx, t, it.SQL)
	if useSoar {
		withOnline := t != nil && db != "" && toolDB(db) == nil
		var conn engine.Conn
		if inst != nil {
			conn = connOf(s.repo, inst)
		}
		if one, err := prepareExternal(it.SQL); err != nil {
			rep.SoarError = err.Error()
		} else if out, trunc, err := s.runSoar(ictx, soar, conn, db, one, MaxItemReportSize, withOnline); err != nil {
			rep.SoarError = err.Error()
		} else {
			rep.Soar = out
			if trunc {
				rep.Notes = append(rep.Notes, "SOAR 报告过长，已截断")
			}
		}
	}
	return rep
}

// ---- 优化工具任务 ----

type OptimizeReq struct {
	Tool       string   `json:"tool"` // soar / sqltuning
	InstanceID uint     `json:"instance_id"`
	DB         string   `json:"db"`
	SQL        string   `json:"sql"`
	Options    []string `json:"options"` // sqltuning：sys_parm / sql_plan / obj_stat / sql_profile
}

// ToolPerm 返回工具对应的权限码；工具名不认识返回空串。
func ToolPerm(tool string) string {
	switch tool {
	case ToolSOAR:
		return PermSOAR
	case ToolTuning:
		return PermSQLTuning
	}
	return ""
}

// StartOptimize 启动一次 SOAR / SQLTuning（后台任务，页面轮询结果）。权限码由调用方（HTTP 层）按 ToolPerm 校验。
func (s *Service) StartOptimize(ctx context.Context, actor audit.Actor, req OptimizeReq) (*JobView, error) {
	if ToolPerm(req.Tool) == "" {
		return nil, bad("不支持的优化方式")
	}
	var stmt string
	var err error
	if req.Tool == ToolTuning {
		stmt, err = prepareSelect(req.SQL)
	} else {
		stmt, err = prepareExternal(req.SQL)
	}
	if err != nil {
		return nil, err
	}
	inst, err := s.instanceFor(actor, req.InstanceID)
	if err != nil {
		return nil, err
	}
	if !toolDBTypes[inst.DBType] {
		return nil, newErr(http.StatusBadRequest, "unsupported", "SOAR / SQLTuning 仅支持 MySQL 系的实例（mysql / polardb / polardb-x），该类型实例请使用“执行计划”")
	}
	if req.DB == "" {
		return nil, bad("请选择数据库")
	}
	if req.DB != "" && !validDBName(req.DB) {
		return nil, bad("数据库名不合法")
	}
	var opts map[string]bool
	var soar soarConf
	switch req.Tool {
	case ToolSOAR:
		if err := toolDB(req.DB); err != nil {
			return nil, err
		}
		if soar, err = s.soarConfig(); err != nil {
			return nil, err
		}
		if soar.TestDSN == "" {
			return nil, newErr(http.StatusConflict, "tool_not_configured", "尚未配置 SOAR 的测试环境，请联系超级用户在“系统配置”页的“SQL 优化工具”里设置。")
		}
	case ToolTuning:
		opts = map[string]bool{}
		for _, o := range req.Options {
			switch o {
			case OptSysParm, OptSQLPlan, OptObjStat, OptSQLProfile:
				opts[o] = true
			default:
				return nil, bad(fmt.Sprintf("未知的 SQLTuning 选项：%s", o))
			}
		}
		if len(opts) == 0 {
			return nil, bad("请至少选择一项要显示的内容")
		}
	}
	label := map[string]string{ToolSOAR: "SOAR", ToolTuning: "SQLTuning"}[req.Tool]
	j := s.newJob(actor, KindOptimize, label+" · "+inst.InstanceName+"/"+req.DB)
	it := &Item{ID: "1", SQL: stmt, Status: StatusWaiting}
	p := parseStmt(stmt)
	it.FP = Fingerprint(stmt)
	it.FPID, it.Type, it.Tables = FingerprintID(it.FP), p.kind, p.tableNames()
	j.Items = []*Item{it}
	if err := s.jobs.add(j, s.now()); err != nil {
		return nil, err
	}
	s.record(actor, "SQL优化-"+label, fmt.Sprintf("任务:%s 实例:%s 库:%s SQL:%s", j.ID, inst.InstanceName, req.DB, stmt))
	s.launch(j, func(ctx context.Context, j *Job) error {
		rel, err := s.acquire(ctx, 0, s.sem)
		if err != nil {
			return nil
		}
		defer rel()
		j.setItem(it, StatusRunning, "")
		res, err := s.optimizeOne(ctx, inst, req, stmt, soar, opts)
		if err != nil {
			j.setItem(it, StatusFailed, err.Error())
			return nil
		}
		j.finishItem(it, nil, res)
		return nil
	})
	return j.view(false), nil
}

func (s *Service) optimizeOne(ctx context.Context, inst *model.Instance, req OptimizeReq, stmt string, soar soarConf, opts map[string]bool) (*OptimizeResult, error) {
	switch req.Tool {
	case ToolSOAR:
		// 先确认库在用户可见范围内
		tctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		t, err := s.connect(tctx, inst, req.DB)
		cancel()
		if err != nil {
			return nil, err
		}
		err = t.checkRefs(parseStmt(stmt))
		t.close()
		if err != nil {
			return nil, err
		}
		out, trunc, err := s.runSoar(ctx, soar, connOf(s.repo, inst), req.DB, stmt, MaxToolOutput, true)
		if err != nil {
			return nil, err
		}
		return &OptimizeResult{Tool: ToolSOAR, Output: out, Truncated: trunc}, nil
	default:
		tctx, cancel := context.WithTimeout(ctx, tuningTimeout)
		defer cancel()
		t, err := s.connect(tctx, inst, req.DB)
		if err != nil {
			return nil, err
		}
		defer t.close()
		if err := t.checkRefs(parseStmt(stmt)); err != nil {
			return nil, err
		}
		return &OptimizeResult{Tool: ToolTuning, Tuning: s.runTuning(tctx, t, stmt, opts)}, nil
	}
}

// cleanFileName 取上传文件名用于展示：去掉路径与控制字符，最多 80 个字符。
func cleanFileName(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	return truncRunes(name, 80)
}
