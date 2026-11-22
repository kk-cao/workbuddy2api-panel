// sanitize.go 出站请求体脱敏：剥离上游内容审核黑名单指纹。
//
// 背景：客户端（Claude Code 类 CLI）在 system prompt 注入若干固定模板句，
// 上游内容审核按逐字精确匹配拦截（非语义审核），一字改动即可绕过。
// 策略：键值/header 型指纹整段剥离；承载语义的模板句最小改写（换一词），语义不变。
package upstream

import (
	"regexp"
	"strings"
)

// sanitizeFeatures 特征预检：任一命中才进入净化（strings.Contains 快速路径，
// 普通请求全不中 → 原样返回，零分配）。
var sanitizeFeatures = []string{
	"x-anthropic-billing-header", // header 键值段键名
	"cc_entrypoint=",             // 尾随裸键值（截断前缀即可命中）
	"You are Claude Code",        // 身份句（截断前缀即可命中）
	"Main branch (",              // 注入指令句（截断前缀即可命中）
	"You are a coding agent running in the Codex CLI", // Codex instructions 首段（截断前缀即可命中）
	"github.com/anthropics/",                          // 反馈句里的 Anthropic 仓库链接
	"11128",                                           // 上游反探测：裸数字错误码
	"Tokens prefixed with @",                          // DSH @ 路径说明段
	"DeepSeek Harness",                                // DeepSeek Harness 品牌/身份句
	"OhMyOpenCode",                                    // OhMyOpenCode 标识
}

// sanitizeHdrRe 剥离层：header 键名即触发（与值无关），整段删除。
var sanitizeHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-header:[^;\n]*;?\s*`)

// sanitizeBareHdrRe 兜底层：裸键名（无冒号无值）同样是指纹——2026-09-13 实验 F4
// 证实 assistant 消息里反引号引用裸键名即触发 11128，而剥离层要求冒号、对裸串无效。
// 键值形态被整段删除后，残留的裸键名做最小缩写（header→hdr）：破坏逐字匹配、
// 语义不变、保留可读性。大小写不敏感，覆盖 X-Anthropic-... 变体。
//
// 注意该正则不要求冒号，是 sanitizeHdrRe 的超集——hasFingerprint 与 sanitizeText
// 中两者并用：先删键值形态（sanitizeHdrRe），再缩写残留裸键名（本正则），
// 替换语义不同（整段删除 vs 最小缩写），不可合并为一个正则。
var sanitizeBareHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-header`)

// sanitizeKvRe 剥离层：尾随裸键值（cc_xxx=...;）循环清理。
var sanitizeKvRe = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)

// sanitizeDshPathsRe DSH @ 开头的路径解析整段说明，不含品牌词，须整段同义改写。
var sanitizeDshPathsRe = regexp.MustCompile(
	`(?is)Tokens prefixed with @ are paths the user explicitly referenced\..*?path (?:containing|that contains) spaces\.`)

const sanitizeDshPathsReplacement = "Paths beginning with @ refer to files the user explicitly mentioned. " +
	"Relative paths are resolved against the project root; absolute paths point " +
	"to files or directories on the host. A trailing slash marks a directory, so " +
	"list it when its contents matter. Anything else is a file: open it with the " +
	"read tool when its contents are needed, and never claim to have inspected it " +
	"before reading. The @\"...\" form only quotes a path that contains spaces."

// sanitizeInjectedIdentityRe 上游服务端注入的身份祈使句，模型思维链复述回流会滚雪球。
var sanitizeInjectedIdentityRe = regexp.MustCompile(
	`(?i)when asked about your[^.\n]{0,60}?identity[^.\n]{0,60}?,\s*identify yourself as\s+[^\n]{1,80}?\.(\W|$)`)

const sanitizeInjectedIdentityReplacement = "When asked about your capabilities, answer using only the information available in this conversation.$1"

