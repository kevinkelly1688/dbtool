// @author: kevinkelly
// @license: Apache Licence
// @file: client.go
// @time: 2026/10/28

package clickhouse

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
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
	"unicode/utf8"

	"dbtool/internal/engine"
)

// ClickHouse 的 HTTP 接口客户端（仅标准库）。
//
// 每个请求：POST 到 http(s)://host:port/，语句放在 body，参数放在 URL 查询串，认证用 HTTP Basic（密码不进 URL、
// 不进日志、不进错误信息）。所有请求都带 readonly 与各类限额，见 reqOpts.values。

// Config 是连接配置。
type Config struct {
	Host     string
	Port     int // 0：http 用 8123，https 用 8443
	User     string
	Password string
	Database string // 默认库；空表示不传 database 参数（服务端用账号的默认库）
	TLS      bool
	// VerifyTLS 为 false 时不校验服务端证书（与实例管理里“校验证书”开关一致）。
	VerifyTLS bool
	// Readonly 是发送的 readonly 级别：1（默认）或 2。
	//
	// 选 1 的原因：readonly=1 时服务端除了禁止写入 / DDL / 改设置，还会禁止 file / url / remote 等“非只读安全”的表函数，
	// 是比 2 更严的第二道防线；代价是账号自身 profile 为 readonly=1 时，请求里带的限额类设置会被服务端拒绝（错误码 164），
	// 此时需要把账号 profile 调成 readonly=2（并用 constraints 固化限额），或者在这里改成 2 并接受较弱的防护。
	Readonly       int
	ConnectTimeout time.Duration // 默认 10 秒
}

// ConfigFromConn 把实例连接信息转成客户端配置。
func ConfigFromConn(c engine.Conn) Config {
	return Config{Host: c.Host, Port: c.Port, User: c.User, Password: c.Password, Database: strings.TrimSpace(c.DBName), TLS: c.SSL, VerifyTLS: c.VerifySSL}
}

func (c Config) port() int {
	if c.Port > 0 {
		return c.Port
	}
	if c.TLS {
		return 8443
	}
	return 8123
}

func (c Config) readonly() int {
	if c.Readonly == 2 {
		return 2
	}
	return 1
}

// Client 是线程安全的 HTTP 客户端。
type Client struct {
	cfg  Config
	hc   *http.Client
	base string
}

// NewClient 创建客户端（不立即连接）。
func NewClient(cfg Config) *Client {
	dt := cfg.ConnectTimeout
	if dt <= 0 {
		dt = 10 * time.Second
	}
	scheme := "http"
	if cfg.TLS {
		scheme = "https"
	}
	tr := &http.Transport{
		Proxy:                 nil, // 数据库实例走内网直连，不使用环境里的代理
		DialContext:           (&net.Dialer{Timeout: dt, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   dt,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       60 * time.Second,
		DisableCompression:    true, // 字节数按服务端实际输出计，不让传输层悄悄解压
		ExpectContinueTimeout: 0,
	}
	if cfg.TLS {
		tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: !cfg.VerifyTLS} // #nosec G402 -- 由实例的“校验证书”开关决定
	}
	host := cfg.Host
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]" // IPv6
	}
	return &Client{cfg: cfg, hc: &http.Client{Transport: tr}, base: scheme + "://" + net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(cfg.port())) + "/"}
}

// Close 关闭空闲连接。
func (c *Client) Close() { c.hc.CloseIdleConnections() }

// ---- 限额 ----

// QueryLimits 是一次查询的限额；0 表示用默认值，超过硬上限的值会被收紧。
type QueryLimits struct {
	MaxRows        int           // 返回行数上限，默认 1000，硬上限 1,000,000
	MaxBytes       int64         // 结果集字节上限（单元格内容合计），默认 4 MiB，硬上限 64 MiB
	Timeout        time.Duration // 执行超时，默认 30 秒，硬上限 10 分钟
	MaxRowsToRead  int64         // max_rows_to_read，默认 100,000,000
	MaxBytesToRead int64         // max_bytes_to_read，默认 10 GiB
	MaxMemory      int64         // max_memory_usage，默认 2 GiB，硬上限 16 GiB
	MaxThreads     int           // max_threads，默认 4，硬上限 16
	MaxCellBytes   int           // 单个单元格显示上限，默认 64 KiB
}

