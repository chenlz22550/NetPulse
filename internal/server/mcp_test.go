package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"netping/internal/alert"
	"netping/internal/config"
	"netping/internal/discovery"
	"netping/internal/locate"
	"netping/internal/monitor"
	"netping/internal/store"
	"netping/internal/topology"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	topo, err := topology.Open(filepath.Join(dir, "topology.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	st, _ := store.New(filepath.Join(dir, "history"))
	mon := monitor.New(st, func() int { return 5 })
	mon.SetPaused(true) // 测试中不实际 ping
	al := alert.NewEngine(cfg.Get, mon, "")
	c := cfg.Get()
	c.Auth = config.Auth{Password: testPassword}
	if err := cfg.Update(c); err != nil {
		t.Fatal(err)
	}
	s := &Server{Cfg: cfg, Topo: topo, Mon: mon, Alerts: al, Store: st, DocsMD: []byte("# docs"), SchemaJSON: []byte("{}"), BaseDir: dir,
		Disc: discovery.New(cfg, topo, mon, filepath.Join(dir, "data")), Loc: locate.New(cfg, topo, filepath.Join(dir, "data"))}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.handleMCP)
	mux.HandleFunc("/api/", s.routeAPI)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("page")) })
	ts := httptest.NewServer(s.withAuth(mux))
	t.Cleanup(ts.Close)
	return s, ts
}

const testPassword = "test-password"

// rpc 默认用 HTTP Basic 带上网页登录密码，hdr 中的 Authorization 会覆盖它。
func rpc(t *testing.T, ts *httptest.Server, body string, hdr ...string) (int, map[string]interface{}) {
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(body))
	req.SetBasicAuth("", testPassword)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func call(t *testing.T, ts *httptest.Server, name string, args interface{}) (string, bool) {
	a, _ := json.Marshal(args)
	_, out := rpc(t, ts, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+string(a)+`}}`)
	res, ok := out["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("%s: no result: %v", name, out)
	}
	content := res["content"].([]interface{})[0].(map[string]interface{})["text"].(string)
	return content, res["isError"] == true
}

