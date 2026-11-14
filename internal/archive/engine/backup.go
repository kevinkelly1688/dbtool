// @author: kevinkelly
// @license: Apache Licence
// @file: backup.go
// @time: 2026/10/28

package engine

import (
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// backupWriter 把读到的行写成备份文件：先写本地临时文件（每批一个压缩成员 / 加密分块，写完 fsync），
// 文件达到 PartMB 就滚动：计算 sha256、Put 到 FileStore、回读校验大小与 sha256，然后删掉本地临时文件。
// 运行结束时 Finish 上传最后一个文件并生成清单（manifest.json，同样回读校验）。
//
// 失败语义：任何一步失败都返回错误，本地临时文件保留（错误信息里带路径），由上层把运行标为失败并通知。
type backupWriter struct {
	store  FileStore
	fmtter batchFormatter
	gz     bool
	aead   cipher.AEAD
	dir    string // 存储里的目录（ExpandPath 的结果）
	tmpDir string
	tag    string
	table  string
	cols   []string
	kinds  []colKind
	sleep  func(ctx context.Context, d time.Duration) error

	partMax int64
	nextIdx int
	cur     *backupPart
	files   []ManifestFile
	rows    int64
}

type backupPart struct {
	f      *os.File
	path   string
	key    string
	size   int64
	rows   int64
	chunks uint64
	header bool
}

func newBackupWriter(store FileStore, dest FileDest, key []byte, dir, tmpDir, tag, table string, cols []string, kinds []colKind,
	sleep func(context.Context, time.Duration) error) (*backupWriter, error) {
	w := &backupWriter{store: store, fmtter: formatterFor(dest.Format), gz: dest.Gzip, dir: dir, tmpDir: tmpDir, tag: tag,
		table: table, cols: cols, kinds: kinds, sleep: sleep, partMax: int64(effPartMB(&dest)) << 20, nextIdx: 1}
	if dest.Encrypt {
		aead, err := newGCM(key)
		if err != nil {
			return nil, err
		}
		w.aead = aead
	}
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return nil, fmt.Errorf("创建本地临时目录失败：%w", err)
	}
	return w, nil
}

func (w *backupWriter) partName(idx int) string {
	name := fmt.Sprintf("part-%04d.%s", idx, w.fmtter.ext())
	if w.gz {
		name += ".gz"
	}
	if w.aead != nil {
		name += ".enc"
	}
	return name
}

func joinKey(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// freeKey 返回存储里还不存在的下一个 part 的序号和 Key（避免覆盖同目录下已有备份）。
func (w *backupWriter) freeKey(ctx context.Context) (int, string, error) {
	for idx := w.nextIdx; idx < w.nextIdx+10000; idx++ {
		key := joinKey(w.dir, w.partName(idx))
		_, err := w.store.Stat(ctx, key)
		if err == nil {
			continue
		}
		if errors.Is(err, ErrNotExist) {
			return idx, key, nil
		}
		return 0, "", fmt.Errorf("检查备份对象是否已存在失败：%w", err)
	}
	return 0, "", errors.New("备份目录下文件序号用尽")
}

func (w *backupWriter) openPart(ctx context.Context) error {
	idx, key, err := w.freeKey(ctx)
	if err != nil {
		return err
	}
	w.nextIdx = idx + 1
	path := filepath.Join(w.tmpDir, fmt.Sprintf("%s-part-%04d.tmp", w.tag, idx))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("创建本地临时文件失败：%w", err)
	}
	w.cur = &backupPart{f: f, path: path, key: key}
	return nil
}

// encode 把一批行编码成要追加到文件里的字节（含必要的文件头 / 压缩 / 加密）。
func (w *backupWriter) encode(p *backupPart, rowsBytes []byte) ([]byte, error) {
	plain := rowsBytes
	if !p.header {
		plain = append(append([]byte{}, w.fmtter.header(w.table, w.cols)...), rowsBytes...)
	}
	payload := plain
	if w.gz {
		z, err := gzipBytes(plain)
		if err != nil {
			return nil, err
		}
		payload = z
	}
	if w.aead == nil {
		return payload, nil
	}
	chunk, err := encryptChunk(w.aead, p.chunks, payload)
	if err != nil {
		return nil, err
	}
	if p.size == 0 {
		return append([]byte(encMagic), chunk...), nil
	}
	return chunk, nil
}

// Append 写入一批行，返回已落盘（fsync）的行数。写失败时把文件截回写入前的长度，不会留下半批数据。
func (w *backupWriter) Append(ctx context.Context, rows []Row) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	if w.cur == nil {
		if err := w.openPart(ctx); err != nil {
			return 0, err
		}
	}
	rowsBytes := w.fmtter.rows(w.table, w.cols, w.kinds, rows)
	data, err := w.encode(w.cur, rowsBytes)
	if err != nil {
		return 0, err
	}
	if w.cur.size > 0 && w.cur.size+int64(len(data)) > w.partMax {
		if err := w.finalizePart(ctx); err != nil {
			return 0, err
		}
		if err := w.openPart(ctx); err != nil {
			return 0, err
		}
		if data, err = w.encode(w.cur, rowsBytes); err != nil {
			return 0, err
		}
	}
	p := w.cur
	if _, err := p.f.Write(data); err != nil {
		w.rollback(p)
		return 0, fmt.Errorf("写备份文件失败：%w", err)
	}
	if err := p.f.Sync(); err != nil {
		w.rollback(p)
		return 0, fmt.Errorf("写备份文件失败：%w", err)
	}
	p.size += int64(len(data))
	p.rows += int64(len(rows))
	p.chunks++
	p.header = true
	w.rows += int64(len(rows))
	return len(rows), nil
}

