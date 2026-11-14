// @author: kevinkelly
// @license: Apache Licence
// @file: http.go
// @time: 2026/10/28

package retention

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"dbtool/internal/i18n"
	"dbtool/internal/localize"
	"dbtool/internal/model"
	"dbtool/internal/server"
	"dbtool/internal/sqlflow"
)

func init() {
	server.AddRoutes(func(s *server.Server, rt server.Routes) {
		db, mirage, ok := s.DB() // 仓库不是 store.Store（测试 / 演示）时没有元数据库，不登记
		if !ok {
			return
		}
		svc := NewService(NewGormRepo(db, mirage), s.Config().ExportBaseDir)
		// 导出下载 / 详情页把“文件不存在”换成“已于 xx 按保留策略清理”（sqlflow 不能反向依赖本包，所以由这里注入查询）
		sqlflow.SetExportPurgedLookup(func(id uint) (time.Time, bool) { return ExportPurgedAt(db, id) })
		register(s, rt, svc)
		server.AddWorker(svc.Worker)
	})
}

type handlers struct {
	s   *server.Server
	svc *Service
}

// register 登记接口（测试里直接传入假依赖的 Service）。两个接口都只有超级用户可用，不占用按钮权限码。
//
//	GET /api/v1/retention/preview   dry-run：按当前配置统计各目标现在有多少数据会被清理，不删除任何东西
//	GET /api/v1/retention/runs      最近若干轮清理的汇总（需要已执行 205_retention.sql）
func register(s *server.Server, rt server.Routes, svc *Service) {
	h := &handlers{s: s, svc: svc}
	g := rt.API.Group("/retention", s.SuperuserAPI())
	g.GET("/preview", h.preview)
	g.GET("/runs", h.runs)
}

func (h *handlers) preview(c *gin.Context) {
	a := server.ActorOf(c)
	p := h.svc.Preview(c.Request.Context())
	e := &model.AuditEntry{UserID: a.ID, UserName: a.Username, UserDisplay: a.Display, Action: "数据清理预览",
		ExtraInfo: "查看数据清理的 dry-run 统计（不删除任何数据）"}
	if err := h.svc.repo.InsertAudit(e); err != nil {
		slog.Error("写数据清理预览审计失败", "err", err)
	}
	// 清理目标的说明、提示与失败原因是服务端拼出的中文，按请求语言翻译（中文原样输出）。
	server.APIOK(c, localize.Value(i18n.FromContext(c.Request.Context()), p))
}

func (h *handlers) runs(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	rows, err := h.svc.repo.ListRounds(limit)
	if err != nil {
		slog.Error("读取数据清理汇总失败", "err", err)
		server.APIFail(c, http.StatusInternalServerError, "internal", "服务内部错误，请稍后重试或联系管理员")
		return
	}
	if rows == nil {
		rows = []RoundRecord{}
	}
	server.APIOK(c, localize.Value(i18n.FromContext(c.Request.Context()), rows))
}
