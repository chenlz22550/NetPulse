package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"netping/internal/config"
	"netping/internal/plugin"
	"netping/internal/plugin/plugintest"
)

func TestMain(m *testing.M) {
	plugintest.RunIfHelper()
	os.Exit(m.Run())
}

const testPluginManifest = `name: 资产库
version: 0.1.0
hooks: [notify]
tools:
  - name: lookup
    title: 查资产
    description: 按 IP 查询资产信息
    ai: true
    input_schema: {type: object, properties: {ip: {type: string}}}
  - name: silent_lookup
    description: 不开放给 AI 的查询
  - name: open_ticket
    description: 创建工单
    write: true
settings:
  - {key: token, label: 令牌, type: secret, required: true}
  - {key: mode, type: string}
`

func withPlugins(t *testing.T, s *Server) {
	t.Helper()
	dir := filepath.Join(s.BaseDir, "plugins")
	plugintest.Install(t, dir, "assets", testPluginManifest)
	s.Plugins = plugin.New(dir, filepath.Join(s.BaseDir, "data"))
	s.PluginDocMD = []byte("# 插件开发指南")
	t.Cleanup(s.Plugins.Close)
}

func toolNames(t *testing.T, out map[string]interface{}) map[string]map[string]interface{} {
	t.Helper()
	names := map[string]map[string]interface{}{}
	for _, x := range out["result"].(map[string]interface{})["tools"].([]interface{}) {
		tl := x.(map[string]interface{})
		names[tl["name"].(string)] = tl
	}
	return names
}

