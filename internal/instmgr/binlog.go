// @author: kevinkelly
// @license: Apache Licence
// @file: binlog.go
// @time: 2026/10/28

package instmgr

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"dbtool/internal/i18n"
	"dbtool/internal/model"
)

// Binlog 清理前的从库进度核对。
//
// PURGE BINARY LOGS TO 'X' 会删除 X 之前的所有文件。如果某个从库的 IO 线程还在读 X 之前的文件，清理后复制就断了。
// 主库自己只知道有哪些从库连着（processlist 里的 Binlog Dump 线程），不知道它们读到了哪里；
// 所以对连着的从库，凡是在平台里登记过的（按 HOST 对应，角色为从库的 MySQL 实例），都连上去读 SHOW REPLICA STATUS，
// 取它正在读取的主库 binlog 文件，与清理目标比较。没登记的从库只能提示，无法核对。

// ReplicaInfo 是一个连着的从库。
type ReplicaInfo struct {
	Name       string `json:"name"` // 平台登记的实例名；未登记为空
	InstanceID uint   `json:"instance_id,omitempty"`
	Host       string `json:"host"`
	Registered bool   `json:"registered"`
	Checked    bool   `json:"checked"` // 读到了复制进度
	ReadFile   string `json:"read_file"`
	ReadPos    int64  `json:"read_pos"`
	IORunning  string `json:"io_running"`
	SQLRunning string `json:"sql_running"`
	Behind     bool   `json:"behind"` // 清理目标越过了它的读取进度
	Error      string `json:"error,omitempty"`
}

const (
	replicaProbeTimeout = 6 * time.Second
	replicaMaxParallel  = 4
	replicaMaxInstances = 500
)

// hostOfDump 取 processlist.host（形如 10.0.0.5:34567 或 name:34567）里的主机部分。
func hostOfDump(h string) string {
	h = strings.TrimSpace(h)
	if strings.HasPrefix(h, "[") { // [::1]:port
		if i := strings.Index(h, "]"); i > 0 {
			return strings.ToLower(h[1:i])
		}
	}
	if i := strings.LastIndex(h, ":"); i > 0 && strings.Count(h, ":") == 1 {
		h = h[:i]
	}
	return strings.ToLower(h)
}

// checkReplicas 核对从库进度，结果写进 p（Replicas / Blocked / BlockReason / Warnings）。files 是主库当前的 binlog 列表，toIdx 是清理目标的下标。
// 任何一步失败都只降级为提示，不让预览失败。lang 是请求语言：提示文字整句按它生成（列表型的句子无法事后按目录匹配）。
func (s *Service) checkReplicas(se *session, p *PurgePlan, files []BinlogFile, toIdx int, lang string) {
	r, err := se.query("SELECT host AS host FROM information_schema.processlist WHERE command LIKE 'Binlog Dump%' ORDER BY id")
	if err != nil {
		return
	}
	var hosts []string
	seen := map[string]bool{}
	for _, rec := range r.Records() {
		h := hostOfDump(rec.Str("host"))
		if h != "" && !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		return
	}
	slaves, _, err := s.repo.ListInstances(InstanceFilter{Role: "slave", DBType: "mysql", Limit: replicaMaxInstances}, nil)
	if err != nil {
		slaves = nil
	}
	byHost := map[string][]model.Instance{}
	for _, sl := range slaves {
		k := strings.ToLower(strings.TrimSpace(sl.Host))
		byHost[k] = append(byHost[k], sl)
	}

	type job struct {
		inst model.Instance
		idx  int
	}
	var results []ReplicaInfo
	var jobs []job
	for _, h := range hosts {
		cands := byHost[h]
		if len(cands) == 0 {
			results = append(results, ReplicaInfo{Host: h})
			continue
		}
		for _, c := range cands {
			jobs = append(jobs, job{inst: c, idx: len(results)})
			results = append(results, ReplicaInfo{Host: h, Name: c.InstanceName, InstanceID: c.ID, Registered: true})
		}
	}
	var wg sync.WaitGroup
	gate := make(chan struct{}, replicaMaxParallel)
	for _, j := range jobs {
		wg.Add(1)
		gate <- struct{}{}
		go func(j job) {
			defer wg.Done()
			defer func() { <-gate }()
			results[j.idx] = s.probeReplica(se.ctx, &j.inst, se.inst.Host, se.inst.Port, results[j.idx], files, lang)
		}(j)
	}
	wg.Wait()

	var behind, behindZh, unknown []string
	for i := range results {
		rp := &results[i]
		label := nonEmptyStr(rp.Name, rp.Host)
		if !rp.Checked {
			if rp.Registered {
				unknown = append(unknown, i18n.T(lang, "{label}（{why}）", "label", label, "why", nonEmptyStr(rp.Error, i18n.T(lang, "未能读取复制进度"))))
			} else {
				unknown = append(unknown, i18n.T(lang, "{host}（平台里没有登记）", "host", rp.Host))
			}
			continue
		}
		ri := binlogIndex(files, rp.ReadFile)
		switch {
		case ri < 0:
			p.Warnings = append(p.Warnings, i18n.T(lang, "从库 {label} 正在读取 {file}，它不在本实例当前的 binlog 列表里，无法比较进度", "label", label, "file", rp.ReadFile))
		case toIdx > ri:
			rp.Behind = true
			behind = append(behind, i18n.T(lang, "{label} 正在读取 {file}", "label", label, "file", rp.ReadFile))
			behindZh = append(behindZh, i18n.T(i18n.ZH, "{label} 正在读取 {file}", "label", label, "file", rp.ReadFile))
		}
	}
	p.Replicas = results
	if len(behind) > 0 {
		p.Blocked = true
		const blockKey = "从库尚未读完要清理的 binlog（{list}）；清理到 {to} 会让复制中断。请选择不晚于从库读取位置的文件"
		p.BlockReason = i18n.T(lang, blockKey, "list", strings.Join(behind, i18n.T(lang, "；")), "to", p.To)
		p.blockZh = i18n.T(i18n.ZH, blockKey, "list", strings.Join(behindZh, "；"), "to", p.To)
	}
	if len(unknown) > 0 {
		p.Warnings = append(p.Warnings, i18n.T(lang, "以下从库连着本实例，但读不到它们的复制进度，请自行确认它们已读到 {to} 之后：{list}", "to", p.To, "list", strings.Join(unknown, i18n.T(lang, "；"))))
	}
}

