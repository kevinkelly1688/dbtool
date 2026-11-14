// @author: kevinkelly
// @license: Apache Licence
// @file: watermark.go
// @time: 2026/10/28

package authx

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"dbtool/internal/settings"
)

// 页面水印的内容与样式。开关 security.watermark_enabled，内容与样式 security.watermark_*（都在系统设置 dbt_setting 里）：
// security.watermark_text / security.watermark_extra / security.watermark_opacity / security.watermark_font_size / security.watermark_angle / security.watermark_gap / security.watermark_color。
// 只有超级用户能改（登录与安全设置页）；旧库里没有这些项时用默认值（内容 “{姓名}（{账号}）{时间}”）。

const (
	settingWMText     = settings.KeySecurityWatermarkText
	settingWMExtra    = settings.KeySecurityWatermarkExtra
	settingWMOpacity  = settings.KeySecurityWatermarkOpacity
	settingWMFontSize = settings.KeySecurityWatermarkFontSize
	settingWMAngle    = settings.KeySecurityWatermarkAngle
	settingWMGap      = settings.KeySecurityWatermarkGap
	settingWMColor    = settings.KeySecurityWatermarkColor
)

// 水印疏密与颜色的取值。
var (
	wmGaps   = []string{"tight", "normal", "loose"}
	wmColors = []string{"auto", "gray", "red", "blue"}
	// WatermarkVars 是内容模板里可用的变量（花括号包起来写，如 {账号}）。
	WatermarkVars = []string{"账号", "姓名", "部门", "IP", "日期", "时间"} // i18n-ignore 水印变量名 / 默认模板（存库的模板里用）
)

const (
	wmTextMax  = 60
	wmExtraMax = 40
)

var wmVarRe = regexp.MustCompile(`\{([^{}]*)\}`)

// WatermarkStyle 是水印的内容模板与样式。
type WatermarkStyle struct {
	Text     string `json:"text"`
	Extra    string `json:"extra"`
	Opacity  int    `json:"opacity"`   // 百分比，3-30
	FontSize int    `json:"font_size"` // px，12-28
	Angle    int    `json:"angle"`     // 度，-60-0
	Gap      string `json:"gap"`       // tight / normal / loose
	Color    string `json:"color"`     // auto / gray / red / blue
}

// DefaultWatermark 是没有任何水印设置时的默认值：姓名与账号都带上。
func DefaultWatermark() WatermarkStyle {
	return WatermarkStyle{Text: "{姓名}（{账号}）{时间}", Opacity: 14, FontSize: 14, Angle: -22, Gap: "normal", Color: "auto"} // i18n-ignore 水印变量名 / 默认模板（存库的模板里用）
}

