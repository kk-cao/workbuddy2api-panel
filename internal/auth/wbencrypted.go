// wbencrypted.go 解密新版桌面端 auth 文件里的 $wbEncrypted 字段，并在回写时重新封装。
package auth

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// wbAtRestSecretKey 桌面端 at-rest 静态密钥（canonical base64，44 位）。
const wbAtRestSecretKey = "Sik9U5aXhCdwTVEwsEySDOmDoB9r9ntFxHF1fst9LQI="

// decryptWBEncrypted 递归解出 doc 里所有 $wbEncrypted 字段并重新 marshal。
// 无加密字段时返回原字节；sealed 记录原本是信封的路径，doc 为完整解密文档。
func decryptWBEncrypted(raw []byte) (out []byte, sealed map[string]bool, doc map[string]any, err error) {
	if !bytes.Contains(raw, []byte(`"$wbEncrypted"`)) {
		return raw, nil, nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return raw, nil, nil, nil
	}
	key := sha256.Sum256([]byte(wbAtRestSecretKey))
	sealed = map[string]bool{}
	v, changed, err := wbDecryptValue(doc, key, "", sealed)
	if err != nil || !changed {
		return raw, nil, nil, err
	}
	out, err = json.Marshal(v)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("at_rest_marshal_failed: %w", err)
	}
	return out, sealed, doc, nil
}

// wbDecryptValue 深度遍历并就地替换 $wbEncrypted 信封，同时登记信封路径。
func wbDecryptValue(v any, key [32]byte, prefix string, sealed map[string]bool) (any, bool, error) {
	switch t := v.(type) {
	case map[string]any:
		if enc, ok := t["$wbEncrypted"].(json.Number); ok && enc == "1" {
			if env, ok := t["envelope"].(string); ok && env != "" {
				plain, err := wbDecryptEnvelope(env, key)
				if err != nil {
					return nil, false, err
				}
				sealed[prefix] = true
				return plain, true, nil
			}
		}
		changed := false
		for k, item := range t {
			nv, c, err := wbDecryptValue(item, key, wbJoinPath(prefix, k), sealed)
			if err != nil {
				return nil, false, err
			}
			if c {
				t[k] = nv
				changed = true
			}
		}
		return t, changed, nil
	case []any:
		changed := false
		for i, item := range t {
			nv, c, err := wbDecryptValue(item, key, fmt.Sprintf("%s[%d]", prefix, i), sealed)
			if err != nil {
				return nil, false, err
			}
			if c {
				t[i] = nv
				changed = true
			}
		}
		return t, changed, nil
	}
	return v, false, nil
}

func wbJoinPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// sealForSave 只重新封装读入时是信封的字段，并强制封装 accessToken 与 refreshToken。
// 任一字段封装失败立即返回错误，防止以明文覆盖加密凭据文件。
func sealForSave(doc map[string]any, sealed map[string]bool) error {
	if err := sealWalk(doc, "", sealed); err != nil {
		return err
	}
	creds := doc
	if a, ok := doc["auth"].(map[string]any); ok {
		creds = a
	}
	for _, k := range []string{"accessToken", "refreshToken"} {
		if err := sealField(creds, k); err != nil {
			return err
		}
		if v := creds[k]; v != nil && !isWBEnvelope(v) {
			return fmt.Errorf("at_rest_seal_failed: %s 未能封装，拒绝以明文落盘", k)
		}
	}
	return nil
}

func sealWalk(v any, prefix string, sealed map[string]bool) error {
	switch t := v.(type) {
	case map[string]any:
		if isWBEnvelope(t) {
			return nil
		}
		for k, item := range t {
			p := wbJoinPath(prefix, k)
			if sealed[p] {
				if err := sealField(t, k); err != nil {
					return err
				}
				continue
			}
			if err := sealWalk(item, p, sealed); err != nil {
				return err
			}
		}
	case []any:
		for i, item := range t {
			p := fmt.Sprintf("%s[%d]", prefix, i)
			if sealed[p] {
				env, err := sealValue(item)
				if err != nil {
					return err
				}
				t[i] = env
				continue
			}
			if err := sealWalk(item, p, sealed); err != nil {
				return err
			}
		}
	}
	return nil
}

func sealField(parent map[string]any, k string) error {
	v, ok := parent[k]
	if !ok {
		return nil
	}
	env, err := sealValue(v)
	if err != nil {
		return err
	}
	parent[k] = env
	return nil
}

func sealValue(v any) (any, error) {
	if v == nil || isWBEnvelope(v) {
		return v, nil
	}
	return sealWBValue(v)
}

func isWBEnvelope(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	_, ok = m["$wbEncrypted"]
	return ok
}

