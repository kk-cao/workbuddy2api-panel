// headorder.go wire 级出站请求头拼写与官方发送顺序还原。
package upstream

import (
	"bytes"
	"strings"
)

var officialHeadOrder = map[string][]string{
	"/v2/chat/completions": {
		"Accept",
		"Content-Type",
		"X-Requested-With",
		"x-stainless-arch",
		"x-stainless-lang",
		"x-stainless-os",
		"x-stainless-package-version",
		"x-stainless-retry-count",
		"x-stainless-runtime",
		"x-stainless-runtime-version",
		"X-Conversation-ID",
		"X-Agent-Purpose",
		"X-IDE-Type",
		"X-IDE-Name",
		"X-IDE-Version",
		"X-Agent-Intent",
		"X-Agent-Runtime",
		"X-Client-Version",
		"X-Client-Platform",
		"X-Client-Product",
		"X-Private-Data",
		"acp-connection-id",
		"x-codebuddy-request",
		"X-Request-ID",
		"X-Conversation-Message-ID",
		"X-Conversation-Request-ID",
		"X-Root-Request-ID",
		"X-Agent-Type",
		"baggage",
		"Content-Encoding",
		"traceparent",
		"b3",
		"X-B3-TraceId",
		"X-B3-ParentSpanId",
		"X-B3-SpanId",
		"X-B3-Sampled",
		"X-Trace-ID",
		"X-Product",
		"User-Agent",
		"Authorization",
		"X-User-Id",
		"X-Domain",
		"Content-Length",
		"Accept-Encoding",
		"Host",
		"Connection",
	},
	"/v2/report": {
		"Accept",
		"Content-Type",
		"X-Request-ID",
		"X-Client-Version",
		"X-Client-Platform",
		"X-Client-Product",
		"X-Product",
		"User-Agent",
		"Authorization",
		"X-User-Id",
		"X-Domain",
		"Content-Length",
		"Accept-Encoding",
		"Host",
		"Connection",
	},
	"/v2/chat/busy": {
		"Accept",
		"Content-Type",
		"User-Agent",
		"X-Request-ID",
		"X-Client-Version",
		"X-Client-Platform",
		"X-Client-Product",
		"X-Product",
		"Authorization",
		"X-User-Id",
		"X-Domain",
		"Content-Length",
		"Host",
		"Connection",
	},
	"/v3/config": {
		"Accept",
		"Connection",
		"X-Client-Version",
		"X-Client-Platform",
		"X-Client-Product",
		"Authorization",
		"X-User-Id",
		"X-Domain",
		"X-Product",
		"User-Agent",
		"X-Request-ID",
		"Host",
	},
}

func headRank(path string) (map[string]int, bool) {
	order, ok := officialHeadOrder[path]
	if !ok {
		return nil, false
	}
	rank := make(map[string]int, len(order))
	for i, k := range order {
		rank[strings.ToLower(k)] = i
	}
	return rank, true
}

func requestPathOf(line string) string {
	sp1 := bytes.IndexByte([]byte(line), ' ')
	if sp1 <= 0 {
		return ""
	}
	rest := line[sp1+1:]
	sp2 := strings.IndexByte(rest, ' ')
	if sp2 < 0 {
		return ""
	}
	target := rest[:sp2]
	if i := strings.IndexByte(target, '?'); i >= 0 {
		target = target[:i]
	}
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		if i := strings.Index(target[8:], "/"); i >= 0 {
			target = target[8+i:]
		}
	}
	return target
}

// reorderHeadBlock 在 wire 层还原官方请求头拼写并（对已知路径）按官方发送顺序重排：
//   - 所有路径的每个 Header 名先经过 officialHeaderName 还原准确大小写；
//   - 若请求路径命中 officialHeadOrder，再按官方顺序稳定重排（表外头保持相对顺序追加在末尾）；
//   - 未命中顺序表且无需修改拼写时直接返回原切片。
func reorderHeadBlock(block []byte) []byte {
	if len(block) == 0 || !bytes.HasSuffix(block, []byte("\r\n\r\n")) {
		return block
	}
	body := block[:len(block)-4]
	lines := strings.Split(string(body), "\r\n")
	if len(lines) < 2 {
		return block
	}
	path := requestPathOf(lines[0])
	rank, hasOrder := headRank(path)

	type entry struct {
		name string
		raw  string
	}
	entries := make([]entry, 0, len(lines)-1)
	caseChanged := false
	for _, ln := range lines[1:] {
		if ln == "" {
			continue
		}
		colon := strings.IndexByte(ln, ':')
		if colon <= 0 {
			continue
		}
		name := ln[:colon]
		wantName := officialHeaderName(name)
		if wantName != name {
			ln = wantName + ln[colon:]
			name = wantName
			caseChanged = true
		}
		entries = append(entries, entry{name: name, raw: ln})
	}
	if !hasOrder && !caseChanged {
		return block
	}

	ordered := entries
	if hasOrder {
		ordered = make([]entry, 0, len(entries))
		used := make([]bool, len(entries))
		for want := 0; want < len(rank); want++ {
			for i, e := range entries {
				if used[i] {
					continue
				}
				if r, ok := rank[strings.ToLower(e.name)]; ok && r == want {
					ordered = append(ordered, e)
					used[i] = true
				}
			}
		}
		for i, e := range entries {
			if !used[i] {
				ordered = append(ordered, e)
			}
		}
	}

	var out bytes.Buffer
	out.Grow(len(block))
	out.WriteString(lines[0])
	out.WriteString("\r\n")
	for _, e := range ordered {
		out.WriteString(e.raw)
		out.WriteString("\r\n")
	}
	out.WriteString("\r\n")
	return out.Bytes()
}
