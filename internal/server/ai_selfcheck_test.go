package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestExtractIPv4(t *testing.T) {
	got := extractIPv4("设备 192.168.1.5、10.0.0.1。版本 1.2.3.4.5，非法 300.1.1.1，重复 192.168.1.5，结尾 172.16.0.9.")
	want := []string{"192.168.1.5", "10.0.0.1", "172.16.0.9"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestReviewGate(t *testing.T) {
	draft := "原回答"
	long := strings.Repeat("修正后的完整回答。", 30)
	cases := []struct {
		name   string
		chunks []string
		result string
		final  string // 空表示不检查
		note   string
	}{
		{"pass", []string{"【通", "过】已核对 3 台设备", "的状态与数据。"}, "pass", draft, "已核对 3 台设备的状态与数据。"},
		{"revise", []string{"【修正】", "\n**结论**：", "交换机重启。"}, "revised", "**结论**：交换机重启。", ""},
		{"revise bold", []string{"**【修正】**\n**结论**：新结论"}, "revised", "**结论**：新结论", ""},
		{"revise same line", []string{"【修正】结论：端口故障"}, "revised", "结论：端口故障", ""},
		{"pass later", []string{"核对完毕。\n【通过】数据一致"}, "pass", draft, "数据一致"},
		{"short free text", []string{"核对无误。"}, "pass", draft, "核对无误。"},
		{"long free text", []string{long}, "revised", long, ""},
		{"empty", nil, "failed", draft, ""},
		{"empty revise", []string{"【修正】"}, "failed", draft, ""},
	}
	for _, c := range cases {
		var evs []map[string]interface{}
		g := &reviewGate{emit: func(m map[string]interface{}) { evs = append(evs, m) }}
		for _, ch := range c.chunks {
			g.delta(ch)
		}
		res, note, final := g.finish(draft)
		if res != c.result || (c.final != "" && final != c.final) || (c.note != "" && note != c.note) {
			t.Fatalf("%s: result=%s note=%q final=%q", c.name, res, note, final)
		}
		var streamed strings.Builder
		revise := false
		for _, e := range evs {
			if e["t"] == "revise" {
				revise = true
			}
			if e["t"] == "delta" {
				streamed.WriteString(e["text"].(string))
			}
		}
		if revise != (res == "revised") || (res == "revised" && strings.TrimSpace(streamed.String()) != final) {
			t.Fatalf("%s: events %v", c.name, evs)
		}
	}
}

// scriptLLM 模拟模型：主诊断先调用 list_devices 再给出初稿；自检阶段按 review 脚本逐轮回复。
// review 中以 "TOOL:" 开头的项表示调用该工具（"TOOL:名称 参数JSON"）。
func scriptLLM(t *testing.T, draft string, review []string) (*httptest.Server, *[]map[string]interface{}) {
	var mu sync.Mutex
	var reqs []map[string]interface{}
	reviewRound := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		reqs = append(reqs, body)
		n := len(reqs)
		inReview := strings.Contains(fmtJSON(body["messages"]), "【自检】")
		step := ""
		if inReview {
			if reviewRound < len(review) {
				step = review[reviewRound]
			}
			reviewRound++
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(v string) { fmt.Fprintf(w, "data: %s\n\n", v); w.(http.Flusher).Flush() }
		text := func(s string) {
			for _, part := range strings.SplitAfter(s, "\n") { // 分块流式输出
				b, _ := json.Marshal(part)
				send(`{"choices":[{"delta":{"content":` + string(b) + `}}]}`)
			}
		}
		tool := func(id, name, args string) {
			b, _ := json.Marshal(args)
			send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"` + id + `","type":"function","function":{"name":"` + name + `","arguments":` + string(b) + `}}]}}]}`)
		}
		switch {
		case !inReview && n == 1:
			tool("call_a", "list_devices", `{"state":"abnormal"}`)
		case !inReview:
			text(draft)
		case strings.HasPrefix(step, "TOOL:"):
			f := strings.SplitN(strings.TrimPrefix(step, "TOOL:"), " ", 2)
			tool(fmt.Sprintf("call_r%d", n), f[0], f[1])
		default:
			text(step)
		}
		send(`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110}}`)
		send(`[DONE]`)
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func diagnoseEvents(t *testing.T, llmURL string, selfCheck bool) (*Server, []map[string]interface{}) {
	s, ts := newTestServer(t)
	cf := s.Cfg.Get()
	cf.AI.BaseURL, cf.AI.APIKey = llmURL, "sk-test-123456"
	cf.AI.SelfCheck = &selfCheck
	_ = s.Cfg.Update(cf)
	c := newClient()
	authPost(t, c, ts, "/api/auth/login", `{"password":"`+testPassword+`"}`)
	resp, err := c.Post(ts.URL+"/api/ai/diagnose", "application/json", strings.NewReader(`{"messages":[{"role":"user","content":"现在网络怎么样？"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var evs []map[string]interface{}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var ev map[string]interface{}
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			evs = append(evs, ev)
		}
	}
	return s, evs
}

