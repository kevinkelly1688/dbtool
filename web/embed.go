// @author: kevinkelly
// @license: Apache Licence
// @file: embed.go
// @time: 2026/10/28

// Package web 内嵌页面模板与静态资源，随二进制一起发布。
package web

import "embed"

// i18n 是界面译文目录（扁平 JSON：中文原文 → 译文；web/i18n/<语言>/<模块>.json，一个模块一个文件），见 internal/i18n。
//
//go:embed templates static i18n
var FS embed.FS
