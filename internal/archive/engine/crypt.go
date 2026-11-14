// @author: kevinkelly
// @license: Apache Licence
// @file: crypt.go
// @time: 2026/10/28

package engine

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// 加密文件格式（aes-256-gcm）：
//
//	文件头  8 字节魔数 "DBAARC1\n"
//	分块    重复：4 字节大端密文长度 L | 12 字节随机 nonce | L 字节密文（含 16 字节 GCM 认证标签）
//
// 每块是一批数据（压缩时是一个完整的 gzip 成员）的密文，附加认证数据 AAD = 魔数 + 8 字节大端块序号，
// 所以块不能被调换顺序；整体被截断 / 追加由 manifest 里的行数、大小和 sha256 发现。
// nonce 每块随机生成（96 位，同一密钥下块数远小于 2^32，碰撞概率可忽略）。
const encMagic = "DBAARC1\n"

const maxChunkCipher = 1 << 30

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("加密密钥必须是 32 字节（当前 %d）", len(key))
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}

func chunkAAD(idx uint64) []byte {
	aad := make([]byte, len(encMagic)+8)
	copy(aad, encMagic)
	binary.BigEndian.PutUint64(aad[len(encMagic):], idx)
	return aad
}

// encryptChunk 把明文加密成一个分块（含长度前缀和 nonce）。
func encryptChunk(aead cipher.AEAD, idx uint64, plain []byte) ([]byte, error) {
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	ct := aead.Seal(nil, nonce, plain, chunkAAD(idx))
	out := make([]byte, 4, 4+len(nonce)+len(ct))
	binary.BigEndian.PutUint32(out, uint32(len(ct)))
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

type decryptReader struct {
	r    io.Reader
	aead cipher.AEAD
	idx  uint64
	buf  bytes.Reader
}

// NewDecryptReader 读取加密文件并还原明文（压缩的话还是 gzip 流，用 OpenBackup 一步到位）。
func NewDecryptReader(r io.Reader, key []byte) (io.Reader, error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	magic := make([]byte, len(encMagic))
	if _, err := io.ReadFull(r, magic); err != nil {
		return nil, fmt.Errorf("读取加密文件头失败：%w", err)
	}
	if string(magic) != encMagic {
		return nil, errors.New("不是 dbtool 归档加密文件（文件头不匹配）")
	}
	return &decryptReader{r: r, aead: aead}, nil
}

func (d *decryptReader) Read(p []byte) (int, error) {
	for d.buf.Len() == 0 {
		var lb [4]byte
		if _, err := io.ReadFull(d.r, lb[:]); err != nil {
			if err == io.EOF {
				return 0, io.EOF
			}
			return 0, fmt.Errorf("加密文件被截断：%w", err)
		}
		n := binary.BigEndian.Uint32(lb[:])
		if n > maxChunkCipher || n < uint32(d.aead.Overhead()) {
			return 0, errors.New("加密文件分块长度异常")
		}
		nonce := make([]byte, d.aead.NonceSize())
		if _, err := io.ReadFull(d.r, nonce); err != nil {
			return 0, fmt.Errorf("加密文件被截断：%w", err)
		}
		ct := make([]byte, n)
		if _, err := io.ReadFull(d.r, ct); err != nil {
			return 0, fmt.Errorf("加密文件被截断：%w", err)
		}
		pt, err := d.aead.Open(nil, nonce, ct, chunkAAD(d.idx))
		if err != nil {
			return 0, errors.New("解密失败：密钥不对或文件被篡改 / 调换了顺序")
		}
		d.idx++
		d.buf.Reset(pt)
	}
	return d.buf.Read(p)
}

type multiCloser struct {
	io.Reader
	closers []io.Closer
}

func (m *multiCloser) Close() error {
	var first error
	for _, c := range m.closers {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// OpenBackup 把备份文件还原成明文流：enc 对应 manifest.Enc != ""，gz 对应 FileDest.Gzip。关闭返回值不会关闭 r。
func OpenBackup(r io.Reader, enc, gz bool, key []byte) (io.ReadCloser, error) {
	var closers []io.Closer
	if enc {
		dr, err := NewDecryptReader(r, key)
		if err != nil {
			return nil, err
		}
		r = dr
	}
	if gz {
		zr, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("解压失败：%w", err)
		}
		closers = append(closers, zr)
		r = zr
	}
	return &multiCloser{Reader: r, closers: closers}, nil
}

// gzipBytes 把一批数据压成一个独立的 gzip 成员（多个成员首尾相接仍是合法的 gzip 文件）。
func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