func inList(v string, list []string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func knownVar(name string) bool { return inList(name, WatermarkVars) }

// checkWMTemplate 校验一段模板：变量必须是已知的，花括号必须成对。
func checkWMTemplate(s string) error {
	for _, m := range wmVarRe.FindAllStringSubmatch(s, -1) {
		if !knownVar(m[1]) {
			names := make([]string, len(WatermarkVars))
			for i, v := range WatermarkVars {
				names[i] = "{" + v + "}"
			}
			return fmt.Errorf("不认识的变量 {%s}，可用：%s", m[1], strings.Join(names, " "))
		}
	}
	if strings.ContainsAny(wmVarRe.ReplaceAllString(s, ""), "{}") {
		return errors.New("花括号不成对：变量要写成 {账号} 这样的形式")
	}
	return nil
}

// Validate 校验内容与样式；失败时的信息可直接展示。
func (w WatermarkStyle) Validate() error {
	text := strings.TrimSpace(w.Text)
	switch {
	case text == "":
		return errors.New("水印内容不能为空")
	case utf8.RuneCountInString(w.Text) > wmTextMax:
		return fmt.Errorf("水印内容最长 %d 个字符", wmTextMax)
	case utf8.RuneCountInString(w.Extra) > wmExtraMax:
		return fmt.Errorf("附加文字最长 %d 个字符", wmExtraMax)
	}
	if err := checkWMTemplate(w.Text); err != nil {
		return err
	}
	if err := checkWMTemplate(w.Extra); err != nil {
		return err
	}
	if !strings.Contains(w.Text, "{账号}") && !strings.Contains(w.Text, "{姓名}") { // i18n-ignore 水印变量名
		return errors.New("水印内容必须包含 {账号} 或 {姓名}，否则截图无法追溯到具体的人")
	}
	if w.Opacity < 3 || w.Opacity > 30 {
		return errors.New("水印透明度须为 3–30 之间的整数")
	}
	if w.FontSize < 12 || w.FontSize > 28 {
		return errors.New("水印字号须为 12–28 之间的整数")
	}
	if w.Angle < -60 || w.Angle > 0 {
		return errors.New("水印倾斜角度须为 -60–0 之间的整数")
	}
	if !inList(w.Gap, wmGaps) || !inList(w.Color, wmColors) {
		return errors.New("水印疏密或颜色取值不合法")
	}
	return nil
}

// wmFromKV 从系统设置的键值读取水印样式：缺项或不合法的项各自回落到默认值（读取不报错，不影响登录）。
func wmFromKV(kv map[string]string) WatermarkStyle {
	w := DefaultWatermark()
	if v, ok := kv[settingWMText]; ok {
		cand := WatermarkStyle{Text: v, Extra: kv[settingWMExtra], Opacity: 14, FontSize: 14, Angle: -22, Gap: "normal", Color: "auto"}
		if cand.Validate() == nil {
			w.Text = v
		}
	}
	if v, ok := kv[settingWMExtra]; ok && utf8.RuneCountInString(v) <= wmExtraMax && checkWMTemplate(v) == nil {
		w.Extra = v
	}
	if n, err := strconv.Atoi(strings.TrimSpace(kv[settingWMOpacity])); err == nil && n >= 3 && n <= 30 {
		w.Opacity = n
	}
	if n, err := strconv.Atoi(strings.TrimSpace(kv[settingWMFontSize])); err == nil && n >= 12 && n <= 28 {
		w.FontSize = n
	}
	if n, err := strconv.Atoi(strings.TrimSpace(kv[settingWMAngle])); err == nil && n >= -60 && n <= 0 {
		w.Angle = n
	}
	if v := kv[settingWMGap]; inList(v, wmGaps) {
		w.Gap = v
	}
	if v := kv[settingWMColor]; inList(v, wmColors) {
		w.Color = v
	}
	return w
}

// kv 转成系统设置的键值。
func (w WatermarkStyle) kv() map[string]string {
	return map[string]string{
		settingWMText: w.Text, settingWMExtra: w.Extra, settingWMOpacity: strconv.Itoa(w.Opacity),
		settingWMFontSize: strconv.Itoa(w.FontSize), settingWMAngle: strconv.Itoa(w.Angle),
		settingWMGap: w.Gap, settingWMColor: w.Color,
	}
}

// ExpandWatermark 把模板里的 {账号} {姓名} … 换成 vars 里的值；vars 里没有的变量原样保留（页面脚本再替换 {日期} {时间}）。
func ExpandWatermark(tpl string, vars map[string]string) string {
	return wmVarRe.ReplaceAllStringFunc(tpl, func(m string) string {
		if v, ok := vars[m[1:len(m)-1]]; ok {
			return v
		}
		return m
	})
}

// WatermarkPatch 是保存时提交的水印项；字段为空（nil）表示不修改，所以旧版页面 / 脚本不带这些字段也能保存。
type WatermarkPatch struct {
	Text     *string `json:"watermark_text,omitempty"`
	Extra    *string `json:"watermark_extra,omitempty"`
	Opacity  *int    `json:"watermark_opacity,omitempty"`
	FontSize *int    `json:"watermark_font_size,omitempty"`
	Angle    *int    `json:"watermark_angle,omitempty"`
	Gap      *string `json:"watermark_gap,omitempty"`
	Color    *string `json:"watermark_color,omitempty"`
}

// apply 在当前样式上套用补丁并校验；返回新的样式。
func (p WatermarkPatch) apply(cur WatermarkStyle) (WatermarkStyle, error) {
	w := cur
	if p.Text != nil {
		w.Text = strings.TrimSpace(*p.Text)
	}
	if p.Extra != nil {
		w.Extra = strings.TrimSpace(*p.Extra)
	}
	if p.Opacity != nil {
		w.Opacity = *p.Opacity
	}
	if p.FontSize != nil {
		w.FontSize = *p.FontSize
	}
	if p.Angle != nil {
		w.Angle = *p.Angle
	}
	if p.Gap != nil {
		w.Gap = strings.TrimSpace(*p.Gap)
	}
	if p.Color != nil {
		w.Color = strings.TrimSpace(*p.Color)
	}
	if err := w.Validate(); err != nil {
		return cur, err
	}
	return w, nil
}

func (p WatermarkPatch) empty() bool {
	return p.Text == nil && p.Extra == nil && p.Opacity == nil && p.FontSize == nil && p.Angle == nil && p.Gap == nil && p.Color == nil
}

// patchOf 把样式转成补丁（视图里回显用）。
func patchOf(w WatermarkStyle) WatermarkPatch {
	return WatermarkPatch{Text: &w.Text, Extra: &w.Extra, Opacity: &w.Opacity, FontSize: &w.FontSize, Angle: &w.Angle, Gap: &w.Gap, Color: &w.Color}
}

// UserWatermark 是 /api/v1/auth/ui 给页面的水印配置：模板已按当前用户展开了 {账号} {姓名} {部门} {IP}，
// {日期} {时间} 留给页面脚本每分钟替换。
type UserWatermark struct {
	Enabled  bool              `json:"watermark"`
	Text     string            `json:"watermark_text"`
	Extra    string            `json:"watermark_extra"`
	Opacity  int               `json:"watermark_opacity"`
	FontSize int               `json:"watermark_font_size"`
	Angle    int               `json:"watermark_angle"`
	Gap      string            `json:"watermark_gap"`
	Color    string            `json:"watermark_color"`
	Vars     map[string]string `json:"watermark_vars"` // 当前用户的 账号 / 姓名 / 部门 / IP，设置页预览用
}

// UserWatermarkFor 按设置与当前用户生成页面水印配置。
func UserWatermarkFor(set Settings, vars map[string]string) UserWatermark {
	w := set.WM
	return UserWatermark{Enabled: set.Watermark, Text: ExpandWatermark(w.Text, vars), Extra: ExpandWatermark(w.Extra, vars),
		Opacity: w.Opacity, FontSize: w.FontSize, Angle: w.Angle, Gap: w.Gap, Color: w.Color, Vars: vars}
}