// replay 按网页的处理方式还原最终显示的回答。
func replay(evs []map[string]interface{}) (content, draft string, last map[string]interface{}) {
	for _, e := range evs {
		switch e["t"] {
		case "delta":
			content += e["text"].(string)
		case "revise":
			draft, content = content, ""
		case "review_end":
			if e["restore"] == true {
				content = draft
			}
		case "done":
			last = e
		case "error":
			panic(fmt.Sprint(e))
		}
	}
	return
}

const testDraft = "**结论**：接入交换机 1 口下的 192.168.101.30 离线，另外 10.9.9.9 也不通。\n**依据**：概况显示该设备 down。\n**建议排查**：检查网线（把握：中）。"

func TestAISelfCheckPass(t *testing.T) {
	llmSrv, reqs := scriptLLM(t, testDraft, []string{"【通过】已核对 192.168.101.30 的状态与概况一致。"})
	_, evs := diagnoseEvents(t, llmSrv.URL, true)
	content, _, done := replay(evs)
	if content != testDraft || done["review"] != "pass" {
		t.Fatalf("content=%q done=%v", content, done)
	}
	var end map[string]interface{}
	for _, e := range evs {
		if e["t"] == "review_end" {
			end = e
		}
	}
	if end == nil || end["result"] != "pass" || !strings.Contains(end["text"].(string), "已核对") {
		t.Fatalf("review_end: %v", end)
	}
	if len(*reqs) != 3 {
		t.Fatalf("llm requests = %d", len(*reqs))
	}
	// 自检请求：带着初稿、工具和程序核对表
	rq := (*reqs)[2]
	msgs := rq["messages"].([]interface{})
	last := msgs[len(msgs)-1].(map[string]interface{})["content"].(string)
	prev := msgs[len(msgs)-2].(map[string]interface{})
	if prev["role"] != "assistant" || prev["content"] != testDraft || rq["tools"] == nil {
		t.Fatalf("review request: %v", prev)
	}
	for _, want := range []string{"10.9.9.9：拓扑中没有这个地址", "都没有出现过", "192.168.101.30", "本次未做现场核实", "list_devices", reviewPassMark, reviewReviseMark} {
		if !strings.Contains(last, want) {
			t.Fatalf("fact sheet missing %q:\n%s", want, last)
		}
	}
}

func TestAISelfCheckReviseAfterProbe(t *testing.T) {
	fixed := "**结论**：192.168.101.30 实测可以 ping 通，监控显示的离线已恢复。\n**依据**：ping_now 实测 0% 丢包。\n**建议排查**：继续观察（把握：高）。"
	llmSrv, reqs := scriptLLM(t, testDraft, []string{`TOOL:tcp_check {"ip":"127.0.0.1","ports":[1]}`, "【修正】\n" + fixed})
	_, evs := diagnoseEvents(t, llmSrv.URL, true)
	content, draft, done := replay(evs)
	if strings.TrimSpace(content) != fixed || draft != testDraft || done["review"] != "revised" {
		t.Fatalf("content=%q draft=%q done=%v", content, draft, done)
	}
	var reviewTool map[string]interface{}
	for _, e := range evs {
		if e["t"] == "tool" && e["phase"] == "review" {
			reviewTool = e
		}
	}
	if reviewTool == nil || reviewTool["name"] != "tcp_check" {
		t.Fatalf("review tool event missing: %v", evs)
	}
	if done["tools"].(float64) != 2 || len(*reqs) != 4 {
		t.Fatalf("calls=%v requests=%d", done["tools"], len(*reqs))
	}
	// 第二轮自检带上了核实结果
	msgs := (*reqs)[3]["messages"].([]interface{})
	if m := msgs[len(msgs)-1].(map[string]interface{}); m["role"] != "tool" || !strings.Contains(m["content"].(string), "refused") {
		t.Fatalf("probe result not passed back: %v", m)
	}
}

func TestAISelfCheckOff(t *testing.T) {
	llmSrv, reqs := scriptLLM(t, testDraft, []string{"【修正】\n不应出现"})
	_, evs := diagnoseEvents(t, llmSrv.URL, false)
	content, _, done := replay(evs)
	if content != testDraft || done["review"] != "off" || len(*reqs) != 2 {
		t.Fatalf("content=%q done=%v requests=%d", content, done, len(*reqs))
	}
}

func TestAISettingsSelfCheck(t *testing.T) {
	s, _ := newTestServer(t)
	if !s.Cfg.Get().AI.SelfCheckOn() || aiSettingsView(s.Cfg.Get().AI)["self_check"] != true {
		t.Fatal("self check should default on")
	}
}