// 默认值与硬上限。
const (
	DefaultMaxRows        = 1000
	HardMaxRows           = 1_000_000
	DefaultMaxBytes       = 4 << 20
	HardMaxBytes          = 64 << 20
	DefaultTimeout        = 30 * time.Second
	HardTimeout           = 10 * time.Minute
	DefaultMaxRowsToRead  = 100_000_000
	DefaultMaxBytesToRead = 10 << 30
	DefaultMaxMemory      = 2 << 30
	HardMaxMemory         = 16 << 30
	DefaultMaxThreads     = 4
	HardMaxThreads        = 16
	DefaultMaxCellBytes   = 64 << 10
)

// timeoutGrace 是客户端超时比服务端 query.max_execution_seconds 多留的时间，让服务端先报出带错误码的超时。
var timeoutGrace = 5 * time.Second

// Normalize 填默认值并收紧到硬上限。
func (l QueryLimits) Normalize() QueryLimits {
	clamp := func(v, def, hard int64) int64 {
		if v <= 0 {
			return def
		}
		if v > hard {
			return hard
		}
		return v
	}
	l.MaxRows = int(clamp(int64(l.MaxRows), DefaultMaxRows, HardMaxRows))
	l.MaxBytes = clamp(l.MaxBytes, DefaultMaxBytes, HardMaxBytes)
	l.Timeout = time.Duration(clamp(int64(l.Timeout), int64(DefaultTimeout), int64(HardTimeout)))
	if l.MaxRowsToRead <= 0 {
		l.MaxRowsToRead = DefaultMaxRowsToRead
	}
	if l.MaxBytesToRead <= 0 {
		l.MaxBytesToRead = DefaultMaxBytesToRead
	}
	l.MaxMemory = clamp(l.MaxMemory, DefaultMaxMemory, HardMaxMemory)
	l.MaxThreads = int(clamp(int64(l.MaxThreads), DefaultMaxThreads, HardMaxThreads))
	if l.MaxCellBytes <= 0 {
		l.MaxCellBytes = DefaultMaxCellBytes
	}
	return l
}

// reqOpts 是一次 HTTP 请求的参数。
type reqOpts struct {
	db string
	QueryLimits
}

// values 生成 URL 查询串里的设置。每个请求都带 readonly、超时、行数 / 字节限制。
func (c *Client) values(o reqOpts) url.Values {
	secs := int64((o.Timeout + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	q := url.Values{}
	if o.db != "" {
		q.Set("database", o.db)
	}
	q.Set("readonly", strconv.Itoa(c.cfg.readonly()))
	q.Set("max_execution_time", strconv.FormatInt(secs, 10))
	q.Set("timeout_overflow_mode", "throw")
	// 多取 1 行：超过上限的那一行用来判断“被截断”，随后在客户端丢弃。result_overflow_mode=break 以块为粒度，可能多出一些行。
	q.Set("max_result_rows", strconv.Itoa(o.MaxRows+1))
	q.Set("max_result_bytes", strconv.FormatInt(o.MaxBytes, 10))
	q.Set("result_overflow_mode", "break")
	q.Set("max_rows_to_read", strconv.FormatInt(o.MaxRowsToRead, 10))
	q.Set("max_bytes_to_read", strconv.FormatInt(o.MaxBytesToRead, 10))
	q.Set("read_overflow_mode", "throw")
	q.Set("max_memory_usage", strconv.FormatInt(o.MaxMemory, 10))
	q.Set("max_threads", strconv.Itoa(o.MaxThreads))
	q.Set("default_format", "JSONCompact")
	// 查询跑完再开始回包：出错时是带状态码和错误头的整包错误，而不是 200 + 半截 JSON。结果受 max_result_* 限制，缓冲量有界。
	q.Set("wait_end_of_query", "1")
	// readonly 查询在客户端断开（超时 / 取消）时由服务端终止，不让它继续占资源。
	q.Set("cancel_http_readonly_queries_on_client_close", "1")
	q.Set("output_format_json_quote_64bit_integers", "1")
	q.Set("output_format_json_quote_decimals", "1")
	q.Set("output_format_json_quote_denormals", "1")
	q.Set("query_id", newQueryID())
	return q
}

func newQueryID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "dbtool-" + hex.EncodeToString(b[:])
}

// ---- 错误 ----

var (
	// ErrTimeout 表示查询超时（服务端 query.max_execution_seconds 或客户端超时）。ServerError 在错误码为 159 / 160 时 errors.Is 它。
	ErrTimeout = errors.New("ClickHouse 查询超时")
	// ErrCanceled 表示调用方取消了查询。
	ErrCanceled = errors.New("查询已取消")
)

// ServerError 是 ClickHouse 服务端返回的错误。
type ServerError struct {
	Status int    // HTTP 状态码
	Code   int    // ClickHouse 错误码（取 X-ClickHouse-Exception-Code 头或正文里的 Code: N），未知为 0
	Detail string // 已脱敏、截断的服务端原文
}

func (e *ServerError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("ClickHouse 错误 %d（%s）：%s", e.Code, codeHint(e.Code), e.Detail)
	}
	return fmt.Sprintf("ClickHouse 返回 HTTP %d：%s", e.Status, e.Detail)
}

