package plugin_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"netping/internal/plugin"
	"netping/internal/plugin/plugintest"
)

func TestMain(m *testing.M) {
	plugintest.RunIfHelper()
	os.Exit(m.Run())
}

const fullManifest = `name: 测试插件
version: 1.2.3
description: 用于测试
timeout_sec: 20
hooks: [notify, alert, discovery, locate]
tools:
  - name: lookup
    title: 查询
    description: 按 IP 查询资产
    ai: true
    input_schema:
      type: object
      properties:
        ip: {type: string}
  - name: restart
    description: 重启设备
    write: true
    ai: true
settings:
  - key: mode
    label: 模式
    type: select
    options: [echo, fail, exit, raw, silent, sleep, big]
    default: echo
  - key: token
    label: 令牌
    type: secret
    required: true
  - key: retries
    type: number
    default: 3
  - key: verbose
    type: bool
`

func newManager(t *testing.T) (*plugin.Manager, string, string) {
	t.Helper()
	base := t.TempDir()
	dir, data := filepath.Join(base, "plugins"), filepath.Join(base, "data")
	plugintest.Install(t, dir, "demo", fullManifest)
	m := plugin.New(dir, data)
	m.Host = func() plugin.HostInfo {
		return plugin.HostInfo{Version: "9.9", Site: "测试机房", URL: "http://127.0.0.1:8090/"}
	}
	t.Cleanup(m.Close)
	return m, dir, data
}