func binlogIndex(files []BinlogFile, name string) int {
	for i, f := range files {
		if f.Name == name {
			return i
		}
	}
	return -1
}

// probeReplica 连上从库读复制状态。失败写进 Error，不返回错误。
func (s *Service) probeReplica(ctx context.Context, sl *model.Instance, masterHost string, masterPort int, info ReplicaInfo, files []BinlogFile, lang string) ReplicaInfo {
	if err := s.rejectDisabled(sl.ID); err != nil {
		info.Error = i18n.T(lang, "该从库实例已停用，未探测")
		return info
	}
	ci, err := s.connInfo(sl)
	if err != nil {
		info.Error = i18n.T(lang, "读取连接信息失败")
		return info
	}
	cctx, cancel := context.WithTimeout(ctx, replicaProbeTimeout)
	defer cancel()
	t, err := s.dialer.Dial(cctx, ci)
	if err != nil {
		info.Error = i18n.T(lang, "无法连接：{err}", "err", clip(err.Error(), 100))
		return info
	}
	defer t.Close()
	res, err := t.Query(cctx, "SHOW REPLICA STATUS") // 8.0.22+ / 8.4；老版本语法不认，回退 SHOW SLAVE STATUS
	if err != nil || res == nil || len(res.Rows) == 0 {
		res, err = t.Query(cctx, "SHOW SLAVE STATUS")
	}
	if err != nil {
		info.Error = i18n.T(lang, "读取复制状态失败：{err}", "err", clip(err.Error(), 100))
		return info
	}
	recs := res.Records()
	if len(recs) == 0 {
		info.Error = i18n.T(lang, "该实例没有在做复制")
		return info
	}
	var match []Record
	for _, rec := range recs {
		h := strings.ToLower(pickStr(rec, "master_host", "source_host"))
		port, _ := strconv.Atoi(pickStr(rec, "master_port", "source_port"))
		if h == strings.ToLower(masterHost) && port == masterPort {
			match = append(match, rec)
		}
	}
	if len(match) == 0 { // 经 VIP / 域名复制：主机写法对不上，按从库自己的所有通道取最慢的
		match = recs
	}
	best, bestIdx := match[0], -2
	for _, rec := range match {
		if i := binlogIndex(files, pickStr(rec, "master_log_file", "source_log_file")); bestIdx == -2 || (i >= 0 && (bestIdx < 0 || i < bestIdx)) {
			best, bestIdx = rec, i
		}
	}
	info.Checked = true
	info.ReadFile = pickStr(best, "master_log_file", "source_log_file")
	if n, err := strconv.ParseInt(pickStr(best, "read_master_log_pos", "read_source_log_pos"), 10, 64); err == nil {
		info.ReadPos = n
	}
	info.IORunning = pickStr(best, "slave_io_running", "replica_io_running")
	info.SQLRunning = pickStr(best, "slave_sql_running", "replica_sql_running")
	if info.ReadFile == "" {
		info.Checked = false
		info.Error = i18n.T(lang, "复制状态里没有读取位置（IO 线程未启动）")
	}
	return info
}

func pickStr(rec Record, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(rec.Str(k)); v != "" {
			return v
		}
	}
	return ""
}
