package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestReportDesktopEventFingerprint 断言桌面指纹上报：走 chatBase、UA 为桌面形状、
// 事件数组自动注入 workbuddy-desktop 公共指纹（覆盖同名业务键）。
func TestReportDesktopEventFingerprint(t *testing.T) {
	var got []map[string]any
	var gotUA string
	var gotHdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/report" {
			t.Errorf("path=%s want /v2/report", r.URL.Path)
		}
		gotUA = r.Header.Get("User-Agent")
		gotHdr = r.Header.Clone()
		if r.Header.Get("X-User-Id") != "u-dt" {
			t.Errorf("X-User-Id=%q want u-dt", r.Header.Get("X-User-Id"))
		}
		if r.Header.Get("X-Product") != "SaaS" {
			t.Errorf("X-Product=%q want SaaS", r.Header.Get("X-Product"))
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("body must be array: %v", err)
		}
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	err := c.ReportDesktopEvent(&auth.Auth{AccessToken: "at", UID: "u-dt", Nickname: "RenJie", DeviceToken: "dt-1"},
		DesktopEvent{"eventCode": "agent_task_created", "mode": "craft"})
	if err != nil {
		t.Fatalf("report desktop: %v", err)
	}
	if gotUA != desktopUA {
		t.Errorf("UA=%q want %q", gotUA, desktopUA)
	}
	if gotHdr.Get("X-CodeBuddy-Request") != "1" {
		t.Errorf("X-CodeBuddy-Request=%q want 1", gotHdr.Get("X-CodeBuddy-Request"))
	}
	if gotHdr.Get("Accept-Language") != "" {
		t.Errorf("Accept-Language=%q want empty (RPC no longer sends Accept-Language)", gotHdr.Get("Accept-Language"))
	}
	if gotHdr.Get("X-Client-Version") != defaultClientVersion || gotHdr.Get("X-Client-Platform") != "windows" || gotHdr.Get("X-Client-Product") != "WorkBuddy" {
		t.Errorf("unexpected X-Client-* headers: ver=%q plat=%q prod=%q",
			gotHdr.Get("X-Client-Version"), gotHdr.Get("X-Client-Platform"), gotHdr.Get("X-Client-Product"))
	}
	if gotHdr.Get("X-Device-Token") != "dt-1" {
		t.Errorf("X-Device-Token=%q want dt-1", gotHdr.Get("X-Device-Token"))
	}
	if len(got) != 1 {
		t.Fatalf("events=%d want 1", len(got))
	}
	ev := got[0]
	if ev["extName"] != "workbuddy-desktop" {
		t.Errorf("extName=%v want workbuddy-desktop", ev["extName"])
	}
	if ev["ideType"] != "WorkBuddy" || ev["ideName"] != "WorkBuddy" {
		t.Errorf("ideType/ideName=%v/%v want WorkBuddy", ev["ideType"], ev["ideName"])
	}
	if ev["ideVersion"] != "5.11.3" || ev["extVersion"] != "5.11.3" {
		t.Errorf("ideVersion/extVersion=%v/%v want 5.11.3", ev["ideVersion"], ev["extVersion"])
	}
	if ev["userId"] != "u-dt" {
		t.Errorf("userId=%v want u-dt", ev["userId"])
	}
	if ev["userNickname"] != "RenJie" {
		t.Errorf("userNickname=%v want RenJie", ev["userNickname"])
	}
	if ev["mode"] != "craft" {
		t.Errorf("业务字段 mode=%v 被指纹覆盖或缺失", ev["mode"])
	}
	// machineId / sessionId 保留在 JSON body 中且为 36 hex；HTTP 请求头不再发送 X-Machine-ID / X-Session-ID。
	if len(ev["machineId"].(string)) != 36 || ev["machineId"] != deriveAccountStableID("u-dt", "machine") {
		t.Errorf("ev.machineId=%v want 36-char stable machine ID", ev["machineId"])
	}
	if len(ev["sessionId"].(string)) != 36 || ev["sessionId"] != deriveAccountStableID("u-dt", "session") {
		t.Errorf("ev.sessionId=%v want 36-char stable session ID", ev["sessionId"])
	}
	if gotHdr.Get("X-Machine-ID") != "" || gotHdr.Get("X-Session-ID") != "" {
		t.Errorf("X-Machine-ID/X-Session-ID HTTP headers must not be sent, got %q / %q",
			gotHdr.Get("X-Machine-ID"), gotHdr.Get("X-Session-ID"))
	}
}

// TestDesktopChatSequenceShape 断言完整对话事件链的事件码顺序与关键字段。
func TestDesktopChatSequenceShape(t *testing.T) {
	events := DesktopChatSequence("conv-1", "req-1", "msg-1", "fast-model", "fast-model")
	wantCodes := []string{
		"agent_task_created", "chat_message_send", "chat_request_send",
		"chat_message_response", "chat_message_status", "chat_request_response",
	}
	if len(events) != len(wantCodes) {
		t.Fatalf("events=%d want %d", len(events), len(wantCodes))
	}
	for i, want := range wantCodes {
		if events[i]["eventCode"] != want {
			t.Errorf("events[%d].eventCode=%v want %v", i, events[i]["eventCode"], want)
		}
	}
	resp := events[3]
	if resp["isSuccessful"] != true {
		t.Errorf("chat_message_response.isSuccessful=%v want true（RichMeow 判据）", resp["isSuccessful"])
	}
	if resp["finishReason"] != "stop" {
		t.Errorf("finishReason=%v want stop", resp["finishReason"])
	}
	if resp["conversationId"] != "conv-1" {
		t.Errorf("conversationId=%v want conv-1", resp["conversationId"])
	}
}

