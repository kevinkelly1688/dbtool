// @author: kevinkelly
// @license: Apache Licence
// @file: advice.go
// @time: 2026/10/28

package inspect

import "strings"

// 检查项的处理建议。文案放在检查项定义里（mysqlSteps 的 Advice、msChecks 的 Advice），这里只负责把它挂到报告的检查项上：
// 报告详情的“建议”列在关注 / 告警 / 未采集的行上显示它。

// adviceFailed 是采集失败（无权限、超时、未执行）的通用建议。
const adviceFailed = "采集失败：请确认巡检账号权限足够、实例网络可达后重新巡检。"

// mysqlStepAdvice 是 MySQL 系 19 个采集步骤的建议，键是步骤名（mysqlSteps 的 Key）。
func mysqlStepAdvice() map[string]string {
	m := make(map[string]string, len(mysqlSteps))
	for _, st := range mysqlSteps {
		m[st.Key] = st.Advice
	}
	return m
}

// msCheckAdvice 是 SQL Server 27 个检查项的建议，键是 item_key（如 memory.ple）。
func msCheckAdvice() map[string]string {
	m := make(map[string]string, len(msChecks))
	for _, c := range msChecks {
		m[c.Key] = c.Advice
	}
	return m
}

// mysqlSectionStep 把报告分区代码对应到采集步骤：缓冲池的检查项在“内存 / 缓冲池”分区里，其余一一对应。
func mysqlSectionStep(code, itemName string) string {
	if code == "memory" && strings.HasPrefix(itemName, "缓冲池") { // i18n-ignore 与检查项名称（本包生成的中文）比较
		return "buffer_pool"
	}
	return code
}

// applyMySQLAdvice 给 MySQL 报告的每个检查项挂上所属步骤的建议（明细行不挂）；采集失败的项用通用建议。
func applyMySQLAdvice(secs []Section) []Section {
	adv := mysqlStepAdvice()
	for i := range secs {
		for j := range secs[i].Items {
			it := &secs[i].Items[j]
			if it.Detail {
				continue
			}
			if it.Level == LevelFailed {
				it.Advice = adviceFailed
				continue
			}
			it.Advice = adv[mysqlSectionStep(secs[i].Code, it.Name)]
		}
	}
	return secs
}

// msAdviceFor 返回 SQL Server 某个检查项的建议；采集失败用通用建议。
func msAdviceFor(adv map[string]string, key string, level int) string {
	if level == LevelFailed {
		return adviceFailed
	}
	return adv[key]
}
