package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"netping/internal/discovery"
)

// stubProbe 让 probe_subnet 返回固定结果，不实际发包。
func stubProbe(t *testing.T, hosts []discovery.Host) *[]string {
	var got []string
	old := probeSubnets
	probeSubnets = func(s *Server, subnets []string, _ string) ([]discovery.Host, error) {
		got = append(got, subnets...)
		tp, _ := s.Topo.Snapshot()
		hs := append([]discovery.Host(nil), hosts...)
		discovery.MarkKnown(hs, tp, nil)
		return hs, nil
	}
	t.Cleanup(func() { probeSubnets = old })
	return &got
}

func TestProbeScopeLimited(t *testing.T) {
	s, ts := newTestServer(t)
	for _, ok := range []string{"192.168.7.0/24", "10.1.2.0/24", "172.20.0.0/22", "192.168.101.0/24"} {
		if _, err := s.checkProbeSubnet(ok); err != nil {
			t.Errorf("%s 应允许：%v", ok, err)
		}
	}
	for _, bad := range []string{"8.8.8.0/24", "1.1.1.1/32", "172.32.0.0/24", "abc"} {
		if _, err := s.checkProbeSubnet(bad); err == nil {
			t.Errorf("%s 应拒绝", bad)
		}
	}
	if _, err := s.checkProbeIP("223.5.5.5"); err == nil {
		t.Error("公网地址应拒绝")
	}
	// 配置的扫描网段即使不是私有地址也允许（有的机房内网用公网地址段）
	c := s.Cfg.Get()
	c.Discovery.Jobs[0].Subnets = []string{"58.20.1.0/24"}
	_ = s.Cfg.Update(c)
	if _, err := s.checkProbeSubnet("58.20.1.0/25"); err != nil {
		t.Errorf("已配置网段应允许：%v", err)
	}
	// MCP 调用同样受限
	text, isErr := call(t, ts, "probe_subnet", map[string]interface{}{"subnets": []string{"8.8.8.0/24"}})
	if !isErr || !strings.Contains(text, "不会探测") {
		t.Fatalf("MCP probe_subnet 应拒绝公网：%s", text)
	}
	text, isErr = call(t, ts, "fingerprint_host", map[string]interface{}{"ip": "1.1.1.1"})
	if !isErr || !strings.Contains(text, "不会探测") {
		t.Fatalf("MCP fingerprint_host 应拒绝公网：%s", text)
	}
}

func TestProbeSubnetTool(t *testing.T) {
	s, ts := newTestServer(t)
	got := stubProbe(t, []discovery.Host{
		{IP: "192.168.7.10", MAC: "AA:BB:CC:00:00:10", Ping: true, ARP: true, Hostname: "NPI0A1B2C"},
		{IP: "192.168.101.232", Ping: true},
	})
	text, isErr := call(t, ts, "probe_subnet", map[string]interface{}{"subnets": []string{"192.168.7.0/24"}})
	if isErr {
		t.Fatal(text)
	}
	var out map[string]interface{}
	_ = json.Unmarshal([]byte(text), &out)
	if len(*got) != 1 || (*got)[0] != "192.168.7.0/24" {
		t.Fatalf("subnets: %v", *got)
	}
	nw := out["new"].([]interface{})
	if len(nw) != 1 || nw[0].(map[string]interface{})["ip"] != "192.168.7.10" {
		t.Fatalf("new: %v", out["new"])
	}
	if out["counts"].(map[string]interface{})["registered"].(float64) != 1 {
		t.Fatalf("counts: %v", out["counts"])
	}
	if h, ok := s.observed("192.168.7.10"); !ok || h.MAC != "AA:BB:CC:00:00:10" {
		t.Fatalf("应记住探测到的地址：%+v", h)
	}
	// 网络线索
	text, isErr = call(t, ts, "get_network_context", map[string]interface{}{})
	if isErr || !strings.Contains(text, "candidate_subnets") || !strings.Contains(text, "192.168.101.0/24") {
		t.Fatalf("get_network_context: %s", text)
	}
}