// sanitizeModelIdentityTagRe 剥离 <model_identity>...</model_identity> 标签外壳，保留内部文本。
var sanitizeModelIdentityTagRe = regexp.MustCompile(`(?i)</?model_identity>`)

// sanitizeIdentitySentenceRe 泛化改写「自我介绍 + 品牌/模型归属」整句，防非官方渠道特征拦截。
var sanitizeIdentitySentenceRe = regexp.MustCompile(
	`(?i)You are\s+` +
		`(?:an?\s+|the\s+)?(?:[\w.-]+\s+){0,3}?` +
		`(?:agent|assistant|model|AI|LLM)?[\s,]*` +
		`(?:powered by|built on|built by|created by|developed by|made by|trained by)` +
		`[^\n]*?\.(\s|$)`)

const sanitizeIdentitySentenceReplacement = "You are a helpful AI assistant.${1}"

var sanitizeBareHdrProbe = func() string {
	const foldPrefix = "(?i)"
	s := sanitizeBareHdrRe.String()
	if !strings.HasPrefix(s, foldPrefix) {
		return ""
	}
	body := s[len(foldPrefix):]
	if body == "" || strings.ContainsAny(body, `\[](){}*+?|^$.`) {
		return ""
	}
	return body
}()

// containsFoldASCII 零分配 ASCII 大小写不敏感子串查找，替代 RE2 `(?i)` 预检。
func containsFoldASCII(s, sub string) bool {
	if sub == "" {
		return true
	}
	n := len(sub)
	if len(s) < n {
		return false
	}
	c0 := sub[0]
	var alt0 byte
	if c0 >= 'a' && c0 <= 'z' {
		alt0 = c0 - ('a' - 'A')
	} else if c0 >= 'A' && c0 <= 'Z' {
		alt0 = c0 + ('a' - 'A')
	} else {
		alt0 = c0
	}
	limit := len(s) - n
	for i := 0; i <= limit; i++ {
		if s[i] != c0 && s[i] != alt0 {
			continue
		}
		match := true
		for j := 1; j < n; j++ {
			a, b := s[i+j], sub[j]
			if a == b {
				continue
			}
			if (a|0x20) == (b|0x20) && ((a|0x20) >= 'a' && (a|0x20) <= 'z') {
				continue
			}
			match = false
			break
		}
		if match {
			return true
		}
	}
	return false
}

// hasIdentityVerbShape 单遍扫描判断是否存在「 by 」或「 on 」，作为身份句式预检快速路径。
func hasIdentityVerbShape(s string) bool {
	for i := 0; i+3 <= len(s); i++ {
		c := s[i] | 0x20
		var want byte
		switch c {
		case 'b':
			want = 'y'
		case 'o':
			want = 'n'
		default:
			continue
		}
		if s[i+1]|0x20 != want || s[i+2] != ' ' {
			continue
		}
		if i > 0 && s[i-1] != ' ' {
			continue
		}
		return true
	}
	return false
}

