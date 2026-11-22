package upstream

import (
	"bytes"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func splitFixture(t *testing.T, name string) (string, []string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读夹具 %s: %v", name, err)
	}
	var first string
	var rest []string
	for i, ln := range strings.Split(string(raw), "\n") {
		ln = strings.TrimRight(ln, "\r")
		if ln == "" {
			continue
		}
		if i == 0 {
			first = ln
			continue
		}
		rest = append(rest, ln)
	}
	return first, rest
}

func shuffleAndCanonicalizeHeaders(h []string) []string {
	out := make([]string, len(h))
	for i := range h {
		raw := h[len(h)-1-i]
		if colon := strings.IndexByte(raw, ':'); colon > 0 {
			raw = http.CanonicalHeaderKey(raw[:colon]) + raw[colon:]
		}
		out[i] = raw
	}
	return out
}

func buildBlock(first string, headers []string) []byte {
	var b bytes.Buffer
	b.WriteString(first + "\r\n")
	for _, h := range headers {
		b.WriteString(h + "\r\n")
	}
	b.WriteString("\r\n")
	return b.Bytes()
}

// TestReorderHeadBlockMatchesOfficial 验证 Go Canonical 乱序输入经 wire 还原后与官方夹具逐字节一致。
func TestReorderHeadBlockMatchesOfficial(t *testing.T) {
	for _, f := range []string{
		"official_chat_head.txt",
		"official_report_cn_head.txt",
		"official_chat_busy_head.txt",
		"official_v3config_head.txt",
	} {
		t.Run(strings.TrimSuffix(strings.TrimPrefix(f, "official_"), "_head.txt"), func(t *testing.T) {
			first, want := splitFixture(t, f)
			got := reorderHeadBlock(buildBlock(first, shuffleAndCanonicalizeHeaders(want)))
			wantBlock := buildBlock(first, want)
			if string(got) != string(wantBlock) {
				t.Errorf("重排与拼写还原结果与官方不一致\n--- got ---\n%s\n--- want ---\n%s", got, wantBlock)
			}
		})
	}
}

func TestReorderHeadBlockUnknownPathUntouchedOrderRestoresCase(t *testing.T) {
	in := buildBlock("GET /v9/nothing HTTP/1.1", []string{"Zeta: 1", "Alpha: 2"})
	if got := reorderHeadBlock(in); string(got) != string(in) {
		t.Errorf("未知路径和未知头不该改写:\n%s", got)
	}
	inCase := buildBlock("POST /v2/plugin/auth/token/refresh HTTP/1.1", []string{"X-Request-Id: r1", "Z-Custom: z"})
	wantCase := buildBlock("POST /v2/plugin/auth/token/refresh HTTP/1.1", []string{"X-Request-ID: r1", "Z-Custom: z"})
	if got := reorderHeadBlock(inCase); string(got) != string(wantCase) {
		t.Errorf("未知路径应还原已知头拼写且保持原序:\ngot:\n%swant:\n%s", got, wantCase)
	}
}

func TestReorderHeadBlockExtraHeadersAppended(t *testing.T) {
	first, headers := splitFixture(t, "official_v3config_head.txt")
	withExtra := append([]string{"X-Brand-New: 1", "X-Another: 2"}, headers...)
	shuffled := shuffleAndCanonicalizeHeaders(withExtra)
	got := string(reorderHeadBlock(buildBlock(first, shuffled)))

	if tail := got[len(got)-len("X-Brand-New: 1\r\n\r\n"):]; tail != "X-Brand-New: 1\r\n\r\n" {
		t.Errorf("表外头应追加在末尾，尾部=%q\n全文:\n%s", tail, got)
	}
	if !strings.Contains(got, "X-Another: 2\r\nX-Brand-New: 1") {
		t.Errorf("表外头相对顺序应保留:\n%s", got)
	}
	if !strings.HasPrefix(got, "GET /v3/config HTTP/1.1\r\nAccept: ") {
		t.Errorf("表内头顺序被破坏:\n%s", got)
	}
}

func TestReorderHeadBlockMalformed(t *testing.T) {
	cases := []string{"", "POST /v2/chat/completions HTTP/1.1\r\n", "garbage", "\r\n\r\n"}
	for _, in := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("输入 %q panic: %v", in, r)
				}
			}()
			_ = reorderHeadBlock([]byte(in))
		}()
	}
}

func newTestPipe(t *testing.T) (*headOrderConn, net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	return &headOrderConn{Conn: a}, b
}

func TestHeadOrderConnWriteReordersAndSplitAcrossCalls(t *testing.T) {
	c, peer := newTestPipe(t)
	first, headers := splitFixture(t, "official_v3config_head.txt")
	in := buildBlock(first, shuffleAndCanonicalizeHeaders(headers))
	cut := len(in) / 2

	var wg sync.WaitGroup
	wg.Add(1)
	var got []byte
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		n, _ := peer.Read(buf)
		got = buf[:n]
	}()
	if _, err := c.Write(in[:cut]); err != nil {
		t.Fatalf("write1: %v", err)
	}
	if _, err := c.Write(in[cut:]); err != nil {
		t.Fatalf("write2: %v", err)
	}
	wg.Wait()
	if !strings.HasPrefix(string(got), "GET /v3/config HTTP/1.1\r\nAccept: ") {
		t.Errorf("分块写未重排:\n%s", got)
	}
}

