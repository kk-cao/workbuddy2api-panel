// sse_frame_split.go 单个 SSE data 帧里塞了多个首尾相接 JSON 对象时的拆分。
package upstream

import (
	"encoding/json"
	"strings"
)

const maxSplitParts = 64

func looksLikeJSON(payload string) bool {
	t := strings.TrimLeft(payload, " \t\r\n")
	if t == "" {
		return false
	}
	return t[0] == '{' || t[0] == '['
}

// SplitConcatenatedJSON 把首尾相接的多个 JSON 值切成独立字符串；切不动返回单元素保持原样透传。
func SplitConcatenatedJSON(payload string) []string {
	trimmed := strings.TrimSpace(payload)
	if trimmed == "" {
		return []string{payload}
	}
	var (
		parts   []string
		depth   int
		inStr   bool
		escaped bool
		start   = -1
	)
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		if start < 0 {
			if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
				continue
			}
			if c != '{' && c != '[' && c != '"' && c != '-' && (c < '0' || c > '9') &&
				c != 't' && c != 'f' && c != 'n' {
				return []string{payload}
			}
			start = i
		}
		if inStr {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inStr = false
				if depth == 0 {
					if part, ok := trimJSONValue(trimmed[start : i+1]); ok {
						parts = append(parts, part)
						if len(parts) >= maxSplitParts {
							return []string{payload}
						}
						start = -1
					} else {
						return []string{payload}
					}
				}
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth < 0 {
				return []string{payload}
			}
			if depth == 0 {
				if part, ok := trimJSONValue(trimmed[start : i+1]); ok {
					parts = append(parts, part)
					if len(parts) >= maxSplitParts {
						return []string{payload}
					}
					start = -1
				} else {
					return []string{payload}
				}
			}
		}
	}
	if start >= 0 || inStr || depth != 0 || len(parts) == 0 {
		return []string{payload}
	}
	return parts
}

func trimJSONValue(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" || !json.Valid([]byte(v)) {
		return "", false
	}
	return v, true
}