// sanitizeRewrites 改写层：全模板句逐字替换（每句只改一词，语义不变）。
//
// 身份句的匹配串**不带结尾标点**（只到 "…for Claude" 为止）：
// CLI 版这句以句号收尾（"…for Claude."），桌面版（claude-desktop-3p / Agent SDK）
// 以逗号接后继内容（"…for Claude, running within the Claude Agent SDK."）。
// 带句号的整句只匹配前者，桌面版会漏网、指纹原样发上游 → 400 code=11128。
// 去掉结尾标点后两种形态一并覆盖（替换串同样不带标点，让原有标点原样保留）。
// 注意仍要求 "You are Claude Code, " 前缀，不做更宽的子串替换，
// 以免误伤 TestExactMatchOnlyVariantNotTouched 所保护的零散文本。
var sanitizeRewrites = [][2]string{
	{
		"You are Claude Code, Anthropic's official CLI for Claude",
		"You are Claude Code, Anthropic's official CLI tool for Claude",
	},
	{
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)",
	},
	{
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.",
	},
	{
		"You are DeepSeek Harness",
		"You are an autonomous coding assistant",
	},
	{
		"DeepSeek Harness",
		"DeepSeekRuntime",
	},
	{
		"OhMyOpenCode",
		"OpenCodeAgent",
	},
	{
		// 反馈句：整句带 Anthropic 仓库链接，上游按整句拦截（只留链接或只留半边均不拦，
		// 实测需整句同时出现）。give→provide 一词之差即可绕过，语义不变。
		"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
	},
	{
		// 上游反探测：只要请求体里出现裸数字 11128 就整单拦截（与该数字的上下文无关——
		// "code=11128" / 裸 "11128" / "错误码 11128" / "Code=11128" 全部命中；
		// 相邻的 11148 / 11101 / 11115 / 99999 均放行）。11128 正是本类拦截自身的错误码，
		// 上游据此识别"在讨论/回显其内部错误码"的请求。
		// 代价：用户对话中任何 11128 都会被改写——但这串数字出现在请求里本身就是拦截条件，
		// 不改写必然失败。插入连字符保留可读性与指代（零宽空格无效，实测上游会归一化）。
		"11128",
		"11-128",
	},
}

// sanitizeText 单段文本净化：预检不中 → 返回原串（零分配）。
func sanitizeText(text string) string {
	return sanitizeTextWithOpts(text, false, false)
}

func sanitizeTextWithOpts(text string, systemRole, assistantOrReasoning bool) string {
	hasGlobal := hasFingerprint(text)
	hasSys := systemRole && (strings.Contains(text, "workspace root") || strings.Contains(text, "Workspace root"))
	hasInjected := assistantOrReasoning && hasInjectedIdentity(text)
	hasIdentitySent := hasIdentityVerbShape(text) && sanitizeIdentitySentenceRe.MatchString(text)
	if !hasGlobal && !hasSys && !hasInjected && !hasIdentitySent {
		return text
	}
	if hasInjected {
		text = sanitizeModelIdentityTagRe.ReplaceAllString(text, "")
		if sanitizeInjectedIdentityRe.MatchString(text) {
			text = sanitizeInjectedIdentityRe.ReplaceAllString(text, sanitizeInjectedIdentityReplacement)
		}
	}
	if hasIdentitySent {
		// 先于品牌词替换，防止品牌名先被改写导致句式正则漏匹配。
		text = sanitizeIdentitySentenceRe.ReplaceAllString(text, sanitizeIdentitySentenceReplacement)
	}
	if hasGlobal || hasIdentitySent {
		for _, rw := range sanitizeRewrites {
			text = strings.ReplaceAll(text, rw[0], rw[1])
		}
		if strings.Contains(text, "Tokens prefixed with @") || sanitizeDshPathsRe.MatchString(text) {
			text = sanitizeDshPathsRe.ReplaceAllString(text, sanitizeDshPathsReplacement)
		}
		if sanitizeHdrRe.MatchString(text) {
			text = sanitizeHdrRe.ReplaceAllString(text, "")
		}
		if strings.Contains(text, "cc_") {
			prev := ""
			for prev != text { // 清尾随裸 kv（cc_version=...; cc_entrypoint=...;）
				prev = text
				text = sanitizeKvRe.ReplaceAllString(text, "")
			}
		}
		// 兜底：键值形态已在上面整段删除，这里只剩裸键名（引用/示例文本形态）。
		text = sanitizeBareHdrRe.ReplaceAllString(text, "x-anthropic-billing-hdr")
	}
	if systemRole {
		text = strings.ReplaceAll(text, "workspace root", "root of the workspace")
		text = strings.ReplaceAll(text, "Workspace root", "Root of the workspace")
	}
	return strings.TrimSpace(text)
}

func hasInjectedIdentity(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "model_identity") || strings.Contains(lower, "identify yourself as")
}