func TestInferHints(t *testing.T) {
	r := &fpResult{IP: "192.168.7.5", Online: true, Open: []string{"80/HTTP", "9100/打印机 RAW"},
		HTTP: []map[string]any{{"port": 80, "title": "HP LaserJet MFP M428"}}}
	h := strings.Join(inferHints(r), "；")
	if !strings.Contains(h, "打印机") {
		t.Fatalf("应识别打印机：%s", h)
	}
	r = &fpResult{IP: "192.168.7.6", Online: true, Open: []string{"22/SSH", "23/Telnet"},
		SNMP: map[string]string{"sys_descr": "H3C Comware Platform Software, H3C S5120-28P-SI"}}
	h = strings.Join(inferHints(r), "；")
	if !strings.Contains(h, "交换机") || !strings.Contains(h, "SNMP") {
		t.Fatalf("应识别交换机：%s", h)
	}
	r = &fpResult{IP: "192.168.1.1", Online: true, Open: []string{"80/HTTP"}, HTTP: []map[string]any{{"port": 80, "title": "中国电信智能网关"}}}
	if h = strings.Join(inferHints(r), "；"); !strings.Contains(h, "光猫") {
		t.Fatalf("应识别光猫：%s", h)
	}
	r = &fpResult{IP: "192.168.7.7", Online: true, Open: []string{"554/RTSP 视频流", "8000/HTTP(备用)/海康"}}
	if h = strings.Join(inferHints(r), "；"); !strings.Contains(h, "海康") {
		t.Fatalf("应识别海康：%s", h)
	}
}