func enable(t *testing.T, m *plugin.Manager, mode string) {
	t.Helper()
	if err := m.UpdateSettings("demo", map[string]interface{}{"token": "s3cret", "mode": mode}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetEnabled("demo", true); err != nil {
		t.Fatal(err)
	}
}

func find(t *testing.T, m *plugin.Manager, id string) plugin.Info {
	t.Helper()
	for _, p := range m.List() {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("plugin %s not listed", id)
	return plugin.Info{}
}

func TestManifestValidation(t *testing.T) {
	cases := []struct{ name, id, body, want string }{
		{"bad id", "Bad_ID", "hooks: [notify]\n", "id"},
		{"unknown hook", "p1", "hooks: [nope]\n", "未知钩子"},
		{"nothing declared", "p1", "name: x\n", "没有声明"},
		{"bad tool name", "p1", "tools:\n  - name: Bad-Name\n    description: d\n", "工具名"},
		{"tool without description", "p1", "tools:\n  - name: t1\n", "description"},
		{"duplicate tool", "p1", "tools:\n  - {name: t1, description: d}\n  - {name: t1, description: d}\n", "重复"},
		{"schema not object", "p1", "tools:\n  - name: t1\n    description: d\n    input_schema: {type: string}\n", "object"},
		{"select without options", "p1", "hooks: [notify]\nsettings:\n  - {key: k, type: select}\n", "options"},
		{"bad setting type", "p1", "hooks: [notify]\nsettings:\n  - {key: k, type: date}\n", "类型"},
		{"bad default", "p1", "hooks: [notify]\nsettings:\n  - {key: k, type: number, default: abc}\n", "default"},
		{"future api", "p1", "api_version: 99\nhooks: [notify]\n", "api_version"},
		{"timeout too long", "p1", "timeout_sec: 999\nhooks: [notify]\n", "timeout_sec"},
		{"bad yaml", "p1", "hooks: [notify\n", "YAML"},
	}
	for _, c := range cases {
		dir := plugintest.Install(t, t.TempDir(), c.id, c.body)
		if _, err := plugin.LoadManifest(dir); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want containing %q", c.name, err, c.want)
		}
	}
	// 目录名必须与 id 相同；缺少清单；缺少 command。
	base := t.TempDir()
	dir := plugintest.Install(t, base, "p1", "hooks: [notify]\n")
	other := filepath.Join(base, "other")
	if err := os.Rename(dir, other); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.LoadManifest(other); err == nil || !strings.Contains(err.Error(), "目录名") {
		t.Errorf("dir mismatch: %v", err)
	}
	if _, err := plugin.LoadManifest(t.TempDir()); err == nil || !strings.Contains(err.Error(), "缺少") {
		t.Errorf("missing manifest: %v", err)
	}
	nocmd := filepath.Join(base, "nocmd")
	_ = os.MkdirAll(nocmd, 0o755)
	_ = os.WriteFile(filepath.Join(nocmd, "plugin.yaml"), []byte("id: nocmd\nhooks: [notify]\n"), 0o644)
	if _, err := plugin.LoadManifest(nocmd); err == nil || !strings.Contains(err.Error(), "command") {
		t.Errorf("missing command: %v", err)
	}
	// 写工具不能开放给 AI 诊断；默认值与名称补全。
	ok := plugintest.Install(t, t.TempDir(), "demo", fullManifest)
	mf, err := plugin.LoadManifest(ok)
	if err != nil {
		t.Fatal(err)
	}
	if !mf.Tools[0].AI || mf.Tools[1].AI || mf.Tools[1].Title != "restart" || mf.APIVersion != 1 || !mf.HasHook("alert") {
		t.Errorf("normalize: %+v", mf)
	}
	if got := plugin.FullToolName("demo", "lookup"); got != "plugin_demo_lookup" {
		t.Errorf("FullToolName = %s", got)
	}
}

func TestLifecycleAndSettings(t *testing.T) {
	m, dir, data := newManager(t)
	// 无效插件也列出，但不能启用；隐藏目录忽略。
	plugintest.Install(t, dir, "broken", "hooks: [nope]\n")
	_ = os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	if n := m.Reload(); n != 1 {
		t.Fatalf("valid plugins = %d", n)
	}
	if len(m.List()) != 2 {
		t.Fatalf("list = %+v", m.List())
	}
	if b := find(t, m, "broken"); b.Error == "" || b.Enabled {
		t.Fatalf("broken: %+v", b)
	}
	if err := m.SetEnabled("broken", true); err == nil {
		t.Fatal("enabled an invalid plugin")
	}
	if err := m.SetEnabled("nope", true); err == nil {
		t.Fatal("enabled a missing plugin")
	}
	// 默认未启用；必填项未填不能启用。
	p := find(t, m, "demo")
	if p.Enabled || p.Version != "1.2.3" || len(p.Tools) != 2 || p.Tools[0].FullName != "plugin_demo_lookup" {
		t.Fatalf("demo: %+v", p)
	}
	if m.HasNotifiers() || len(m.Tools()) != 0 {
		t.Fatal("disabled plugin is active")
	}
	if err := m.SetEnabled("demo", true); err == nil || !strings.Contains(err.Error(), "令牌") {
		t.Fatalf("enable without required: %v", err)
	}
	// 设置校验。
	for _, bad := range []map[string]interface{}{
		{"nope": "x"}, {"mode": "weird"}, {"retries": "many"}, {"verbose": "yes"}, {"token": 5},
	} {
		if err := m.UpdateSettings("demo", bad); err == nil {
			t.Errorf("accepted bad settings %v", bad)
		}
	}
	if err := m.UpdateSettings("demo", map[string]interface{}{"token": " s3cret ", "retries": float64(5), "verbose": true}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetEnabled("demo", true); err != nil {
		t.Fatal(err)
	}
	if !m.HasNotifiers() || len(m.Tools()) != 2 {
		t.Fatal("enabled plugin is not active")
	}
	// 密钥不回显；空字符串保持原值；已启用时不能清空必填项；null 恢复默认。
	views := map[string]plugin.SettingView{}
	for _, v := range find(t, m, "demo").Settings {
		views[v.Key] = v
	}
	if views["token"].Value != "" || !views["token"].Set || views["retries"].Value != float64(5) || views["mode"].Value != "echo" {
		t.Fatalf("settings view: %+v", views)
	}
	if err := m.UpdateSettings("demo", map[string]interface{}{"token": "", "retries": nil}); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateSettings("demo", map[string]interface{}{"token": nil}); err == nil || !strings.Contains(err.Error(), "必填") {
		t.Fatalf("cleared required setting: %v", err)
	}
	// 状态与设置写入 data/plugins.json，重启后保留。
	m2 := plugin.New(dir, data)
	defer m2.Close()
	p2 := find(t, m2, "demo")
	if !p2.Enabled {
		t.Fatal("enabled state not persisted")
	}
	res, err := m2.Test("demo", "notify", "", nil)
	if err != nil || !res.OK {
		t.Fatalf("test: %+v %v", res, err)
	}
	got := res.Result.(map[string]interface{})["settings"].(map[string]interface{})
	if got["token"] != "s3cret" || got["retries"] != float64(3) || got["verbose"] != true {
		t.Fatalf("settings passed to plugin: %v", got)
	}
	if shown, _ := json.Marshal(res.Request); strings.Contains(string(shown), "s3cret") {
		t.Fatalf("secret leaked in test response: %s", shown)
	}
	if err := m2.SetEnabled("demo", false); err != nil {
		t.Fatal(err)
	}
	if m2.HasNotifiers() {
		t.Fatal("still active after disable")
	}
	// 插件目录删除后不再列出。
	_ = os.RemoveAll(filepath.Join(dir, "demo"))
	m2.Reload()
	if len(m2.List()) != 1 {
		t.Fatalf("list after delete: %+v", m2.List())
	}
}

func TestNotify(t *testing.T) {
	m, _, data := newManager(t)
	if n, err := m.Notify("alert", "x"); n != 0 || err != nil {
		t.Fatalf("notify with nothing enabled: %d %v", n, err)
	}
	enable(t, m, "echo")
	n, err := m.Notify("alert", "### 机房 · 一号\n<font color=\"warning\">● 离线</font> **10.0.0.1** 网关\n>位置：A01")
	if n != 1 || err != nil {
		t.Fatalf("notify: %d %v", n, err)
	}
	calls := plugintest.Calls(t, data, "demo")
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	req := calls[0]
	pl := req["payload"].(map[string]interface{})
	np := req["netping"].(map[string]interface{})
	if req["hook"] != "notify" || req["api_version"] != float64(1) || req["test"] != false || req["id"] == "" ||
		pl["source"] != "alert" || pl["title"] != "机房 · 一号" || strings.Contains(pl["text"].(string), "<font") ||
		!strings.Contains(pl["markdown"].(string), "<font") || np["site"] != "测试机房" || np["version"] != "9.9" {
		t.Fatalf("request: %v", req)
	}
	if _, err := time.Parse(time.RFC3339, req["time"].(string)); err != nil {
		t.Fatalf("time: %v", err)
	}
	pi := req["plugin"].(map[string]interface{})
	if pi["id"] != "demo" || pi["version"] != "1.2.3" || pi["data_dir"] != filepath.Join(data, "plugins", "demo") {
		t.Fatalf("plugin info: %v", pi)
	}
	// 插件报告失败。
	enable(t, m, "fail")
	if n, err := m.Notify("alert", "x"); n != 1 || err == nil || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "测试插件") {
		t.Fatalf("failing notify: %d %v", n, err)
	}
	runs := m.Runs(0)
	if len(runs) != 2 || runs[0].OK || runs[0].Error != "boom" || !runs[1].OK || runs[1].Message != "hook=notify" {
		t.Fatalf("runs: %+v", runs)
	}
	if lr := find(t, m, "demo").LastRun; lr == nil || lr.OK {
		t.Fatalf("last run: %+v", lr)
	}
}

func TestOutputHandling(t *testing.T) {
	m, _, _ := newManager(t)
	run := func(mode string) *plugin.TestResult {
		t.Helper()
		enable(t, m, mode)
		res, err := m.Test("demo", "alert", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if r := run("exit"); r.OK || !strings.Contains(r.Error, "退出码 3") || !strings.Contains(r.Error, "something broke") || !strings.Contains(r.Stderr, "line one") {
		t.Errorf("exit: %+v", r)
	}
	if r := run("raw"); !r.OK || r.Result != "plain text output" {
		t.Errorf("raw: %+v", r)
	}
	if r := run("silent"); !r.OK || r.Result != nil {
		t.Errorf("silent: %+v", r)
	}
	if r := run("big"); r.OK || !strings.Contains(r.Error, "标准输出超过") {
		t.Errorf("big: %+v", r)
	}
	if r := run("echo"); !r.OK || !r.Test || r.Hook != "alert" {
		t.Errorf("echo: %+v", r)
	}
	// 未声明的钩子、不存在的工具。
	if _, err := m.Test("demo", "nope", "", nil); err == nil {
		t.Error("tested undeclared hook")
	}
	if _, err := m.Test("demo", "", "nope", nil); err == nil {
		t.Error("tested unknown tool")
	}
	if _, err := m.Test("missing", "", "", nil); err == nil {
		t.Error("tested missing plugin")
	}
	for _, h := range plugin.Hooks {
		if plugin.SamplePayload(h) == nil {
			t.Errorf("no sample payload for %s", h)
		}
	}
}

func TestTimeout(t *testing.T) {
	base := t.TempDir()
	dir, data := filepath.Join(base, "plugins"), filepath.Join(base, "data")
	plugintest.Install(t, dir, "slow", "timeout_sec: 1\nhooks: [alert]\nsettings:\n  - {key: mode, default: sleep}\n")
	m := plugin.New(dir, data)
	defer m.Close()
	start := time.Now()
	res, err := m.Test("slow", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || !strings.Contains(res.Error, "超过 1 秒") {
		t.Fatalf("timeout: %+v", res)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("timeout took %v", d)
	}
}

func TestMissingProgram(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "plugins", "ghost")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte("id: ghost\ncommand: [\"./not-there\"]\nhooks: [alert]\n"), 0o644)
	m := plugin.New(filepath.Join(base, "plugins"), filepath.Join(base, "data"))
	defer m.Close()
	res, err := m.Test("ghost", "", "", nil)
	if err != nil || res.OK || !strings.Contains(res.Error, "找不到") {
		t.Fatalf("missing program: %+v %v", res, err)
	}
	_ = os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte("id: ghost\ncommand: [\"netping-no-such-program\"]\nhooks: [alert]\n"), 0o644)
	m.Reload()
	if res, _ := m.Test("ghost", "", "", nil); res.OK || !strings.Contains(res.Error, "PATH") {
		t.Fatalf("missing PATH program: %+v", res)
	}
}

func TestEmitAndTools(t *testing.T) {
	m, _, data := newManager(t)
	// 未启用时不分发、不能调用工具。
	m.Emit(plugin.HookAlert, map[string]interface{}{"events": []string{}})
	if _, err := m.CallTool("plugin_demo_lookup", nil, "t"); err == nil {
		t.Fatal("called tool of disabled plugin")
	}
	enable(t, m, "echo")
	for i := 0; i < 3; i++ {
		m.Emit(plugin.HookAlert, map[string]interface{}{"seq": i})
	}
	m.Emit("unknown-hook", nil)
	deadline := time.Now().Add(20 * time.Second)
	for len(m.Runs(0)) < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	calls := plugintest.Calls(t, data, "demo")
	if len(calls) != 3 {
		t.Fatalf("emitted calls = %d", len(calls))
	}
	for i, c := range calls { // 按顺序送达
		if c["hook"] != "alert" || c["payload"].(map[string]interface{})["seq"] != float64(i) {
			t.Fatalf("call %d: %v", i, c)
		}
	}
	res, err := m.CallTool("plugin_demo_lookup", map[string]json.RawMessage{"ip": json.RawMessage(`"10.0.0.1"`)}, "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	pl := res.(map[string]interface{})["payload"].(map[string]interface{})
	if pl["tool"] != "lookup" || pl["remote"] != "1.2.3.4" || pl["arguments"].(map[string]interface{})["ip"] != "10.0.0.1" {
		t.Fatalf("tool payload: %v", pl)
	}
	if _, err := m.CallTool("plugin_demo_nope", nil, "t"); err == nil {
		t.Fatal("called unknown tool")
	}
	enable(t, m, "silent")
	if res, err := m.CallTool("plugin_demo_lookup", nil, "t"); err != nil || res.(map[string]interface{})["ok"] != true {
		t.Fatalf("silent tool: %v %v", res, err)
	}
	enable(t, m, "fail")
	if _, err := m.CallTool("plugin_demo_lookup", nil, "t"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("failing tool: %v", err)
	}
	if tr, err := m.Test("demo", "", "lookup", nil); err != nil || tr.Hook != "tool" || tr.Tool != "lookup" {
		t.Fatalf("tool test: %+v %v", tr, err)
	}
}

func TestNilManager(t *testing.T) {
	var m *plugin.Manager
	m.Close()
	m.Emit(plugin.HookAlert, nil)
	if m.Dir() != "" || m.Reload() != 0 || len(m.List()) != 0 || len(m.Runs(0)) != 0 || len(m.Tools()) != 0 || m.HasNotifiers() {
		t.Fatal("nil manager returned data")
	}
	if n, err := m.Notify("alert", "x"); n != 0 || err != nil {
		t.Fatal("nil manager notified")
	}
	if m.SetEnabled("x", true) == nil || m.UpdateSettings("x", nil) == nil {
		t.Fatal("nil manager accepted changes")
	}
	if _, err := m.Test("x", "", "", nil); err == nil {
		t.Fatal("nil manager ran a test")
	}
	if _, err := m.CallTool("plugin_x_y", nil, ""); err == nil {
		t.Fatal("nil manager called a tool")
	}
}