// Is 让 errors.Is(err, ErrTimeout) 对服务端超时成立。
func (e *ServerError) Is(target error) bool {
	return target == ErrTimeout && (e.Code == 159 || e.Code == 160)
}

// codeHint 把常见错误码翻译成中文。
func codeHint(code int) string {
	switch code {
	case 516:
		return "认证失败，请检查账号密码"
	case 497:
		return "没有权限"
	case 164:
		return "只读模式不允许该操作；若是“Cannot modify ... setting in readonly mode”，说明账号 profile 本身是 readonly=1，请改为 readonly=2"
	case 60:
		return "表不存在"
	case 81:
		return "库不存在"
	case 47:
		return "未知的列或标识符"
	case 46:
		return "未知的函数"
	case 62:
		return "SQL 语法错误"
	case 159, 160:
		return "查询超时"
	case 158, 396:
		return "超过读取行数 / 字节上限，请加过滤条件或缩小范围"
	case 241:
		return "超过内存上限，请缩小范围"
	case 202, 203:
		return "服务端并发查询过多，请稍后重试"
	case 394:
		return "查询被取消"
	case 209, 210:
		return "网络错误"
	case 115:
		return "服务端不识别请求中的某个设置（版本过旧）"
	case 43, 53:
		return "类型不匹配"
	case 42:
		return "函数参数个数不对"
	case 36:
		return "参数不合法"
	case 48:
		return "服务端不支持该功能"
	case 395:
		return "throwIf 触发的错误"
	case 107:
		return "文件不存在"
	}
	return "服务端错误"
}

// IsTimeout 判断是不是超时类错误。
func IsTimeout(err error) bool {
	return errors.Is(err, ErrTimeout) || errors.Is(err, context.DeadlineExceeded)
}

// IsCanceled 判断是不是调用方取消。
func IsCanceled(err error) bool {
	return errors.Is(err, ErrCanceled) || errors.Is(err, context.Canceled)
}

const maxErrDetail = 600

var (
	reExcCode   = regexp.MustCompile(`(?s)^\s*Code:\s*(\d+)\.\s*`)
	reExcInBody = regexp.MustCompile(`Code:\s*(\d+)\.\s*DB::Exception`)
	reVersion   = regexp.MustCompile(`\s*\(version .*\)\s*$`)
	reURLCreds  = regexp.MustCompile(`(?i)(://)[^/\s:@]+:[^/\s@]+@`)
	rePassword  = regexp.MustCompile(`(?i)((?:password|passwd|pwd|secret|token)\s*[=:]\s*)('[^']*'|"[^"]*"|\S+)`)
	reIdentBy   = regexp.MustCompile(`(?i)(identified\s+(?:with\s+\w+\s+)?by\s+)('[^']*'|"[^"]*"|\S+)`)
	reBasicAuth = regexp.MustCompile(`(?i)(authorization:\s*basic\s+)\S+`)
)