func wbDecryptEnvelope(envelopeB64 string, key [32]byte) (any, error) {
	rawEnv, err := base64.StdEncoding.DecodeString(envelopeB64)
	if err != nil {
		return nil, fmt.Errorf("at_rest_bad_envelope: %w", err)
	}
	var env struct {
		Suite      uint32 `json:"suite"`
		KeyID      string `json:"keyId"`
		Nonce      string `json:"nonce"`
		AuthTag    string `json:"authTag"`
		Ciphertext string `json:"ciphertext"`
	}
	if err := json.Unmarshal(rawEnv, &env); err != nil {
		return nil, fmt.Errorf("at_rest_bad_envelope: %w", err)
	}
	wantID := wbKeyID(key)
	if env.KeyID != wantID {
		return nil, fmt.Errorf("at_rest_key_mismatch: envelope keyId=%s want %s"+
			"（桌面端换了 at-rest 密钥，需重新逆向 loggerGet）", env.KeyID, wantID)
	}
	nonce, tag, ct, err := b64Triple(env.Nonce, env.AuthTag, env.Ciphertext)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("at_rest_cipher_failed: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("at_rest_gcm_failed: %w", err)
	}
	if len(nonce) != gcm.NonceSize() || len(tag) != gcm.Overhead() {
		return nil, fmt.Errorf("at_rest_bad_envelope: invalid nonce (%d) or authTag (%d) size", len(nonce), len(tag))
	}
	plain, err := gcm.Open(nil, nonce, append(ct, tag...), wbFieldAAD(env.Suite, env.KeyID))
	if err != nil {
		return nil, fmt.Errorf("at_rest_decrypt_failed: %w", err)
	}
	return wbDecodePlain(plain), nil
}

func wbKeyID(key [32]byte) string {
	h := sha256.Sum256(key[:])
	return hex.EncodeToString(h[:])[:16]
}

func wbFieldAAD(suite uint32, keyID string) []byte {
	var b bytes.Buffer
	b.WriteString("WB-AAD\x00")
	b.WriteByte(1)
	wbWriteLP(&b, "WBEV1")
	wbWriteLP(&b, "sym-v1")
	var u [4]byte
	binary.BigEndian.PutUint32(u[:], suite)
	b.Write(u[:])
	wbWriteLP(&b, keyID)
	b.WriteByte(2)
	b.WriteByte(0)
	b.WriteByte(0)
	return b.Bytes()
}

func wbWriteLP(b *bytes.Buffer, s string) {
	var u [4]byte
	binary.BigEndian.PutUint32(u[:], uint32(len(s)))
	b.Write(u[:])
	b.WriteString(s)
}

func b64Triple(nonce, tag, ct string) (n, t, c []byte, err error) {
	for _, f := range []struct {
		name string
		in   string
		out  *[]byte
	}{{"nonce", nonce, &n}, {"authTag", tag, &t}, {"ciphertext", ct, &c}} {
		v, e := base64.StdEncoding.DecodeString(f.in)
		if e != nil {
			return nil, nil, nil, fmt.Errorf("at_rest_bad_envelope: %s: %w", f.name, e)
		}
		*f.out = v
	}
	return n, t, c, nil
}

func wbDecodePlain(plain []byte) any {
	var v any
	if json.Unmarshal(plain, &v) != nil {
		return string(plain)
	}
	v = wbUnwrapBuffer(v)
	if s, ok := v.(string); ok {
		var again any
		if json.Unmarshal([]byte(s), &again) == nil {
			switch again.(type) {
			case map[string]any, []any:
				return wbUnwrapBuffer(again)
			}
		}
	}
	return v
}

func sealWBValue(v any) (map[string]any, error) {
	text, ok := v.(string)
	if ok && json.Valid([]byte(text)) {
		b, err := json.Marshal(wbBuffer(text))
		if err != nil {
			return nil, fmt.Errorf("at_rest_seal_failed: %w", err)
		}
		text = string(b)
	} else if !ok {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("at_rest_seal_failed: %w", err)
		}
		text = string(b)
	}
	key := sha256.Sum256([]byte(wbAtRestSecretKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("at_rest_cipher_failed: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("at_rest_gcm_failed: %w", err)
	}
	keyID := wbKeyID(key)
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("at_rest_seal_failed: %w", err)
	}
	ct := gcm.Seal(nil, nonce, []byte(text), wbFieldAAD(1, keyID))
	env, err := json.Marshal(map[string]any{
		"suite":      1,
		"keyId":      keyID,
		"nonce":      base64.StdEncoding.EncodeToString(nonce),
		"authTag":    base64.StdEncoding.EncodeToString(ct[len(ct)-16:]),
		"ciphertext": base64.StdEncoding.EncodeToString(ct[:len(ct)-16]),
	})
	if err != nil {
		return nil, fmt.Errorf("at_rest_seal_failed: %w", err)
	}
	return map[string]any{
		"$wbEncrypted": 1,
		"envelope":     base64.StdEncoding.EncodeToString(env),
	}, nil
}

func wbBuffer(s string) map[string]any {
	data := make([]int, len(s))
	for i := 0; i < len(s); i++ {
		data[i] = int(s[i])
	}
	return map[string]any{"type": "Buffer", "data": data}
}

func wbUnwrapBuffer(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if t["type"] == "Buffer" {
			if arr, ok := t["data"].([]any); ok {
				buf := make([]byte, 0, len(arr))
				for _, n := range arr {
					f, ok := n.(float64)
					if !ok {
						return t
					}
					buf = append(buf, byte(f))
				}
				return string(buf)
			}
		}
		for k, item := range t {
			t[k] = wbUnwrapBuffer(item)
		}
		return t
	case []any:
		for i, item := range t {
			t[i] = wbUnwrapBuffer(item)
		}
		return t
	}
	return v
}
