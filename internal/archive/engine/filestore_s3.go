// @author: kevinkelly
// @license: Apache Licence
// @file: filestore_s3.go
// @time: 2026/10/28

package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// s3Store 是 S3 兼容对象存储（AWS S3、MinIO，以及阿里云 OSS 的 S3 兼容端点）。
// 标准库 net/http + SigV4 请求头签名；支持路径风格（endpoint/bucket/key）和虚拟主机风格（bucket.endpoint/key）。
//
// 说明与限制：
//   - OSS 只通过其 S3 兼容端点访问，没有实现 OSS 原生签名（OSS4 / OSS V1）；OSS 要求虚拟主机风格，类型为 oss 时强制使用。
//   - Put 是单对象上传（不分片），靠上层的 PartMB 滚动避免超大对象；超过 S3 单次 PUT 的 5 GiB 上限会失败。
//   - 签名时对 Put 的负载计算真实 sha256（要求 Reader 可 Seek，引擎传入的都是文件 / 内存数据）；不可 Seek 时用 UNSIGNED-PAYLOAD。
//   - 凭据只放在 Authorization 头里，不进日志和错误信息。
type s3Store struct {
	cfg    StoreConfig
	base   *url.URL
	region string
	virt   bool
	prefix string
	hc     *http.Client
	now    func() time.Time
}

var ossRegionRe = regexp.MustCompile(`(?i)^(?:[^.]+\.)?oss-([a-z0-9-]+?)(?:-internal)?\.aliyuncs\.com$`)

func newS3Store(cfg StoreConfig) (*s3Store, error) {
	ep := strings.TrimSpace(cfg.Endpoint)
	if ep == "" {
		return nil, errors.New("S3 / OSS 存储需要填写地址（Endpoint）")
	}
	if !strings.Contains(ep, "://") {
		ep = "https://" + ep
	}
	u, err := url.Parse(ep)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("存储地址无效，应形如 https://host[:port]")
	}
	if strings.Trim(u.Path, "/") != "" {
		return nil, errors.New("存储地址不要带路径")
	}
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, errors.New("请填写桶（Bucket）")
	}
	if cfg.AK == "" || cfg.SK == "" {
		return nil, errors.New("请填写 AK / SK")
	}
	s := &s3Store{cfg: cfg, base: &url.URL{Scheme: u.Scheme, Host: u.Host}, now: time.Now}
	s.region = strings.TrimSpace(cfg.Region)
	if s.region == "" {
		s.region = "us-east-1"
		if m := ossRegionRe.FindStringSubmatch(u.Hostname()); m != nil {
			s.region = m[1]
		}
	}
	s.virt = !cfg.PathStyle
	if strings.EqualFold(cfg.Type, "oss") {
		s.virt = true
	}
	if net.ParseIP(u.Hostname()) != nil || u.Hostname() == "localhost" { // IP / localhost 没法用桶名做子域名
		s.virt = false
	}
	if p := cleanKey(cfg.Prefix); p != "" {
		if err := checkKey(p); err != nil {
			return nil, fmt.Errorf("路径前缀无效：%w", err)
		}
		s.prefix = p + "/"
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       60 * time.Second,
	}
	s.hc = &http.Client{Transport: tr}
	return s, nil
}

func (s *s3Store) fullKey(key string) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	return s.prefix + key, nil
}

// target 返回请求的 Host 与已编码路径。
func (s *s3Store) target(key string) (host, encPath string) {
	if s.virt {
		return s.cfg.Bucket + "." + s.base.Host, "/" + uriEncode(key, false)
	}
	return s.base.Host, "/" + uriEncode(s.cfg.Bucket, true) + "/" + uriEncode(key, false)
}

func (s *s3Store) secrets() []string { return []string{s.cfg.AK, s.cfg.SK} }

