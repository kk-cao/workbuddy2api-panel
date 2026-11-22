// payload.go 改写发往上游的 chat 请求体：
//  1. 强制 stream:true（上游拒绝非流式）
//  2. tool_choice 归一化（上游该字段是 string，对象形式会 400 code=11101）
//  3. image_url 归一化（上游只认 OpenAI 对象形态，字符串会 400 code=11101）
package upstream

import (
	"encoding/json"
	"log"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var reuEscape = regexp.MustCompile(`\\u\{([0-9a-fA-F]{1,6})\}|\\u([0-9a-fA-F]{4})`)

// re2Pattern 把 JS 正则里 RE2 不认的 \uXXXX / \u{XXXX} 译成 \x{...} 并验证能否编译。
func re2Pattern(p string) (string, bool) {
	if !strings.Contains(p, `\u`) {
		return p, true
	}
	q := reuEscape.ReplaceAllStringFunc(p, func(m string) string {
		return `\x{` + strings.Trim(m[2:], "{}") + `}`
	})
	if _, err := regexp.Compile(q); err != nil {
		return "", false
	}
	return q, true
}

// SplitRealmPrefix 解析 "[cn:|global:]model" 路由前缀。
func SplitRealmPrefix(model string) (realm, bare string) {
	if i := strings.IndexByte(model, ':'); i >= 0 {
		switch model[:i] {
		case "cn", "global":
			return model[:i], model[i+1:]
		}
	}
	return "cn", model
}

// PrepareBodyOpt 单 pass 改写；sanitize=false 时行为完全还原（仅强制 stream + 归一化 tool_choice）。
func PrepareBodyOpt(src []byte, sanitize bool) []byte {
	return PrepareBodyOptWithEffortsAndDefault(src, sanitize, nil, nil)
}

// PrepareBodyOptWithEfforts 在 PrepareBodyOpt 基础上按模型 supportedEfforts 降级 reasoning_effort：
// 仅当请求显式携带且模型不支持该档位时，改为 ≤请求档位的最高支持档；支持档全部高于请求档时取最低档；
// 未知模型/未知档位/未携带该字段一律透传。efforts 为 nil 表示未知（不降级）。
//
// 向后兼容封装：不传 defaultEfforts（无模型声明默认档），thinking.go 回退硬编码 high。
func PrepareBodyOptWithEfforts(src []byte, sanitize bool, efforts map[string][]string) []byte {
	return PrepareBodyOptWithEffortsAndDefault(src, sanitize, efforts, nil)
}

// PrepareBodyOptWithEffortsAndDefault 完整管线：efforts 降级 + thinking.go 按
// defaultEfforts（模型声明默认档）补档。defaultEfforts 为 nil 时与旧行为一致
// （deepseek 缺档回退硬编码 high）。
func PrepareBodyOptWithEffortsAndDefault(src []byte, sanitize bool, efforts map[string][]string, defaultEfforts map[string]string) []byte {
	return PrepareBodyOptWithEffortsDefaultAndCaps(src, sanitize, efforts, defaultEfforts, nil)
}

// PrepareBodyOptWithEffortsDefaultAndCaps 在 …AndDefault 之上按目录声明 maxOutputTokens 补默认 max_tokens。
func PrepareBodyOptWithEffortsDefaultAndCaps(src []byte, sanitize bool, efforts map[string][]string, defaultEfforts map[string]string, maxOut map[string]int64) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	translateMaxCompletionTokens(obj)
	clampGPTMinMaxTokens(obj)
	injectCatalogMaxTokens(obj, maxOut)
	if _, has := obj["stream_options"]; !has {
		obj["stream_options"] = map[string]any{"include_usage": true}
	}
	normalizeToolChoice(obj)
	normalizeStop(obj)
	normalizeToolPatterns(obj)
	normalizeRoles(obj)
	normalizeImageURL(obj)
	dropReasoningContentParts(obj)
	if msgs, ok := obj["messages"].([]any); ok {
		msgs, _ = mergeAdjacentToolCalls(msgs)
		msgs, _ = reorderEarlyToolResults(msgs)
		msgs, _ = repackToolResultBlocks(msgs)
		msgs, _ = cleanupOrphanToolCalls(msgs)
		obj["messages"] = msgs
	}
	modelName, _ := obj["model"].(string)
	injectThinking(obj, lookupDefaultEffort(defaultEfforts, modelName))
	normalizeReasoningEffort(obj, efforts)
	backfillReasoningContent(obj)
	mirrorUpstreamReasoning(obj)
	if sanitize {
		if msgs, ok := obj["messages"].([]any); ok {
			sanitizeMessages(msgs)
		}
		sanitizeTools(obj["tools"])
	}
	if msgs, ok := obj["messages"].([]any); ok {
		guardEmptyTextParts(msgs)
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// translateMaxCompletionTokens 把 OpenAI 别名 max_completion_tokens 翻译为上游
// 认的 max_tokens（吸收上游 PR #116）。规则：显式 max_tokens 优先（别名只删）；
// 别名非正数值（0/null/负数）不翻译（0/null 语义是「未设置」，负数是非法值，
// 翻译等于把垃圾搬进 max_tokens）；非数值别名（字符串等畸形）不翻译（原样
// 透传由上游报 11101 参数错）。两域同口径：CN /v2 与 global /console 是同一套
// API，翻译不分 realm。
func translateMaxCompletionTokens(obj map[string]any) {
	alias, has := obj["max_completion_tokens"]
	delete(obj, "max_completion_tokens") // 无论翻译与否，别名一律删（减少 body 体积与排障噪音）
	if !has {
		return
	}
	if _, explicit := obj["max_tokens"]; explicit {
		return // 显式 max_tokens 优先：别名只删不译
	}
	// json.Unmarshal 数字 → float64（整数去整后回写，避免 1.28e5 科学计数法/小数
	// 尾巴进上游 body）；其他数值类型防御性兼容（int 家族——手构造 map 的调用方）。
	switch v := alias.(type) {
	case float64:
		if v > 0 && v == float64(int64(v)) {
			obj["max_tokens"] = int64(v)
		}
	case int64:
		if v > 0 {
			obj["max_tokens"] = v
		}
	case int:
		if v > 0 {
			obj["max_tokens"] = int64(v)
		}
	}
}

// gptMinMaxTokens GPT 系上游接受的 max_tokens 下限。
const gptMinMaxTokens = 16

// clampGPTMinMaxTokens 把 GPT 系模型过小的 max_tokens 抬到下限。
//
// 背景：上游 GPT 系（实测 gpt-6-sol / gpt-6-luna / gpt-5.6-sol）对 max_tokens < 16
// 一律 400 code=11133 model_param_invalid（15 拒、16 过，同号同 body 对照）；hy4 等
// 非 GPT 模型无此限制。Claude Code 切模型时发 max_tokens 极小的探针，全号轮转同样
// 被拒 → 客户端 503，模型永远切不过去。账号与 body 其余部分无关，换号无用，只能
// 在发送前修。抬到下限只放宽输出上限、不改语义；未携带字段 / 非数值 / 已达下限一律不动。
func clampGPTMinMaxTokens(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !strings.Contains(strings.ToLower(model), "gpt-") {
		return
	}
	var v int64
	switch n := obj["max_tokens"].(type) {
	case float64:
		v = int64(n)
	case int64:
		v = n
	case int:
		v = int64(n)
	default:
		return
	}
	if v < gptMinMaxTokens {
		obj["max_tokens"] = int64(gptMinMaxTokens)
		log.Printf("max_tokens clamped model=%s %d -> %d", model, v, gptMinMaxTokens)
	}
}

// injectCatalogMaxTokens 无显式输出限额时按目录 maxOutputTokens 补 max_tokens。
func injectCatalogMaxTokens(obj map[string]any, maxOut map[string]int64) {
	if len(maxOut) == 0 {
		return
	}
	if _, has := obj["max_tokens"]; has {
		return
	}
	model, _ := obj["model"].(string)
	if v, ok := maxOut[model]; ok && v > 0 {
		obj["max_tokens"] = v
		return
	}
	_, bare := SplitRealmPrefix(model)
	if v, ok := maxOut[bare]; ok && v > 0 {
		obj["max_tokens"] = v
	}
}

// normalizeStop 把字符串形态 stop 转为单元素 []string（不 TrimSpace）。
func normalizeStop(obj map[string]any) {
	s, ok := obj["stop"].(string)
	if !ok {
		return
	}
	obj["stop"] = []string{s}
}

// effortRank 档位从低到高。
var effortRank = map[string]int{"off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6}

// normalizeReasoningEffort 按模型 supportedEfforts 降级 reasoning_effort（snake/camel 双字段兼容）。
//   - 请求档位模型支持 → 原样透传
//   - 请求档位不支持 → 改为 ≤请求档位的最高支持档（降级）
//   - 支持档全部高于请求档 → 取最低支持档（偏离最小）
//   - 未知模型/未知档位/未携带字段/模型未缓存 → 一律透传
func normalizeReasoningEffort(obj map[string]any, efforts map[string][]string) {
	if len(efforts) == 0 {
		return
	}
	model, _ := obj["model"].(string)
	if model == "" {
		return
	}
	supported, ok := efforts[model]
	if !ok || len(supported) == 0 {
		return
	}
	key := ""
	if _, present := obj["reasoning_effort"]; present {
		key = "reasoning_effort"
	} else if _, present := obj["reasoningEffort"]; present {
		key = "reasoningEffort"
	} else {
		return
	}
	reqStr, ok := obj[key].(string)
	if !ok {
		return
	}
	reqStr = strings.TrimSpace(strings.ToLower(reqStr))
	reqIdx, known := effortRank[reqStr]
	if !known {
		return
	}
	// 在 ≤请求档位的支持档里选最高档；命中且与请求不同才改写。
	best, bestIdx := "", -1
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx <= reqIdx && idx > bestIdx {
			best, bestIdx = s, idx
		}
	}
	if best != "" {
		if !strings.EqualFold(best, reqStr) {
			obj[key] = best
			log.Printf("reasoning_effort downgraded model=%s %s -> %s", model, reqStr, best)
		}
		return
	}
	// 支持档全部高于请求档：取最低支持档。
	lowest, lowestIdx := "", 1<<30
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx < lowestIdx {
			lowest, lowestIdx = s, idx
		}
	}
	if lowest != "" {
		obj[key] = lowest
		log.Printf("reasoning_effort floored model=%s %s -> %s", model, reqStr, lowest)
	}
}

// normalizeRoles 把 messages 里的 developer 角色归一为 system。
//
// 背景：上游对 messages 的 role 字段做白名单校验，developer 不在白名单内，
// 命中即 HTTP 400 code=11128。developer 是 OpenAI 新规范里 system 的别名
// （Codex / Cursor 等新客户端用它承载 system 级指令），改写为 system 不丢语义。
//
// 此归一化是「协议兼容」（补上游 role 白名单），不是「内容脱敏」，
// 因此有意与 SanitizeFingerprints / sanitize 参数解耦：即使 sanitize=false 也照常归一。
//
// 只认 developer 这一个值：其余 role（system/user/assistant/tool/任意未知值）一律原样保留，
// 不合并、不重排、不删除任何消息（上游对多 system 的行为尚未实测，合并会引入新变量）。
func normalizeRoles(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for i, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, ok := msg["role"].(string)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			msg["role"] = "system"
			log.Printf("role normalized developer->system idx=%d", i)
		}
	}
}

