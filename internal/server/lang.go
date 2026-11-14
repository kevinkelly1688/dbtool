// @author: kevinkelly
// @license: Apache Licence
// @file: lang.go
// @time: 2026/10/28

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"dbtool/internal/i18n"
)

// 界面语言：与主题皮肤同一思路，选择记在 cookie（不需要改元数据库），服务端渲染时据此选模板与译文。
// 判定顺序：页面 URL 的 ?lang=（一次性，同时写 cookie）> cookie > 系统默认语言（DEFAULT_LANG，缺省 zh-CN）。
// 不看 Accept-Language：默认就是中文，要英文由用户自己切换。

const (
	langCookie = "dbtool_lang"
	langKey    = "lang"
)

// langMiddleware 判定本次请求的语言，放进 gin 上下文与 request.Context（业务层用 i18n.FromContext 取）。
func (s *Server) langMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		lang := ""
		p := c.Request.URL.Path
		if c.Request.Method == http.MethodGet && !strings.HasPrefix(p, "/api/") && !strings.HasPrefix(p, "/static/") {
			if l, ok := i18n.Normalize(c.Query("lang")); ok {
				lang = l
				setLangCookie(c, l)
			}
		}
		if lang == "" {
			if ck, err := c.Request.Cookie(langCookie); err == nil {
				if l, ok := i18n.Normalize(ck.Value); ok {
					lang = l
				}
			}
		}
		if lang == "" {
			lang = i18n.Default()
		}
		c.Set(langKey, lang)
		c.Request = c.Request.WithContext(i18n.WithLang(c.Request.Context(), lang))
		if lang == i18n.ZH {
			c.Next()
			return
		}
		w := &errTranslateWriter{ResponseWriter: c.Writer, lang: lang}
		c.Writer = w
		c.Next()
		w.flush()
	}
}

// errTranslateWriter 在非中文请求里兜住 4xx / 5xx 的 JSON 响应，把 {"message": "..."} 翻译成请求语言再输出。
// 各模块自己写错误 JSON（gin.H{"code","message"}），消息是中文原文或 fmt 拼出的文字，统一在这里按目录翻译，
// 模块不用逐个改调用点；其它响应（成功响应、HTML、下载、流式）原样直通，没有缓冲开销。
type errTranslateWriter struct {
	gin.ResponseWriter
	lang    string
	buf     bytes.Buffer
	decided bool
	hold    bool
}

func (w *errTranslateWriter) decide() {
	if w.decided {
		return
	}
	w.decided = true
	w.hold = w.Status() >= 400 && strings.HasPrefix(w.Header().Get("Content-Type"), "application/json")
}

func (w *errTranslateWriter) Write(b []byte) (int, error) {
	w.decide()
	if w.hold {
		return w.buf.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

func (w *errTranslateWriter) WriteString(s string) (int, error) {
	w.decide()
	if w.hold {
		return w.buf.WriteString(s)
	}
	return w.ResponseWriter.WriteString(s)
}

// flush 输出被兜住的错误响应：message 能翻译就换成译文，解析不了的原样输出。
func (w *errTranslateWriter) flush() {
	if !w.hold || w.buf.Len() == 0 {
		return
	}
	body := w.buf.Bytes()
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) == nil {
		var msg string
		if json.Unmarshal(m["message"], &msg) == nil && msg != "" {
			if out := i18n.TP(w.lang, msg); out != msg {
				if raw, err := json.Marshal(out); err == nil {
					m["message"] = raw
					if nb, err := json.Marshal(m); err == nil {
						body = nb
					}
				}
			}
		}
	}
	_, _ = w.ResponseWriter.Write(body)
}

func setLangCookie(c *gin.Context, lang string) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name: langCookie, Value: lang, Path: "/", MaxAge: 365 * 24 * 3600,
		SameSite: http.SameSiteLaxMode, Secure: c.Request.TLS != nil,
	})
}

// langOf 返回本次请求的界面语言（没经过中间件的路径回落系统默认语言）。
func langOf(c *gin.Context) string {
	if v, ok := c.Get(langKey); ok {
		if l, ok := v.(string); ok {
			return l
		}
	}
	return i18n.Default()
}

// tr 按请求语言翻译一条中文原文（带 {名字} 占位时成对传参）：tr(c, "共 {n} 条", "n", 3)。
func tr(c *gin.Context, key string, kv ...any) string { return i18n.T(langOf(c), key, kv...) }

// trMsg 翻译运行时生成的文案（含 fmt.Errorf / Sprintf 拼出来的）：精确匹配原文，匹配不到再按带 %s %d 的格式串匹配。
func trMsg(c *gin.Context, msg string) string { return i18n.TP(langOf(c), msg) }

// trErr 把业务错误渲染成请求语言的文案（见 i18n.Localize）。
func trErr(c *gin.Context, err error) string { return i18n.Localize(langOf(c), err) }

// i18nCatalog 下发前端用的译文目录：GET /i18n/<语言>.json。目录只含界面文案，不含任何业务数据，所以不要求登录
// （登录页也要用）。带 ETag，浏览器每次条件请求，目录没变就 304。
func (s *Server) i18nCatalog(c *gin.Context) {
	code, ok := strings.CutSuffix(c.Param("file"), ".json")
	lang, ok2 := i18n.Normalize(code)
	if !ok || !ok2 || lang != code { // 只认规范写法，避免同一份目录有多个 URL
		c.Status(http.StatusNotFound)
		return
	}
	body, etag := i18n.JSON(lang)
	c.Header("ETag", etag)
	c.Header("Cache-Control", "no-cache")
	if c.GetHeader("If-None-Match") == etag {
		c.Status(http.StatusNotModified)
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}
