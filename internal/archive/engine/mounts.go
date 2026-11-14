// @author: kevinkelly
// @license: Apache Licence
// @file: mounts.go
// @time: 2026/10/28

package engine

import (
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
)

// mountEntry 是挂载表的一行（挂载点与文件系统类型）。
type mountEntry struct {
	Point  string // 用 "/" 分隔
	FSType string
}

// readMounts 读系统挂载表；读不到（非 Linux、容器里没有 /proc）返回 ok=false，调用方跳过挂载判断。测试里替换。
var readMounts = func() ([]mountEntry, bool) {
	if runtime.GOOS != "linux" {
		return nil, false
	}
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, false
	}
	return parseMountInfo(string(b)), true
}

// parseMountInfo 解析 /proc/self/mountinfo：第 5 列是挂载点（空格等以 \040 这样的八进制转义），" - " 之后第一个词是文件系统类型。
func parseMountInfo(s string) []mountEntry {
	var out []mountEntry
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		i := strings.Index(line, " - ")
		if i < 0 {
			continue
		}
		pre, post := strings.Fields(line[:i]), strings.Fields(line[i+3:])
		if len(pre) < 5 || len(post) < 1 {
			continue
		}
		out = append(out, mountEntry{Point: unescapeMount(pre[4]), FSType: post[0]})
	}
	return out
}

func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// mountOf 返回包含 p 的最长挂载点（p 用 "/" 分隔的绝对路径）。
func mountOf(mounts []mountEntry, p string) (mountEntry, bool) {
	var best mountEntry
	found := false
	for _, m := range mounts {
		pt := path.Clean(m.Point)
		if p == pt || strings.HasPrefix(p, strings.TrimSuffix(pt, "/")+"/") {
			if !found || len(pt) > len(path.Clean(best.Point)) {
				best, found = m, true
			}
		}
	}
	return best, found
}

// isRootMount 判断挂载点是文件系统根（Linux "/"，Windows "C:/"）。
func isRootMount(point string) bool {
	pt := path.Clean(point)
	return pt == "/" || (len(pt) == 2 && pt[1] == ':') || (len(pt) == 3 && pt[1] == ':' && pt[2] == '/')
}
