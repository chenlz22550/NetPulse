package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"netping/internal/config"
	"netping/internal/llm"
	"netping/internal/topology"
)

func toolCallOf(id, name, args string) llm.ToolCall {
	var tc llm.ToolCall
	tc.ID, tc.Type = id, "function"
	tc.Function.Name, tc.Function.Arguments = name, args
	return tc
}

func allCalls(t *testing.T, s *Server) []MCPCallLog {
	t.Helper()
	var out []MCPCallLog
	if err := s.callLogger().each(func(e MCPCallLog) bool { out = append(out, e); return true }); err != nil {
		t.Fatal(err)
	}
	return out
}

func findCall(t *testing.T, s *Server, pred func(MCPCallLog) bool) MCPCallLog {
	t.Helper()
	for _, e := range allCalls(t, s) {
		if pred(e) {
			return e
		}
	}
	t.Fatalf("no matching call log; have %d entries", len(allCalls(t, s)))
	return MCPCallLog{}
}

func TestAIToolCallLogSuccessFailureDenied(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := withAIRun(s.withProfileEpoch(context.Background()), "ai_diagnose")
	run := aiRunFrom(ctx).RunID
	if !strings.HasPrefix(run, "run-") {
		t.Fatalf("run id: %q", run)
	}

	title, res, rec := s.runAIToolCtx(ctx, aiToolNames, toolCallOf("call_ok", "get_overview", `{}`), "AI诊断/1.2.3.4", "")
	if title == "" || !strings.Contains(res, "state_counts") || rec.Status != "ok" {
		t.Fatalf("ok call: %q %q %+v", title, res, rec)
	}
	_, _, recErr := s.runAIToolCtx(ctx, aiToolNames, toolCallOf("call_err", "get_device", `{"id":"no-such-device"}`), "AI诊断/1.2.3.4", "review")
	_, _, recBadJSON := s.runAIToolCtx(ctx, aiToolNames, toolCallOf("call_bad", "get_device", `{"id":`), "AI诊断/1.2.3.4", "")
	_, denied, recDenied := s.runAIToolCtx(ctx, aiToolNames, toolCallOf("call_no", "delete_device", `{"id":"core"}`), "AI诊断/1.2.3.4", "")
	if !strings.Contains(denied, "不允许") {
		t.Fatalf("denied result: %s", denied)
	}

	ok := findCall(t, s, func(e MCPCallLog) bool { return e.ID == rec.ID })
	if ok.RunID != run || ok.ToolCallID != "call_ok" || ok.Source != "ai_diagnose" || ok.Tool != "get_overview" ||
		ok.ProfileID == "" || ok.StartedAt == 0 || ok.DurationMs < 0 || ok.Remote != "AI诊断/1.2.3.4" || ok.Args != "{}" {
		t.Fatalf("ok entry: %+v", ok)
	}
	if !json.Valid([]byte(ok.Result)) || !strings.Contains(ok.Result, "state_counts") || ok.ResultBytes != len(ok.Result) {
		t.Fatalf("ok result not stored in full: %d %q", ok.ResultBytes, truncRunes(ok.Result, 80))
	}
	e := findCall(t, s, func(e MCPCallLog) bool { return e.ID == recErr.ID })
	if e.Status != "error" || e.Error == "" || e.Phase != "review" || e.RunID != run {
		t.Fatalf("error entry: %+v", e)
	}
	b := findCall(t, s, func(e MCPCallLog) bool { return e.ID == recBadJSON.ID })
	if b.Status != "error" || !strings.Contains(b.Error, "JSON") || b.Args != `{"id":` {
		t.Fatalf("bad json entry: %+v", b)
	}
	d := findCall(t, s, func(e MCPCallLog) bool { return e.ID == recDenied.ID })
	if d.Status != "denied" || d.Tool != "delete_device" {
		t.Fatalf("denied entry: %+v", d)
	}

	// 磁盘持久化：按天写入 data/mcp_calls/*.jsonl，新进程（新 Server）可读到。
	files, _ := filepath.Glob(filepath.Join(s.BaseDir, "data", mcpLogDirName, "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("log files: %v", files)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(files[0]); fi.Mode().Perm() != 0o600 {
			t.Fatalf("log file perm %v", fi.Mode().Perm())
		}
	}
	s2 := &Server{BaseDir: s.BaseDir}
	if n := len(allCalls(t, s2)); n != 4 {
		t.Fatalf("reloaded entries = %d", n)
	}
}

