// empty_text_part.go 出站前修复空 text 段（上游 400 code=11151: a message has empty content）。
//
// 只对空 text 段（text 为 "" / null / 缺失）补单空格 " "，不删除段落，也不改写非 text 类型段落。
// 对以 "[" 开头的合法 JSON 对象数组字符串，在原始字节区间上定向补位，避免重排键序、大整数精度损失或 HTML 转义。
package upstream

import (
	"encoding/json"
	"strings"
)

const emptyTextPartFill = " "

// guardEmptyTextParts 将 messages 中的空 text 段就地补位为单个空格。
func guardEmptyTextParts(messages []any) {
	if len(messages) == 0 {
		return
	}
	for _, rawMsg := range messages {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			continue
		}
		content, has := msg["content"]
		if !has {
			continue
		}
		switch v := content.(type) {
		case []any:
			parts := make([]any, len(v))
			copied := false
			for i, rawPart := range v {
				part, ok := rawPart.(map[string]any)
				if !ok || partType(part) != "text" || !isEmptyText(part) {
					parts[i] = rawPart
					continue
				}
				clone := make(map[string]any, len(part)+1)
				for k, val := range part {
					clone[k] = val
				}
				clone["text"] = emptyTextPartFill
				parts[i] = clone
				copied = true
			}
			if copied {
				msg["content"] = parts
			}
		case string:
			if s, ok := fillSniffedEmptyText(v); ok {
				msg["content"] = s
			}
		}
	}
}

func partType(part map[string]any) string {
	t, _ := part["type"].(string)
	return strings.TrimSpace(t)
}

func isEmptyText(part map[string]any) bool {
	val, exists := part["text"]
	if !exists || val == nil {
		return true
	}
	s, ok := val.(string)
	return ok && s == ""
}

type textSpan struct {
	start int
	end   int
	repl  string
}

// fillSniffedEmptyText 对去前导空白后以 "[" 开头且为非空 JSON 对象数组的字符串做字节级补位。
func fillSniffedEmptyText(content string) (string, bool) {
	stripped := strings.TrimLeft(content, " \t\r\n")
	lead := len(content) - len(stripped)
	if !strings.HasPrefix(stripped, "[") || !json.Valid([]byte(stripped)) {
		return content, false
	}
	elems, ok := scanArrayElements(stripped)
	if !ok || len(elems) == 0 {
		return content, false
	}
	var spans []textSpan
	for _, elem := range elems {
		raw := stripped[elem.start:elem.end]
		if len(raw) == 0 || raw[0] != '{' {
			return content, false
		}
		if sp, ok := findEmptyTextSpan(raw); ok {
			sp.start += elem.start
			sp.end += elem.start
			spans = append(spans, sp)
		}
	}
	if len(spans) == 0 {
		return content, false
	}
	var b strings.Builder
	b.Grow(len(content) + 16*len(spans))
	b.WriteString(content[:lead])
	prev := 0
	for _, sp := range spans {
		b.WriteString(stripped[prev:sp.start])
		b.WriteString(sp.repl)
		prev = sp.end
	}
	b.WriteString(stripped[prev:])
	return b.String(), true
}

type jsonRange struct {
	start int
	end   int
}

func skipJSONSpace(s string, i int) int {
	for i < len(s) {
		switch s[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return i
		}
	}
	return i
}

func scanJSONValueEnd(s string, i int) (int, bool) {
	if i >= len(s) {
		return 0, false
	}
	switch s[i] {
	case '"':
		i++
		for i < len(s) {
			if s[i] == '\\' {
				i += 2
				continue
			}
			if s[i] == '"' {
				return i + 1, true
			}
			i++
		}
		return 0, false
	case '{', '[':
		open := s[i]
		closeCh := byte('}')
		if open == '[' {
			closeCh = ']'
		}
		depth := 1
		i++
		for i < len(s) && depth > 0 {
			switch s[i] {
			case '"':
				end, ok := scanJSONValueEnd(s, i)
				if !ok {
					return 0, false
				}
				i = end
			case open:
				depth++
				i++
			case closeCh:
				depth--
				i++
			default:
				i++
			}
		}
		return i, depth == 0
	default:
		for i < len(s) {
			switch s[i] {
			case ' ', '\t', '\r', '\n', ',', ']', '}':
				return i, true
			default:
				i++
			}
		}
		return len(s), true
	}
}

