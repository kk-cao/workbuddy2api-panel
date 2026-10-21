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