func TestAIToolCallLogKeepsFullLongResult(t *testing.T) {
	s, _ := newTestServer(t)
	topo, _ := s.Topo.Snapshot()
	next := topo.Clone()
	for i := 0; i < 400; i++ {
		next.Nodes = append(next.Nodes, &topology.Node{ID: fmt.Sprintf("bulk%03d", i), Name: fmt.Sprintf("批量测试设备-%03d-带一段较长的名称用于撑大返回结果", i),
			IP: fmt.Sprintf("10.77.%d.%d", i/200, i%200+1), Note: strings.Repeat("备注", 10)})
	}
	if err := s.Topo.Replace(next); err != nil {
		t.Fatal(err)
	}
	_, modelRes, rec := s.runAIToolCtx(context.Background(), aiToolNames, toolCallOf("c1", "list_devices", `{"limit":1000}`), "AI报告/x", "")
	if !strings.Contains(modelRes, "已截断") || len(modelRes) > aiMaxToolResult+200 {
		t.Fatalf("model should get truncated result, len=%d", len(modelRes))
	}
	e := findCall(t, s, func(e MCPCallLog) bool { return e.ID == rec.ID })
	if !e.ModelTruncated || e.ResultBytes <= aiMaxToolResult || len(e.Result) != e.ResultBytes {
		t.Fatalf("long result: truncated=%v bytes=%d", e.ModelTruncated, e.ResultBytes)
	}
	if !json.Valid([]byte(e.Result)) || strings.Contains(e.Result, "已截断") || !strings.Contains(e.Result, "bulk399") {
		t.Fatalf("log result must be complete JSON including the last device")
	}
	if e.Source != "ai_report" {
		t.Fatalf("source from remote prefix: %q", e.Source)
	}
}

func TestRedactText(t *testing.T) {
	cases := []struct{ in, mustNot, must string }{
		{`{"api_key":"sk-example-fake-key","model":"x"}`, "sk-example-fake-key", `"model":"x"`},
		{`{"auth":{"password":"example-fake-password"},"ip":"10.0.0.1"}`, "example-fake-password", "10.0.0.1"},
		{`{"switches":{"sw1":{"community":"example-community"}}}`, "example-community", "sw1"},
		{`{"webhook_url":"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=example-room-key"}`, "example-room-key", redactedMark},
		{`call https://oapi.dingtalk.com/robot/send?access_token=example-fake-token now`, "example-fake-token", "oapi.dingtalk.com"},
		{`Authorization: Bearer example-fake-bearer-value`, "example-fake-bearer-value", "Bearer"},
		{`url?token=example-secret&x=1`, "example-secret", "x=1"},
		{`{"note":"key is sk-proj-example-fake-value"}`, "sk-proj-example-fake-value", "note"},
		{`{"nested":"{\"secret\":\"example-inner-secret\"}"}`, "example-inner-secret", "nested"},
		{`{"proxy_url":"http://example-user:example-pass@192.0.2.10:3128"}`, "example-pass", "192.0.2.10:3128"},
	}
	for _, c := range cases {
		out, changed := redactText(c.in)
		if !changed || strings.Contains(out, c.mustNot) || !strings.Contains(out, c.must) {
			t.Errorf("redact %q => %q (changed=%v)", c.in, out, changed)
		}
	}
	// 非凭据字段与布尔/数字提示不应被误伤。
	keep := `{"api_key_set":true,"total_tokens":120,"token_required":false,"name":"core","mac":"aa:bb"}`
	if out, changed := redactText(keep); changed || out != keep {
		t.Errorf("should not change %q => %q", keep, out)
	}
}