// do 构造、签名并发送请求。body 为 nil 表示无负载。
func (s *s3Store) do(ctx context.Context, method, key string, body io.Reader, size int64) (*http.Response, error) {
	fk, err := s.fullKey(key)
	if err != nil {
		return nil, err
	}
	host, encPath := s.target(fk)
	payloadHash := emptyPayloadSHA
	var rd io.Reader
	if body != nil {
		rd = body
		if rs, ok := body.(io.ReadSeeker); ok {
			h := sha256.New()
			if _, err := io.Copy(h, io.LimitReader(rs, size)); err != nil {
				return nil, err
			}
			if _, err := rs.Seek(-size, io.SeekCurrent); err != nil {
				return nil, err
			}
			payloadHash = hex.EncodeToString(h.Sum(nil))
		} else {
			payloadHash = "UNSIGNED-PAYLOAD"
		}
	}
	u := s.base.Scheme + "://" + host + encPath
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, redact(err, s.secrets()...)
	}
	if body != nil {
		req.ContentLength = size
		if size == 0 {
			req.Body = http.NoBody
		}
	}
	now := s.now().UTC()
	hdrs := map[string]string{"host": host, "x-amz-content-sha256": payloadHash, "x-amz-date": now.Format("20060102T150405Z")}
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("x-amz-date", hdrs["x-amz-date"])
	req.Header.Set("Authorization", sigV4(method, encPath, "", hdrs, payloadHash, s.cfg.AK, s.cfg.SK, s.region, "s3", now))
	resp, err := s.hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // 去掉 URL
		}
		return nil, redact(fmt.Errorf("%s %s 请求失败：%w", method, s.cfg.Bucket, err), s.secrets()...)
	}
	return resp, nil
}

type s3Err struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

// statusErr 把非预期的响应转成错误（只含状态码与对象存储返回的 Code / Message）。
func (s *s3Store) statusErr(op string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	var e s3Err
	if xml.Unmarshal(b, &e) == nil && e.Code != "" {
		return redact(fmt.Errorf("%s 失败：HTTP %d %s：%s", op, resp.StatusCode, e.Code, e.Message), s.secrets()...)
	}
	return fmt.Errorf("%s 失败：HTTP %d", op, resp.StatusCode)
}

func (s *s3Store) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if size < 0 {
		// 不知道大小就先读进内存（探测对象、清单等小对象才会走到这里）
		b, err := io.ReadAll(ctxReader{ctx, r})
		if err != nil {
			return err
		}
		r, size = bytes.NewReader(b), int64(len(b))
	}
	resp, err := s.do(ctx, http.MethodPut, key, r, size)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return s.statusErr("上传", resp)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	return nil
}

func (s *s3Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := s.do(ctx, http.MethodGet, key, nil, 0)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, fmt.Errorf("对象 %s 不存在：%w", key, ErrNotExist)
	}
	if resp.StatusCode/100 != 2 {
		return nil, s.statusErr("读取", resp)
	}
	return resp.Body, nil
}

func (s *s3Store) Stat(ctx context.Context, key string) (FileInfo, error) {
	resp, err := s.do(ctx, http.MethodHead, key, nil, 0)
	if err != nil {
		return FileInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return FileInfo{}, fmt.Errorf("对象 %s 不存在：%w", key, ErrNotExist)
	}
	if resp.StatusCode/100 != 2 {
		return FileInfo{}, fmt.Errorf("读取对象信息失败：HTTP %d", resp.StatusCode)
	}
	n, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if err != nil {
		return FileInfo{}, errors.New("响应里没有有效的 Content-Length")
	}
	return FileInfo{Key: key, Size: n}, nil
}

func (s *s3Store) Delete(ctx context.Context, key string) error {
	resp, err := s.do(ctx, http.MethodDelete, key, nil, 0)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 == 2 || resp.StatusCode == http.StatusNotFound {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		return nil
	}
	return s.statusErr("删除", resp)
}

func (s *s3Store) Test(ctx context.Context) (time.Duration, error) { return testStore(ctx, s) }

// Free 对象存储没有“可用空间”的概念。
func (s *s3Store) Free(ctx context.Context) (int64, bool, error) { return 0, false, nil }