// normalizeImageURL 兼容 OpenAI chat 多模态内容的两种 image_url 写法。
//
// OpenAI Chat Completions 规范使用对象形态 {"url":"...","detail":"..."}，
// 部分客户端（以及 Responses -> Chat 转换器）会发送字符串形态 "data:..." 或
// "https://..."。WorkBuddy 上游只接受对象形态，字符串会返回 400 code=11101
// "cannot unmarshal string into ... ImageContent"。
//
// 这里只做形状转换：字符串转 {"url": 原值}；已有对象及其中 url/detail/mime_type
// 原样保留；空字符串、缺失值、对象内非法 url 一律不补默认值，让上游返回真实错误。
func normalizeImageURL(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, rawMsg := range msgs {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok || part["type"] != "image_url" {
				continue
			}
			imageURL, ok := part["image_url"].(string)
			if !ok || imageURL == "" {
				continue
			}
			part["image_url"] = map[string]any{"url": imageURL}
		}
	}
}

// dropReasoningContentParts 剔除 chat content 数组里的 reasoning_text / reasoning 片段。
//
// Codex 只走 /v1/responses：new-api 的 responses→chat 转换器
// （openai_responses_to_openai_chat_completions）会把助手历史里的思考片段原样透传进
// /v1/chat/completions。上游 content 类型白名单不含 reasoning_text，命中即 HTTP 400
// code=11101 "unsupported content type at index 0: reasoning_text"。
//
// 思考片段只是模型私有推理，下游续写不需要，剔除无副作用：
//   - 删除 type 为 reasoning_text / reasoning 的片段（TrimSpace 后比较）
//   - 若删除后 content 为空且该消息无 tool_calls，整条消息一并删除
func dropReasoningContentParts(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	out := make([]any, 0, len(msgs))
	for _, rawMsg := range msgs {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			out = append(out, rawMsg)
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			out = append(out, rawMsg)
			continue
		}
		kept := make([]any, 0, len(parts))
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				kept = append(kept, rawPart)
				continue
			}
			ptype, _ := part["type"].(string)
			if strings.TrimSpace(ptype) == "reasoning_text" || strings.TrimSpace(ptype) == "reasoning" {
				continue
			}
			kept = append(kept, rawPart)
		}
		if len(kept) == 0 {
			if _, hasToolCalls := msg["tool_calls"]; !hasToolCalls {
				continue
			}
		}
		msg["content"] = kept
		out = append(out, rawMsg)
	}
	obj["messages"] = out
}