func (w *backupWriter) rollback(p *backupPart) {
	if err := p.f.Truncate(p.size); err == nil {
		_, _ = p.f.Seek(p.size, io.SeekStart)
	}
}

func sha256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func (w *backupWriter) wait(ctx context.Context, d time.Duration) {
	if w.sleep != nil {
		_ = w.sleep(ctx, d)
		return
	}
	_ = sleepCtx(ctx, d)
}

// putVerified 上传一个对象并回读校验大小与 sha256；上传前先用 open 取一个新的读取器（重试时要从头读）。
func (w *backupWriter) putVerified(ctx context.Context, key string, open func() (io.ReadCloser, error), size int64, sum string) error {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			w.wait(ctx, time.Duration(attempt)*time.Second)
			if ctx.Err() != nil {
				break
			}
		}
		r, err := open()
		if err != nil {
			return err
		}
		err = w.store.Put(ctx, key, r, size)
		r.Close()
		if err != nil {
			last = fmt.Errorf("上传失败：%w", err)
			continue
		}
		fi, err := w.store.Stat(ctx, key)
		if err != nil {
			last = fmt.Errorf("上传后读取对象信息失败：%w", err)
			continue
		}
		if fi.Size != size {
			last = fmt.Errorf("上传后对象大小 %d 与本地 %d 不一致", fi.Size, size)
			continue
		}
		gr, err := w.store.Get(ctx, key)
		if err != nil {
			last = fmt.Errorf("回读失败：%w", err)
			continue
		}
		h := sha256.New()
		n, err := io.Copy(h, gr)
		gr.Close()
		if err != nil {
			last = fmt.Errorf("回读失败：%w", err)
			continue
		}
		if n != size || hex.EncodeToString(h.Sum(nil)) != sum {
			last = fmt.Errorf("回读校验不一致（大小 %d / %d，sha256 不匹配）", n, size)
			continue
		}
		return nil
	}
	if last == nil {
		last = ctx.Err()
	}
	return last
}

func (w *backupWriter) finalizePart(ctx context.Context) error {
	p := w.cur
	if p == nil {
		return nil
	}
	w.cur = nil
	if err := p.f.Close(); err != nil {
		return fmt.Errorf("关闭本地备份文件失败（保留在 %s）：%w", p.path, err)
	}
	sum, size, err := sha256File(p.path)
	if err != nil {
		return fmt.Errorf("读取本地备份文件失败（保留在 %s）：%w", p.path, err)
	}
	err = w.putVerified(ctx, p.key, func() (io.ReadCloser, error) { return os.Open(p.path) }, size, sum)
	if err != nil {
		return fmt.Errorf("备份文件 %s 未能存入备份存储（本地文件保留在 %s）：%w", p.key, p.path, err)
	}
	_ = os.Remove(p.path)
	w.files = append(w.files, ManifestFile{Key: p.key, Rows: p.rows, Size: size, SHA256: sum})
	return nil
}

// Finish 上传最后一个文件并生成清单。没有写过任何行时返回 (nil, nil)。
func (w *backupWriter) Finish(ctx context.Context, info TableInfo, minPK, maxPK []string, dest FileDest) (*Manifest, error) {
	if err := w.finalizePart(ctx); err != nil {
		return nil, err
	}
	if len(w.files) == 0 {
		return nil, nil
	}
	m := &Manifest{Files: w.files, Rows: w.rows, MinPK: minPK, MaxPK: maxPK, Cols: info.Columns,
		Format: dest.Format, Gzip: dest.Gzip, Table: info.Name}
	if w.aead != nil {
		m.Enc = "aes-256-gcm"
	}
	key := joinKey(w.dir, "manifest.json")
	for i := 2; ; i++ {
		_, err := w.store.Stat(ctx, key)
		if errors.Is(err, ErrNotExist) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("检查清单是否已存在失败：%w", err)
		}
		key = joinKey(w.dir, fmt.Sprintf("manifest-%d.json", i))
		if i > 10000 {
			return nil, errors.New("清单文件序号用尽")
		}
	}
	m.ManifestKey = key
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	err = w.putVerified(ctx, key, func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil },
		int64(len(body)), hex.EncodeToString(sum[:]))
	if err != nil {
		return nil, fmt.Errorf("清单 %s 未能存入备份存储：%w", key, err)
	}
	return m, nil
}

// Rows 返回已落盘的行数。
func (w *backupWriter) Rows() int64 { return w.rows }

// LocalLeftovers 返回未成功上传、仍留在本地的临时文件路径（用于报错提示）。
func (w *backupWriter) LocalLeftovers() []string {
	if w.cur != nil {
		return []string{w.cur.path}
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
