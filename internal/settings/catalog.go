// @author: kevinkelly
// @license: Apache Licence
// @file: catalog.go
// @time: 2026/10/28

package settings

import "fmt"

var byKey map[string]*Def

func init() {
	byKey = make(map[string]*Def, len(defs))
	for _, d := range defs {
		if d.Key == "" || d.Group == "" || d.Label == "" || d.Type == "" {
			panic(fmt.Sprintf("settings: 登记不完整：%+v", d)) // i18n-ignore 启动期登记错误，给开发者看
		}
		if _, dup := byKey[d.Key]; dup {
			panic("settings: 重复的设置键 " + d.Key) // i18n-ignore 启动期登记错误，给开发者看
		}
		if d.Type == TypeEnum && len(d.Options) == 0 {
			panic("settings: 枚举没有取值 " + d.Key) // i18n-ignore 启动期登记错误，给开发者看
		}
		if d.Default != "" {
			// 默认值必须通过自己的校验，登记错误在启动 / 测试时就暴露
			if v, err := d.Normalize(d.Default); err != nil || v != d.Default {
				panic(fmt.Sprintf("settings: %s 的默认值 %q 不合法：%v（规整后 %q）", d.Key, d.Default, err, v)) // i18n-ignore 启动期登记错误，给开发者看
			}
		}
		byKey[d.Key] = d
	}
}

// Find 返回登记项；没有登记返回 nil。
func Find(key string) *Def { return byKey[key] }

// Defs 返回全部登记项（登记顺序，含隐藏项）。返回的是副本，修改不影响登记。
func Defs() []Def {
	out := make([]Def, len(defs))
	for i, d := range defs {
		out[i] = *d
	}
	return out
}

// GroupLabel 返回分组的中文名。
func GroupLabel(key string) string {
	for _, g := range Groups {
		if g.Key == key {
			return g.Label
		}
	}
	return key
}