func TestToolCallLogRedactsSecretsEndToEnd(t *testing.T) {
	s, ts := newTestServer(t)
	c := s.Cfg.Get()
	c.Alert.WebhookURL = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=very-secret-room-key"
	c.Locate.Community = "private-community-x"
	c.MCP.Enabled = true
	if err := s.Cfg.Update(c); err != nil {
		t.Fatal(err)
	}
	// 外部 MCP 客户端调用：参数里夹带凭据。
	_, out := rpc(t, ts, `{"jsonrpc":"2.0","id":"req-77","method":"tools/call","params":{"name":"get_settings","arguments":{"token":"example-leaky-token","password":"example-pass-word"}}}`)
	if out["result"] == nil {
		t.Fatalf("tools/call failed: %v", out)
	}
	e := findCall(t, s, func(e MCPCallLog) bool { return e.RPCID == "req-77" })
	if e.Source != "mcp" || e.Status != "ok" || e.Tool != "get_settings" {
		t.Fatalf("external entry: %+v", e)
	}
	raw, _ := json.Marshal(e)
	for _, secret := range []string{"example-leaky-token", "example-pass-word", "very-secret-room-key", "private-community-x", testPassword} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("log leaked %q: %s", secret, raw)
		}
	}
	if !e.Redacted || !strings.Contains(e.Args, redactedMark) {
		t.Fatalf("args should be redacted: %+v", e.Args)
	}
	// 未知工具、只读模式拒绝也要留痕。
	rpc(t, ts, `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"no_such_tool","arguments":{}}}`)
	if d := findCall(t, s, func(e MCPCallLog) bool { return e.Tool == "no_such_tool" }); d.Status != "denied" || d.RPCID != "8" {
		t.Fatalf("unknown tool entry: %+v", d)
	}
	// 工具执行错误。
	rpc(t, ts, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"get_device","arguments":{"id":"nope"}}}`)
	if d := findCall(t, s, func(e MCPCallLog) bool { return e.RPCID == "9" }); d.Status != "error" || d.Error == "" {
		t.Fatalf("error entry: %+v", d)
	}

	// 查询接口：列表摘要 + 单条完整记录 + 下载。
	get := func(path string) (int, []byte) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		req.SetBasicAuth("", testPassword)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	code, body := get("/api/mcp/calls?source=mcp&limit=2")
	var list struct {
		Items []map[string]interface{} `json:"items"`
		More  bool                     `json:"more"`
	}
	if code != 200 || json.Unmarshal(body, &list) != nil || len(list.Items) != 2 || !list.More {
		t.Fatalf("list: %d %s", code, body)
	}
	if list.Items[0]["rpc_id"] != "9" { // 新的在前
		t.Fatalf("order: %v", list.Items[0])
	}
	if _, ok := list.Items[0]["result"]; ok {
		t.Fatalf("list must not carry full results")
	}
	// 游标分页：按 after=<上一页最后一条 id> 继续，不重复不遗漏。
	seen := map[string]bool{}
	after := ""
	for page := 0; page < 10; page++ {
		path := "/api/mcp/calls?source=mcp&limit=1"
		if after != "" {
			path += "&after=" + after
		}
		_, pb := get(path)
		var pl struct {
			Items []map[string]interface{} `json:"items"`
			More  bool                     `json:"more"`
		}
		_ = json.Unmarshal(pb, &pl)
		for _, it := range pl.Items {
			id := it["id"].(string)
			if seen[id] {
				t.Fatalf("duplicate across pages: %s", id)
			}
			seen[id] = true
			after = id
		}
		if !pl.More {
			break
		}
	}
	if len(seen) != 3 {
		t.Fatalf("paging saw %d of 3 mcp entries", len(seen))
	}
	code, body = get("/api/mcp/calls?status=denied")
	if code != 200 || !strings.Contains(string(body), "no_such_tool") || strings.Contains(string(body), "get_settings") {
		t.Fatalf("status filter: %s", body)
	}
	code, body = get("/api/mcp/calls/" + e.ID)
	var full MCPCallLog
	if code != 200 || json.Unmarshal(body, &full) != nil || full.Result != e.Result {
		t.Fatalf("detail: %d", code)
	}
	if code, body = get("/api/mcp/calls/" + e.ID + "/download"); code != 200 || !strings.Contains(string(body), e.ID) {
		t.Fatalf("download: %d", code)
	}
	if code, _ = get("/api/mcp/calls/call-missing"); code != 404 {
		t.Fatalf("missing: %d", code)
	}
}

func TestToolCallLogCrossRoomFilter(t *testing.T) {
	s, ts := newProfileTestServer(t)
	_, _, recA := s.runAIToolCtx(s.withProfileEpoch(context.Background()), aiToolNames, toolCallOf("a", "get_overview", `{}`), "AI诊断/x", "")
	if recA.ProfileID != "default" || recA.ProfileName == "" {
		t.Fatalf("room A entry: %+v", recA)
	}
	info, err := s.Profiles.Create("日志隔离机房", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Profiles.Activate(info.ID); err != nil {
		t.Fatal(err)
	}
	_, _, recB := s.runAIToolCtx(s.withProfileEpoch(context.Background()), aiToolNames, toolCallOf("b", "get_overview", `{}`), "AI诊断/x", "")
	if recB.ProfileID != info.ID || recB.ProfileName != "日志隔离机房" {
		t.Fatalf("room B entry: %+v", recB)
	}
	cur := apiReqProfile(t, ts, "GET", "/api/mcp/calls", "", info.ID, 200)
	if !strings.Contains(string(cur), recB.ID) || strings.Contains(string(cur), recA.ID) {
		t.Fatalf("current-room filter wrong: %s", cur)
	}
	all := apiReqProfile(t, ts, "GET", "/api/mcp/calls?profile=all", "", info.ID, 200)
	if !strings.Contains(string(all), recB.ID) || !strings.Contains(string(all), recA.ID) {
		t.Fatalf("all-rooms listing wrong: %s", all)
	}
	other := apiReqProfile(t, ts, "GET", "/api/mcp/calls?profile=default", "", info.ID, 200)
	if strings.Contains(string(other), recB.ID) || !strings.Contains(string(other), recA.ID) {
		t.Fatalf("explicit room filter wrong: %s", other)
	}
}

// thinkingLLM 模拟一个会返回思考内容的 OpenAI 兼容接口：第一轮思考 + 调用工具，第二轮思考 + 结论。
func thinkingLLM(t *testing.T, fail bool) *httptest.Server {
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(500)
			_, _ = io.WriteString(w, `{"error":{"message":"upstream down"}}`)
			return
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		n++
		round := n
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(v string) { fmt.Fprintf(w, "data: %s\n\n", v); w.(http.Flusher).Flush() }
		if round == 1 {
			send(`{"choices":[{"delta":{"reasoning_content":"先看概况，"}}]}`)
			send(`{"choices":[{"delta":{"reasoning_content":"再查异常设备。"}}]}`)
			send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_t","type":"function","function":{"name":"list_devices","arguments":"{\"state\":\"abnormal\"}"}}]}}]}`)
			send(`[DONE]`)
			return
		}
		send(`{"choices":[{"delta":{"reasoning_content":"数据显示一切正常。"}}]}`)
		send(`{"choices":[{"delta":{"content":"**结论**：正常。"}}]}`)
		send(`[DONE]`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func streamEvents(t *testing.T, ts *httptest.Server, path, body string) []map[string]interface{} {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
	req.SetBasicAuth("", testPassword)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var events []map[string]interface{}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var ev map[string]interface{}
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			events = append(events, ev)
		}
	}
	return events
}

