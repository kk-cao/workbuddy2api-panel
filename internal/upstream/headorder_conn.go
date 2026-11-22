// headorder_conn.go 在明文 HTTP/1.1 传输层重排请求头块与还原拼写，失败安全透传。
package upstream

import (
	"bytes"
	"net"
	"strconv"
	"strings"
	"sync"
)

type connPhase int

const (
	phaseAwaitRequest connPhase = iota
	phaseReqBody
	phaseReqDone
	phasePassthrough
)

// maxPendingHead 请求/响应头累积上限（64KB）：超过或无法定位头边界时转安全透传。
const maxPendingHead = 64 * 1024

type headOrderConn struct {
	net.Conn

	mu      sync.Mutex
	phase   connPhase
	pending []byte
	reqBody bodyTracker

	respMu sync.Mutex
	resp   respTracker
}

func newHeadOrderConn(c net.Conn) net.Conn {
	return &headOrderConn{Conn: c}
}

func (c *headOrderConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch c.phase {
	case phasePassthrough:
		if len(c.pending) > 0 {
			out := append(c.pending, p...)
			c.pending = nil
			if _, err := c.Conn.Write(out); err != nil {
				return 0, err
			}
			return len(p), nil
		}
		return c.Conn.Write(p)

	case phaseReqBody:
		done := c.reqBody.feed(p)
		switch {
		case c.reqBody.dead:
			c.phase = phasePassthrough
		case done:
			// 先标记 ReqDone 再写底层 socket，防止最后一帧刚发出就收到对端快速回包产生竞态。
			c.phase = phaseReqDone
		}
		n, err := c.Conn.Write(p)
		if err != nil {
			c.phase = phasePassthrough
		}
		return n, err

	case phaseReqDone:
		c.phase = phasePassthrough
		return c.Conn.Write(p)
	}

	// phaseAwaitRequest
	c.pending = append(c.pending, p...)
	i := bytes.Index(c.pending, []byte("\r\n\r\n"))
	if i < 0 {
		if len(c.pending) > maxPendingHead {
			out := c.pending
			c.pending = nil
			c.phase = phasePassthrough
			if _, err := c.Conn.Write(out); err != nil {
				return 0, err
			}
		}
		return len(p), nil
	}

	head := c.pending[:i+4]
	rest := c.pending[i+4:]
	c.pending = nil

	tracker, ok := parseReqBodyTracker(head[:i])
	if !ok {
		c.phase = phasePassthrough
	} else {
		c.reqBody = tracker
		done := len(rest) == 0 && tracker.complete()
		if !done && len(rest) > 0 {
			done = c.reqBody.feed(rest)
		}
		switch {
		case c.reqBody.dead:
			c.phase = phasePassthrough
		case done:
			c.phase = phaseReqDone
		default:
			c.phase = phaseReqBody
		}
	}

	out := reorderHeadBlock(head)
	if _, err := c.Conn.Write(out); err != nil {
		c.phase = phasePassthrough
		return 0, err
	}
	if len(rest) > 0 {
		if _, err := c.Conn.Write(rest); err != nil {
			c.phase = phasePassthrough
			return 0, err
		}
	}
	return len(p), nil
}

func (c *headOrderConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.respMu.Lock()
		done, failed := c.resp.feed(p[:n])
		if done {
			c.resp = respTracker{}
		}
		c.respMu.Unlock()

		if failed || done {
			earlyOrFailed := failed
			c.mu.Lock()
			if !failed && c.phase == phaseReqDone {
				c.phase = phaseAwaitRequest
				c.pending = nil
				c.reqBody = bodyTracker{}
			} else {
				// 协议出错，或服务端在客户端写完请求体前提前回了最终响应：不可再复位重排。
				c.phase = phasePassthrough
				earlyOrFailed = true
			}
			c.mu.Unlock()
			if earlyOrFailed {
				c.respMu.Lock()
				c.resp.dead = true
				c.respMu.Unlock()
			}
		}
	}
	return n, err
}

func parseReqBodyTracker(headNoCRLF []byte) (bodyTracker, bool) {
	lines := bytes.Split(headNoCRLF, []byte("\r\n"))
	clen, hasCL, chunked, ok := parseFramingHeaders(lines[1:])
	if !ok {
		return bodyTracker{}, false
	}
	if chunked {
		return bodyTracker{chunked: true}, true
	}
	if hasCL {
		return bodyTracker{remain: clen}, true
	}
	return bodyTracker{remain: 0}, true
}

func parseFramingHeaders(lines [][]byte) (clen int64, hasCL, chunked, ok bool) {
	clen = -1
	for _, ln := range lines {
		l := string(ln)
		low := strings.ToLower(l)
		switch {
		case strings.HasPrefix(low, "content-length:"):
			v := parseInt64(strings.TrimSpace(l[len("content-length:"):]))
			if v < 0 {
				return 0, false, false, false
			}
			if hasCL && clen != v {
				return 0, false, false, false
			}
			clen = v
			hasCL = true
		case strings.HasPrefix(low, "transfer-encoding:") && strings.Contains(low, "chunked"):
			chunked = true
		}
	}
	return clen, hasCL, chunked, true
}

type respTracker struct {
	started bool
	inHead  bool
	headBuf []byte
	body    bodyTracker
	dead    bool
}