func TestHeadOrderConnKeepAliveSecondRequest(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	conn := newHeadOrderConn(a)

	done := make(chan []byte, 1)
	go func() {
		var collected bytes.Buffer
		buf := make([]byte, 4096)
		// 读请求1并写 chunked 响应1
		n, _ := b.Read(buf)
		collected.Write(buf[:n])
		_, _ = b.Write([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2\r\nok\r\n0\r\n\r\n"))
		// 读请求2
		n2, _ := b.Read(buf)
		collected.Write(buf[:n2])
		done <- collected.Bytes()
	}()

	req1 := buildBlock("GET /v3/config HTTP/1.1", []string{"Host: www.workbuddy.cn", "Accept: */*"})
	if _, err := conn.Write(req1); err != nil {
		t.Fatal(err)
	}
	rbuf := make([]byte, 256)
	if _, err := conn.Read(rbuf); err != nil {
		t.Fatal(err)
	}
	req2 := buildBlock("POST /v2/chat/busy HTTP/1.1", []string{"X-Request-Id: r2", "Accept: application/json"})
	if _, err := conn.Write(req2); err != nil {
		t.Fatal(err)
	}
	wire := string(<-done)
	if !strings.Contains(wire, "X-Request-ID: r2") {
		t.Fatalf("keep-alive 第二条请求未还原拼写:\n%s", wire)
	}
}

func TestHeadOrderConnChunkedSplitAcrossReads(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []string
	}{
		{
			name:   "chunk size split across reads",
			chunks: []string{"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n1", "a\r\n0123456789abcdef0123456789\r\n0\r\n\r\n"},
		},
		{
			name:   "chunk size before CRLF split",
			chunks: []string{"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2", "\r\nok\r\n0\r\n\r\n"},
		},
		{
			name:   "chunk CRLF split across reads",
			chunks: []string{"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2\r\nok\r", "\n0\r\n\r\n"},
		},
		{
			name:   "trailer and empty terminator split",
			chunks: []string{"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2\r\nok\r\n0\r\nX-Tr", "ail: 1\r\n\r", "\n"},
		},
		{
			name:   "100 continue then 204 no content",
			chunks: []string{"HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 204 No Content\r\nX-Foo: 1\r\n\r\n"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			conn := newHeadOrderConn(a)

			done := make(chan string, 1)
			go func() {
				buf := make([]byte, 4096)
				// 读请求1
				_, _ = b.Read(buf)
				for _, ch := range tc.chunks {
					_, _ = b.Write([]byte(ch))
				}
				// 读请求2（完整读到 \r\n\r\n 后退出）
				var req2 bytes.Buffer
				for !bytes.Contains(req2.Bytes(), []byte("\r\n\r\n")) {
					n, err := b.Read(buf)
					if n > 0 {
						req2.Write(buf[:n])
					}
					if err != nil {
						break
					}
				}
				done <- req2.String()
			}()

			req1 := buildBlock("GET /v3/config HTTP/1.1", []string{"Host: www.workbuddy.cn", "Accept: */*"})
			if _, err := conn.Write(req1); err != nil {
				t.Fatal(err)
			}
			rbuf := make([]byte, 4096)
			// 读完整个响应（直到对端所有 chunks 写入并消费完毕；通过短轮询 Read 到全串）
			var gotResp strings.Builder
			wantResp := strings.Join(tc.chunks, "")
			for gotResp.Len() < len(wantResp) {
				n, err := conn.Read(rbuf)
				if n > 0 {
					gotResp.Write(rbuf[:n])
				}
				if err != nil {
					t.Fatal(err)
				}
			}

			req2 := buildBlock("POST /v2/chat HTTP/1.1", []string{"X-Request-Id: r2", "Accept: application/json"})
			if _, err := conn.Write(req2); err != nil {
				t.Fatal(err)
			}
			wire := <-done
			if !strings.Contains(wire, "X-Request-ID: r2") {
				t.Fatalf("第二条请求未还原拼写:\n%s", wire)
			}
		})
	}
}

func TestHeadOrderConnEarlyResponseDoesNotCorruptRemainingBody(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	conn := newHeadOrderConn(a)

	bodyPart1 := `{"hello":`
	// 特意在剩余请求体中夹带 \r\n\r\n 和不含冒号的行，验证转透传后原封不动到达对端。
	bodyPart2 := "\r\n\r\nRAW_LINE_NO_COLON}"
	reqHead := buildBlock("POST /v2/chat HTTP/1.1", []string{
		"Host: www.workbuddy.cn",
		"Content-Length: " + strconv.Itoa(len(bodyPart1)+len(bodyPart2)),
		"Accept: application/json",
	})

	wireCh := make(chan string, 1)
	go func() {
		var collected bytes.Buffer
		buf := make([]byte, 4096)
		// 读请求头 + bodyPart1
		for !bytes.Contains(collected.Bytes(), []byte(bodyPart1)) {
			n, _ := b.Read(buf)
			if n > 0 {
				collected.Write(buf[:n])
			}
		}
		// 客户端尚未写完 bodyPart2，服务端提前返回完整 413 响应。
		_, _ = b.Write([]byte("HTTP/1.1 413 Payload Too Large\r\nContent-Length: 3\r\n\r\nerr"))
		// 继读客户端剩余请求体 bodyPart2
		for !bytes.Contains(collected.Bytes(), []byte(bodyPart2)) {
			n, err := b.Read(buf)
			if n > 0 {
				collected.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		wireCh <- collected.String()
	}()

	if _, err := conn.Write(append(reqHead, []byte(bodyPart1)...)); err != nil {
		t.Fatal(err)
	}
	rbuf := make([]byte, 256)
	if _, err := conn.Read(rbuf); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte(bodyPart2)); err != nil {
		t.Fatal(err)
	}

	got := <-wireCh
	if !strings.Contains(got, bodyPart2) {
		t.Fatalf("提前回包后剩余请求体被吞或篡改:\n%q", got)
	}
}
