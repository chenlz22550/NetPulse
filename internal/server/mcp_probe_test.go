package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"testing"

	"netping/internal/discovery"
)

var probeToolList = []string{"get_switch_ports", "tcp_check", "arp_lookup", "trace_route", "dns_lookup"}

// listTools 通过 MCP tools/list 返回 名称 → 工具定义。
func listTools(t *testing.T, ts *httptest.Server) map[string]map[string]interface{} {
	t.Helper()
	_, out := rpc(t, ts, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	m := map[string]map[string]interface{}{}
	for _, x := range out["result"].(map[string]interface{})["tools"].([]interface{}) {
		tl := x.(map[string]interface{})
		m[tl["name"].(string)] = tl
	}
	return m
}

func TestMCPProbeTools(t *testing.T) {
	s, ts := newTestServer(t)

	// 1. tools/list：新工具全部列出，只读、带参数说明；向外发包的标记 openWorldHint
	tools := listTools(t, ts)
	openWorld := map[string]bool{"tcp_check": true, "trace_route": true, "dns_lookup": true, "ping_now": true}
	for _, name := range append(probeToolList, "ping_now") {
		tl := tools[name]
		if tl == nil {
			t.Fatalf("tools/list 缺少 %s", name)
		}
		ann := tl["annotations"].(map[string]interface{})
		if ann["readOnlyHint"] != true || ann["destructiveHint"] != false || ann["openWorldHint"] != openWorld[name] {
			t.Fatalf("%s annotations: %v", name, ann)
		}
		if props := tl["inputSchema"].(map[string]interface{})["properties"]; props == nil {
			t.Fatalf("%s 没有参数定义", name)
		}
	}
	if tl := tools["set_ai_settings"]; tl == nil || tl["annotations"].(map[string]interface{})["readOnlyHint"] != false {
		t.Fatalf("set_ai_settings: %v", tl)
	}

	// 2. tools/call：通过 MCP 实际调用每个新工具
	_, addr := fakePortAgent(t)
	if txt, isErr := call(t, ts, "get_switch_ports", map[string]interface{}{"switch": "access", "sample_sec": 0}); !isErr || !strings.Contains(txt, "团体名") {
		t.Fatalf("no snmp: %s", txt)
	}
	call(t, ts, "set_locate_settings", map[string]interface{}{"community": "s3cret-ro", "switches": map[string]interface{}{"access": map[string]interface{}{"host": addr}}})
	txt, isErr := call(t, ts, "get_switch_ports", map[string]interface{}{"ip": "192.168.50.29", "sample_sec": 0})
	if isErr || !strings.Contains(txt, "半双工") || strings.Contains(txt, "s3cret") {
		t.Fatalf("get_switch_ports: %s", txt)
	}

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	if txt, isErr := call(t, ts, "tcp_check", map[string]interface{}{"ip": "127.0.0.1", "ports": []int{port}}); isErr || !strings.Contains(txt, `"open"`) {
		t.Fatalf("tcp_check: %s", txt)
	}
	if txt, isErr := call(t, ts, "tcp_check", map[string]interface{}{"ip": "224.0.0.1"}); !isErr || !strings.Contains(txt, "组播") {
		t.Fatalf("tcp_check bad ip should be isError: %s", txt)
	}

	old := readARP
	defer func() { readARP = old }()
	readARP = func() ([]discovery.ARPEntry, error) {
		return []discovery.ARPEntry{{IP: "192.168.50.29", MAC: "02:00:00:00:00:12"}}, nil
	}
	if txt, isErr := call(t, ts, "arp_lookup", map[string]interface{}{"ip": "192.168.50.29", "refresh": false}); isErr || !strings.Contains(txt, `"mac_matches_registered": true`) {
		t.Fatalf("arp_lookup: %s", txt)
	}
	if txt, isErr := call(t, ts, "dns_lookup", map[string]interface{}{"name": "localhost"}); isErr || !strings.Contains(txt, `"ok": true`) {
		t.Fatalf("dns_lookup: %s", txt)
	}
	if txt, isErr := call(t, ts, "trace_route", map[string]interface{}{"ip": "127.0.0.1", "max_hops": 2, "timeout_ms": 500}); isErr || !strings.Contains(txt, `"hops"`) {
		t.Fatalf("trace_route: %s", txt)
	}

	// 3. set_ai_settings / get_settings：可开关自检，不能改 Key 和接口地址，不泄露 Key
	cf := s.Cfg.Get()
	cf.AI.APIKey = "sk-secret-999"
	_ = s.Cfg.Update(cf)
	if txt, isErr := call(t, ts, "set_ai_settings", map[string]interface{}{"protocol": "gemini", "self_check": false, "thinking": true}); isErr || !strings.Contains(txt, `"self_check": false`) || !strings.Contains(txt, `"protocol": "gemini"`) || strings.Contains(txt, "sk-secret") {
		t.Fatalf("set_ai_settings: %s", txt)
	}
	if c := s.Cfg.Get().AI; c.SelfCheckOn() || !c.Thinking || c.Protocol != "gemini" {
		t.Fatalf("config not updated: %+v", c)
	}
	if txt, _ := call(t, ts, "get_settings", map[string]interface{}{}); !strings.Contains(txt, `"self_check": false`) || strings.Contains(txt, "sk-secret") {
		t.Fatalf("get_settings: %s", txt)
	}
	for _, bad := range []map[string]interface{}{{"api_key": "sk-x"}, {"base_url": "http://evil"}, {"protocol": "bad-proto"}, {"timeout_sec": 5}, {}} {
		if txt, isErr := call(t, ts, "set_ai_settings", bad); !isErr {
			t.Fatalf("%v should fail: %s", bad, txt)
		}
	}
	if c := s.Cfg.Get().AI; c.APIKey != "sk-secret-999" || c.BaseURL != "https://api.deepseek.com" {
		t.Fatalf("key/base_url changed: %+v", c)
	}

	// 4. 只读模式：探测类工具照常可见、可调用；set_ai_settings 隐藏且被拒绝
	cf = s.Cfg.Get()
	cf.MCP.ReadOnly = true
	_ = s.Cfg.Update(cf)
	tools = listTools(t, ts)
	for _, name := range probeToolList {
		if tools[name] == nil {
			t.Fatalf("只读模式下缺少 %s", name)
		}
	}
	if tools["set_ai_settings"] != nil {
		t.Fatal("只读模式下不应列出 set_ai_settings")
	}
	if txt, isErr := call(t, ts, "tcp_check", map[string]interface{}{"ip": "127.0.0.1", "ports": []int{port}}); isErr || !strings.Contains(txt, `"open"`) {
		t.Fatalf("read-only tcp_check: %s", txt)
	}
	if txt, isErr := call(t, ts, "get_switch_ports", map[string]interface{}{"switch": "access", "port": "1", "sample_sec": 0}); isErr {
		t.Fatalf("read-only get_switch_ports: %s", txt)
	}
	if txt, isErr := call(t, ts, "set_ai_settings", map[string]interface{}{"self_check": true}); !isErr || !strings.Contains(txt, "只读") {
		t.Fatalf("read-only set_ai_settings: %s", txt)
	}

	// 5. 网页「MCP」页的工具清单：新工具标为实时探测
	c := newClient()
	authPost(t, c, ts, "/api/auth/login", `{"password":"`+testPassword+`"}`)
	resp, err := c.Get(ts.URL + "/api/mcp")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var info struct {
		Tools []struct {
			Name  string `json:"name"`
			Write bool   `json:"write"`
			Probe bool   `json:"probe"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatal(err)
	}
	probe := map[string]bool{}
	for _, tl := range info.Tools {
		probe[tl.Name] = tl.Probe
	}
	for _, name := range probeToolList {
		if !probe[name] {
			t.Fatalf("MCP 页工具清单中 %s 未标为实时探测: %s", name, fmt.Sprint(probe))
		}
	}
}