func TestPluginAPIAndTools(t *testing.T) {
	s, ts := newTestServer(t)
	// 未配置插件管理器时接口仍可用，返回空列表。
	var empty struct {
		Available bool          `json:"available"`
		Plugins   []plugin.Info `json:"plugins"`
	}
	if err := json.Unmarshal(apiRequest(t, ts, http.MethodGet, "/api/plugins", "", 200), &empty); err != nil || empty.Available || len(empty.Plugins) != 0 {
		t.Fatalf("no manager: %+v %v", empty, err)
	}
	apiRequest(t, ts, http.MethodPost, "/api/plugins/reload", "", 400)

	withPlugins(t, s)
	var list struct {
		Dir     string             `json:"dir"`
		Plugins []plugin.Info      `json:"plugins"`
		Runs    []plugin.RunRecord `json:"runs"`
	}
	get := func(method, path, body string, want int) {
		t.Helper()
		data := apiRequest(t, ts, method, path, body, want)
		if want == 200 {
			list.Plugins, list.Runs = nil, nil
			if err := json.Unmarshal(data, &list); err != nil {
				t.Fatalf("%s %s: %v: %s", method, path, err, data)
			}
		}
	}
	get(http.MethodGet, "/api/plugins", "", 200)
	if len(list.Plugins) != 1 || list.Plugins[0].Enabled || list.Dir == "" {
		t.Fatalf("list: %+v", list)
	}
	// 未启用：MCP 看不到插件工具。
	_, out := rpc(t, ts, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if names := toolNames(t, out); len(names) != len(mcpTools) {
		t.Fatalf("tools with disabled plugin = %d", len(names))
	}
	// 必填项未填不能启用；未知插件、未知设置项报错。
	get(http.MethodPut, "/api/plugins/assets", `{"enabled":true}`, 400)
	get(http.MethodPut, "/api/plugins/nope", `{"enabled":true}`, 400)
	get(http.MethodPut, "/api/plugins/assets", `{"settings":{"bogus":1}}`, 400)
	get(http.MethodPut, "/api/plugins/assets", `{"settings":{"token":"tk-123"},"enabled":true}`, 200)
	if !list.Plugins[0].Enabled {
		t.Fatalf("not enabled: %+v", list.Plugins[0])
	}
	if raw := string(apiRequest(t, ts, http.MethodGet, "/api/plugins", "", 200)); strings.Contains(raw, "tk-123") {
		t.Fatalf("secret returned by API: %s", raw)
	}
	// 测试运行：钩子与工具。
	var tr plugin.TestResult
	if err := json.Unmarshal(apiRequest(t, ts, http.MethodPost, "/api/plugins/assets/test", `{"hook":"notify"}`, 200), &tr); err != nil || !tr.OK || tr.Hook != "notify" {
		t.Fatalf("test notify: %+v %v", tr, err)
	}
	if err := json.Unmarshal(apiRequest(t, ts, http.MethodPost, "/api/plugins/assets/test", `{"tool":"lookup","arguments":{"ip":"10.0.0.1"}}`, 200), &tr); err != nil || !tr.OK || tr.Tool != "lookup" {
		t.Fatalf("test tool: %+v %v", tr, err)
	}
	apiRequest(t, ts, http.MethodPost, "/api/plugins/assets/test", `{"hook":"alert"}`, 400)
	get(http.MethodPost, "/api/plugins/reload", "", 200)
	if len(list.Runs) != 2 {
		t.Fatalf("runs: %+v", list.Runs)
	}
	if doc := string(apiRequest(t, ts, http.MethodGet, "/api/plugins/guide", "", 200)); !strings.Contains(doc, "插件开发指南") {
		t.Fatalf("guide: %s", doc)
	}
	apiRequest(t, ts, http.MethodDelete, "/api/plugins/assets", "", 404)

	// 已启用：MCP 列出并可调用插件工具。
	_, out = rpc(t, ts, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	names := toolNames(t, out)
	if len(names) != len(mcpTools)+3 {
		t.Fatalf("tools with plugin = %d", len(names))
	}
	lk, tk := names["plugin_assets_lookup"], names["plugin_assets_open_ticket"]
	if lk == nil || tk == nil || !strings.Contains(lk["description"].(string), "[插件 资产库]") ||
		lk["annotations"].(map[string]interface{})["readOnlyHint"] != true || tk["annotations"].(map[string]interface{})["readOnlyHint"] != false {
		t.Fatalf("plugin tools: %v %v", lk, tk)
	}
	txt, isErr := call(t, ts, "plugin_assets_lookup", map[string]interface{}{"ip": "10.0.0.9"})
	if isErr || !strings.Contains(txt, "10.0.0.9") || !strings.Contains(txt, `"tool": "lookup"`) {
		t.Fatalf("call plugin tool: %s", txt)
	}
	if txt, isErr := call(t, ts, "plugin_assets_open_ticket", map[string]interface{}{}); isErr {
		t.Fatalf("call write tool: %s", txt)
	}
	// MCP 信息页与资源。
	if info := string(apiRequest(t, ts, http.MethodGet, "/api/mcp", "", 200)); !strings.Contains(info, "plugin_assets_lookup") {
		t.Fatalf("mcp info misses plugin tools")
	}
	_, out = rpc(t, ts, `{"jsonrpc":"2.0","id":3,"method":"resources/read","params":{"uri":"netping://docs/plugin-development.md"}}`)
	if !strings.Contains(fmtJSON(out), "插件开发指南") {
		t.Fatalf("guide resource: %v", out)
	}

	// AI 诊断：只开放 ai: true 的只读工具。
	ai := s.aiDiagnoseNames()
	if len(ai) != len(aiToolNames)+1 || !inList(ai, "plugin_assets_lookup") || inList(aiToolNames, "plugin_assets_lookup") {
		t.Fatalf("ai names: %v", ai[len(aiToolNames):])
	}
	if defs := s.aiToolDefs(ai); len(defs) != len(ai) {
		t.Fatalf("ai tool defs = %d, want %d", len(defs), len(ai))
	}
	if title, res := s.runAIToolIn(ai, "plugin_assets_lookup", `{"ip":"10.0.0.7"}`, "ai"); title == "" || !strings.Contains(res, "10.0.0.7") {
		t.Fatalf("ai plugin tool: %s %s", title, res)
	}
	for _, name := range []string{"plugin_assets_open_ticket", "plugin_assets_silent_lookup"} {
		if _, res := s.runAIToolIn(ai, name, `{}`, "ai"); !strings.Contains(res, "不允许") {
			t.Fatalf("ai ran %s: %s", name, res)
		}
	}
	if _, res := s.runAIToolIn(append(ai, "plugin_assets_open_ticket"), "plugin_assets_open_ticket", `{}`, "ai"); !strings.Contains(res, "不允许") {
		t.Fatalf("ai ran a write tool: %s", res)
	}

	// MCP 只读模式隐藏写工具。
	c := s.Cfg.Get()
	c.MCP.ReadOnly = true
	if err := s.Cfg.Update(c); err != nil {
		t.Fatal(err)
	}
	_, out = rpc(t, ts, `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`)
	if names := toolNames(t, out); names["plugin_assets_open_ticket"] != nil || names["plugin_assets_lookup"] == nil {
		t.Fatalf("read-only tools wrong")
	}
	if txt, isErr := call(t, ts, "plugin_assets_open_ticket", map[string]interface{}{}); !isErr || !strings.Contains(txt, "只读") {
		t.Fatalf("write tool in read-only mode: %s", txt)
	}

	// 停用后工具消失。
	get(http.MethodPut, "/api/plugins/assets", `{"enabled":false}`, 200)
	_, out = rpc(t, ts, `{"jsonrpc":"2.0","id":5,"method":"tools/list"}`)
	if names := toolNames(t, out); names["plugin_assets_lookup"] != nil {
		t.Fatalf("tool still listed after disable")
	}
	if s.lookupTool("plugin_assets_lookup") != nil || len(s.aiDiagnoseNames()) != len(aiToolNames) {
		t.Fatalf("tool still resolvable after disable")
	}
}

// 只启用了通知插件、没有内置渠道时也允许开启告警。
func TestAlertEnableWithNotifyPlugin(t *testing.T) {
	s, _ := newTestServer(t)
	withPlugins(t, s)
	config.SetExtraChannels(s.Plugins.HasNotifiers)
	t.Cleanup(func() { config.SetExtraChannels(nil) })
	c := s.Cfg.Get()
	c.Alert.Enabled = true
	if err := s.Cfg.Update(c); err == nil || !strings.Contains(err.Error(), "通知类插件") {
		t.Fatalf("enabled alert without any channel: %v", err)
	}
	if err := s.Plugins.UpdateSettings("assets", map[string]interface{}{"token": "x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Plugins.SetEnabled("assets", true); err != nil {
		t.Fatal(err)
	}
	if err := s.Cfg.Update(c); err != nil {
		t.Fatalf("alert with notify plugin: %v", err)
	}
}