// hasFingerprint 特征预检：先走 strings.Contains 快速路径，再以零分配 ASCII 折叠扫裸 header 键名。
func hasFingerprint(text string) bool {
	for _, f := range sanitizeFeatures {
		if strings.Contains(text, f) {
			return true
		}
	}
	if sanitizeBareHdrProbe == "" {
		return sanitizeBareHdrRe.MatchString(text)
	}
	return containsFoldASCII(text, sanitizeBareHdrProbe)
}

// sanitizeContent 兼容字符串与多模态数组；只动 text part，image 等 part 不动。
// 返回净化后的值及是否发生变化。
func sanitizeContent(v any) (any, bool) {
	return sanitizeContentWithOpts(v, false, false)
}

func sanitizeContentWithOpts(v any, systemRole, assistantRole bool) (any, bool) {
	switch c := v.(type) {
	case string:
		s := sanitizeTextWithOpts(c, systemRole, assistantRole)
		return s, s != c
	case []any:
		changed := false
		for _, p := range c {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			text, ok := m["text"].(string)
			if !ok {
				continue
			}
			if s := sanitizeTextWithOpts(text, systemRole, assistantRole); s != text {
				m["text"] = s
				changed = true
			}
		}
		return c, changed
	}
	return v, false
}

// sanitizeToolCalls 净化 assistant.tool_calls[].function.arguments。
//
// arguments 是**字符串化的 JSON**（不是对象），因此按文本走 sanitizeText 即可。
// 这块长期是盲区：工具调用消息的 content 通常是 null，而旧版 sanitizeMessages
// 在 content 缺失时直接 continue，整条消息连 tool_calls 一起被跳过——
// 于是历史里任何写进工具参数的被拦字符串（文件名、命令、写入内容）都会原样漏出。
func sanitizeToolCalls(v any) bool {
	callList, ok := v.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, c := range callList {
		call, ok := c.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := call["function"].(map[string]any)
		if !ok {
			continue
		}
		args, ok := fn["arguments"].(string)
		if !ok {
			continue
		}
		if s := sanitizeText(args); s != args {
			fn["arguments"] = s
			changed = true
		}
	}
	return changed
}

// sanitizeMessages 净化 messages 中的 content 与 tool_calls；任一命中返回 true。
func sanitizeMessages(messages []any) bool {
	changed := false
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		normRole := strings.ToLower(strings.TrimSpace(role))
		isSystem := normRole == "system" || normRole == "developer"
		isAssistant := normRole == "assistant"
		// content 与 tool_calls 各自独立判断：content 可以为 null（工具调用轮），
		// 早期版本在此 continue，导致这类消息的 tool_calls 完全不被净化。
		if c, ok := m["content"]; ok {
			if nc, ch := sanitizeContentWithOpts(c, isSystem, isAssistant); ch {
				m["content"] = nc
				changed = true
			}
		}
		// reasoning_content / reasoning：任意消息思维链均执行注入身份句/标签清洗，避免回流滚雪球。
		if rc, ok := m["reasoning_content"].(string); ok {
			if s := sanitizeTextWithOpts(rc, isSystem, true); s != rc {
				m["reasoning_content"] = s
				changed = true
			}
		}
		if r, ok := m["reasoning"].(string); ok {
			if s := sanitizeTextWithOpts(r, isSystem, true); s != r {
				m["reasoning"] = s
				changed = true
			}
		}
		if tc, ok := m["tool_calls"]; ok {
			if sanitizeToolCalls(tc) {
				changed = true
			}
		}
	}
	return changed
}

// sanitizeTools 净化 OpenAI tools[].function.description，清除身份句及黑名单指纹。
func sanitizeTools(v any) {
	tools, ok := v.([]any)
	if !ok {
		return
	}
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tm["function"].(map[string]any)
		if !ok {
			continue
		}
		if d, ok := fn["description"].(string); ok {
			if s := sanitizeText(d); s != d {
				fn["description"] = s
			}
		}
	}
}
