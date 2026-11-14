// @author: kevinkelly
// @license: Apache Licence
// @file: lang_test.go
// @time: 2026/10/28

package server

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dbtool/internal/i18n"
)

func withLangCookie(req *http.Request, lang string) *http.Request {
	req.AddCookie(&http.Cookie{Name: langCookie, Value: lang})
	return req
}

// 默认中文；cookie 选了英文就渲染英文，<html lang> 与切换器跟着变；没有英文译文的文案回落中文。
func TestLangPageRender(t *testing.T) {
	e := newEnv(t)
	zh := e.do(httptest.NewRequest("GET", "/login", nil)).Body.String()
	if !strings.Contains(zh, `<html lang="zh-CN"`) || !strings.Contains(zh, "欢迎登录") || strings.Contains(zh, "Welcome back") {
		t.Errorf("默认应是中文登录页:\n%.600s", zh)
	}
	if !strings.Contains(zh, `data-lang-set="zh-CN" lang="zh-CN" title="中文" aria-label="中文" aria-pressed="true"`) {
		t.Error("中文页的切换器应标出当前语言为中文")
	}

	en := e.do(withLangCookie(httptest.NewRequest("GET", "/login", nil), "en")).Body.String()
	for _, want := range []string{`<html lang="en"`, "Welcome back", "Username", `<em>seen</em> before go-live`,
		`data-lang-set="en" lang="en" title="English" aria-label="English" aria-pressed="true"`} {
		if !strings.Contains(en, want) {
			t.Errorf("英文登录页缺 %q", want)
		}
	}
	if strings.Contains(en, "欢迎登录") || strings.Contains(en, "用户名") {
		t.Error("英文登录页不应再出现已有译文的中文")
	}
	// 切换器上的语言名永远是各语言自己的写法，不翻译
	if !strings.Contains(en, `title="中文"`) {
		t.Error("英文页的切换器里中文仍应写作“中文”")
	}
}

// ?lang= 一次性切换并写 cookie；非法值不生效；API 路径不受 ?lang= 影响。
func TestLangQueryParamAndCookie(t *testing.T) {
	e := newEnv(t)
	w := e.do(httptest.NewRequest("GET", "/login?lang=en", nil))
	ck := cookieNamed(w, langCookie)
	if ck == nil || ck.Value != "en" || ck.MaxAge < 86400 {
		t.Fatalf("?lang=en 应写 cookie: %+v", ck)
	}
	if !strings.Contains(w.Body.String(), `<html lang="en"`) {
		t.Error("?lang=en 当次请求就应渲染英文")
	}
	// ?lang= 压过已有 cookie
	req := withLangCookie(httptest.NewRequest("GET", "/login?lang=zh-CN", nil), "en")
	if body := e.do(req).Body.String(); !strings.Contains(body, `<html lang="zh-CN"`) {
		t.Error("?lang=zh-CN 应压过 cookie")
	}
	// 非法值：不写 cookie，仍用 cookie / 默认
	w = e.do(httptest.NewRequest("GET", "/login?lang=%22%3E%3Cscript%3E", nil))
	if cookieNamed(w, langCookie) != nil || !strings.Contains(w.Body.String(), `<html lang="zh-CN"`) {
		t.Error("非法 lang 应被忽略")
	}
	// 非法 cookie 值同样回落默认
	if body := e.do(withLangCookie(httptest.NewRequest("GET", "/login", nil), "fr")).Body.String(); !strings.Contains(body, `<html lang="zh-CN"`) {
		t.Error("未知语言 cookie 应回落默认语言")
	}
	// API 请求不处理 ?lang=（业务参数可能同名），也不写 cookie
	w = e.do(httptest.NewRequest("GET", "/api/v1/query/instances?lang=en", nil))
	if cookieNamed(w, langCookie) != nil {
		t.Error("API 路径不应被 ?lang= 写 cookie")
	}
}

