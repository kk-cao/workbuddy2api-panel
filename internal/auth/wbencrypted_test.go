package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const wbVectorPath = "testdata/wbencrypted-auth.json"

func TestParseWBEncryptedAuthFile(t *testing.T) {
	raw, err := os.ReadFile(wbVectorPath)
	if err != nil {
		t.Fatalf("读向量失败: %v", err)
	}
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("解析加密 auth 文件失败: %v", err)
	}
	for _, c := range []struct{ name, got, want string }{
		{"accessToken", a.AccessToken, "at-from-wbencrypted"},
		{"refreshToken", a.RefreshToken, "rt-from-wbencrypted"},
		{"nickname", a.Nickname, "小凛"},
		{"uid", a.UID, "uid-wb-1"},
		{"deviceToken", a.DeviceToken, "v3:test-device-token"},
		{"realm", a.Realm(), "cn"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q want %q", c.name, c.got, c.want)
		}
	}
	if a.ExpiresAt != 1800000000 {
		t.Errorf("expiresAt = %d want 1800000000（毫秒应归一为秒）", a.ExpiresAt)
	}
}

func TestWBFieldAADMatchesReference(t *testing.T) {
	const want = "57422d41414400010000000557424556310000000673796d2d763100000001" +
		"0000001039313237646561316234343032306137020000"
	got := hex.EncodeToString(wbFieldAAD(1, wbKeyID(sha256.Sum256([]byte(wbAtRestSecretKey)))))
	if got != want {
		t.Fatalf("AAD 与参考实现不一致:\n got %s\nwant %s", got, want)
	}
}

func TestParseWBEncryptedKeyIDMismatchFails(t *testing.T) {
	raw, err := os.ReadFile(wbVectorPath)
	if err != nil {
		t.Fatalf("读向量失败: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("向量不是合法 JSON: %v", err)
	}
	authObj := doc["auth"].(map[string]any)
	env := authObj["accessToken"].(map[string]any)
	rawEnv, err := base64.StdEncoding.DecodeString(env["envelope"].(string))
	if err != nil {
		t.Fatalf("信封不是合法 base64: %v", err)
	}
	keyID := wbKeyID(sha256.Sum256([]byte(wbAtRestSecretKey)))
	plainEnv := strings.Replace(string(rawEnv), keyID, "0000000000000000", 1)
	if plainEnv == string(rawEnv) {
		t.Fatal("向量里找不到 keyId，测试自身失效")
	}
	env["envelope"] = base64.StdEncoding.EncodeToString([]byte(plainEnv))
	bad, _ := json.Marshal(doc)

	if _, err := Parse(bad); err == nil {
		t.Fatal("keyId 不匹配时应当报错")
	} else if !strings.Contains(err.Error(), "at_rest_key_mismatch") {
		t.Fatalf("错误应说明是密钥不匹配，实际: %v", err)
	}
}

func TestSaveAtomicKeepsEnvelopeForEncryptedSource(t *testing.T) {
	raw, err := os.ReadFile(wbVectorPath)
	if err != nil {
		t.Fatalf("读向量失败: %v", err)
	}
	path := filepath.Join(t.TempDir(), "auth.info")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("写临时 auth 失败: %v", err)
	}
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	a.FilePath = path
	a.AccessToken = "at-refreshed"
	a.RefreshToken = "rt-refreshed"
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("写回失败: %v", err)
	}

	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if strings.Contains(string(saved), "at-refreshed") || strings.Contains(string(saved), "rt-refreshed") {
		t.Fatal("写回文件里出现了明文 token")
	}
	if !strings.Contains(string(saved), `"$wbEncrypted"`) {
		t.Fatal("写回文件丢了信封形态")
	}
	again, err := Parse(saved)
	if err != nil {
		t.Fatalf("回写后再解析失败: %v", err)
	}
	if again.AccessToken != "at-refreshed" || again.RefreshToken != "rt-refreshed" {
		t.Fatalf("回写后的 token 读不回来: %+v", again)
	}
	if again.Nickname != "小凛" {
		t.Fatalf("回写后的昵称读不回来: %q", again.Nickname)
	}
}