// TestSetAppearanceTheme 断言主题设置端点形状（点亮 Hp_Appearance 的 API）。
func TestSetAppearanceTheme(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/user-asset/appearance/set" {
			t.Errorf("path=%s want /v2/user-asset/appearance/set", r.URL.Path)
		}
		if r.Header.Get("User-Agent") != desktopUA {
			t.Errorf("UA=%q want desktop UA", r.Header.Get("User-Agent"))
		}
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &got)
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	if err := c.SetAppearanceTheme(&auth.Auth{AccessToken: "at", UID: "u1"}, "theme-tkmw7j"); err != nil {
		t.Fatalf("set theme: %v", err)
	}
	if got["kind"] != "theme" || got["resource_key"] != "theme-tkmw7j" {
		t.Errorf("body=%v want kind=theme resource_key=theme-tkmw7j", got)
	}
}

// TestSetAppearanceThemeAndMarketExpertListHeaders 断言 SetAppearanceTheme 与 MarketExpertList 发送裸 X-Domain、X-CodeBuddy-Request: 1、X-Machine-ID 与 X-Session-ID。
func TestSetAppearanceThemeAndMarketExpertListHeaders(t *testing.T) {
	var gotHdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHdr = r.Header.Clone()
		if r.URL.Path == "/portal/operation-platform/market/expert/list" {
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"experts":[{"expert_id":"e1","display_name_zh":"专家1"}]}}`))
			return
		}
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	if err := c.SetAppearanceTheme(&auth.Auth{AccessToken: "at", UID: "u-theme"}, "theme-tkmw7j"); err != nil {
		t.Fatalf("set theme: %v", err)
	}
	assertDesktopCommonHeaders(t, gotHdr, "u-theme")

	experts, err := c.MarketExpertList(&auth.Auth{AccessToken: "at", UID: "u-exp"}, "agent")
	if err != nil {
		t.Fatalf("market expert list: %v", err)
	}
	if len(experts) != 1 || experts[0].ExpertID != "e1" {
		t.Errorf("experts=%+v want [e1]", experts)
	}
	assertDesktopCommonHeaders(t, gotHdr, "u-exp")
}

func assertDesktopCommonHeaders(t *testing.T, hdr http.Header, uid string) {
	t.Helper()
	dom := hdr.Get("X-Domain")
	if dom == "" || len(dom) >= 7 && (dom[:7] == "http://" || (len(dom) >= 8 && dom[:8] == "https://")) {
		t.Errorf("X-Domain=%q want bare host without http(s)://", dom)
	}
	if hdr.Get("X-CodeBuddy-Request") != "1" {
		t.Errorf("X-CodeBuddy-Request=%q want 1", hdr.Get("X-CodeBuddy-Request"))
	}
	if hdr.Get("X-Machine-ID") != "" || hdr.Get("X-Session-ID") != "" || hdr.Get("Accept-Language") != "" {
		t.Errorf("desktop headers must not send X-Machine-ID/X-Session-ID/Accept-Language, got %q/%q/%q",
			hdr.Get("X-Machine-ID"), hdr.Get("X-Session-ID"), hdr.Get("Accept-Language"))
	}
	if hdr.Get("X-Client-Version") != defaultClientVersion || hdr.Get("X-Client-Platform") != "windows" || hdr.Get("X-Client-Product") != "WorkBuddy" {
		t.Errorf("unexpected X-Client-* headers: ver=%q plat=%q prod=%q",
			hdr.Get("X-Client-Version"), hdr.Get("X-Client-Platform"), hdr.Get("X-Client-Product"))
	}
}

// TestDeriveDesktopHW 验证 deriveDesktopHW 按 UID 幂等稳定、空 UID 回退默认且不同 UID 产生不同硬件画像。
func TestDeriveDesktopHW(t *testing.T) {
	def := deriveDesktopHW("")
	if def != (desktopHWSpec{osVersion: "10.0.26220", cpuCores: 20, memorySize: 24}) {
		t.Errorf("empty uid hw=%+v want default 10.0.26220/20/24", def)
	}
	if deriveDesktopHW("user-alpha") != deriveDesktopHW("user-alpha") {
		t.Error("deriveDesktopHW should be deterministic for the same UID")
	}
	seen := map[desktopHWSpec]bool{}
	for i := 0; i < 32; i++ {
		seen[deriveDesktopHW("uid-"+string(rune('a'+i)))] = true
	}
	if len(seen) < 2 {
		t.Errorf("deriveDesktopHW did not vary across UIDs: unique=%d", len(seen))
	}
}
