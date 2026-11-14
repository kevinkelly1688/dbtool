// @author: kevinkelly
// @license: Apache Licence
// @file: batch.go
// @time: 2026/10/28

package instmgr

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"dbtool/internal/instext"
)

// 实例列表的批量操作：批量停用 / 启用、批量开启 / 关闭采集、批量设置环境、批量删除。
//
// 每个实例仍然走单个实例的同一条路径（权限范围、状态检查、引用检查、审计），批量只是循环调用并逐个报告结果，
// 所以安全语义与逐个操作完全一致：被引用的实例删不掉、已经是目标状态的跳过、没有权限看到的实例报错。
// 批量删除先 dry_run 预检（只检查、不删），页面据此把“能删的 / 不能删的及原因”列给用户确认，再真正执行。

const maxBatch = 100 // 一次最多处理的实例数

// BatchItem 是批量操作里一个实例的结果。
type BatchItem struct {
	ID      uint   `json:"id"`
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// BatchResult 是一次批量操作的结果。
type BatchResult struct {
	Items  []BatchItem `json:"items"`
	Done   int         `json:"done"`
	Failed int         `json:"failed"`
	DryRun bool        `json:"dry_run,omitempty"`
}

func (r *BatchResult) add(it BatchItem) {
	r.Items = append(r.Items, it)
	if it.OK {
		r.Done++
	} else {
		r.Failed++
	}
}

// cleanIDs 去重并校验数量。
func cleanIDs(ids []uint) ([]uint, error) {
	seen := map[uint]bool{}
	var out []uint
	for _, id := range ids {
		if id != 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		return nil, bad("请先选择实例")
	}
	if len(out) > maxBatch {
		return nil, bad(fmt.Sprintf("一次最多处理 %d 个实例（现在选了 %d 个）", maxBatch, len(out)))
	}
	return out, nil
}

func errMessage(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Message
	}
	return "操作失败，请稍后重试"
}

// nameOf 取实例名用于结果展示（取不到时用 #ID，不让一个实例的错误影响其它实例）。
func (s *Service) nameOf(c Caller, id uint) string {
	if inst, err := s.visibleInstance(c, id); err == nil && inst != nil {
		return inst.InstanceName
	}
	return fmt.Sprintf("#%d", id)
}

// BatchDisable 批量停用（disabled=true）或启用（disabled=false）实例。已经是目标状态的算成功（无需操作）。
func (s *Service) BatchDisable(c Caller, ids []uint, disabled bool) (*BatchResult, error) {
	ids, err := cleanIDs(ids)
	if err != nil {
		return nil, err
	}
	if !s.instanceExtReady() {
		return nil, errExtMissing("停用 / 启用实例", "070_instance_ext.sql")
	}
	res := &BatchResult{Items: []BatchItem{}}
	for _, id := range ids {
		name := s.nameOf(c, id)
		err := s.SetInstanceDisabled(c, id, disabled)
		switch {
		case err == nil:
			res.add(BatchItem{ID: id, Name: name, OK: true})
		case strings.Contains(errMessage(err), "已经是"): // i18n-ignore 与本包生成的中文错误文案做匹配
			res.add(BatchItem{ID: id, Name: name, OK: true, Message: fmt.Sprintf("%s，无需操作", errMessage(err))})
		default:
			res.add(BatchItem{ID: id, Name: name, Message: errMessage(err)})
		}
	}
	action := "批量启用实例" // i18n-ignore 审计动作名，存库的枚举
	if disabled {
		action = "批量停用实例" // i18n-ignore 审计动作名，存库的枚举
	}
	s.audit(c, action, fmt.Sprintf("共 %d 个，成功 %d，失败 %d；%s", len(ids), res.Done, res.Failed, batchNames(res))) // i18n-ignore 审计详情，存库的文本不翻译
	return res, nil
}

// BatchSetEnv 批量设置实例环境（prod / test；空串表示“未设置”）。环境存旁路表 dbt_instance_ext。
// 每个实例仍然先过可见范围检查；已经是目标环境的算成功（无需操作）。
func (s *Service) BatchSetEnv(c Caller, ids []uint, env string) (*BatchResult, error) {
	ids, err := cleanIDs(ids)
	if err != nil {
		return nil, err
	}
	if err := validEnv(env); err != nil {
		return nil, err
	}
	if !s.instanceExtReady() {
		return nil, errExtMissing("设置实例环境", "070_instance_ext.sql")
	}
	label := nonEmptyStr(instext.EnvLabel(env), "未设置")
	res := &BatchResult{Items: []BatchItem{}}
	exts := s.extOf(ids)
	for _, id := range ids {
		inst, err := s.visibleInstance(c, id)
		if err != nil {
			res.add(BatchItem{ID: id, Name: fmt.Sprintf("#%d", id), Message: errMessage(err)})
			continue
		}
		name := inst.InstanceName
		if exts[id].Env == env {
			res.add(BatchItem{ID: id, Name: name, OK: true, Message: fmt.Sprintf("环境已经是%s，无需操作", label)})
			continue
		}
		if err := s.ext.SetInstanceEnv(id, env, s.now()); err != nil {
			slog.Error("批量设置实例环境失败", "instance", id, "err", err)
			res.add(BatchItem{ID: id, Name: name, Message: "操作失败，请稍后重试"})
			continue
		}
		res.add(BatchItem{ID: id, Name: name, OK: true})
	}
	s.audit(c, "批量设置环境", fmt.Sprintf("环境：%s；共 %d 个，成功 %d，失败 %d；%s", label, len(ids), res.Done, res.Failed, batchNames(res))) // i18n-ignore 审计，存库的文本不翻译
	return res, nil
}

