// @author: kevinkelly
// @license: Apache Licence
// @file: files.go
// @time: 2026/10/28

package export

import (
	"archive/zip"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Windows 保留设备名，作为文件名会写入设备而不是文件。
var reserved = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true}

func init() {
	for i := 1; i <= 9; i++ {
		reserved[fmt.Sprintf("COM%d", i)] = true
		reserved[fmt.Sprintf("LPT%d", i)] = true
	}
}

var unsafeChars = regexp.MustCompile(`[\/:*?"<>|\x00-\x1f]`)

// SafeName 把任意文本转成安全的文件名主干：路径与特殊字符换成下划线，去掉结尾的点和空格，规避保留设备名，限制长度。
func SafeName(s, fallback string) string {
	s = unsafeChars.ReplaceAllString(s, "_")
	s = strings.ReplaceAll(s, "..", "_") // 文件名里不留 ".."
	s = strings.TrimRight(s, ". ")
	if strings.Trim(s, "_") == "" {
		s = fallback
	}
	if reserved[strings.ToUpper(strings.SplitN(s, ".", 2)[0])] {
		s = "_" + s
	}
	if r := []rune(s); len(r) > 80 {
		s = string(r[:80])
	}
	return s
}

// InstanceFileName 基于实例名生成安全且唯一的文件名。实例名经替换后可能相互冲突（如 a/b 与 a:b），
// 冲突时追加实例 ID，避免后写入的文件静默覆盖前一个。used 是已用文件名（小写）集合，会被更新。
func InstanceFileName(name string, id uint, format string, used map[string]bool) string {
	safe := SafeName(name, "unknown")
	fn := safe + "." + format
	if used[strings.ToLower(fn)] {
		fn = fmt.Sprintf("%s_%d.%s", safe, id, format)
	}
	used[strings.ToLower(fn)] = true
	return fn
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// SingleArchiveName 单实例导出的压缩包名：<库名>_<时间戳>_<8位随机>.zip，压缩包内是同名主干的导出文件。
func SingleArchiveName(db string, at time.Time) (zipName, entryBase string) {
	base := fmt.Sprintf("%s_%s_%s", SafeName(db, "export"), at.Format("20060102150405"), randHex(4))
	return base + ".zip", base
}

// BatchArchiveName 批量导出的压缩包名：<工单号>_<时间戳>_<8位随机>.zip。
func BatchArchiveName(workflowID uint, at time.Time) string {
	return fmt.Sprintf("%d_%s_%s.zip", workflowID, at.Format("20060102150405"), randHex(4))
}

// ZipSingle 把一个文件打成 zip。
func ZipSingle(dst io.Writer, entryName string, write func(io.Writer) error) error {
	zw := zip.NewWriter(dst)
	w, err := zw.Create(entryName)
	if err != nil {
		return err
	}
	if err := write(w); err != nil {
		return err
	}
	return zw.Close()
}

// ZipDir 把目录整体打成 zip，包内保留 <dirName>/ 这一层（批量导出里 dirName 是工单号），文件按名字排序保证包内顺序稳定。
func ZipDir(dst io.Writer, dir, dirName string) error {
	var files []string
	if err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			files = append(files, rel)
		}
		return nil
	}); err != nil {
		return err
	}
	sort.Strings(files)
	zw := zip.NewWriter(dst)
	for _, rel := range files {
		w, err := zw.Create(dirName + "/" + filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		f, err := os.Open(filepath.Join(dir, rel))
		if err != nil {
			return err
		}
		_, err = io.Copy(w, f)
		f.Close()
		if err != nil {
			return err
		}
	}
	return zw.Close()
}

// Unzip 把 zip 解到目录。拒绝会逃出目录的条目（zip-slip）。
func Unzip(zipPath, dest string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	root, _ := filepath.Abs(dest)
	for _, f := range zr.File {
		target := filepath.Join(root, filepath.FromSlash(f.Name))
		if !strings.HasPrefix(target, root+string(os.PathSeparator)) && target != root {
			return fmt.Errorf("压缩包里有不安全的路径：%s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