// scrub 去掉文本里可能带的凭据（配置的密码本身、URL 里的 user:pass@、password=…、Basic 头），并限制长度。
func scrub(s string, secrets ...string) string {
	for _, sec := range secrets {
		if len(sec) >= 3 {
			s = strings.ReplaceAll(s, sec, "***")
		}
	}
	s = reURLCreds.ReplaceAllString(s, "${1}***@")
	s = rePassword.ReplaceAllString(s, "${1}***")
	s = reIdentBy.ReplaceAllString(s, "${1}***")
	s = reBasicAuth.ReplaceAllString(s, "${1}***")
	s = strings.ToValidUTF8(s, "?")
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > maxErrDetail {
		r := []rune(s)
		s = string(r[:maxErrDetail]) + "…"
	}
	return s
}

// serverError 解析非 200 响应（或 200 响应里夹带的异常文本）。
func (c *Client) serverError(status int, hdr http.Header, body []byte) *ServerError {
	text := string(body)
	code := 0
	if v := hdr.Get("X-ClickHouse-Exception-Code"); v != "" {
		code, _ = strconv.Atoi(v)
	}
	if m := reExcCode.FindStringSubmatch(text); m != nil {
		if code == 0 {
			code, _ = strconv.Atoi(m[1])
		}
		text = reExcCode.ReplaceAllString(text, "")
	} else if m := reExcInBody.FindStringSubmatch(text); m != nil && code == 0 {
		code, _ = strconv.Atoi(m[1])
	}
	text = reVersion.ReplaceAllString(strings.TrimSpace(text), "")
	text = strings.TrimPrefix(text, "DB::Exception: ")
	if text == "" {
		text = http.StatusText(status)
	}
	return &ServerError{Status: status, Code: code, Detail: scrub(text, c.cfg.Password, c.cfg.User+":"+c.cfg.Password)}
}

// netError 把传输层错误转成不含 URL / 凭据的可读错误。
func (c *Client) netError(callerCtx, reqCtx context.Context, err error) error {
	switch {
	case errors.Is(callerCtx.Err(), context.Canceled):
		return ErrCanceled
	case errors.Is(reqCtx.Err(), context.DeadlineExceeded) || errors.Is(callerCtx.Err(), context.DeadlineExceeded):
		return ErrTimeout
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err // 去掉 url.Error 里的请求地址（含查询串）
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("连接 ClickHouse（%s）超时", c.hostPort())
	}
	var ce *tls.CertificateVerificationError
	if errors.As(err, &ce) {
		return fmt.Errorf("ClickHouse（%s）的 TLS 证书校验失败：%s；可在实例设置里关闭“校验证书”", c.hostPort(), scrub(ce.Err.Error(), c.cfg.Password))
	}
	return fmt.Errorf("无法连接 ClickHouse HTTP 接口（%s）：%s", c.hostPort(), scrub(err.Error(), c.cfg.Password))
}

func (c *Client) hostPort() string { return net.JoinHostPort(c.cfg.Host, strconv.Itoa(c.cfg.port())) }

// ---- 请求 ----

// capReader 在读够 left 字节后返回 EOF 并记下“被截断”；同时保留最后一小段内容，用于在 JSON 解析失败时识别服务端异常文本。
type capReader struct {
	r    io.Reader
	left int64
	hit  bool
	tail []byte
}

const tailKeep = 4096

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		c.hit = true
		return 0, io.EOF
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if n > 0 {
		c.tail = append(c.tail, p[:n]...)
		if len(c.tail) > tailKeep {
			c.tail = c.tail[len(c.tail)-tailKeep:]
		}
	}
	return n, err
}

// do 发出请求。成功返回（200 状态的）响应，调用方负责关闭 Body 与调用 cancel。
func (c *Client) do(ctx context.Context, sql string, o reqOpts) (*http.Response, context.CancelFunc, error) {
	rctx, cancel := context.WithTimeout(ctx, o.Timeout+timeoutGrace)
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, c.base+"?"+c.values(o).Encode(), strings.NewReader(sql))
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("构造请求失败：%s", scrub(err.Error(), c.cfg.Password))
	}
	user := c.cfg.User
	if user == "" {
		user = "default"
	}
	req.SetBasicAuth(user, c.cfg.Password)
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("User-Agent", "dbtool")
	resp, err := c.hc.Do(req)
	if err != nil {
		cancel()
		return nil, nil, c.netError(ctx, rctx, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
		_ = resp.Body.Close()
		cancel()
		return nil, nil, c.serverError(resp.StatusCode, resp.Header, body)
	}
	return resp, cancel, nil
}
