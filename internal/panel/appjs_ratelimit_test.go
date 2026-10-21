package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// rateLimitMeta 在 app.js 里是 'use strict' 作用域，任何未声明的赋值都会抛
// ReferenceError —— 而它被 renderAccounts 调用，一抛就整张账号池表格空白。
// 这里把该函数原样抽出来在 node 里跑，覆盖两个曾经漏声明 detail 的分支。
func TestRateLimitMetaNoUndeclaredAssignment(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	src := string(js)

	// 静态断言：detail 必须显式声明（防止有人把 let 删回去）。
	if !strings.Contains(src, "let detail;") {
		t.Fatal("app.js 缺少 `let detail;` 声明 —— rateLimitMeta 在 use strict 下会抛 ReferenceError")
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}

	start := strings.Index(src, "function rateLimitMeta(")
	end := strings.Index(src, "function rateLimitRowsHtml(")
	if start < 0 || end < 0 || end < start {
		t.Fatal("app.js 里找不到 rateLimitMeta 函数区间")
	}
	fn := src[start:end]

	harness := `'use strict';
const parseAPITime = s => (s ? Date.parse(s) : 0);
const fmtLocalDateTime = ms => new Date(ms).toISOString();
const dur = s => s + 's';
const esc = s => String(s);
` + fn + `
const now = Date.parse('2026-10-22T02:00:00+08:00');
const cases = [
  {name: 'until 早于 reset_at（双时钟分支）', row: {model: 'm', kind: 'rate_limit',
     reset_at: '2026-10-22T03:00:00+08:00', until: '2026-10-22T02:30:00+08:00'}},
  {name: '只有 reset_at', row: {model: 'm', kind: 'rate_limit', reset_at: '2026-10-22T03:00:00+08:00'}},
  {name: 'model_unavailable', row: {model: 'm', kind: 'model_unavailable', reset_at: '2026-10-22T03:00:00+08:00'}},
];
const out = [];
for (const c of cases) {
  try {
    const r = rateLimitMeta(c.row, now);
    out.push({name: c.name, ok: true, detail: r.detail, title: !!r.title});
  } catch (e) {
    out.push({name: c.name, ok: false, error: String(e && e.message || e)});
  }
}
console.log(JSON.stringify(out));
`
	f, err := os.CreateTemp(t.TempDir(), "ratelimit-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(harness); err != nil {
		t.Fatal(err)
	}
	f.Close()

	raw, err := exec.Command(node, f.Name()).CombinedOutput()
	if err != nil {
		t.Fatalf("node harness: %v\n%s", err, raw)
	}
	out := string(raw)
	if strings.Contains(out, `"ok":false`) {
		t.Fatalf("rateLimitMeta 抛异常：%s", out)
	}
	for _, want := range []string{"双时钟分支", "只有 reset_at", "model_unavailable"} {
		if !strings.Contains(out, want) {
			t.Errorf("用例 %s 未执行：%s", want, out)
		}
	}
	if !strings.Contains(out, `"detail":"`) {
		t.Errorf("detail 为空：%s", out)
	}
}

// 11102 负缓存（model_unavailable）不得显示成「预计 X 后重试」——重试无意义；
// 且多条必须折叠成一条汇总，避免一次目录差异刷出二十多行。
func TestRateLimitRowsFoldsModelUnavailable(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	src := string(js)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	start := strings.Index(src, "function rateLimitMeta(")
	end := strings.Index(src, "function formatTokenCount(")
	if start < 0 || end < 0 || end < start {
		t.Fatal("找不到 rateLimitMeta..rateLimitRowsHtml 区间")
	}
	fn := src[start:end]
	harness := `'use strict';
const parseAPITime = s => (s ? Date.parse(s) : 0);
const fmtLocalDateTime = ms => new Date(ms).toISOString();
const dur = s => s + 's';
const esc = s => String(s);
` + fn + `
const now = Date.parse('2026-10-22T02:00:00+08:00');
const rows = [
  {model: 'glm-4.7', kind: 'model_unavailable', until: '2026-10-22T08:00:00+08:00'},
  {model: 'glm-5.0', kind: 'model_unavailable', until: '2026-10-22T08:00:00+08:00'},
  {model: 'gpt-6-astra', kind: 'model_unavailable', until: '2026-10-22T08:00:00+08:00'},
  {model: 'deepseek-v4.1-flash', kind: 'rate_limit', reset_at: '2026-10-22T02:05:00+08:00'},
];
const html = rateLimitRowsHtml(rows, now);
console.log(JSON.stringify({
  html,
  count: (html.match(/<div class="rate-limit[ "]/g) || []).length,
  hasCountdown: /后重试/.test(html),
  hasSummary: html.includes('3 个模型'),
  hasRetryable: html.includes('deepseek-v4.1-flash'),
}));
`
	f, err := os.CreateTemp(t.TempDir(), "ratelimitrows-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(harness); err != nil {
		t.Fatal(err)
	}
	f.Close()
	raw, err := exec.Command(node, f.Name()).CombinedOutput()
	if err != nil {
		t.Fatalf("node harness: %v\n%s", err, raw)
	}
	out := string(raw)
	if !strings.Contains(out, `"hasCountdown":false`) {
		t.Errorf("model_unavailable 仍显示倒计时：%s", out)
	}
	if !strings.Contains(out, `"hasSummary":true`) {
		t.Errorf("未折叠成汇总：%s", out)
	}
	if !strings.Contains(out, `"count":2`) {
		t.Errorf("应只有 1 条汇总 + 1 条可重试限流：%s", out)
	}
	if strings.Contains(out, `"glm-4.7</b>`) {
		t.Errorf("负缓存模型不应逐个占行：%s", out)
	}
	if !strings.Contains(out, `"hasRetryable":true`) {
		t.Errorf("可重试限流被误折叠：%s", out)
	}
}