func TestSaveAtomicLeavesPlainFieldsPlain(t *testing.T) {
	raw, err := os.ReadFile(wbVectorPath)
	if err != nil {
		t.Fatalf("读向量失败: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("向量不是合法 JSON: %v", err)
	}
	doc["auth"].(map[string]any)["expiresAt"] = float64(1800000000000)
	raw, _ = json.Marshal(doc)

	path := filepath.Join(t.TempDir(), "auth.info")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("写临时 auth 失败: %v", err)
	}
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	a.FilePath = path
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("写回失败: %v", err)
	}
	saved, _ := os.ReadFile(path)
	var back map[string]any
	if err := json.Unmarshal(saved, &back); err != nil {
		t.Fatalf("写回文件不是合法 JSON: %v", err)
	}
	if _, isEnvelope := back["auth"].(map[string]any)["expiresAt"].(map[string]any); isEnvelope {
		t.Error("expiresAt 原本明文，回写不该加密")
	}
	if _, isEnvelope := back["auth"].(map[string]any)["accessToken"].(map[string]any); !isEnvelope {
		t.Error("accessToken 原本加密，回写必须仍是信封")
	}
	if again, err := Parse(saved); err != nil || again.ExpiresAt != 1800000000 {
		t.Errorf("回写后再解析失败或到期时间不对: %v %+v", err, again)
	}
}

func TestParseUnencryptedStillWorks(t *testing.T) {
	a, err := Parse([]byte(`{"auth":{"accessToken":"at","refreshToken":"rt","realm":"global"},"account":{"uid":"u1"}}`))
	if err != nil {
		t.Fatalf("明文 auth 解析失败: %v", err)
	}
	if a.AccessToken != "at" || a.UID != "u1" || a.Realm() != "global" {
		t.Fatalf("明文解析结果不对: %+v", a)
	}
}

func mustSeal(t *testing.T, v any) map[string]any {
	t.Helper()
	env, err := sealWBValue(v)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func saveRefreshed(t *testing.T, raw []byte) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "workbuddy-wb.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	a.FilePath = path
	a.AccessToken, a.RefreshToken, a.ExpiresAt = "at-refreshed", "rt-refreshed", 1900000000
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("写回失败: %v", err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("凭证文件权限应为 0600: %v %v", st.Mode().Perm(), err)
		}
	}
	return saved
}

func TestSaveAtomicEncryptedSourceKeepsShapeAndSecrets(t *testing.T) {
	flat, _ := json.Marshal(map[string]any{
		"accessToken":  mustSeal(t, "at-old"),
		"refreshToken": mustSeal(t, "rt-old"),
		"expiresAt":    json.Number("1800000000000"),
		"uid":          "uid-flat",
		"phoneNumber":  mustSeal(t, "13800000000"),
		"uin":          json.Number("12345678901234567"),
	})
	mixed, _ := json.Marshal(map[string]any{
		"auth":        map[string]any{"accessToken": "at-old", "refreshToken": "rt-old", "expiresAt": json.Number("1800000000000")},
		"account":     map[string]any{"uid": "uid-mixed", "nickname": mustSeal(t, "小凛"), "uin": "10001"},
		"allAccounts": []any{map[string]any{"accessToken": mustSeal(t, "at-other-account")}, mustSeal(t, "rt-other-account")},
		"a.b":         mustSeal(t, "dotted-secret"),
	})
	vector, err := os.ReadFile(wbVectorPath)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{"flat": flat, "mixed": mixed, "vector": vector} {
		saved := saveRefreshed(t, raw)
		s := string(saved)
		if strings.Contains(s, "at-refreshed") || strings.Contains(s, "rt-refreshed") || strings.Contains(s, "13800000000") ||
			strings.Contains(s, "other-account") || strings.Contains(s, "dotted-secret") {
			t.Errorf("%s: 写回文件里出现了明文机密:\n%s", name, s)
		}
		var back map[string]any
		dec := json.NewDecoder(strings.NewReader(s))
		dec.UseNumber()
		if err := dec.Decode(&back); err != nil {
			t.Fatalf("%s: 写回不是合法 JSON: %v", name, err)
		}
		creds, acc := back, back
		if name != "flat" {
			creds, acc = back["auth"].(map[string]any), back["account"].(map[string]any)
		} else if _, nested := back["auth"]; nested {
			t.Errorf("flat: 扁平形被改写成了嵌套形")
		}
		for _, k := range []string{"accessToken", "refreshToken"} {
			if !isWBEnvelope(creds[k]) {
				t.Errorf("%s: %s 应为信封，实际 %v", name, k, creds[k])
			}
		}
		if name == "flat" {
			if back["uin"] != json.Number("12345678901234567") || !isWBEnvelope(back["phoneNumber"]) {
				t.Errorf("flat: 桌面端字段丢失或被改写: uin=%v phone=%v", back["uin"], back["phoneNumber"])
			}
		} else if acc["uin"] != "10001" {
			t.Errorf("%s: account.uin 丢失: %v", name, acc["uin"])
		}
		if name != "vector" && creds["expiresAt"] != json.Number("1900000000000") {
			t.Errorf("%s: expiresAt 应按毫秒写回，实际 %v", name, creds["expiresAt"])
		}
		again, err := Parse(saved)
		if err != nil || again.AccessToken != "at-refreshed" || again.RefreshToken != "rt-refreshed" || again.ExpiresAt != 1900000000 {
			t.Errorf("%s: 回写后读不回来: %v %+v", name, err, again)
		}
	}
}