// ensureConsoleSystem global realm 兜底 system 注入（吸收 PR #45，防 console 域上游 code 11-128）：
// 首条消息非 system 时在 messages 最前补一条 fallback system（"You are a helpful assistant."）。
// 仅对 global 请求调用（CN 现状不动；即使首条就是 system 也不重复注入）。
// body 不可解析时原样返回（与 prepareBody 语义一致：坏 body 不在这里二次错误化）。
func ensureConsoleSystem(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body
	}
	first, ok := msgs[0].(map[string]any)
	if ok {
		if role, _ := first["role"].(string); strings.EqualFold(strings.TrimSpace(role), "system") {
			return body // 首条已是 system：不注入
		}
	}
	obj["messages"] = append([]any{map[string]any{"role": "system", "content": "You are a helpful assistant."}}, msgs...)
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// normalizeToolChoice 按上游 Go struct（string 类型）改写 OpenAI tool_choice。
//   - "none"            → 删 tool_choice + 删 tools/functions
//   - {"type":"none"}   → 同上
//   - {"type":"auto"/"required"} → 字符串 "auto"/"required"
//   - {"type":"function","function":{"name":"x"}} → 字符串 "x"
//   - 其他对象/非标量 → 删 tool_choice
func normalizeToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

// normalizeToolPatterns 归一化 tools 子树里 pattern 的非标准转义 `\_`（→ `_`）。
//
// 上游对 tools[].function.parameters 做严格 JSON Schema/正则文法校验，pattern 含
// `\_`（转义的字面量下划线）会整体拒收：400 code=11129 invalid_function_call_
// parameters（displayMsg「工具定义不合规」）。`\_` 不是任何正则文法的合法转义，
// 但所有主流引擎（RE2/PCRE/JS Annex B）都宽容地视为 `_` 本身——上游校验器比它们
// 全部更严（对照 V8 严格文法 u 标志，唯一同样拒绝的实现）。实案：ZCode 的 exa 插件
// agent_run 工具 runId/previousRunId 带 `^agent\_run\_`，deepseek 系全家确定性 400
// → 网关侧归 ErrClient 只换号不罚但喂连败计数 → 轮转烧满 5 连败触发连败降权、
// 客户端 503（2026-09-29/30 两次实案）。schema 级拒绝换账号无用，只能在发送前修。
//
// 归一无损：`\_` 与 `_` 在所有引擎匹配语义相同（各引擎实测 + 上游对照探针：归一后
// 200），工具方功能不变。只动 tools 子树（pattern 值 + patternProperties 键）；
// 消息正文里的 `\_`（如 Windows 路径 C:\_x）不碰。其余非标转义（`\:` 等）未证实
// 触发，不扩面——有实案再议。独立于 sanitize 开关：这是「让请求通过」，不是脱敏。
func normalizeToolPatterns(obj map[string]any) {
	rawTools, ok := obj["tools"].([]any)
	if !ok {
		return
	}
	for _, raw := range rawTools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if fn, ok := tool["function"].(map[string]any); ok {
			fn["parameters"] = normalizeToolParameters(fn["parameters"])
		}
		if _, ok := tool["parameters"]; ok {
			tool["parameters"] = normalizeToolParameters(tool["parameters"])
		}
	}
}

