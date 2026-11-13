package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeLLM 模拟 OpenAI 兼容接口：第一轮要求调用 list_devices，第二轮输出结论。
func fakeLLM(t *testing.T) (*httptest.Server, *[]map[string]interface{}) {
	var mu sync.Mutex
	var reqs []map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer sk-test-123456" {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":{"message":"Authentication Fails"}}`)
			return
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		reqs = append(reqs, body)
		n := len(reqs)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(v string) { fmt.Fprintf(w, "data: %s\n\n", v); w.(http.Flusher).Flush() }
		msgs := body["messages"].([]interface{})
		last := msgs[len(msgs)-1].(map[string]interface{})
		if last["role"] == "user" && body["tools"] != nil && n == 1 {
			send(`{"choices":[{"delta":{"content":"我先查一下异常设备。"}}]}`)
			send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"list_devices","arguments":"{\"state\":"}}]}}]}`)
			send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"abnormal\"}"}}]}}]}`)
			send(`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}}`)
			send(`[DONE]`)
			return
		}
		send(`{"choices":[{"delta":{"content":"**结论**：网络"}}]}`)
		send(`{"choices":[{"delta":{"content":"正常。"}}]}`)
		send(`{"choices":[],"usage":{"prompt_tokens":200,"completion_tokens":10,"total_tokens":210}}`)
		send(`[DONE]`)
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func TestAIDiagnose(t *testing.T) {
	s, ts := newTestServer(t)
	llmSrv, reqs := fakeLLM(t)
	c := newClient()
	authPost(t, c, ts, "/api/auth/login", `{"password":"`+testPassword+`"}`)

	put := func(body string) (int, map[string]interface{}) {
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/ai/settings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]interface{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	// 未设置 Key 时拒绝诊断
	if code, _ := authPost(t, c, ts, "/api/ai/diagnose", `{"messages":[{"role":"user","content":"hi"}]}`); code != 400 {
		t.Fatalf("no key: %d", code)
	}
	code, out := put(`{"base_url":"` + llmSrv.URL + `","api_key":"sk-test-123456"}`)
	if code != 200 || out["api_key_set"] != true || strings.Contains(fmtJSON(out), "sk-test-123456") {
		t.Fatalf("settings: %d %v", code, out)
	}
	// 只改其他字段时保留 Key；网页配置接口不返回 Key，也不能通过它改掉 Key
	put(`{"thinking":false}`)
	if s.Cfg.Get().AI.APIKey != "sk-test-123456" {
		t.Fatal("key lost")
	}
	resp, _ := c.Get(ts.URL + "/api/config")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(raw), "sk-test-123456") {
		t.Fatal("key leaked via /api/config")
	}
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/config", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	if r2, err := c.Do(req); err != nil || r2.StatusCode != 200 {
		t.Fatalf("put config: %v", err)
	}
	if s.Cfg.Get().AI.APIKey != "sk-test-123456" {
		t.Fatal("key wiped by /api/config PUT")
	}

	// 诊断：流式事件里应有工具调用、正文增量和完成
	resp, err := c.Post(ts.URL+"/api/ai/diagnose", "application/json", strings.NewReader(`{"messages":[{"role":"user","content":"现在网络怎么样？"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]interface{}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var ev map[string]interface{}
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			events = append(events, ev)
		}
	}
	resp.Body.Close()
	var text strings.Builder
	var tool, done map[string]interface{}
	for _, ev := range events {
		switch ev["t"] {
		case "delta":
			text.WriteString(ev["text"].(string))
		case "tool":
			tool = ev
		case "done":
			done = ev
		case "error":
			t.Fatalf("error event: %v", ev)
		}
	}
	if tool == nil || tool["name"] != "list_devices" || done == nil {
		t.Fatalf("events: %v", events)
	}
	if !strings.Contains(text.String(), "网络正常") {
		t.Fatalf("text: %q", text.String())
	}
	if u := done["usage"].(map[string]interface{}); u["total_tokens"].(float64) != 330 {
		t.Fatalf("usage: %v", u)
	}
	// 第二轮请求带上了工具结果，且第一轮带了只读工具、没有写工具
	if len(*reqs) != 2 {
		t.Fatalf("llm requests = %d", len(*reqs))
	}
	names := map[string]bool{}
	for _, tl := range (*reqs)[0]["tools"].([]interface{}) {
		names[tl.(map[string]interface{})["function"].(map[string]interface{})["name"].(string)] = true
	}
	if !names["get_history"] || names["upsert_device"] || names["scan_network"] || len(names) != len(aiToolNames) {
		t.Fatalf("tools offered: %v", names)
	}
	if th := fmtJSON((*reqs)[0]["thinking"]); !strings.Contains(th, "disabled") {
		t.Fatalf("thinking: %s", th)
	}
	second := (*reqs)[1]["messages"].([]interface{})
	lastMsg := second[len(second)-1].(map[string]interface{})
	if lastMsg["role"] != "tool" || lastMsg["tool_call_id"] != "call_a" {
		t.Fatalf("tool message: %v", lastMsg)
	}
	if !strings.Contains(fmtJSON(second[0]), "当前概况") {
		t.Fatal("system prompt missing overview")
	}
}

func TestAIBadKey(t *testing.T) {
	s, ts := newTestServer(t)
	llmSrv, _ := fakeLLM(t)
	cf := s.Cfg.Get()
	cf.AI.BaseURL = llmSrv.URL
	cf.AI.APIKey = "sk-wrong"
	_ = s.Cfg.Update(cf)
	c := newClient()
	authPost(t, c, ts, "/api/auth/login", `{"password":"`+testPassword+`"}`)
	code, out := authPost(t, c, ts, "/api/ai/test", ``)
	if code != 400 || !strings.Contains(fmtJSON(out), "API Key 无效") {
		t.Fatalf("bad key: %d %v", code, out)
	}
}

func TestAIRejectsWriteTools(t *testing.T) {
	s, _ := newTestServer(t)
	if _, res := s.runAITool("delete_device", `{"id":"core"}`, "test"); !strings.Contains(res, "不允许") {
		t.Fatalf("write tool allowed: %s", res)
	}
	if _, res := s.runAITool("get_overview", `{}`, "test"); !strings.Contains(res, "state_counts") {
		t.Fatalf("overview: %s", res)
	}
}

func TestAIFocusContext(t *testing.T) {
	s, ts := newTestServer(t)
	llmSrv, reqs := fakeLLM(t)
	cf := s.Cfg.Get()
	cf.AI.BaseURL = llmSrv.URL
	cf.AI.APIKey = "sk-test-123456"
	_ = s.Cfg.Update(cf)
	tp, _ := s.Topo.Snapshot()
	id := tp.Nodes[0].ID
	c := newClient()
	authPost(t, c, ts, "/api/auth/login", `{"password":"`+testPassword+`"}`)
	resp, err := c.Post(ts.URL+"/api/ai/diagnose", "application/json", strings.NewReader(`{"focus":"`+id+`","messages":[{"role":"user","content":"诊断这台"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	sys := fmtJSON((*reqs)[0]["messages"].([]interface{})[0])
	if !strings.Contains(sys, "重点设备") || !strings.Contains(sys, id) || !strings.Contains(sys, "近 1 小时") {
		t.Fatalf("focus context missing: %.300s", sys)
	}
}
