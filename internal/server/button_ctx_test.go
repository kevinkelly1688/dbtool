// @author: kevinkelly
// @license: Apache Licence
// @file: button_ctx_test.go
// @time: 2026/10/28

package server

import (
	"html/template"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"dbtool/internal/btnperm"
	"dbtool/internal/model"
	"dbtool/internal/query"
)

type ctxChecker struct {
	allow map[string]bool
	set   btnperm.Setting
	calls int
}

func (f *ctxChecker) HasButton(uid uint, code string) (bool, error) { return f.allow[code], nil }
func (f *ctxChecker) AllowedButtons(uid uint) (map[string]bool, error) {
	f.calls++
	return f.allow, nil
}
func (f *ctxChecker) Setting() (btnperm.Setting, error) { return f.set.Normalize(), nil }

type nopRepo struct{ Repo }

func (nopRepo) HasButton(uint, string) (bool, error)      { return false, nil }
func (nopRepo) GetUserByName(string) (*model.User, error) { return nil, nil }

func ctxFor(s *Server, super bool) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	c.Set(principalKey, query.Principal{ID: 7, Username: "u", IsSuperuser: super})
	return c
}

func TestBtnCtxCanShowAttr(t *testing.T) {
	fc := &ctxChecker{allow: map[string]bool{"sqlworkflow.approve": true}, set: btnperm.Setting{DenyDisplay: btnperm.DenyDisable}}
	s := &Server{repo: nopRepo{}, btnChecker: fc}
	b := s.newBtnCtx(ctxFor(s, false))
	if !b.Can("sqlworkflow.approve") || b.Can("sqlworkflow.schedule") || b.Can("no.such") || b.Can("user.edit") {
		t.Fatal("Can 判断不对")
	}
	b.Can("sqlworkflow.reject")
	if fc.calls != 1 {
		t.Fatalf("每个请求只应算一次，实际 %d", fc.calls)
	}
	if b.DenyMode() != btnperm.DenyDisable || !b.Show("sqlworkflow.schedule") {
		t.Fatal("置灰模式下无权限按钮仍显示")
	}
	if b.Attr("sqlworkflow.approve") != "" {
		t.Fatal("有权限没有额外属性")
	}
	a := string(b.Attr("sqlworkflow.schedule"))
	if !strings.Contains(a, "disabled") || !strings.Contains(a, "定时执行") || !strings.Contains(a, "sqlworkflow.schedule") {
		t.Fatalf("Attr = %s", a)
	}
	// 隐藏模式
	fc.set.DenyDisplay = btnperm.DenyHide
	b2 := s.newBtnCtx(ctxFor(s, false))
	if b2.Show("sqlworkflow.schedule") || !b2.Show("sqlworkflow.approve") {
		t.Fatal("隐藏模式下无权限按钮不显示")
	}
	// 超级用户恒真（含 Locked）
	sb := s.newBtnCtx(ctxFor(s, true))
	if !sb.Can("user.edit") || !sb.Can("sqlworkflow.schedule") || sb.Can("no.such") {
		t.Fatal("超级用户")
	}
	var nilCtx *BtnCtx
	if nilCtx.Can("x.y") || nilCtx.DenyMode() != btnperm.DenyHide {
		t.Fatal("nil 安全")
	}
}

// oneButtonRepo 只授予一个按钮。
type oneButtonRepo struct {
	nopRepo
	code string
}

func (r oneButtonRepo) HasButton(_ uint, code string) (bool, error) { return code == r.code, nil }

func TestBtnCtxWithoutCheckerAsksTheRepo(t *testing.T) {
	s := &Server{repo: oneButtonRepo{code: "qpriv.manage"}}
	b := s.newBtnCtx(ctxFor(s, false))
	if !b.Can("qpriv.manage") || b.Can("inspect.run") || b.Can("user.edit") {
		t.Fatal("没有解析器时问仓库的 HasButton：只有仓库授予的按钮可用，Locked 按钮永远不行")
	}
}

func TestCanTemplateFunc(t *testing.T) {
	fc := &ctxChecker{allow: map[string]bool{"export.download": true}}
	s := &Server{repo: nopRepo{}, btnChecker: fc}
	tp := template.Must(template.New("t").Funcs(tplFuncs).Parse(`{{if can . "export.download"}}A{{end}}{{if .Btn.Can "archive.view"}}B{{end}}{{if .Btn.Show "archive.view"}}C{{end}}`))
	var sb strings.Builder
	d := pageData{Btn: s.newBtnCtx(ctxFor(s, false))}
	if err := tp.Execute(&sb, d); err != nil || sb.String() != "A" {
		t.Fatalf("模板输出 %q err=%v", sb.String(), err)
	}
}