// normalizeToolParameters 归一化工具顶层参数定义并递归规整深层 JSON Schema 树，防 11129 拒收。
func normalizeToolParameters(node any) any {
	if node == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	params, ok := node.(map[string]any)
	if !ok {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	if ref, ok := params["$ref"].(string); ok {
		delete(params, "$ref")
		if defs, ok := params["$defs"].(map[string]any); ok && strings.HasPrefix(ref, "#/$defs/") {
			if t, ok := resolveTopSchemaRef(map[string]any{"$ref": ref}, defs, 0, map[string]bool{}).(map[string]any); ok {
				for k, v := range t {
					if _, dup := params[k]; !dup {
						params[k] = v
					}
				}
			}
		}
	}
	t, ok := params["type"].(string)
	if !ok || strings.TrimSpace(t) == "" {
		params["type"] = "object"
	} else {
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "str":
			params["type"] = "string"
		case "int":
			params["type"] = "integer"
		case "float":
			params["type"] = "number"
		case "bool":
			params["type"] = "boolean"
		case "dict":
			params["type"] = "object"
		case "list":
			params["type"] = "array"
		}
	}
	if params["type"] == "object" {
		if _, hasProps := params["properties"].(map[string]any); !hasProps {
			params["properties"] = map[string]any{}
		}
	}
	normalizeSchemaTree(params)
	return params
}