// 不看 Accept-Language：默认就是中文。系统默认语言改成英文后，没有 cookie 的访客看英文。
func TestLangDefaultAndAcceptLanguage(t *testing.T) {
	e := newEnv(t)
	req := httptest.NewRequest("GET", "/login", nil)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	if body := e.do(req).Body.String(); !strings.Contains(body, `<html lang="zh-CN"`) {
		t.Error("Accept-Language 不应改变默认语言")
	}
	i18n.SetDefault("en")
	defer i18n.SetDefault("zh-CN")
	if body := e.do(httptest.NewRequest("GET", "/login", nil)).Body.String(); !strings.Contains(body, `<html lang="en"`) {
		t.Error("系统默认语言为英文时，无 cookie 的访客应看到英文")
	}
	if body := e.do(withLangCookie(httptest.NewRequest("GET", "/login", nil), "zh-CN")).Body.String(); !strings.Contains(body, `<html lang="zh-CN"`) {
		t.Error("用户自己选的中文应压过系统默认语言")
	}
}

// API 错误按请求语言翻译，code 不变。
func TestLangAPIError(t *testing.T) {
	e := newEnv(t)
	get := func(lang string) (string, string) {
		req := httptest.NewRequest("GET", "/api/v1/query/instances", nil)
		if lang != "" {
			withLangCookie(req, lang)
		}
		w := e.do(req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("未登录应 401: %d", w.Code)
		}
		return w.Body.String(), w.Header().Get("Content-Type")
	}
	zh, _ := get("")
	en, _ := get("en")
	if !strings.Contains(zh, "未登录或登录已过期") || !strings.Contains(zh, `"code":"unauthorized"`) {
		t.Errorf("中文错误: %s", zh)
	}
	if !strings.Contains(en, `"code":"unauthorized"`) || strings.Contains(en, "未登录") {
		t.Errorf("英文错误应翻译且 code 不变: %s", en)
	}
}

// 前端译文目录：公开、带 ETag、条件请求 304；中文是空对象；非法名 404。
func TestI18nCatalogEndpoint(t *testing.T) {
	e := newEnv(t)
	w := e.do(httptest.NewRequest("GET", "/i18n/en.json", nil))
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("en.json: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	etag := w.Header().Get("ETag")
	if etag == "" || !strings.Contains(w.Body.String(), `"首页":"Home"`) {
		t.Errorf("目录内容 / ETag 不对: %q %.200s", etag, w.Body.String())
	}
	req := httptest.NewRequest("GET", "/i18n/en.json", nil)
	req.Header.Set("If-None-Match", etag)
	if w2 := e.do(req); w2.Code != http.StatusNotModified {
		t.Errorf("条件请求应 304: %d", w2.Code)
	}
	// 带 Accept-Encoding: gzip 时压缩（真实目录约 1 MB），解压后与原文一致，ETag 不变
	gzReq := httptest.NewRequest("GET", "/i18n/en.json", nil)
	gzReq.Header.Set("Accept-Encoding", "gzip")
	wz := e.do(gzReq)
	if zr, err := gzip.NewReader(wz.Body); wz.Header().Get("Content-Encoding") != "gzip" || err != nil || wz.Header().Get("ETag") != etag {
		t.Errorf("en.json 应 gzip 并保留 ETag: enc=%q err=%v", wz.Header().Get("Content-Encoding"), err)
	} else if got, _ := io.ReadAll(zr); string(got) != w.Body.String() || wz.Body.Len()*3 > w.Body.Len() {
		t.Errorf("gzip 解压后应与原文相同且明显更小")
	}
	if w := e.do(httptest.NewRequest("GET", "/i18n/zh-CN.json", nil)); w.Code != 200 || strings.TrimSpace(w.Body.String()) != "{}" {
		t.Errorf("中文目录应为空对象: %d %s", w.Code, w.Body.String())
	}
	for _, p := range []string{"/i18n/fr.json", "/i18n/EN.json", "/i18n/en", "/i18n/..%2Fx.json"} {
		if w := e.do(httptest.NewRequest("GET", p, nil)); w.Code != http.StatusNotFound {
			t.Errorf("%s 应 404: %d", p, w.Code)
		}
	}
}
