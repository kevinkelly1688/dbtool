// @author: kevinkelly
// @license: Apache Licence
// @file: l10n.go
// @time: 2026/10/28

package maskadmin

import (
	"fmt"

	"github.com/gin-gonic/gin"

	"dbtool/internal/i18n"
)

// 界面多语言：服务层只产出中文原文，接口响应里面向用户的说明在 HTTP 边缘按请求语言翻译（中文请求原样返回）。
// 引擎词（表 / 集合 / 字段 / 列 / 张表 / 行 …）与路径写法（库.表.字段）不在这里翻译：页面按语序嵌进句子，由前端按词翻译。

func langOf(c *gin.Context) string { return i18n.FromContext(c.Request.Context()) }

func tp(lang, s string) string { return i18n.TP(lang, s) }

func localizeEngine(lang string, e *EngineInfo) {
	if e == nil {
		return
	}
	e.Source = tp(lang, e.Source)
	e.How = tp(lang, e.How)
	e.Note = tp(lang, e.Note)
}

func localizeInstances(lang string, list []InstanceItem) {
	for i := range list {
		list[i].Why = tp(lang, list[i].Why)
		localizeEngine(lang, list[i].Engine)
	}
}

func localizeCatalog(lang string, c *Catalog) { c.Note = tp(lang, c.Note) }

func localizeConfig(lang string, v *ConfigView) {
	for i := range v.Items {
		v.Items[i].RuleText = tp(lang, v.Items[i].RuleText)
		v.Items[i].LegacyDesc = tp(lang, v.Items[i].LegacyDesc)
	}
}

// wrapped 是带原因的错误：文字与 fmt.Errorf("…%w") 一致，保留 Unwrap；
// 格式串用 %s 写，是因为 %w 不在按目录翻译时匹配的动词里。
type wrapped struct {
	msg string
	err error
}

func (e *wrapped) Error() string { return e.msg }
func (e *wrapped) Unwrap() error { return e.err }

// wrapf(err, "读取库 %s 的字段清单失败: %s", db)：最后一个 %s 对应 err。
func wrapf(err error, format string, args ...any) error {
	return &wrapped{msg: fmt.Sprintf(format, append(args, err)...), err: err}
}