func resolveTopSchemaRef(v any, defs map[string]any, depth int, visited map[string]bool) any {
	if depth > 16 {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	ref, hasRef := m["$ref"].(string)
	if !hasRef || !strings.HasPrefix(ref, "#/$defs/") {
		return m
	}
	name := strings.TrimPrefix(ref, "#/$defs/")
	if visited[name] {
		return nil
	}
	target, ok := defs[name]
	if !ok {
		return nil
	}
	nextVisited := make(map[string]bool, len(visited)+1)
	for k, val := range visited {
		nextVisited[k] = val
	}
	nextVisited[name] = true
	return resolveTopSchemaRef(target, defs, depth+1, nextVisited)
}

var schemaContainerKeys = map[string]bool{
	"properties":        true,
	"patternProperties": true,
	"$defs":             true,
	"definitions":       true,
	"dependentSchemas":  true,
	"dependentRequired": true,
	"dependencies":      true,
}

var schemaDataKeys = map[string]bool{
	"default":  true,
	"const":    true,
	"enum":     true,
	"example":  true,
	"examples": true,
}

var (
	schemaNumberKeys = []string{
		"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf",
		"minLength", "maxLength", "minItems", "maxItems", "minProperties", "maxProperties",
	}
	schemaBoolKeys   = []string{"uniqueItems", "deprecated", "readOnly", "writeOnly"}
	schemaStringKeys = []string{"description", "title", "format", "pattern"}
)

func coerceSchemaKeywords(n map[string]any) {
	for _, k := range schemaNumberKeys {
		if v, ok := n[k]; ok {
			if num, ok := schemaNumber(v); ok {
				n[k] = num
			} else {
				delete(n, k)
			}
		}
	}
	for _, k := range schemaBoolKeys {
		if v, ok := n[k]; ok {
			if b, ok := schemaBool(v); ok {
				n[k] = b
			} else {
				delete(n, k)
			}
		}
	}
	for _, k := range schemaStringKeys {
		if v, ok := n[k]; ok {
			if _, isStr := v.(string); !isStr {
				delete(n, k)
			}
		}
	}
	if v, ok := n["additionalProperties"]; ok {
		if _, isSchema := v.(map[string]any); !isSchema {
			if b, ok := schemaBool(v); ok {
				n["additionalProperties"] = b
			} else {
				delete(n, "additionalProperties")
			}
		}
	}
	for _, k := range []string{"enum", "examples"} {
		if v, ok := n[k]; ok {
			if _, isList := v.([]any); !isList {
				delete(n, k)
			}
		}
	}
}

func schemaNumber(v any) (any, bool) {
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, false
		}
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, false
		}
		return f, true
	}
	return nil, false
}

