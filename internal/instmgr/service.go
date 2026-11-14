// @author: kevinkelly
// @license: Apache Licence
// @file: service.go
// @time: 2026/10/28

package instmgr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"dbtool/internal/audit"
	"dbtool/internal/model"
)

// Service 是实例管理的业务逻辑：依赖 Repo（元数据库）与 Dialer（目标库）。
type Service struct {
	repo   Repo
	dialer Dialer
	now    func() time.Time
	// opTimeout 是对目标库一次操作（含连接）的总时限。
	opTimeout time.Duration
	// ext 是旁路表（停用状态 / 环境 / 账号登记扩展 / 参数推荐值）的访问；repo 没有实现 ExtRepo 时为 nil，相关功能整体关闭。
	ext ExtRepo
	// hc 是实例健康探测的短缓存与并发闸门。
	hc *healthCache
}

func NewService(repo Repo, d Dialer) *Service {
	s := &Service{repo: repo, dialer: d, now: time.Now, opTimeout: 60 * time.Second, hc: newHealthCache()}
	if e, ok := repo.(ExtRepo); ok {
		s.ext = e
	}
	return s
}

// audit 写操作审计（dbt_audit_log）。操作已经执行，写入失败只记日志，不回滚。extra 里不能有密码。
func (s *Service) audit(c Caller, action, extra string) {
	e := &model.AuditEntry{UserID: c.ID, UserName: clipRunes(c.Username, 30), UserDisplay: clipRunes(c.Display, 50),
		Action: clipRunes(action, 250), ExtraInfo: extra}
	if err := s.repo.InsertAudit(e); err != nil {
		slog.Error("写操作审计失败", "action", action, "user", c.Username, "err", err)
	}
}

// auditRefused 为被服务端拒绝的高危操作（受保护账号、参数黑名单、被引用的实例、受保护的会话等）留痕：
// 动作名 = 原动作 +“（拒绝）”，额外信息写明实例与原因。参数错误、没有确认、账号不存在这类普通校验失败不记。
func (s *Service) auditRefused(c Caller, action string, inst *model.Instance, detail string) {
	where := ""
	if inst != nil {
		where = "实例：" + instLabel(inst) + "；" // i18n-ignore 审计留痕，存库的文本不翻译
	}
	s.audit(c, action+audit.DeniedSuffix, where+"原因："+detail) // i18n-ignore 审计留痕，存库的文本不翻译
}

// refusedBy 判断 err 是不是服务端的“禁止”类拒绝（403）。
func refusedBy(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == 403
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// visibleInstance 取实例并校验可见范围：非 AllInstances 的用户只能访问资源组关联的实例。
func (s *Service) visibleInstance(c Caller, id uint) (*model.Instance, error) {
	if id == 0 {
		return nil, bad("缺少实例")
	}
	inst, err := s.repo.GetInstance(id)
	if err != nil {
		return nil, err
	}
	if !c.AllInstances {
		ids, err := s.repo.UserInstanceIDs(c.ID)
		if err != nil {
			return nil, err
		}
		if inst == nil || !ids[id] {
			return nil, forbidden("你所在组未关联该实例")
		}
	}
	if inst == nil {
		return nil, notFound("实例不存在")
	}
	return inst, nil
}

// session 是对一个目标实例的一次操作：连接在 close 时释放，ctx 带总时限。
type session struct {
	ctx    context.Context
	cancel context.CancelFunc
	inst   *model.Instance
	t      Target
	ver    *Version
}

func (se *session) close() {
	_ = se.t.Close()
	se.cancel()
}

func (se *session) query(sql string, args ...any) (*Result, error) {
	return se.t.Query(se.ctx, sql, plainArgs(args)...)
}

func (se *session) exec(sql string, args ...any) (int64, error) {
	return se.t.Exec(se.ctx, sql, plainArgs(args)...)
}

// version 取并缓存目标库版本。
func (se *session) version() Version {
	if se.ver != nil {
		return *se.ver
	}
	v := Version{}
	if r, err := se.query("SELECT VERSION() AS v"); err == nil && len(r.Rows) > 0 {
		v = parseVersion(r.Records()[0].Str("v"))
	}
	se.ver = &v
	return v
}

// connInfo 解密实例凭据。
func (s *Service) connInfo(inst *model.Instance) (ConnInfo, error) {
	ci, err := s.repo.ConnInfo(inst)
	if err != nil {
		return ci, err
	}
	ci.DBType = inst.DBType
	return ci, nil
}

// open 校验可见范围与引擎类型后连接目标实例。调用者必须 defer se.close()。
func (s *Service) open(ctx context.Context, c Caller, id uint, supports func(string) bool, feature string) (*session, error) {
	inst, err := s.visibleInstance(c, id)
	if err != nil {
		return nil, err
	}
	if !supports(inst.DBType) {
		return nil, unsupported(unsupportedMsg(inst.DBType, feature))
	}
	if err := s.rejectDisabled(inst.ID); err != nil { // 已停用的实例平台不再连接
		return nil, err
	}
	return s.dial(ctx, inst)
}

func (s *Service) dial(ctx context.Context, inst *model.Instance) (*session, error) {
	ci, err := s.connInfo(inst)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, s.opTimeout)
	t, err := s.dialer.Dial(cctx, ci)
	if err != nil {
		cancel()
		return nil, newErr(502, "connect_failed", fmt.Sprintf("无法连接实例：%s", err.Error()))
	}
	return &session{ctx: cctx, cancel: cancel, inst: inst, t: t}, nil
}

// wrapTarget 把目标库返回的错误包装成对用户友好的业务错误。
func wrapTarget(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*Error); ok {
		return err
	}
	return targetErr(err)
}

// isNoSuchGrant 判断是不是 MySQL 1141（没有这条授权）。
func isNoSuchGrant(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "1141") || strings.Contains(strings.ToLower(err.Error()), "no such grant"))
}

// isDupEntry 判断是不是 MySQL 1062（唯一索引冲突）。
func isDupEntry(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "1062") || strings.Contains(err.Error(), "Duplicate entry"))
}

// instLabel 是写进审计的实例标识。
func instLabel(inst *model.Instance) string {
	return inst.InstanceName + "(id=" + utoa(inst.ID) + ")"
}

func utoa(n uint) string { return strconv.FormatUint(uint64(n), 10) }
