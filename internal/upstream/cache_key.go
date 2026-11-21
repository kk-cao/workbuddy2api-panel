// cache_key.go 注入上游 prompt_cache_key 字段（P0 费用优化）。
//
// 逆向实测（buddy-adapter.ts:706-714）：上游服务端支持 prompt_cache_key，
// 同一段 8k token 前缀：
//   - 不带 → prompt_cache_hit_tokens=0, credit≈0.34
//   - 带   → prompt_cache_hit_tokens=7808, credit≈0.02（费用降 ~17×）
//
// 网关在此为每个出站 chat 请求注入一个稳定、按账号隔离的 cache key，
// 让同一客户端对同一账号的连续请求复用上游前缀缓存。
package upstream

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
)

// InjectPromptCacheKey 在已改写的出站 body 上注入 prompt_cache_key 字段。
//
// 优先级：
//  1. body 已带 prompt_cache_key → 原值保留（客户端自知复用哪个键）
//  2. body 已带 conversation_id / conversationId → 用它做会话哈希源
//  3. 都没有 → 用入站 conversationID 参数（来自 X-Conversation-ID 头解析）
//
// 安全约束——按账号隔离：
//   - 生成键格式 `wb2a-<uid8>-<convHex>`
//   - uid8 是账号 UID 前 8 字符，跨账号绝不相同 → 跨账号缓存键绝不碰撞
//   - 跨账号复用同一 cache key 会让上游命中错账号的前缀缓存、泄露对方对话，故 uid 是硬隔离因子
//
// 入参 uid 为账号 UID（空则用 "-"，但仍会注入键；调用方应保证传真实 UID）。
// 入参 conversationID 为网关解析出的会话标识（body 里没有 conversation_id 时用它做哈希源）。
// 两源都空时 convHex 为定值（每次新会话不复用前缀，但仍保留账号隔离段）。
// body 不可解析时原样返回（与 prepareBody 语义一致：坏 body 不二次错误化）。
func InjectPromptCacheKey(body []byte, uid, conversationID string) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	// 优先级 1：客户端已显式带 key → 绝不覆盖。
	if existing, ok := obj["prompt_cache_key"].(string); ok && existing != "" {
		return body
	}
	// 优先级 2：body 里的 conversation_id / conversationId 做会话哈希源。
	conv := conversationID
	if v := strField(obj, "conversation_id"); v != "" {
		conv = v
	} else if v := strField(obj, "conversationId"); v != "" {
		conv = v
	}
	key := buildCacheKey(uid, conv)
	obj["prompt_cache_key"] = key
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// cacheKeySource prompt_cache_key 的会话段来源：客户端声明的会话（ConversationID）优先，
// 否则用网关派生的粘性会话键（CacheKeyID，见 ChatMeta）。
func cacheKeySource(meta ChatMeta) string {
	if meta.ConversationID != "" {
		return meta.ConversationID
	}
	return meta.CacheKeyID
}

// buildCacheKey 生成与官方客户端同形的会话标识：标准 RFC 4122 UUID v4（36 字符）。
// 剥除旧版 `wb2a-<uid8>-` 网关指纹前缀与明文 uid8，保留 uid 哈希隔离与同会话稳定槽。
func buildCacheKey(uid, conversation string) string {
	return buildStableUUID(uid, conversation, "")
}

// buildStableUUID 由 (uid, conversation, purpose) 稳定派生 RFC 4122 UUIDv4。
func buildStableUUID(uid, conversation, purpose string) string {
	in := uid + "|" + conversation
	if purpose != "" {
		in += "|" + purpose
	}
	sum := sha256.Sum256([]byte(in))
	var u [16]byte
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x40 // version 4
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// buildTimeOrderedUUID 由 (uid, conversation, purpose) 稳定派生 UUIDv7（48 bit 确定性伪时间戳
// + 版本 7 + RFC 4122 变体位），用于需要与真机 01xxxxxx-...-7xxx 同形的 X-Conversation-ID。
func buildTimeOrderedUUID(uid, conversation, purpose string) string {
	in := uid + "|" + conversation
	if purpose != "" {
		in += "|" + purpose
	}
	sum := sha256.Sum256([]byte(in))
	const (
		anchorMS = int64(1767225600000)                 // 2026-01-01T00:00:00Z
		windowMS = int64(24 * 30 * 24 * 60 * 60 * 1000) // ~24 个月窗口
	)
	ms := uint64(anchorMS + int64(binary.BigEndian.Uint64(sum[8:16])%uint64(windowMS)))
	var u [16]byte
	u[0] = byte(ms >> 40)
	u[1] = byte(ms >> 32)
	u[2] = byte(ms >> 24)
	u[3] = byte(ms >> 16)
	u[4] = byte(ms >> 8)
	u[5] = byte(ms)
	copy(u[6:], sum[:10])
	u[6] = (u[6] & 0x0f) | 0x70 // version 7
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// strField 从 map 取 string 字段，非 string 或空串返回 ""。
func strField(obj map[string]any, key string) string {
	v, ok := obj[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}
