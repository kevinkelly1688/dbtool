// @author: kevinkelly
// @license: Apache Licence
// @file: status.go
// @time: 2026/10/28

package partman

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"dbtool/internal/model"
)

// 本文件提供「每行状态 / KPI」：读目标表的真实分区，用 plan.go 的计划算法推导状态。只读，不写审计与日志。
//
// 口径（与 BuildPlan 一致，不另起一套算法）：
//   - 待续建：BuildPlan 给出了要新增的分区（ADD，或含 MAXVALUE 时的 REORGANIZE），即“当前分区之后”不足 preCreate 个；
//     adds 为将创建的分区数（REORGANIZE 不计 MAXVALUE 分区本身）。
//   - 待清理：BuildPlan 给出了要删除的分区，即已完全过去的历史分区数超过 retain；drops 为将删除的分区数。
//   - 异常：BuildPlan 拒绝生成计划（表不存在、不是分区表、字段类型不符、边界不连续、一次要建太多……），message 为原因。
//   - 未知：读不到目标表（连不上、超时、无权限、排队超时），不代表配置有问题；message 为原因。
//   - 暂不支持：实例类型不是 MySQL / PolarDB，不连接目标库。
//
// 为了不让慢实例拖垮列表：状态走独立接口、前端逐行异步加载；服务端对单个实例限并发 2、全局限并发 6、
// 单次检查限时 statusTimeout；结果缓存 statusTTL（未知的只缓存 statusUnknownTTL，便于恢复后尽快重试）；
// 编辑 / 启停 / 执行 / 删除配置后立即失效。

const (
	StateOK          = "ok"
	StatePending     = "pending"
	StateError       = "error"
	StateUnknown     = "unknown"
	StateUnsupported = "unsupported"
	// StateUnpartitioned：MySQL 方言、表存在但未分区，可显式“初始化为分区表”（定时维护不会做）。
	StateUnpartitioned = "unpartitioned"

	statusTTL        = 60 * time.Second
	statusUnknownTTL = 15 * time.Second
	globalStatusConc = 6
	perInstConc      = 2
	maxSummaryItems  = 1000
)

// statusTimeout 是单次状态检查（排队 + 建连 + 读分区）的上限；测试里可改小。
var statusTimeout = 8 * time.Second

// StatusView 是一条配置的维护状态。
type StatusView struct {
	ID         uint   `json:"id"`
	State      string `json:"state"`                // ok / pending / error / unknown / unsupported
	Adds       int    `json:"adds"`                 // 待续建的分区数
	Drops      int    `json:"drops"`                // 待清理的分区数
	Reorganize bool   `json:"reorganize,omitempty"` // 续建需要 REORGANIZE（含 MAXVALUE 分区，定时维护不会执行）
	Partitions int    `json:"partitions"`           // 目标表现有分区数（读到时）
	Message    string `json:"message,omitempty"`
	Dialect    string `json:"dialect,omitempty"`
	CanInit    bool   `json:"can_init,omitempty"` // 仅 unpartitioned：前置检查通过，可以初始化
	// 可用分区数量（口径见 alert.go）：读不准时 Available 为 nil（页面显示“无法判断”，不告警）。
	Available     *int   `json:"available,omitempty"`
	AvailableEnd  string `json:"available_end,omitempty"`  // 最后一个可用分区的上界（预计用尽的时刻，分区边界所在时区）
	AvailableSecs int64  `json:"available_secs,omitempty"` // 距预计用尽还有多少秒
	HasMaxValue   bool   `json:"has_maxvalue,omitempty"`   // 表里有 MAXVALUE 兜底分区：用尽后写入不会失败，但续建需要 REORGANIZE
	CheckedAt     string `json:"checked_at"`
	Cached        bool   `json:"cached"`
}

type statusEntry struct {
	sig string // 配置内容签名：编辑过就不再命中旧缓存
	at  time.Time
	v   StatusView
}

func cfgSig(c *Config) string {
	return fmt.Sprintf("%d|%s|%s|%s|%s|%s|%d|%d|%d|%s", c.InstanceID, c.DBName, c.TbName, c.Column, c.ColType, c.Period, c.Interval, c.Retain, c.PreCreate, c.Timezone)
}

func (s *Service) cachedStatus(c *Config) (StatusView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.status[c.ID]
	if !ok || e.sig != cfgSig(c) {
		return StatusView{}, false
	}
	ttl := statusTTL
	if e.v.State == StateUnknown {
		ttl = statusUnknownTTL
	}
	if s.now().Sub(e.at) > ttl {
		return StatusView{}, false
	}
	v := e.v
	v.Cached = true
	return v, true
}