func TestMCPHandshakeAndList(t *testing.T) {
	_, ts := newTestServer(t)
	code, out := rpc(t, ts, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	res := out["result"].(map[string]interface{})
	if code != 200 || res["protocolVersion"] != "2025-03-26" || res["serverInfo"].(map[string]interface{})["name"] != "netPing" {
		t.Fatalf("initialize: %d %v", code, out)
	}
	if code, _ := rpc(t, ts, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != http.StatusAccepted {
		t.Fatalf("notification status %d", code)
	}
	_, out = rpc(t, ts, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools := out["result"].(map[string]interface{})["tools"].([]interface{})
	if len(tools) != len(mcpTools) {
		t.Fatalf("tools = %d", len(tools))
	}
	_, out = rpc(t, ts, `{"jsonrpc":"2.0","id":3,"method":"nope"}`)
	if out["error"].(map[string]interface{})["code"].(float64) != -32601 {
		t.Fatalf("unknown method: %v", out)
	}
	greq, _ := http.NewRequest(http.MethodGet, ts.URL+"/mcp", nil)
	greq.SetBasicAuth("", testPassword)
	resp, _ := http.DefaultClient.Do(greq)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status %d", resp.StatusCode)
	}
	// 资源与提示词
	_, out = rpc(t, ts, `{"jsonrpc":"2.0","id":4,"method":"resources/read","params":{"uri":"netping://topology.yaml"}}`)
	if !strings.Contains(fmtJSON(out), "192.168.50.1") {
		t.Fatal("resource read failed")
	}
	_, out = rpc(t, ts, `{"jsonrpc":"2.0","id":5,"method":"prompts/get","params":{"name":"generate_topology","arguments":{"mac_table":"x","arp_table":"y"}}}`)
	if !strings.Contains(fmtJSON(out), "replace_topology_yaml") {
		t.Fatalf("prompt: %v", out)
	}
}

func fmtJSON(v interface{}) string { b, _ := json.Marshal(v); return string(b) }

func TestMCPReadTools(t *testing.T) {
	_, ts := newTestServer(t)
	text, isErr := call(t, ts, "list_devices", map[string]interface{}{"switch": "access", "limit": 3})
	if isErr || !strings.Contains(text, `"total": 22`) || !strings.Contains(text, `"returned": 3`) {
		t.Fatalf("list_devices: %s", text)
	}
	text, isErr = call(t, ts, "get_device", map[string]string{"ip": "192.168.50.1"})
	if isErr || !strings.Contains(text, "示例核心交换机 Gi0/11") {
		t.Fatalf("get_device: %s", text)
	}
	text, isErr = call(t, ts, "get_history", map[string]string{"ip": "192.168.50.1", "range": "24h"})
	if isErr || !strings.Contains(text, "没有数据") {
		t.Fatalf("get_history: %s", text)
	}
	if text, isErr = call(t, ts, "get_device", map[string]string{"ip": "10.9.9.9"}); !isErr {
		t.Fatalf("expected error: %s", text)
	}
	text, _ = call(t, ts, "get_overview", map[string]string{})
	if !strings.Contains(text, `"devices_with_alert": 0`) {
		t.Fatalf("overview: %s", text)
	}
}

func TestMCPWriteTools(t *testing.T) {
	s, ts := newTestServer(t)
	// 新建
	text, isErr := call(t, ts, "upsert_device", map[string]interface{}{"ip": "192.168.50.77", "name": "新服务器", "attach": map[string]string{"switch": "access", "port": "9"}})
	if isErr || !strings.Contains(text, `"created"`) {
		t.Fatalf("create: %s", text)
	}
	// 局部修改：只改端口与告警，名称保持不变
	text, isErr = call(t, ts, "upsert_device", map[string]interface{}{"ip": "192.168.50.77", "attach": map[string]string{"port": "10"}, "monitor": map[string]bool{"alert": true}})
	if isErr || !strings.Contains(text, `"updated"`) {
		t.Fatalf("update: %s", text)
	}
	topo, _ := s.Topo.Snapshot()
	n := topo.FindNode("n-192-168-50-77")
	if n == nil || n.Name != "新服务器" || n.Attach.Switch != "access" || n.Attach.Port != "10" || !n.Monitor.Alert {
		t.Fatalf("patch result: %+v", n)
	}
	// 校验失败不生效
	text, isErr = call(t, ts, "upsert_device", map[string]interface{}{"id": "n-192-168-50-77", "attach": map[string]string{"switch": "nope"}})
	if !isErr || !strings.Contains(text, "不存在") {
		t.Fatalf("expected validation error: %s", text)
	}
	if text, isErr = call(t, ts, "upsert_device", map[string]interface{}{"ip": "192.168.50.78", "adress": "x"}); !isErr || !strings.Contains(text, "adress") {
		t.Fatalf("unknown field should fail: %s", text)
	}
	// 批量告警
	text, isErr = call(t, ts, "set_alerting", map[string]interface{}{"switch": "access", "enabled": true})
	if isErr || !strings.Contains(text, `"changed": 21`) {
		t.Fatalf("set_alerting: %s", text)
	}
	// 时段
	if text, isErr = call(t, ts, "set_schedule", map[string]interface{}{"enabled": true, "windows": []map[string]string{{"start": "08:00", "end": "08:00"}}}); !isErr {
		t.Fatalf("bad schedule accepted: %s", text)
	}
	text, isErr = call(t, ts, "set_schedule", map[string]interface{}{"enabled": true, "windows": []map[string]interface{}{{"days": []int{1, 2, 3, 4, 5}, "start": "08:00", "end": "18:00"}}})
	if isErr || !strings.Contains(text, "周一、二、三、四、五 08:00-18:00") {
		t.Fatalf("set_schedule: %s", text)
	}
	// 整体替换：dry_run 只返回差异
	yml, _ := s.Topo.YAML()
	edited := strings.Replace(string(yml), "ip: 192.168.50.77", "ip: 192.168.50.79", 1)
	text, isErr = call(t, ts, "replace_topology_yaml", map[string]interface{}{"yaml": edited})
	if isErr || !strings.Contains(text, `"node:n-192-168-50-77"`) || !strings.Contains(text, `"dry_run": true`) {
		t.Fatalf("dry run: %s", text)
	}
	topo, _ = s.Topo.Snapshot()
	if topo.FindNode("n-192-168-50-77").IP != "192.168.50.77" {
		t.Fatal("dry run must not write")
	}
	// 删除
	if text, isErr = call(t, ts, "delete_device", map[string]string{"ip": "192.168.50.77"}); isErr {
		t.Fatalf("delete: %s", text)
	}
	if text, isErr = call(t, ts, "delete_device", map[string]string{"id": "access"}); !isErr {
		t.Fatalf("delete switch via delete_device should fail: %s", text)
	}
}

func TestMCPReadOnlyAndToken(t *testing.T) {
	s, ts := newTestServer(t)
	c := s.Cfg.Get()
	c.MCP.ReadOnly = true
	c.MCP.Token = "secret"
	c.Auth = config.Auth{Username: "admin", Password: "pw"}
	if err := s.Cfg.Update(c); err != nil {
		t.Fatal(err)
	}
	if code, _ := rpc(t, ts, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	code, out := rpc(t, ts, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "Authorization", "Bearer secret")
	if code != 200 {
		t.Fatalf("token: %d", code)
	}
	for _, tl := range out["result"].(map[string]interface{})["tools"].([]interface{}) {
		if strings.HasPrefix(tl.(map[string]interface{})["name"].(string), "upsert") {
			t.Fatal("write tool visible in read-only mode")
		}
	}
	a, _ := json.Marshal(map[string]interface{}{"name": "set_alerting", "arguments": map[string]interface{}{"ids": []string{"access"}, "enabled": true}})
	_, out = rpc(t, ts, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":`+string(a)+`}`, "Authorization", "Bearer secret")
	if !strings.Contains(fmtJSON(out), "只读") {
		t.Fatalf("read-only call: %v", out)
	}
	// 批量请求
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewBufferString(`[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":2,"method":"ping"}]`))
	req.Header.Set("Authorization", "Bearer secret")
	resp, _ := http.DefaultClient.Do(req)
	var arr []interface{}
	_ = json.NewDecoder(resp.Body).Decode(&arr)
	resp.Body.Close()
	if len(arr) != 2 {
		t.Fatalf("batch responses = %d", len(arr))
	}
	// 跨站 Origin 被拒绝
	if code, _ := rpc(t, ts, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "Authorization", "Bearer secret", "Origin", "http://evil.example"); code != http.StatusForbidden {
		t.Fatalf("origin: %d", code)
	}
}