func setAI(t *testing.T, s *Server, base string) {
	t.Helper()
	c := s.Cfg.Get()
	c.AI = config.AIConfig{Enabled: true, Protocol: "openai", BaseURL: base, APIKey: "sk-test-123456", Model: "m", Thinking: true, TimeoutSec: 30}
	off := false
	c.AI.SelfCheck = &off
	if err := s.Cfg.Update(c); err != nil {
		t.Fatal(err)
	}
}

func TestAIDiagnoseStreamsThinkingAndToolLog(t *testing.T) {
	s, ts := newTestServer(t)
	setAI(t, s, thinkingLLM(t, false).URL)
	events := streamEvents(t, ts, "/api/ai/diagnose", `{"messages":[{"role":"user","content":"网络怎么样"}]}`)
	var order []string
	var thinking strings.Builder
	var runID string
	var tool map[string]interface{}
	for _, ev := range events {
		order = append(order, ev["t"].(string))
		switch ev["t"] {
		case "run":
			if runID == "" {
				runID, _ = ev["run_id"].(string)
			}
			if ev["thinking"] != true {
				t.Fatalf("run event should report thinking flag: %v", ev)
			}
		case "thinking":
			thinking.WriteString(ev["text"].(string))
		case "tool":
			tool = ev
		}
	}
	joined := strings.Join(order, ",")
	if thinking.String() != "先看概况，再查异常设备。数据显示一切正常。" {
		t.Fatalf("thinking text: %q (events %s)", thinking.String(), joined)
	}
	if !strings.HasPrefix(joined, "status,run,status") || !strings.HasSuffix(joined, "done") {
		t.Fatalf("event order: %s", joined)
	}
	if strings.Index(joined, "thinking") > strings.Index(joined, "tool") || strings.LastIndex(joined, "thinking") > strings.Index(joined, "delta") {
		t.Fatalf("thinking should precede tool/answer: %s", joined)
	}
	if tool == nil || tool["status"] != "ok" || tool["call_id"] == nil || tool["ms"] == nil {
		t.Fatalf("tool event: %v", tool)
	}
	e := findCall(t, s, func(e MCPCallLog) bool { return e.ID == tool["call_id"] })
	if e.RunID != runID || e.ToolCallID != "call_t" || e.Source != "ai_diagnose" || e.Args != `{"state":"abnormal"}` {
		t.Fatalf("diagnose tool log: %+v (run %s)", e, runID)
	}
}