func (s *Service) storeStatus(c *Config, v StatusView) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status[c.ID] = statusEntry{sig: cfgSig(c), at: s.now(), v: v}
}

// invalidateStatus 在配置被改动 / 执行后丢弃缓存。
func (s *Service) invalidateStatus(id uint) {
	s.mu.Lock()
	delete(s.status, id)
	s.mu.Unlock()
}

// instSem 返回实例的并发闸（容量 perInstConc）。
func (s *Service) instSem(id uint) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.instSems[id]
	if !ok {
		ch = make(chan struct{}, perInstConc)
		s.instSems[id] = ch
	}
	return ch
}

// acquireSlots 依次拿全局与实例并发闸；ctx 到期（排队太久）返回错误。
func (s *Service) acquireSlots(ctx context.Context, instID uint) (func(), error) {
	inst := s.instSem(instID)
	select {
	case s.statusSem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case inst <- struct{}{}:
	case <-ctx.Done():
		<-s.statusSem
		return nil, ctx.Err()
	}
	return func() { <-inst; <-s.statusSem }, nil
}

// Status 返回一条配置的维护状态。refresh 为 true 时忽略缓存。
// 读不到目标表不是接口错误：返回 state=unknown（HTTP 200）；只有配置不存在 / 没有实例权限才返回错误。
func (s *Service) Status(ctx context.Context, c Caller, id uint, refresh bool) (*StatusView, error) {
	cfg, inst, err := s.load(c, id)
	if err != nil {
		return nil, err
	}
	v := StatusView{ID: cfg.ID}
	if !SupportedDBType(inst.DBType) {
		v.State, v.Message, v.CheckedAt = StateUnsupported, fmt.Sprintf("实例类型 %s 暂不支持分区维护", inst.DBType), s.now().Format(timeLayout)
		return &v, nil
	}
	if !refresh {
		if cv, ok := s.cachedStatus(cfg); ok {
			return &cv, nil
		}
	}
	v = s.probe(ctx, inst, cfg)
	s.storeStatus(cfg, v)
	return &v, nil
}

func (s *Service) probe(ctx context.Context, inst *model.Instance, cfg *Config) StatusView {
	v := StatusView{ID: cfg.ID, CheckedAt: s.now().Format(timeLayout)}
	if err := dbAllowed(inst, cfg.DBName); err != nil {
		v.State, v.Message = StateError, err.Error()
		return v
	}
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	release, err := s.acquireSlots(ctx, inst.ID)
	if err != nil {
		v.State, v.Message = StateUnknown, "该实例上正在检查的任务较多，排队超时，稍后重试"
		return v
	}
	defer release()
	tgt, err := s.dial(ctx, inst, s.meta.EngineConn(inst))
	if err != nil {
		v.State, v.Message = StateUnknown, unknownMsg("连接目标实例失败：%s", err)
		return v
	}
	defer tgt.Close()
	st, err := tgt.Inspect(ctx, cfg.DBName, cfg.TbName, cfg.Column)
	if err != nil {
		v.State, v.Message = StateUnknown, unknownMsg("读取目标表的分区信息失败：%s", err)
		return v
	}
	st.Dialect = DialectOf(inst.DBType)
	v.Dialect = st.Dialect
	v.Partitions = len(st.Partitions)
	if ai := availability(*cfg, st, s.now()); ai.Known { // 先于 BuildPlan：计划失败的表更需要知道还剩几个分区
		n := ai.Count
		v.Available, v.HasMaxValue = &n, ai.MaxValue
		if n > 0 {
			v.AvailableEnd, v.AvailableSecs = ai.End.In(ai.Loc).Format(timeLayout), int64(ai.End.Sub(s.now()).Seconds())
		}
	}
	p, err := BuildPlan(*cfg, st, s.now())
	if err != nil {
		v.State, v.Message = StateError, err.Error()
		return v
	}
	if p.Kind == KindInit {
		v.State, v.CanInit = StateUnpartitioned, p.Init.CanExecute
		if p.Init.CanExecute {
			v.Message = "表尚未分区，可在“预览 / 执行”里初始化为分区表"
		} else {
			v.Message = fmt.Sprintf("不能初始化：%s", firstInitError(p.Init))
		}
		return v
	}
	v.Drops = len(p.Drops)
	switch {
	case p.Reorganize != nil:
		v.Adds, v.Reorganize = len(p.Reorganize.Partitions)-1, true // 最后一项是被重建的 MAXVALUE 分区本身
	default:
		v.Adds = len(p.Adds)
	}
	v.State = StateOK
	if v.Adds > 0 || v.Drops > 0 {
		v.State = StatePending
	}
	return v
}

