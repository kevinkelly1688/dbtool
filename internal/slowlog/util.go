// @author: kevinkelly
// @license: Apache Licence
// @file: util.go
// @time: 2026/10/28

package slowlog

import (
	"math"
	"sort"
)

func round(x float64, digits int) float64 {
	p := math.Pow10(digits)
	return math.Round(x*p) / p
}

// percentile95 排序后取下标 min(n-1, int(n*0.95)) 的值；空输入返回 0。
func percentile95(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	v := append([]float64(nil), values...)
	sort.Float64s(v)
	i := int(float64(len(v)) * 0.95)
	if i > len(v)-1 {
		i = len(v) - 1
	}
	return round(v[i], 6)
}