// BatchCollect 批量开启（enable=true）或关闭（enable=false）慢日志采集（dbt_sql_instance.is_enable）。
// 与实例级“停用 / 启用”是两回事，只改采集开关，不动实例本身。
// 每个实例仍走单个实例的同一条路径（可见范围、审计）；已经是目标状态的算成功（无需操作）；
// 只支持在线查询的类型（PostgreSQL / MongoDB / ClickHouse）没有慢日志采集，单个报错，不写开关。
func (s *Service) BatchCollect(c Caller, ids []uint, enable bool) (*BatchResult, error) {
	ids, err := cleanIDs(ids)
	if err != nil {
		return nil, err
	}
	res := &BatchResult{Items: []BatchItem{}}
	for _, id := range ids {
		inst, err := s.visibleInstance(c, id)
		if err != nil {
			res.add(BatchItem{ID: id, Name: fmt.Sprintf("#%d", id), Message: errMessage(err)})
			continue
		}
		name := inst.InstanceName
		switch {
		case queryOnly(inst.DBType):
			res.add(BatchItem{ID: id, Name: name, Message: unsupportedMsg(inst.DBType, "慢日志采集")})
		case inst.IsEnable == enable:
			msg := "慢日志采集已经是关闭状态，无需操作"
			if enable {
				msg = "慢日志采集已经是开启状态，无需操作"
			}
			res.add(BatchItem{ID: id, Name: name, OK: true, Message: msg})
		default:
			if err := s.SetInstanceEnable(c, id, enable); err != nil {
				res.add(BatchItem{ID: id, Name: name, Message: errMessage(err)})
				continue
			}
			res.add(BatchItem{ID: id, Name: name, OK: true})
		}
	}
	action := "批量关闭慢日志采集" // i18n-ignore 审计动作名，存库的枚举
	if enable {
		action = "批量开启慢日志采集" // i18n-ignore 审计动作名，存库的枚举
	}
	s.audit(c, action, fmt.Sprintf("共 %d 个，成功 %d，失败 %d；%s", len(ids), res.Done, res.Failed, batchNames(res))) // i18n-ignore 审计详情，存库的文本不翻译
	return res, nil
}

// BatchDelete 批量删除实例。dryRun 为 true 只检查（谁能删、谁因被引用不能删），不删除；否则需要 confirm。
func (s *Service) BatchDelete(c Caller, ids []uint, confirm, dryRun bool) (*BatchResult, error) {
	ids, err := cleanIDs(ids)
	if err != nil {
		return nil, err
	}
	if !dryRun && !confirm {
		return nil, bad("请先确认删除")
	}
	res := &BatchResult{Items: []BatchItem{}, DryRun: dryRun}
	for _, id := range ids {
		name := s.nameOf(c, id)
		if dryRun {
			refs, err := s.InstanceRefs(c, id)
			if err != nil {
				res.add(BatchItem{ID: id, Name: name, Message: errMessage(err)})
				continue
			}
			var parts []string
			for _, r := range refs {
				if r.Blocking {
					parts = append(parts, c.tr("{label} {n} 条", "label", c.trp(r.Label), "n", r.Count))
				}
			}
			if len(parts) > 0 {
				res.add(BatchItem{ID: id, Name: name, Message: c.tr("仍被引用：{parts}", "parts", strings.Join(parts, c.tr("；")))})
				continue
			}
			res.add(BatchItem{ID: id, Name: name, OK: true})
			continue
		}
		if err := s.DeleteInstance(c, id, true); err != nil {
			res.add(BatchItem{ID: id, Name: name, Message: errMessage(err)})
			continue
		}
		res.add(BatchItem{ID: id, Name: name, OK: true})
	}
	if !dryRun {
		s.audit(c, "批量删除实例", fmt.Sprintf("共 %d 个，成功 %d，失败 %d；%s", len(ids), res.Done, res.Failed, batchNames(res))) // i18n-ignore 审计，存库的文本不翻译
	}
	return res, nil
}

// batchNames 把结果里的实例名拼成审计用的一段文字（成功的、失败的各列一部分）。
func batchNames(r *BatchResult) string {
	var ok, bad []string
	for _, it := range r.Items {
		if it.OK {
			ok = append(ok, it.Name)
		} else {
			bad = append(bad, it.Name)
		}
	}
	cut := func(l []string) string {
		if len(l) > 15 {
			return strings.Join(l[:15], "、") + fmt.Sprintf(" 等 %d 个", len(l)) // i18n-ignore 审计详情，存库的文本不翻译
		}
		return strings.Join(l, "、")
	}
	s := "成功：[" + cut(ok) + "]" // i18n-ignore 审计详情，存库的文本不翻译
	if len(bad) > 0 {
		s += "；失败：[" + cut(bad) + "]" // i18n-ignore 审计详情，存库的文本不翻译
	}
	return s
}