func TestAIDiagnoseErrorAndCancel(t *testing.T) {
	s, ts := newTestServer(t)
	setAI(t, s, thinkingLLM(t, true).URL)
	events := streamEvents(t, ts, "/api/ai/diagnose", `{"messages":[{"role":"user","content":"x"}]}`)
	last := events[len(events)-1]
	if last["t"] != "error" || !strings.Contains(fmt.Sprint(last["error"]), "500") {
		t.Fatalf("expected terminal error event, got %v", last)
	}

	// 取消：模型挂起时客户端断开，服务端应尽快结束且不输出 done。
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"思考中\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(hang.Close)
	setAI(t, s, hang.URL)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/api/ai/diagnose", strings.NewReader(`{"messages":[{"role":"user","content":"x"}]}`))
	req.SetBasicAuth("", testPassword)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	gotThinking := false
	for sc.Scan() {
		if strings.Contains(sc.Text(), `"t":"thinking"`) {
			gotThinking = true
			break
		}
	}
	cancel()
	resp.Body.Close()
	if !gotThinking {
		t.Fatal("expected thinking event before cancel")
	}
	// 并发槽位应被释放（取消的诊断不应一直占用）。
	deadline := time.Now().Add(3 * time.Second)
	for len(aiSem) > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(aiSem) != 0 {
		t.Fatalf("diagnose slot not released after cancel")
	}
}