// fakeDiscLLM 第一次请求时调用 probe_subnet，第二次给出最终清单。
func fakeDiscLLM(t *testing.T, final string) (*httptest.Server, *[]map[string]interface{}) {
	var mu sync.Mutex
	var reqs []map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		reqs = append(reqs, body)
		n := len(reqs)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(v string) { fmt.Fprintf(w, "data: %s\n\n", v); w.(http.Flusher).Flush() }
		if n == 1 {
			send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_p","type":"function","function":{"name":"probe_subnet","arguments":"{\"subnets\":[\"192.168.7.0/24\"]}"}}]}}]}`)
		} else {
			chunk, _ := json.Marshal(map[string]interface{}{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{"content": final}}}})
			send(string(chunk))
		}
		send(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":10,"total_tokens":20}}`)
		send(`[DONE]`)
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func TestAIDiscoverFlow(t *testing.T) {
	s, ts := newTestServer(t)
	stubProbe(t, []discovery.Host{
		{IP: "192.168.7.10", MAC: "AA:BB:CC:00:00:10", Ping: true, ARP: true, Hostname: "NPI0A1B2C"},
		{IP: "192.168.7.2", MAC: "AA:BB:CC:00:00:02", Ping: true, ARP: true},
		{IP: "192.168.101.232", Ping: true},
	})
	final := "**总结**：探测了 192.168.7.0/24，发现 2 台新设备。\n```json\n" + `{
"devices":[
 {"ip":"192.168.7.10","kind":"node","name":"财务部打印机","category":"打印机","alert":false,"switch":"access","port":"5","confidence":"high","evidence":"9100 开放","mac":"FF:FF:FF:FF:FF:FF"},
 {"ip":"192.168.7.2","kind":"switch","name":"二楼接入交换机","alert":true,"confidence":"medium","evidence":"SNMP Comware"},
 {"ip":"192.168.7.99","kind":"node","name":"编造的设备"},
 {"ip":"192.168.101.232","kind":"node","name":"已登记"},
 {"ip":"192.168.7.10","kind":"node","name":"重复"}],
"scan_jobs":[{"name":"二楼","subnets":["192.168.7.0/24"],"interval_min":30,"auto_add":true,"reason":"无任务覆盖"},
 {"name":"已覆盖","subnets":["192.168.101.0/24"]},{"name":"公网","subnets":["8.8.8.0/24"]}],
"notes":["二楼可能有私接路由器"]}` + "\n```"
	llmSrv, reqs := fakeDiscLLM(t, final)
	cf := s.Cfg.Get()
	cf.AI.BaseURL, cf.AI.APIKey = llmSrv.URL, "sk-test-123456"
	_ = s.Cfg.Update(cf)
	c := newClient()
	authPost(t, c, ts, "/api/auth/login", `{"password":"`+testPassword+`"}`)

	resp, err := c.Post(ts.URL+"/api/ai/discover", "application/json", strings.NewReader(`{"hint":"找二楼的设备"}`))
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]interface{}
	var tools []string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var ev map[string]interface{}
		_ = json.Unmarshal(sc.Bytes(), &ev)
		switch ev["t"] {
		case "result":
			result = ev["result"].(map[string]interface{})
		case "tool":
			tools = append(tools, ev["name"].(string))
		case "error":
			t.Fatalf("error: %v", ev["error"])
		}
	}
	resp.Body.Close()
	if len(tools) != 1 || tools[0] != "probe_subnet" {
		t.Fatalf("tools: %v", tools)
	}
	// 模型只拿到只读工具，且第一条消息里有网络线索与用户要求
	first := (*reqs)[0]
	names := ""
	for _, tl := range first["tools"].([]interface{}) {
		names += tl.(map[string]interface{})["function"].(map[string]interface{})["name"].(string) + ","
	}
	if !strings.Contains(names, "probe_subnet") || !strings.Contains(names, "fingerprint_host") || strings.Contains(names, "scan_network") || strings.Contains(names, "delete_device") {
		t.Fatalf("工具清单不对：%s", names)
	}
	msgs, _ := json.Marshal(first["messages"])
	if !strings.Contains(string(msgs), "找二楼的设备") || !strings.Contains(string(msgs), "candidate_subnets") {
		t.Fatal("首条消息缺少网络线索或用户要求")
	}
	if !strings.Contains(fmt.Sprint((*reqs)[1]["messages"]), "192.168.7.10") {
		t.Fatal("第二轮应带上探测结果")
	}

	devs := result["devices"].([]interface{})
	if len(devs) != 2 {
		t.Fatalf("devices: %v", devs)
	}
	d0 := devs[0].(map[string]interface{})
	if d0["mac"] != "AA:BB:CC:00:00:10" || d0["hostname"] != "NPI0A1B2C" || d0["attach"] == "" {
		t.Fatalf("应使用实际观察到的 MAC，并校验接线：%v", d0)
	}
	if len(result["dropped"].([]interface{})) != 2 {
		t.Fatalf("dropped: %v", result["dropped"])
	}
	jobs := result["scan_jobs"].([]interface{})
	if len(jobs) != 1 || fmt.Sprint(jobs[0].(map[string]interface{})["subnets"]) != "[192.168.7.0/24]" {
		t.Fatalf("scan_jobs: %v", jobs)
	}
	if len(result["notes"].([]interface{})) != 1 {
		t.Fatalf("notes: %v", result["notes"])
	}

	// 应用
	body, _ := json.Marshal(map[string]interface{}{"devices": devs, "scan_jobs": jobs})
	code, ap := authPost(t, c, ts, "/api/ai/discover/apply", string(body))
	if code != 200 || ap["nodes"].(float64) != 1 || ap["switches"].(float64) != 1 || ap["jobs"].(float64) != 1 || ap["located"].(float64) != 1 {
		t.Fatalf("apply: %d %v", code, ap)
	}
	tp, _ := s.Topo.Snapshot()
	_, nid := tp.FindByIP("192.168.7.10")
	n := tp.FindNode(nid)
	if n == nil || n.Name != "财务部打印机" || n.Attach.Switch != "access" || n.Attach.Port != "5" || n.HasFlag("unlocated") ||
		!n.HasFlag(discFlag) || !inList(n.Tags, "打印机") || !strings.Contains(n.Note, "AI 自主发现") {
		t.Fatalf("新节点不对：%+v", n)
	}
	_, sid := tp.FindByIP("192.168.7.2")
	if sw := tp.FindSwitch(sid); sw == nil || sw.Name != "二楼接入交换机" || !sw.Monitor.Alert {
		t.Fatalf("新交换机不对：%+v", sw)
	}
	// 再次应用：已登记，全部跳过
	code, ap = authPost(t, c, ts, "/api/ai/discover/apply", string(body))
	if code != 200 || ap["nodes"].(float64) != 0 || len(ap["skipped"].([]interface{})) != 3 {
		t.Fatalf("重复应用应跳过：%v", ap)
	}

	// 撤销（第一次应用返回的 undo）
	undo := map[string]interface{}{"nodes": []string{nid, "n-192-168-101-232"}, "switches": []string{sid, "access"}, "jobs": []string{}}
	for _, j := range s.Cfg.Get().Discovery.Jobs {
		if j.Name == "二楼" {
			undo["jobs"] = []string{j.ID}
		}
	}
	ub, _ := json.Marshal(map[string]interface{}{"undo": undo})
	code, ur := authPost(t, c, ts, "/api/ai/discover/apply", string(ub))
	if code != 200 || ur["removed"].(float64) != 2 || ur["jobs"].(float64) != 1 {
		t.Fatalf("undo: %d %v", code, ur)
	}
	tp, _ = s.Topo.Snapshot()
	if tp.FindNode("n-192-168-101-232") == nil || tp.FindSwitch("access") == nil {
		t.Fatal("撤销不能删除非 AI 发现添加的设备")
	}
	if _, id := tp.FindByIP("192.168.7.10"); id != "" {
		t.Fatal("撤销后新节点应被删除")
	}
}