func scanArrayElements(s string) ([]jsonRange, bool) {
	i := skipJSONSpace(s, 0)
	if i >= len(s) || s[i] != '[' {
		return nil, false
	}
	i = skipJSONSpace(s, i+1)
	if i < len(s) && s[i] == ']' {
		return nil, true
	}
	var out []jsonRange
	for i < len(s) {
		valStart := skipJSONSpace(s, i)
		valEnd, ok := scanJSONValueEnd(s, valStart)
		if !ok {
			return nil, false
		}
		out = append(out, jsonRange{start: valStart, end: valEnd})
		i = skipJSONSpace(s, valEnd)
		if i < len(s) && s[i] == ',' {
			i++
			continue
		}
		if i < len(s) && s[i] == ']' {
			return out, true
		}
		return nil, false
	}
	return nil, false
}

func findEmptyTextSpan(objStr string) (textSpan, bool) {
	i := skipJSONSpace(objStr, 0)
	if i >= len(objStr) || objStr[i] != '{' {
		return textSpan{}, false
	}
	i = skipJSONSpace(objStr, i+1)
	if i < len(objStr) && objStr[i] == '}' {
		return textSpan{}, false
	}

	isTypeText := false
	hasTextKey := false
	hasFields := false
	var emptySpan textSpan

	for i < len(objStr) {
		kStart := skipJSONSpace(objStr, i)
		if kStart >= len(objStr) || objStr[kStart] != '"' {
			return textSpan{}, false
		}
		kEnd, ok := scanJSONValueEnd(objStr, kStart)
		if !ok {
			return textSpan{}, false
		}
		var key string
		if err := json.Unmarshal([]byte(objStr[kStart:kEnd]), &key); err != nil {
			return textSpan{}, false
		}
		colon := skipJSONSpace(objStr, kEnd)
		if colon >= len(objStr) || objStr[colon] != ':' {
			return textSpan{}, false
		}
		vStart := skipJSONSpace(objStr, colon+1)
		vEnd, ok := scanJSONValueEnd(objStr, vStart)
		if !ok {
			return textSpan{}, false
		}
		hasFields = true
		valRaw := objStr[vStart:vEnd]

		switch key {
		case "type":
			var tv string
			if json.Unmarshal([]byte(valRaw), &tv) == nil && strings.TrimSpace(tv) == "text" {
				isTypeText = true
			}
		case "text":
			hasTextKey = true
			if valRaw == "null" {
				emptySpan = textSpan{start: vStart, end: vEnd, repl: `"` + emptyTextPartFill + `"`}
			} else if len(valRaw) > 0 && valRaw[0] == '"' {
				var sv string
				if json.Unmarshal([]byte(valRaw), &sv) == nil && sv == "" {
					emptySpan = textSpan{start: vStart, end: vEnd, repl: `"` + emptyTextPartFill + `"`}
				}
			}
		}

		next := skipJSONSpace(objStr, vEnd)
		if next < len(objStr) && objStr[next] == ',' {
			i = next + 1
			continue
		}
		if next < len(objStr) && objStr[next] == '}' {
			if !isTypeText {
				return textSpan{}, false
			}
			if emptySpan.repl != "" {
				return emptySpan, true
			}
			if !hasTextKey && hasFields {
				return textSpan{start: next, end: next, repl: `,"text":"` + emptyTextPartFill + `"`}, true
			}
			return textSpan{}, false
		}
		return textSpan{}, false
	}
	return textSpan{}, false
}