// feed 解析响应片段，返回 (done, failed)：
// done=true 表示一条完整最终响应结束（1xx 临时响应继续留在头部阶段等待最终响应）。
func (t *respTracker) feed(b []byte) (done, failed bool) {
	for len(b) > 0 {
		if t.dead {
			return false, true
		}
		if !t.started {
			t.started = true
			t.inHead = true
		}
		if !t.inHead {
			finished := t.body.feed(b)
			if t.body.dead {
				t.dead = true
				return false, true
			}
			return finished, false
		}

		t.headBuf = append(t.headBuf, b...)
		i := bytes.Index(t.headBuf, []byte("\r\n\r\n"))
		if i < 0 {
			if len(t.headBuf) > maxPendingHead {
				t.dead = true
				return false, true
			}
			return false, false
		}
		headBytes := t.headBuf[:i]
		over := append([]byte(nil), t.headBuf[i+4:]...)
		t.headBuf = nil

		lines := bytes.Split(headBytes, []byte("\r\n"))
		code, ok := parseHTTPStatusCode(lines[0])
		if !ok {
			t.dead = true
			return false, true
		}
		// 1xx 临时响应无 body，也不代表最终响应结束：留在头状态继续读取后续响应。
		if code >= 100 && code <= 199 {
			t.inHead = true
			b = over
			continue
		}

		t.inHead = false
		if code == 204 || code == 304 {
			t.body = bodyTracker{remain: 0}
			return true, false
		}

		clen, hasCL, chunked, ok := parseFramingHeaders(lines[1:])
		if !ok {
			t.dead = true
			return false, true
		}
		switch {
		case chunked:
			t.body = bodyTracker{chunked: true}
		case hasCL:
			t.body = bodyTracker{remain: clen}
		default:
			// HTTP/1.1 无 chunked 也无 Content-Length 只能读到 EOF 关闭，不支持连接复用。
			t.dead = true
			return false, true
		}
		if len(over) == 0 {
			return t.body.complete(), false
		}
		b = over
	}
	return false, false
}

func parseHTTPStatusCode(statusLine []byte) (int, bool) {
	s := string(statusLine)
	if !strings.HasPrefix(s, "HTTP/") {
		return 0, false
	}
	sp := strings.IndexByte(s, ' ')
	if sp < 0 || sp+4 > len(s) {
		return 0, false
	}
	codeStr := s[sp+1 : sp+4]
	code, err := strconv.Atoi(codeStr)
	if err != nil || code < 100 || code > 999 {
		return 0, false
	}
	return code, true
}

// bodyTracker 通用 HTTP/1.1 请求体与响应体边界追踪器（定长 / chunked）。
type bodyTracker struct {
	chunked  bool
	remain   int64
	chunkRem int64
	chunkSt  int    // 0: size 行, 1: 数据块, 2: 块后 \r\n, 3: trailer 区
	crlfRem  int    // 块后 \r\n 跨包剩余待校验字节数
	lineBuf  []byte // 跨包缓冲未见 \r\n 的不完整行
	dead     bool
}

func (t *bodyTracker) complete() bool {
	return !t.chunked && !t.dead && t.remain == 0
}

func (t *bodyTracker) feed(b []byte) bool {
	if t.dead {
		return false
	}
	if !t.chunked {
		if int64(len(b)) >= t.remain {
			t.remain = 0
			return true
		}
		t.remain -= int64(len(b))
		return false
	}
	i := 0
	for i < len(b) {
		switch t.chunkSt {
		case 0:
			line, ok, tooLong := t.consumeLine(b, &i)
			if tooLong {
				t.dead = true
				return false
			}
			if !ok {
				return false
			}
			if k := strings.IndexByte(line, ';'); k >= 0 {
				line = line[:k]
			}
			n := parseInt64Hex(strings.TrimSpace(line))
			if n < 0 {
				t.dead = true
				return false
			}
			if n == 0 {
				t.chunkSt = 3
				continue
			}
			t.chunkRem = n
			t.chunkSt = 1
		case 1:
			if int64(len(b)-i) < t.chunkRem {
				t.chunkRem -= int64(len(b) - i)
				return false
			}
			i += int(t.chunkRem)
			t.chunkRem = 0
			t.chunkSt = 2
			t.crlfRem = 2
		case 2:
			for t.crlfRem > 0 && i < len(b) {
				want := byte('\r')
				if t.crlfRem == 1 {
					want = '\n'
				}
				if b[i] != want {
					t.dead = true
					return false
				}
				i++
				t.crlfRem--
			}
			if t.crlfRem > 0 {
				return false
			}
			t.chunkSt = 0
		case 3:
			line, ok, tooLong := t.consumeLine(b, &i)
			if tooLong {
				t.dead = true
				return false
			}
			if !ok {
				return false
			}
			if line == "" {
				return true
			}
		}
	}
	return false
}

// consumeLine 从 b[*i:] 读取以 \r\n 结尾的一行，跨包不完整时暂存进 t.lineBuf。
func (t *bodyTracker) consumeLine(b []byte, i *int) (line string, complete, tooLong bool) {
	if len(t.lineBuf) == 0 {
		j := bytes.Index(b[*i:], []byte("\r\n"))
		if j >= 0 {
			if j > maxPendingHead {
				return "", false, true
			}
			line = string(b[*i : *i+j])
			*i += j + 2
			return line, true, false
		}
		if len(b)-*i > maxPendingHead {
			return "", false, true
		}
		t.lineBuf = append(t.lineBuf, b[*i:]...)
		*i = len(b)
		return "", false, false
	}

	for *i < len(b) {
		t.lineBuf = append(t.lineBuf, b[*i])
		*i++
		if n := len(t.lineBuf); n >= 2 && t.lineBuf[n-2] == '\r' && t.lineBuf[n-1] == '\n' {
			line = string(t.lineBuf[:n-2])
			t.lineBuf = t.lineBuf[:0]
			return line, true, false
		}
		if len(t.lineBuf) > maxPendingHead {
			return "", false, true
		}
	}
	return "", false, false
}

func parseInt64(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

func parseInt64Hex(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 16, 64)
	if err != nil || n < 0 {
		return -1
	}
	return n
}
