// @author: kevinkelly
// @license: Apache Licence
// @file: redistest.go
// @time: 2026/10/28

// Package redistest 提供一个进程内的假 Redis 服务端（RESP2），供单元测试使用，不连任何真实 Redis。
package redistest

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
)

// Simple 是简单字符串回复（+OK）。
type Simple string

// Err 是错误回复（-ERR ...）。
type Err string

// Handler 处理一条命令，返回回复：Simple / Err / int64 / int / string（批量）/ nil（空批量）/ []any（数组）。
type Handler func(args []string) any

// Server 是假服务端。
type Server struct {
	ln   net.Listener
	h    Handler
	mu   sync.Mutex
	log  [][]string
	wg   sync.WaitGroup
	conn map[net.Conn]bool
}

// New 在本机随机端口启动假服务端。
func New(h Handler) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln, h: h, conn: map[net.Conn]bool{}}
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

// Host / Port 是监听地址。
func (s *Server) Host() string { return "127.0.0.1" }
func (s *Server) Port() int    { return s.ln.Addr().(*net.TCPAddr).Port }

// Commands 返回收到过的全部命令（大写命令名 + 原参数）。
func (s *Server) Commands() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]string, len(s.log))
	copy(out, s.log)
	return out
}

// Close 关闭监听与所有连接。
func (s *Server) Close() {
	_ = s.ln.Close()
	s.mu.Lock()
	for c := range s.conn {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conn[c] = true
		s.mu.Unlock()
		s.wg.Add(1)
		go s.serve(c)
	}
}

func (s *Server) serve(c net.Conn) {
	defer s.wg.Done()
	defer c.Close()
	r := bufio.NewReader(c)
	w := bufio.NewWriter(c)
	for {
		args, err := readCommand(r)
		if err != nil {
			return
		}
		s.mu.Lock()
		s.log = append(s.log, append([]string{}, args...))
		s.mu.Unlock()
		writeValue(w, s.h(args))
		if w.Flush() != nil {
			return
		}
	}
}

func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "*") {
		return nil, fmt.Errorf("bad command header %q", line)
	}
	n, err := strconv.Atoi(line[1:])
	if err != nil {
		return nil, err
	}
	args := make([]string, 0, n)
	for i := 0; i < n; i++ {
		hdr, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		hdr = strings.TrimRight(hdr, "\r\n")
		l, err := strconv.Atoi(strings.TrimPrefix(hdr, "$"))
		if err != nil {
			return nil, err
		}
		buf := make([]byte, l+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args = append(args, string(buf[:l]))
	}
	return args, nil
}

func writeValue(w *bufio.Writer, v any) {
	switch x := v.(type) {
	case nil:
		_, _ = w.WriteString("$-1\r\n")
	case Simple:
		fmt.Fprintf(w, "+%s\r\n", string(x))
	case Err:
		fmt.Fprintf(w, "-%s\r\n", string(x))
	case int:
		fmt.Fprintf(w, ":%d\r\n", x)
	case int64:
		fmt.Fprintf(w, ":%d\r\n", x)
	case string:
		fmt.Fprintf(w, "$%d\r\n%s\r\n", len(x), x)
	case []any:
		fmt.Fprintf(w, "*%d\r\n", len(x))
		for _, it := range x {
			writeValue(w, it)
		}
	case []string:
		fmt.Fprintf(w, "*%d\r\n", len(x))
		for _, it := range x {
			writeValue(w, it)
		}
	default:
		fmt.Fprintf(w, "-ERR unsupported reply type %T\r\n", v)
	}
}