func schemaBool(v any) (bool, bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}

func normalizeSchemaTree(node any) {
	switch n := node.(type) {
	case map[string]any:
		coerceSchemaKeywords(n)
		for k, v := range n {
			if v == nil && !schemaDataKeys[k] {
				delete(n, k)
			}
		}
		if t, ok := n["type"].(string); ok {
			switch strings.ToLower(strings.TrimSpace(t)) {
			case "str":
				n["type"] = "string"
			case "int":
				n["type"] = "integer"
			case "float":
				n["type"] = "number"
			case "bool":
				n["type"] = "boolean"
			case "dict":
				n["type"] = "object"
			case "list":
				n["type"] = "array"
			case "":
				if _, hasProps := n["properties"]; hasProps {
					n["type"] = "object"
				}
			}
		}
		if req, hasReq := n["required"]; hasReq {
			if reqList, ok := req.([]any); ok {
				var cleaned []any
				seen := make(map[string]bool, len(reqList))
				for _, item := range reqList {
					if s, ok := item.(string); ok {
						s = strings.TrimSpace(s)
						if s != "" && !seen[s] {
							seen[s] = true
							cleaned = append(cleaned, s)
						}
					}
				}
				if len(cleaned) == 0 {
					delete(n, "required")
				} else {
					n["required"] = cleaned
				}
			} else {
				delete(n, "required")
			}
		}
		if items, hasItems := n["items"]; hasItems {
			if items == nil {
				delete(n, "items")
			} else if itemList, ok := items.([]any); ok {
				var firstDict map[string]any
				for _, it := range itemList {
					if d, ok := it.(map[string]any); ok {
						firstDict = d
						break
					}
				}
				if firstDict != nil {
					n["items"] = firstDict
				} else {
					n["items"] = map[string]any{"type": "string"}
				}
			}
		}
		if p, ok := n["pattern"].(string); ok {
			if strings.Contains(p, `\_`) {
				p = strings.ReplaceAll(p, `\_`, `_`)
			}
			if fixed, compiled := re2Pattern(p); compiled {
				n["pattern"] = fixed
			} else {
				delete(n, "pattern")
			}
		}
		if props, ok := n["patternProperties"].(map[string]any); ok {
			rebuilt := false
			fixed := make(map[string]any, len(props))
			for k, v := range props {
				if strings.Contains(k, `\_`) {
					k = strings.ReplaceAll(k, `\_`, `_`)
					rebuilt = true
				}
				fixed[k] = v
			}
			if rebuilt {
				n["patternProperties"] = fixed
			}
		}
		if _, hasType := n["type"]; !hasType {
			if e, ok := n["enum"].([]any); ok && len(e) > 0 {
				if t := inferEnumType(e); t != "" {
					n["type"] = t
				}
			}
		}
		for k, v := range n {
			if schemaDataKeys[k] {
				continue
			}
			if schemaContainerKeys[k] {
				if c, ok := v.(map[string]any); ok {
					for childKey, child := range c {
						if child == nil {
							delete(c, childKey)
							continue
						}
						normalizeSchemaTree(child)
					}
				}
				continue
			}
			normalizeSchemaTree(v)
		}
	case []any:
		for _, v := range n {
			normalizeSchemaTree(v)
		}
	}
}

func inferEnumType(values []any) string {
	var strs, nums, ints, bools int
	for _, v := range values {
		switch x := v.(type) {
		case string:
			strs++
		case bool:
			bools++
		case float64:
			nums++
			if x == math.Trunc(x) {
				ints++
			}
		default:
			return ""
		}
	}
	n := len(values)
	switch {
	case strs == n:
		return "string"
	case bools == n:
		return "boolean"
	case nums == n && ints == n:
		return "integer"
	case nums == n:
		return "number"
	}
	return ""
}
