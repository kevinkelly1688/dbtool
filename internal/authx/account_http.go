// @author: kevinkelly
// @license: Apache Licence
// @file: account_http.go
// @time: 2026/10/28

package authx

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"dbtool/internal/server"
)

// 个人账号页面与接口：自助修改密码（/account/password）、外部账号绑定（/account/bindings）。

// passwordPageData 是修改密码页模板里的 .Data。
type passwordPageData struct {
	Local      bool // 有本地密码：可以自助修改
	Force      bool // 管理员要求先改密（只能改密或退出）
	MinLen     int  // 当前密码策略
	Complexity bool
	Error      string
	Info       string
	Method     string // 外部认证账号的说明
}

func (h *handlers) passwordData(uid uint, lang string) passwordPageData {
	d := passwordPageData{MinLen: h.svc.Settings().PasswordMinLen, Complexity: h.svc.Settings().PasswordComplexity}
	if info, err := h.svc.Account(uid); err == nil {
		localizeAccount(lang, info)
		d.Local, d.Method = info.LocalPassword, info.AuthMethodText
		d.Force = info.MustChange
	}
	return d
}

func (h *handlers) renderPassword(c *gin.Context, status int, d passwordPageData) {
	noStore(c)
	h.s.Render(c, status, "auth/password.html", server.PageData{Title: "修改密码", CSRF: server.CSRFOf(c), Data: d})
}

func (h *handlers) passwordPage(c *gin.Context) {
	d := h.passwordData(server.PrincipalOf(c).ID, reqLang(c))
	h.renderPassword(c, http.StatusOK, d)
}

// passwordForm 是不依赖脚本的表单提交（页面脚本可用时走 /api/v1/account/password，两者逻辑相同）。
func (h *handlers) passwordForm(c *gin.Context) {
	p := server.PrincipalOf(c)
	d := h.passwordData(p.ID, reqLang(c))
	if !constEq(c.PostForm("csrf"), server.CSRFOf(c)) {
		d.Error = "页面已过期，请刷新后重新提交"
		h.renderPassword(c, http.StatusForbidden, d)
		return
	}
	err := h.svc.ChangePassword(p.ID, c.PostForm("current"), c.PostForm("new"), c.PostForm("confirm"), c.ClientIP())
	if err != nil {
		status, msg := passwordFailure(err)
		d.Error = msg
		h.renderPassword(c, status, d)
		return
	}
	d.Force, d.Info = false, "密码已修改。"
	h.renderPassword(c, http.StatusOK, d)
}

// passwordFailure 把改密失败转成 HTTP 状态与可展示的信息；未知错误只给通用提示，细节进日志。
func passwordFailure(err error) (int, string) {
	var fe *FlowError
	if errors.As(err, &fe) {
		switch fe.Code {
		case ErrPwdTooMany.Code:
			return http.StatusTooManyRequests, fe.Msg
		case ErrPwdCurrentBad.Code:
			return http.StatusUnauthorized, fe.Msg
		}
		return http.StatusBadRequest, fe.Msg
	}
	slog.Error("修改密码失败", "err", err)
	return http.StatusInternalServerError, "服务异常，请稍后再试"
}

func (h *handlers) apiChangePassword(c *gin.Context) {
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
		Confirm string `json:"confirm"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "参数错误")
		return
	}
	if server.CSRFOf(c) == "" { // 只接受浏览器会话（带 CSRF）：JWT 不能用来改密码
		server.APIFail(c, http.StatusForbidden, "forbidden", "请在已登录的页面中修改密码")
		return
	}
	if err := h.svc.ChangePassword(server.PrincipalOf(c).ID, in.Current, in.New, in.Confirm, c.ClientIP()); err != nil {
		status, msg := passwordFailure(err)
		code := "bad_request"
		var fe *FlowError
		if errors.As(err, &fe) {
			code = fe.Code
		}
		server.APIFail(c, status, code, msg)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}

// apiAccountInfo 返回当前用户的认证方式摘要（个人菜单据此决定是否显示“修改密码”等）。
func (h *handlers) apiAccountInfo(c *gin.Context) {
	info, err := h.svc.Account(server.PrincipalOf(c).ID)
	if err != nil {
		fail(c, err)
		return
	}
	localizeAccount(reqLang(c), info)
	server.APIOK(c, info)
}

// ---- 账号绑定 ----

// bindingsErrors 是账号绑定页上 ?err= 的固定文案。
var bindingsErrors = map[string]string{
	"bind_off":        ErrBindOff.Msg,
	"bind_taken":      "该外部账号已绑定到其他平台账号",
	"bind_dup":        "你已经绑定过这个登录方式的账号，请先解绑再绑定新的",
	"superuser_local": "超级用户只能使用本地账号密码登录，不能绑定外部账号",
	"state":           ErrStateInvalid.Msg,
	"disabled":        ErrDisabledUser.Msg,
	"failed":          "绑定失败，请重试或联系管理员",
}

type bindingsPageData struct {
	Error string
	Info  string
}

func (h *handlers) bindingsPage(c *gin.Context) {
	d := bindingsPageData{}
	if code := c.Query("err"); code != "" {
		d.Error = bindingsErrors[code] // 未知代码为空，不显示
	}
	if c.Query("bound") == "1" {
		d.Info = "已绑定。"
	}
	noStore(c)
	h.s.Render(c, http.StatusOK, "auth/bindings.html", server.PageData{Title: "账号绑定", CSRF: server.CSRFOf(c), Data: d})
}

func (h *handlers) apiBindings(c *gin.Context) {
	pg, err := h.svc.Bindings(server.PrincipalOf(c).ID)
	if err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, pg)
}

func (h *handlers) apiUnbind(c *gin.Context) {
	n, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || n == 0 {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "参数非法")
		return
	}
	if err := h.svc.Unbind(actorOf(c), uint(n)); err != nil {
		var fe *FlowError
		if errors.As(err, &fe) {
			status := http.StatusBadRequest
			switch fe.Code {
			case ErrBindOff.Code:
				status = http.StatusForbidden
			case ErrBindNotFound.Code:
				status = http.StatusNotFound
			}
			server.APIFail(c, status, fe.Code, fe.Msg)
			return
		}
		fail(c, err)
		return
	}
	server.APIOK(c, gin.H{"ok": true})
}
