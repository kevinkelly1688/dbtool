// @author: kevinkelly
// @license: Apache Licence
// @file: spool.go
// @time: 2026/10/28

package archive

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 文件备份先写到暂存目录（spoolDir），fsync 后上传到备份存储。删源发生在落盘之后、上传之前，
// 所以进程被强杀时，暂存目录里遗留的 *.tmp 可能是“源数据已删除、备份还没上传”的唯一副本。
// 启动时扫描并提示（日志 + 审计 + 运行记录的说明），绝不自动上传或删除：是否补传由人工判断。

var spoolNameRe = regexp.MustCompile(`^t(\d+)-r(\d+)-(\d+)-part-(\d+)\.tmp$`)

type spoolFile struct {
	Name    string
	Size    int64
	ModTime time.Time
	TaskID  uint
	RunID   uint
}

const spoolNoteMarker = "暂存目录" // i18n-ignore：既是写进运行备注的前缀，也是识别它的标记（译文在目录里，见下面的 Sprintf）

// scanSpool 列出目录下符合命名规则的遗留 *.tmp；目录不存在返回空。
func scanSpool(dir string) []spoolFile {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []spoolFile
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		m := spoolNameRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		tid, _ := strconv.ParseUint(m[1], 10, 32)
		rid, _ := strconv.ParseUint(m[2], 10, 32)
		out = append(out, spoolFile{Name: e.Name(), Size: fi.Size(), ModTime: fi.ModTime(), TaskID: uint(tid), RunID: uint(rid)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// scanSpoolAtStart 在 Worker 启动时扫描暂存目录，返回发现的遗留文件。表没建时只写日志。
func (s *Service) scanSpoolAtStart() []spoolFile {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("数据归档暂存目录扫描异常", "panic", r)
		}
	}()
	dir := spoolDir()
	files := scanSpool(dir)
	if len(files) == 0 {
		return nil
	}
	var total int64
	byRun := map[uint][]spoolFile{}
	for _, f := range files {
		total += f.Size
		byRun[f.RunID] = append(byRun[f.RunID], f)
	}
	slog.Warn("数据归档：暂存目录里有遗留的 .tmp 文件（可能是进程被强杀时尚未上传的备份），不会自动上传或删除，请人工核对并补传",
		"dir", dir, "files", len(files), "bytes", total)
	if !s.repo.Ready() {
		return files
	}
	now := s.now()
	noted := 0
	var runIDs []uint
	for id := range byRun {
		runIDs = append(runIDs, id)
	}
	sort.Slice(runIDs, func(i, j int) bool { return runIDs[i] < runIDs[j] })
	for _, id := range runIDs {
		fs := byRun[id]
		r, err := s.repo.GetRun(id)
		if err != nil || r == nil {
			continue
		}
		// 还在运行（租约未过期）的运行可能正在写这些文件（暂存目录被多个进程共用时），不标注
		if (r.Result == ResRunning || r.Result == ResPaused) && r.LeaseUntil != nil && r.LeaseUntil.After(now) {
			continue
		}
		if strings.Contains(r.Note, spoolNoteMarker) {
			continue
		}
		var sz int64
		for _, f := range fs {
			sz += f.Size
		}
		msg := fmt.Sprintf("%s %s 遗留 %d 个未上传的备份临时文件（共 %s），需人工核对并补传", spoolNoteMarker, dir, len(fs), humanBytes(sz))
		if r.Note != "" {
			msg = r.Note + "；" + msg
		}
		r.Note = truncRunes(msg, 500)
		if err := s.repo.SaveRun(r); err != nil {
			slog.Warn("写入暂存遗留说明失败", "run", id, "err", err)
			continue
		}
		noted++
	}
	s.auditAt(systemCaller, "暂存文件遗留", fmt.Sprintf("暂存目录 %s 有 %d 个遗留 .tmp 文件（共 %s，涉及 %d 个运行，已在运行记录里标注 %d 个）；不会自动上传，需人工补传", // i18n-ignore：审计日志正文
		dir, len(files), humanBytes(total), len(byRun), noted))
	return files
}
