// @author: kevinkelly
// @license: Apache Licence
// @file: hostinfo.go
// @time: 2026/10/28

package archive

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"time"

	ieng "dbtool/internal/engine"
)

// ---- 从库发现（Opener.SlaveByHost） ----

// lookupHost 把主机名解析成 IP（测试里替换）。
var lookupHost = func(ctx context.Context, host string) ([]string, error) {
	return net.DefaultResolver.LookupHost(ctx, host)
}

// slaveByHost 返回平台里登记的、地址对应 host 的 MySQL / MariaDB 从库实例连接。
// 源库 processlist 里看到的是从库的连接来源 IP，所以登记的是主机名时要解析后再比；解析失败就当不匹配（探针会报“未检测”）。
func (s *Service) slaveByHost(ctx context.Context, host string) ([]ieng.Conn, error) {
	list, err := s.instances()
	if err != nil {
		return nil, err
	}
	host = strings.ToLower(strings.TrimSpace(host))
	var out []ieng.Conn
	for i := range list {
		in := &list[i]
		if in.Type != "slave" || !in.IsEnable || !supportedDBTypes[strings.ToLower(in.DBType)] {
			continue
		}
		if sameHost(ctx, in.Host, host) {
			out = append(out, s.meta.EngineConn(in))
		}
	}
	return out, nil
}

func sameHost(ctx context.Context, registered, seen string) bool {
	registered = strings.ToLower(strings.TrimSpace(registered))
	if registered == "" || seen == "" {
		return false
	}
	if registered == seen {
		return true
	}
	if net.ParseIP(registered) != nil {
		return false
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	addrs, err := lookupHost(rctx, registered)
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if strings.EqualFold(a, seen) {
			return true
		}
	}
	return false
}

// ---- 磁盘剩余（Opener.Disk）：取巡检报告里的磁盘指标 ----

// inspectDiskMaxAge 是巡检指标的有效期：太旧的采集值不能代表现在的磁盘。
const inspectDiskMaxAge = 48 * time.Hour

// diskFreeFromResult 从 MySQL 巡检报告的 result JSON 里取磁盘剩余百分比。
// 认 capacity.disk_free_pct，或 capacity 里的 disk_free_bytes / disk_total_bytes；没有这些字段（目前的 MySQL 巡检不采集磁盘剩余）返回 ok=false。
func diskFreeFromResult(result string) (float64, bool) {
	var doc map[string]map[string]any
	if json.Unmarshal([]byte(result), &doc) != nil {
		return 0, false
	}
	c := doc["capacity"]
	num := func(k string) (float64, bool) {
		switch v := c[k].(type) {
		case float64:
			return v, true
		}
		return 0, false
	}
	if p, ok := num("disk_free_pct"); ok && p >= 0 && p <= 100 {
		return p, true
	}
	free, ok1 := num("disk_free_bytes")
	total, ok2 := num("disk_total_bytes")
	if ok1 && ok2 && total > 0 && free >= 0 && free <= total {
		return free / total * 100, true
	}
	return 0, false
}

// inspectDiskFreePct 读该实例最近一次成功的 MySQL 巡检报告。表不存在 / 没有报告 / 报告太旧 / 没有磁盘指标都是 ok=false，不是错误。
func (r *gormRepo) inspectDiskFreePct(ctx context.Context, instanceID uint, now time.Time) (float64, bool, error) {
	var row struct {
		Result     *string
		FinishTime *time.Time
	}
	res := r.db.WithContext(ctx).Raw("SELECT result, finish_time FROM dbt_sql_mysql_inspection_report WHERE instance_id = ? AND status = 'finish' ORDER BY id DESC LIMIT 1", instanceID).Scan(&row)
	if res.Error != nil || res.RowsAffected == 0 || row.Result == nil {
		return 0, false, nil
	}
	if row.FinishTime == nil || now.Sub(*row.FinishTime) > inspectDiskMaxAge {
		return 0, false, nil
	}
	pct, ok := diskFreeFromResult(*row.Result)
	return pct, ok, nil
}