// unknownMsg 给“未知”状态附上原因；原因里不会有密码（驱动的错误只含地址与错误码）。
// format 是带一个 %s 的完整中文格式串（如 "连接目标实例失败：%s"），整句写出来才能按目录翻译。
func unknownMsg(format string, err error) string {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Message
	}
	m := err.Error()
	if r := []rune(m); len(r) > 200 {
		m = string(r[:200]) + "…"
	}
	return fmt.Sprintf(format, m)
}

// ---- 汇总（KPI 的分母 / 前端逐个拉状态用） ----

type SummaryItem struct {
	ID        uint `json:"id"`
	Enabled   bool `json:"isenable"`
	Supported bool `json:"supported"`
	// 可用分区告警：这张表自己的阈值（nil = 跟随全局，0 = 不告警）与是否在静默期内；KPI 的“可用分区不足”按它们判定。
	AlertThreshold *int `json:"alert_threshold"`
	AlertSilenced  bool `json:"alert_silenced,omitempty"`
}

// Summary 是调用者可见范围内全部配置的概况。配置数与“自动开”直接来自元数据库，不连目标库；
// 待续建 / 待清理 / 异常由前端对 Items 逐个调用状态接口后累计（状态走缓存，重复进入页面很快）。
type Summary struct {
	Total     int           `json:"total"`
	Enabled   int           `json:"enabled"`
	Supported int           `json:"supported"`
	Items     []SummaryItem `json:"items"`
	Truncated bool          `json:"truncated,omitempty"`
}

func (s *Service) Summary(c Caller) (*Summary, error) {
	insts, err := s.visible(c)
	if err != nil {
		return nil, wrap(err)
	}
	out := &Summary{Items: []SummaryItem{}}
	if len(insts) == 0 {
		return out, nil
	}
	byID := make(map[uint]*model.Instance, len(insts))
	ids := make([]uint, 0, len(insts))
	for i := range insts {
		byID[insts[i].ID] = &insts[i]
		ids = append(ids, insts[i].ID)
	}
	rows, _, err := s.repo.ListConfigs(ConfigFilter{InstanceIDs: ids})
	if err != nil {
		return nil, err
	}
	alerts := s.alertSettingsOf(rows)
	for _, r := range rows {
		sup := SupportedDBType(byID[r.InstanceID].DBType)
		out.Total++
		if r.IsEnable {
			out.Enabled++
		}
		if sup {
			out.Supported++
		}
		if len(out.Items) < maxSummaryItems {
			a := alerts[r.CnfID]
			out.Items = append(out.Items, SummaryItem{ID: r.ID, Enabled: r.IsEnable, Supported: sup, AlertThreshold: a.Threshold,
				AlertSilenced: a.SilenceUntil != nil && a.SilenceUntil.After(s.now())})
		} else {
			out.Truncated = true
		}
	}
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].ID < out.Items[j].ID })
	return out, nil
}

// ---- 一键启用 / 停用 ----

// SetEnabled 切换配置的“开启维护”开关。与编辑同权限（change_partmanconfig，按钮 partman.edit），同样要求实例受支持；写审计。
func (s *Service) SetEnabled(c Caller, id uint, enable bool) (*ConfigView, error) {
	if err := c.need(permChange, "启用 / 停用分区配置"); err != nil {
		return nil, err
	}
	cfg, inst, err := s.load(c, id)
	if err != nil {
		return nil, err
	}
	if !SupportedDBType(inst.DBType) {
		return nil, unsupportedErr(inst.DBType)
	}
	if cfg.IsEnable != enable {
		cfg.IsEnable = enable
		cfg.LastestTime = s.now()
		if err := s.repo.UpdateConfig(cfg); err != nil {
			return nil, err
		}
		s.invalidateStatus(cfg.ID)
		verb := "停用" // i18n-ignore：审计动作名，不翻译
		if enable {
			verb = "启用" // i18n-ignore：审计动作名，不翻译
		}
		s.auditBestEffort(c, "分区维护-"+verb+"配置", fmt.Sprintf("配置 #%d（实例 %s）：%s.%s %s维护", cfg.CnfID, inst.InstanceName, cfg.DBName, cfg.TbName, verb)) // i18n-ignore：审计日志正文，不翻译
	}
	v := view(*cfg, inst, time.Time{})
	return &v, nil
}
