package upstream

import (
	"os"
	"testing"
)

// regChannelBody 是 2026-10-20 从线上 journald 原样抽出的 11-128 报文（截断于日志行长度）。
// 注意 `"code":11-128` —— 数字里带连字符，不是合法 JSON，json.Unmarshal 必然失败。
const regChannelBody = `{"code":11-128,"msg":"request illegal","requestId":"40b921d43e807e038894fe837d6887ab","displayMsg":{"en":"Request blocked. Please send it again"}`

func TestBusinessCodeToleratesRealChannelBody(t *testing.T) {
	if !hasBusinessCode(regChannelBody, "11-128") {
		t.Fatal("真实报文 code=11-128 未被识别（会退回 client 分类，通道拒绝不生效）")
	}
	if !isChannelRefused(regChannelBody) {
		t.Fatal("isChannelRefused=false：通道拒绝未被识别")
	}
	if IsAccountRestricted(regChannelBody) {
		t.Fatal("11-128 被判成账号限制（会误禁用健康账号）")
	}
	if got := Classify(400, regChannelBody); got != ErrChannelRefused {
		t.Fatalf("Classify(400, 真实11-128)=%v, want ErrChannelRefused", got)
	}
	if got := Classify(403, regChannelBody); got != ErrChannelRefused {
		t.Fatalf("Classify(403, 真实11-128)=%v, want ErrChannelRefused", got)
	}
}

func TestBusinessCodeNormalizationVariants(t *testing.T) {
	cases := []string{
		`{"code":"11-128","msg":"request illegal"}`,
		`{"code":11-128,"msg":"request illegal"}`,
		`{"code":"11–128","msg":"request illegal"}`, // en dash（渲染/复制粘贴）
		`{"error":{"code":"11-128"},"msg":"request illegal"}`,
		`{"code":" 11-128 ","msg":"request illegal"}`,
	}
	for _, body := range cases {
		if !isChannelRefused(body) {
			t.Fatalf("未识别通道拒绝: %s", body)
		}
		if IsAccountRestricted(body) {
			t.Fatalf("误判为账号限制: %s", body)
		}
	}
}

func TestBusinessCodeIgnoresRequestIDSubstring(t *testing.T) {
	// requestId 里出现 11140 子串不算数：只锚定 `"code":` 字段。
	body := `{"msg":"request illegal","requestId":"11140abcdef1114012345678901"}`
	if IsAccountRestricted(body) {
		t.Fatal("requestId 里的 11140 子串被当成账号限制")
	}
	if isChannelRefused(body) {
		t.Fatal("requestId 里的子串被当成通道拒绝")
	}
}

func TestBusinessCodeTrueRestrictionStillDisables(t *testing.T) {
	body := `{"code":11140,"msg":"request illegal","requestId":"abc"}`
	if !IsAccountRestricted(body) {
		t.Fatal("真实 11140 账号限制未被识别")
	}
	if isChannelRefused(body) {
		t.Fatal("11140 被归成通道拒绝（应优先判账号限制）")
	}
}

func TestBusinessCodeModelRateLimitStaysSoftRate(t *testing.T) {
	body := `{"code":11140,"msg":"request illegal","displayMsg":{"en":"The model provider is rate-limiting requests."}}`
	if IsAccountRestricted(body) {
		t.Fatal("11140+模型限流文案被判成账号限制（应走 ErrSoftRate）")
	}
}

func TestBusinessCodePlainJSONPathUnaffected(t *testing.T) {
	if !hasBusinessCode(`{"code":14018}`, "14018") || !hasBusinessCode(`{"code":"6004"}`, "6004") {
		t.Fatal("合法 JSON 路径回归")
	}
	if hasBusinessCode(`{"code":6004}`, "6021") {
		t.Fatal("不同业务码误命中")
	}
}

func TestRealFixtureFromTestdata(t *testing.T) {
	b, err := os.ReadFile("testdata/channel_refused_real_body.txt")
	if err != nil {
		t.Skip(err)
	}
	if !isChannelRefused(string(b)) {
		t.Fatalf("testdata 真实报文未被识别: %s", b)
	}
}