func TestMCPMalformedParamsAndDeniedAreLoggedWithFullReturn(t *testing.T) {
	s, ts := newTestServer(t)
	// arguments 为字符串（含凭据）、数组；params 本身不是对象。
	_, out := rpc(t, ts, `{"jsonrpc":"2.0","id":"bad-1","method":"tools/call","params":{"name":"get_device","arguments":"token=example-malformed-secret"}}`)
	if out["error"] == nil {
		t.Fatalf("expected rpc error: %v", out)
	}
	rpc(t, ts, `{"jsonrpc":"2.0","id":"bad-2","method":"tools/call","params":{"name":"get_overview","arguments":[1,2]}}`)
	rpc(t, ts, `{"jsonrpc":"2.0","id":"bad-3","method":"tools/call","params":[1,2]}`)
	rpc(t, ts, `{"jsonrpc":"2.0","id":"unk","method":"tools/call","params":{"name":"nope_tool","arguments":{}}}`)
	rpc(t, ts, `{"jsonrpc":"2.0","id":"err","method":"tools/call","params":{"name":"get_device","arguments":{"id":"nope"}}}`)

	b1 := findCall(t, s, func(e MCPCallLog) bool { return e.RPCID == "bad-1" })
	if b1.Status != "error" || b1.Tool != "get_device" || !strings.Contains(b1.Error, "参数无效") || b1.ProfileID == "" {
		t.Fatalf("bad-1: %+v", b1)
	}
	if strings.Contains(b1.Args, "example-malformed-secret") || !strings.Contains(b1.Args, redactedMark) {
		t.Fatalf("bad-1 args must keep raw arguments but redacted: %q", b1.Args)
	}
	if !strings.Contains(b1.Result, "-32602") {
		t.Fatalf("bad-1 result must hold the returned rpc error: %q", b1.Result)
	}
	b2 := findCall(t, s, func(e MCPCallLog) bool { return e.RPCID == "bad-2" })
	if b2.Status != "error" || b2.Tool != "get_overview" || b2.Args != "[1,2]" || b2.Result == "" {
		t.Fatalf("bad-2: %+v", b2)
	}
	b3 := findCall(t, s, func(e MCPCallLog) bool { return e.RPCID == "bad-3" })
	if b3.Status != "error" || !strings.Contains(b3.Args, "[1,2]") || b3.Result == "" {
		t.Fatalf("bad-3: %+v", b3)
	}
	u := findCall(t, s, func(e MCPCallLog) bool { return e.RPCID == "unk" })
	if u.Status != "denied" || !strings.Contains(u.Result, "未知工具") {
		t.Fatalf("unknown tool result missing: %+v", u)
	}
	e := findCall(t, s, func(e MCPCallLog) bool { return e.RPCID == "err" })
	if e.Status != "error" || !strings.Contains(e.Result, `"isError": true`) || !strings.Contains(e.Result, "找不到 id 为") {
		t.Fatalf("tool error result missing: %+v", e)
	}
	// 只读模式拒绝也记录返回内容。
	c := s.Cfg.Get()
	c.MCP.ReadOnly = true
	if err := s.Cfg.Update(c); err != nil {
		t.Fatal(err)
	}
	rpc(t, ts, `{"jsonrpc":"2.0","id":"ro","method":"tools/call","params":{"name":"delete_device","arguments":{"id":"core"}}}`)
	ro := findCall(t, s, func(e MCPCallLog) bool { return e.RPCID == "ro" })
	if ro.Status != "denied" || !strings.Contains(ro.Result, "只读模式") || ro.Title == "" {
		t.Fatalf("read-only denial: %+v", ro)
	}
}

func TestAIStreamToolArgsAreRedacted(t *testing.T) {
	s, ts := newTestServer(t)
	var mu sync.Mutex
	n := 0
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		round := n
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(v string) { fmt.Fprintf(w, "data: %s\n\n", v); w.(http.Flusher).Flush() }
		if round == 1 {
			send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_sec","type":"function","function":{"name":"get_device","arguments":"{\"id\":\"core\",\"token\":\"example-sse-secret\",\"note\":\"Bearer example-fake-bearer-sse\"}"}}]}}]}`)
			send(`[DONE]`)
			return
		}
		send(`{"choices":[{"delta":{"content":"完成"}}]}`)
		send(`[DONE]`)
	}))
	t.Cleanup(llmSrv.Close)
	setAI(t, s, llmSrv.URL)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/ai/diagnose", strings.NewReader(`{"messages":[{"role":"user","content":"看看 core"}]}`))
	req.SetBasicAuth("", testPassword)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(raw), "example-sse-secret") || strings.Contains(string(raw), "example-fake-bearer-sse") {
		t.Fatalf("SSE leaked raw tool arguments: %s", raw)
	}
	var tool map[string]interface{}
	for _, line := range strings.Split(string(raw), "\n") {
		var ev map[string]interface{}
		if json.Unmarshal([]byte(line), &ev) == nil && ev["t"] == "tool" {
			tool = ev
		}
	}
	if tool == nil || !strings.Contains(fmt.Sprint(tool["args"]), redactedMark) || !strings.Contains(fmt.Sprint(tool["args"]), `"id":"core"`) {
		t.Fatalf("tool event args should be redacted but keep normal fields: %v", tool)
	}
	e := findCall(t, s, func(e MCPCallLog) bool { return e.ID == tool["call_id"] })
	if e.ToolCallID != "call_sec" || e.Status != "ok" || !strings.Contains(e.Args, `"id":"core"`) || !strings.Contains(e.Args, redactedMark) ||
		strings.Contains(e.Args, "example-sse-secret") || !strings.Contains(e.Result, "core") {
		t.Fatalf("call log: %+v", e)
	}
}
