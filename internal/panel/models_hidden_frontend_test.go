package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// 模型黑名单（models.hidden）从「只能手改 config.json」提到面板表单后，
// 三个环节都只在浏览器里生效：表单字段存在、CSS/JS 能回填、collectConfig 会把它
// 组装成数组下发。Go 侧对 JS 完全失明，故用 node harness 钉死组装语义。
func TestModelsHiddenFormWiring(t *testing.T) {
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	if !strings.Contains(string(html), `name="models_hidden"`) {
		t.Fatal(`index.html 缺少 name="models_hidden" 表单字段（黑名单无法从 UI 编辑）`)
	}

	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	if !strings.Contains(string(js), `models_hidden: ['models', 'hidden']`) {
		t.Fatal("app.js CFG_MAP 缺少 models_hidden -> models.hidden 映射")
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; collectConfig assembly test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const CFG_MAP');
const end = src.indexOf('/* Go 时长字段即时校验');
if (start < 0 || end < 0 || end < start) throw new Error('cfg region not found');
const formValue = process.argv[3];
const ctx = {
  console,
  $: () => ({ elements: { models_hidden: { value: formValue, type: 'textarea' } } }),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end), ctx);
const out = vm.runInContext('collectConfig()', ctx);
console.log(JSON.stringify(out));
`
	f, err := os.CreateTemp(t.TempDir(), "hidden-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	run := func(value string) string {
		out, err := exec.Command(node, f.Name(), "app.js", value).CombinedOutput()
		if err != nil {
			t.Fatalf("node harness failed: %v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}

	if got := run("cn:glm-4.7, global:qwen3-max"); got != `{"models":{"hidden":["cn:glm-4.7","global:qwen3-max"]}}` {
		t.Fatalf("逗号分隔未组装成数组: %s", got)
	}
	// 换行/空格/中文逗号同样要能拆开（用户从文档里粘贴的形态）
	if got := run("cn:a\n global:b，cn:c"); got != `{"models":{"hidden":["cn:a","global:b","cn:c"]}}` {
		t.Fatalf("换行/中文逗号未正确拆分: %s", got)
	}
	// 清空 = 显式下发空数组（必须能移除黑名单，而不是"没改"）
	if got := run("   "); got != `{"models":{"hidden":[]}}` {
		t.Fatalf("空值应下发空数组以清空黑名单，实际: %s", got)
	}
}
