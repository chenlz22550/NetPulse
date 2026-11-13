package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeOptLLM 直接返回一段带 JSON 建议的回复，并记录收到的请求。
func fakeOptLLM(t *testing.T, reply string) (*httptest.Server, *[]map[string]interface{}) {
	var reqs []map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		reqs = append(reqs, body)
		w.Header().Set("Content-Type", "text/event-stream")
		chunk, _ := json.Marshal(map[string]interface{}{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{"content": reply}}}})
		fmt.Fprintf(w, "data: %s\n\n", chunk)
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":10,\"total_tokens\":20}}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func TestAIOptimize(t *testing.T) {
	s, ts := newTestServer(t)
	a, b := "n-192-168-101-232", "n-192-168-101-219"
	fillMinutes(t, s, a, time.Now(), 60, 2, func(ago int) int {
		if ago < 5 {
			return 0
		}
		return 60
	})
	reply := "**结论**：整体正常，1 台需要检查。\n```json\n" + `{
"classify":[{"category":"摄像头","ids":["` + a + `","不存在的"],"reason":"测试"},{"category":"外星设备","ids":["` + b + `"]}],
"config":[{"id":"` + a + `","action":"alert_on","reason":"关键"},{"id":"` + b + `","action":"rename","name":"前台 iPhone","reason":"Apple"},
  {"id":"` + b + `","action":"thresholds","latency_warn_ms":300,"reason":"无线"},{"id":"","action":"advice","reason":"建议做端口定位"},
  {"id":"` + a + `","action":"drop_table","reason":"恶意"},{"id":"` + a + `","action":"interval","interval_ms":50,"reason":"太快"}],
"check":[{"id":"` + a + `","priority":"high","reason":"最近 4 分钟中断","steps":"查网线"},{"id":"nope","priority":"high"}]}` + "\n```"
	llmSrv, reqs := fakeOptLLM(t, reply)
	cf := s.Cfg.Get()
	cf.AI.BaseURL, cf.AI.APIKey = llmSrv.URL, "sk-test-123456"
	_ = s.Cfg.Update(cf)
	c := newClient()
	authPost(t, c, ts, "/api/auth/login", `{"password":"`+testPassword+`"}`)

	resp, err := c.Post(ts.URL+"/api/ai/optimize", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]interface{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var ev map[string]interface{}
		_ = json.Unmarshal(sc.Bytes(), &ev)
		if ev["t"] == "error" {
			t.Fatalf("error: %v", ev)
		}
		if ev["t"] == "result" {
			result = ev["result"].(map[string]interface{})
		}
	}
	resp.Body.Close()
	if result == nil {
		t.Fatal("没有 result 事件")
	}
	// 上下文里带了设备表和历史统计
	user := fmtJSON((*reqs)[0]["messages"].([]interface{})[1])
	if !strings.Contains(user, a) || !strings.Contains(user, "7天丢包") {
		t.Fatalf("context: %.300s", user)
	}
	cls := result["classify"].([]interface{})
	if len(cls) != 1 || len(cls[0].(map[string]interface{})["devices"].([]interface{})) != 1 {
		t.Fatalf("classify 应只保留有效类别和设备: %v", cls)
	}
	cfgs := result["config"].([]interface{})
	if len(cfgs) != 4 {
		t.Fatalf("config 应过滤掉无效动作和越界参数: %v", cfgs)
	}
	if chk := result["check"].([]interface{}); len(chk) != 1 || !strings.Contains(fmtJSON(result["summary"]), "整体正常") {
		t.Fatalf("check/summary: %v %v", chk, result["summary"])
	}

	// 应用分类、开告警、改名
	body, _ := json.Marshal(map[string]interface{}{
		"classify": []interface{}{map[string]string{"id": a, "category": "摄像头"}},
		"config":   []interface{}{cfgs[0], cfgs[1], map[string]interface{}{"id": a, "action": "advice"}},
	})
	code, out := authPost(t, c, ts, "/api/ai/optimize/apply", string(body))
	if code != 200 || out["changed"].(float64) != 2 || out["skipped"].(float64) != 1 {
		t.Fatalf("apply: %d %v", code, out)
	}
	tp, _ := s.Topo.Snapshot()
	na, nb := tp.FindNode(a), tp.FindNode(b)
	if !na.Monitor.Alert || nodeCategory(na) != "摄像头" || nb.Name != "前台 iPhone" {
		t.Fatalf("应用结果不对: %+v / %+v", na, nb)
	}
	// 再分到另一类时替换原类别标签
	code, _ = authPost(t, c, ts, "/api/ai/optimize/apply", `{"classify":[{"id":"`+a+`","category":"录像机"}]}`)
	tp, _ = s.Topo.Snapshot()
	if code != 200 || strings.Join(tp.FindNode(a).Tags, ",") != "录像机" {
		t.Fatalf("类别替换: %v", tp.FindNode(a).Tags)
	}
	// 撤销第一次应用
	undo, _ := json.Marshal(map[string]interface{}{"restore": out["undo"]})
	if code, _ := authPost(t, c, ts, "/api/ai/optimize/apply", string(undo)); code != 200 {
		t.Fatalf("undo: %d", code)
	}
	tp, _ = s.Topo.Snapshot()
	if na, nb := tp.FindNode(a), tp.FindNode(b); na.Monitor.Alert || nodeCategory(na) != "" || nb.Name != "" {
		t.Fatalf("撤销后应恢复: %+v / %+v", na, nb)
	}
}

func TestAIOptimizeBadOutput(t *testing.T) {
	s, ts := newTestServer(t)
	llmSrv, _ := fakeOptLLM(t, "抱歉，我无法完成。")
	cf := s.Cfg.Get()
	cf.AI.BaseURL, cf.AI.APIKey = llmSrv.URL, "sk-test-123456"
	_ = s.Cfg.Update(cf)
	c := newClient()
	authPost(t, c, ts, "/api/auth/login", `{"password":"`+testPassword+`"}`)
	resp, err := c.Post(ts.URL+"/api/ai/optimize", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf strings.Builder
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		buf.WriteString(sc.Text())
	}
	if !strings.Contains(buf.String(), "格式不正确") {
		t.Fatalf("应提示格式错误: %s", buf.String())
	}
	if code, _ := authPost(t, c, ts, "/api/ai/optimize/apply", `{}`); code != 400 {
		t.Fatalf("空请求应拒绝: %d", code)
	}
}