func TestWBEncryptedNumericStringsRoundTrip(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"auth": map[string]any{"accessToken": mustSeal(t, "at-old"), "refreshToken": mustSeal(t, "rt-old"), "realm": "cn"},
		"account": map[string]any{"uid": "uid-num",
			"nickname": mustSealPlain(t, `{"type":"Buffer","data":[49,50,51,52,53]}`)},
	})
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy-num.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := Parse(raw)
	if err != nil || a.Nickname != "12345" {
		t.Fatalf("桌面端 Buffer 包装的数字形昵称应读成字符串: %v %+v", err, a)
	}
	a.FilePath = path
	a.RefreshToken = "123"
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	list, err := LoadDir(dir)
	if err != nil || len(list) != 1 {
		t.Fatalf("写回后重新载入失败: %v n=%d", err, len(list))
	}
	if got := list[0]; got.Nickname != "12345" || got.RefreshToken != "123" || got.AccessToken != "at-old" {
		t.Fatalf("写回后类型或值变了: %+v", got)
	}
}

func mustSealPlain(t *testing.T, plain string) map[string]any {
	t.Helper()
	key := sha256.Sum256([]byte(wbAtRestSecretKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, gcm.NonceSize())
	keyID := wbKeyID(key)
	ct := gcm.Seal(nil, nonce, []byte(plain), wbFieldAAD(1, keyID))
	env, _ := json.Marshal(map[string]any{"suite": 1, "keyId": keyID,
		"nonce": base64.StdEncoding.EncodeToString(nonce), "authTag": base64.StdEncoding.EncodeToString(ct[len(ct)-16:]),
		"ciphertext": base64.StdEncoding.EncodeToString(ct[:len(ct)-16])})
	return map[string]any{"$wbEncrypted": 1, "envelope": base64.StdEncoding.EncodeToString(env)}
}

func TestWBEncryptedInvalidNonceOrTagDoesNotPanic(t *testing.T) {
	key := sha256.Sum256([]byte(wbAtRestSecretKey))
	keyID := wbKeyID(key)
	for _, tc := range []struct {
		name  string
		nonce []byte
		tag   []byte
	}{
		{"short nonce", []byte{1, 2, 3}, make([]byte, 16)},
		{"long nonce", make([]byte, 24), make([]byte, 16)},
		{"short tag", make([]byte, 12), []byte{1, 2, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envRaw, _ := json.Marshal(map[string]any{
				"suite":      1,
				"keyId":      keyID,
				"nonce":      base64.StdEncoding.EncodeToString(tc.nonce),
				"authTag":    base64.StdEncoding.EncodeToString(tc.tag),
				"ciphertext": base64.StdEncoding.EncodeToString([]byte("abc")),
			})
			raw, _ := json.Marshal(map[string]any{
				"auth": map[string]any{
					"accessToken": map[string]any{"$wbEncrypted": 1, "envelope": base64.StdEncoding.EncodeToString(envRaw)},
				},
				"account": map[string]any{"uid": "u-bad-nonce"},
			})
			if _, err := Parse(raw); err == nil {
				t.Fatal("非法 nonce/tag 长度必须返回 error（不能成功也不能 panic）")
			}
		})
	}
}
