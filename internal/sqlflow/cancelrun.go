// @author: kevinkelly
// @license: Apache Licence
// @file: cancelrun.go
// @time: 2026/10/28

package sqlflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"dbtool/internal/audit"
	"dbtool/internal/model"
	"dbtool/internal/review"
)

// 取消执行：工单“执行中”的时间太长（大表变更、锁等待……）时，有权限的人（DBA）可以主动取消。
//
// 做法：
//  1. 每张正在执行的工单在本进程里登记一个取消句柄（runCtl），取消就是取消执行用的 context：
//     原生执行（MySQL 系 / SQL Server / Redis 等）会中断正在执行的那条语句，其后的语句不再执行；
//     MySQL 系在驱动关连接之外，还会补一条 KILL QUERY，让目标库真的停掉这条语句（见 engine/mysql 的 ExecEach）。
//  2. 取消不会回滚已经执行成功的语句；被中断的那一条由目标库回滚（大事务回滚本身也要时间）。
//  3. 工单最终是“执行有异常”，工单日志里写明谁、为什么取消，不会自动重试，必须由人到目标库核对。
//  4. 只能取消“由本进程在执行”的工单；别的进程在执行时无法远程取消，要到那个进程所在节点操作。
//
// goInception 执行的工单：取消只会断开与 goInception 的连接并把工单标成异常，goInception 已经下发到目标库的语句
// 可能仍在执行，需要到“会话管理”里核对并终止；页面确认框里有这条提示。

// errRunCancelled 是取消执行时放进 context 的原因。
var errRunCancelled = errors.New("工单执行被人工取消")

// runCtl 是一张正在执行的工单的取消句柄（只存在于本进程内存）。
type runCtl struct {
	cancel context.CancelCauseFunc

	mu     sync.Mutex
	asked  bool
	by     string
	remark string
}

// request 发出取消请求，只有第一次返回 true。
func (c *runCtl) request(by, remark string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.asked {
		return false
	}
	c.asked, c.by, c.remark = true, by, remark
	c.cancel(errRunCancelled)
	return true
}

func (c *runCtl) state() (by, remark string, asked bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.by, c.remark, c.asked
}

// canCancelRun 谁可以取消执行中的工单：超级用户，或在该工单资源组里有执行权限的人（DBA）。
// 提交人本人、审批人没有这个权限（提交人要停手可以请 DBA；审批人的职责在审批）。
func (pm perms) canCancelRun(p audit.Actor, w *model.SqlWorkflow) bool {
	return p.IsSuperuser || (pm.groups[w.GroupID] && pm.executeRG)
}

// canCancelRunNow 详情页是否显示“取消执行”：执行中、有权限、且正由本进程在执行。
func (s *Service) canCancelRunNow(p audit.Actor, pm perms, w *model.SqlWorkflow) bool {
	if w.Status != model.WfExecuting || !pm.canCancelRun(p, w) {
		return false
	}
	_, ok := s.runs.Load(w.ID)
	return ok
}

// CancelRun 取消正在执行的工单。remark（原因）必填。返回时只表示取消请求已发出：
// 执行协程会很快收尾并把工单置为“执行有异常”（通知照常发送），页面刷新即可看到。
func (s *Service) CancelRun(p audit.Actor, id uint, remark string) (*model.SqlWorkflow, error) {
	remark = strings.TrimSpace(remark)
	if remark == "" {
		return nil, bad("请填写取消原因")
	}
	if len([]rune(remark)) > 500 {
		return nil, bad("原因过长（最多 500 字）")
	}
	pm, err := s.loadPerms(p)
	if err != nil {
		return nil, err
	}
	w, err := s.load(id, false)
	if err != nil {
		return nil, err
	}
	if err := s.visible(p, pm, w); err != nil {
		return nil, err
	}
	if !pm.canCancelRun(p, w) {
		return nil, forbid("你无权取消这张工单的执行！")
	}
	if w.Status != model.WfExecuting {
		return nil, conflict(fmt.Sprintf("工单当前状态为「%s」，只有执行中的工单才能取消执行", StatusLabel(w.Status)))
	}
	v, ok := s.runs.Load(id)
	if !ok {
		return nil, conflict("这张工单不是由当前服务进程在执行（可能在另一个节点上，或刚刚结束），无法取消。进程已失联请用「标记为执行异常」")
	}
	au, err := s.repo.AuditByWorkflow(audit.TypeSQLReview, w.ID, false)
	if err != nil {
		return nil, translate(err)
	}
	// 先写日志（谁、为什么取消）再发取消，这样无论执行协程多快收尾，日志里的先后顺序都是对的。
	if au != nil {
		if err := s.repo.InTx(func(tx Repo) error {
			return s.addLog(tx, au.AuditID, opExecute, "取消执行", fmt.Sprintf("人工取消执行。原因：%s", remark), &p)
		}); err != nil {
			return nil, translate(err)
		}
	}
	who := p.Display
	if who == "" {
		who = p.Username
	}
	if !v.(*runCtl).request(who, remark) {
		return nil, conflict("已经发出过取消请求，正在停止，请稍候刷新页面")
	}
	s.sysAudit(p, "取消工单执行", withRemark(wfAuditInfo(w), remark)) // i18n-ignore 审计动作名
	return w, nil
}

// cancelledSet 把被取消的执行结果整理成“执行有异常”：总说明写进 set.Error，被中断的那一行改成“已取消”。
func cancelledSet(set *review.Set, by, remark string) *review.Set {
	if set == nil {
		set = &review.Set{}
	}
	for i := range set.Rows {
		r := &set.Rows[i]
		if r.ErrLevel >= review.LevelError && isCancelErr(r.ErrorMessage) { // 被中断的那一行：写明是谁、为什么取消（通知里的“错误信息”取的就是它）
			r.ErrorMessage = fmt.Sprintf("已被 %s 取消执行（原因：%s）；驱动返回：%s", by, remark, r.ErrorMessage)
		}
	}
	note := fmt.Sprintf("已被 %s 取消执行（原因：%s）。正在执行的语句已中断、其后的语句没有执行；已经成功的语句不会回滚，请到目标库核对", by, remark)
	if set.Error != "" {
		note = fmt.Sprintf("已被 %s 取消执行（原因：%s）。正在执行的语句已中断、其后的语句没有执行；已经成功的语句不会回滚，请到目标库核对。原始错误：%s", by, remark, set.Error)
	}
	set.Error = note
	if len(set.Rows) == 0 {
		set.Rows = []review.Row{{ID: 1, Stage: "Execute failed", ErrLevel: review.LevelError, StageStatus: "异常终止", ErrorMessage: note, BackupDBName: "None"}}
	}
	return set
}

// isCancelErr 判断一行错误是不是 context 被取消引起的（各驱动的说法不一样）。
func isCancelErr(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "context canceled") || strings.Contains(m, "operation was canceled") ||
		strings.Contains(m, "operation was cancelled") || strings.Contains(m, "query execution was interrupted")
}

// hasFailure 结果集里有没有失败的行（取消请求发出时，已经全部成功执行完的工单不能被标成异常）。
func hasFailure(set *review.Set) bool {
	if set == nil {
		return false
	}
	if set.Error != "" || set.Warning != "" {
		return true
	}
	for _, r := range set.Rows {
		if r.ErrLevel >= review.LevelError {
			return true
		}
	}
	return false
}
