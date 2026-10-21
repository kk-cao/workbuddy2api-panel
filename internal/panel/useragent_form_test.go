package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// 出站 UA 与「按 realm 分档的客户端版本」在面板上的接线：
//   - 三个字段必须存在于表单（user_agent / client_version / client_version_global）
//   - CFG_MAP 映射到 upstream.*
//   - 三者都是「覆盖型」字段：留空必须**照发空串**（= 回落内置默认，按 realm 分档），
//     不能像普通文本字段那样「空 = 不下发」，否则清空后旧值会留在 config.json。
func TestUserAgentVersionFormWiring(t *testing.T) {
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	for _, name := range []string{"user_agent", "client_version", "client_version_global",
		"cli_version", "cli_version_global"} {
		if !strings.Contains(string(html), `name="`+name+`"`) {
			t.Errorf("index.html 缺少 name=%q 表单字段", name)
		}
	}

	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	src := string(js)
	for _, m := range []string{
		"client_version: ['upstream', 'client_version']",
		"client_version_global: ['upstream', 'client_version_global']",
		"cli_version: ['upstream', 'cli_version']",
		"cli_version_global: ['upstream', 'cli_version_global']",
	} {
		if !strings.Contains(src, m) {
			t.Errorf("app.js CFG_MAP 缺少映射 %q", m)
		}
	}
	if !strings.Contains(src, "CLEARABLE_CFG = new Set([") {
		t.Fatal("CLEARABLE_CFG 定义缺失")
	}
	for _, k := range []string{"'client_version'", "'client_version_global'", "'cli_version'", "'cli_version_global'"} {
		if !strings.Contains(src, k) {
			t.Errorf("CLEARABLE_CFG 未包含 %s（留空无法回落默认）", k)
		}
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
const vals = JSON.parse(process.argv[3]);
const elements = {};
for (const k of ['user_agent', 'client_version', 'client_version_global', 'cli_version', 'cli_version_global']) {
  elements[k] = { value: vals[k] === undefined ? '' : vals[k], type: 'text' };
}
const ctx = { console, $: () => ({ elements }) };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end), ctx);
console.log(JSON.stringify(vm.runInContext('collectConfig()', ctx)));
`
	f, err := os.CreateTemp(t.TempDir(), "ua-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()

	run := func(vals string) string {
		out, err := exec.Command(node, f.Name(), "app.js", vals).CombinedOutput()
		if err != nil {
			t.Fatalf("node harness: %v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}

	// 全空：三个键都必须出现在下发载荷里，值为空串（= 回落默认）。
	empty := run(`{}`)
	for _, want := range []string{`"user_agent":""`, `"client_version":""`, `"client_version_global":""`,
		`"cli_version":""`, `"cli_version_global":""`} {
		if !strings.Contains(empty, want) {
			t.Errorf("留空未下发 %s（载荷=%s）", want, empty)
		}
	}

	// 显式值：原样透传。
	filled := run(`{"user_agent":"Custom/1.0","client_version":"5.8.3","client_version_global":"5.8.4","cli_version":"2.161.0","cli_version_global":"2.170.0"}`)
	for _, want := range []string{`"user_agent":"Custom/1.0"`, `"client_version":"5.8.3"`, `"client_version_global":"5.8.4"`,
		`"cli_version":"2.161.0"`, `"cli_version_global":"2.170.0"`} {
		if !strings.Contains(filled, want) {
			t.Errorf("显式值未透传 %s（载荷=%s）", want, filled)
		}
	}
}
