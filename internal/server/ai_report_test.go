package server

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAIReport(t *testing.T) {
	s, ts := newTestServer(t)
	a := "n-192-168-101-232"
	// 最近 60 分钟中有 5 分钟全部丢包
	fillMinutes(t, s, a, time.Now(), 60, 2, func(ago int) int {
		if ago <= 5 {
			return 0
		}
		return 60
	})
	reply := "# 测试机房 网络运维日报（2026-11-14）\n\n## 一、总体结论\n健康评级：良。\n\n```chart\n{\"type\":\"bar\",\"title\":\"丢包\",\"items\":[{\"label\":\"x\",\"value\":8.33}]}\n```\n"
	llmSrv, reqs := fakeOptLLM(t, reply)
	cf := s.Cfg.Get()
	cf.AI.BaseURL, cf.AI.APIKey = llmSrv.URL, "sk-test-123456"
	_ = s.Cfg.Update(cf)
	c := newClient()
	authPost(t, c, ts, "/api/auth/login", `{"password":"`+testPassword+`"}`)

	if code, _ := authPost(t, c, ts, "/api/ai/report", `{"range":"1y"}`); code != 400 {
		t.Fatalf("非法周期应拒绝: %d", code)
	}

	resp, err := c.Post(ts.URL+"/api/ai/report", "application/json", strings.NewReader(`{"range":"24h","note":"重点关注打印机"}`))
	if err != nil {
		t.Fatal(err)
	}
	var rp map[string]interface{}
	sawStats := false
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var ev map[string]interface{}
		_ = json.Unmarshal(sc.Bytes(), &ev)
		switch ev["t"] {
		case "error", "warn":
			t.Fatalf("事件: %v", ev)
		case "stats":
			sawStats = true
		case "report":
			rp = ev["report"].(map[string]interface{})
		}
	}
	resp.Body.Close()
	if rp == nil || !sawStats {
		t.Fatal("缺少 report / stats 事件")
	}
	// 给模型的资料里有统计和排行，系统提示带上了用户要求和图表格式
	msgs := (*reqs)[0]["messages"].([]interface{})
	sys, user := fmtJSON(msgs[0]), fmtJSON(msgs[1])
	if !strings.Contains(sys, "重点关注打印机") || !strings.Contains(sys, `\"type\":\"timeline\"`) {
		t.Fatalf("system: %.300s", sys)
	}
	if !strings.Contains(user, "丢包率排行") || !strings.Contains(user, a) || !strings.Contains(user, "【趋势】") {
		t.Fatalf("context: %.500s", user)
	}
	md := rp["markdown"].(string)
	if rp["title"] != "测试机房 网络运维日报（2026-11-14）" || !strings.Contains(md, "```chart") || !strings.Contains(md, "附录：程序统计数据") {
		t.Fatalf("report: %v", rp)
	}
	sum := rp["summary"].(map[string]interface{})
	if sum["down_minutes"].(float64) != 5 || sum["with_data"].(float64) != 1 {
		t.Fatalf("summary: %v", sum)
	}
	id := rp["id"].(string)

	// 列表、查看、下载 Markdown
	r, _ := c.Get(ts.URL + "/api/ai/reports")
	var list []map[string]interface{}
	_ = json.NewDecoder(r.Body).Decode(&list)
	r.Body.Close()
	if len(list) != 1 || list[0]["id"] != id {
		t.Fatalf("list: %v", list)
	}
	r, _ = c.Get(ts.URL + "/api/ai/reports/" + id + ".md")
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || !strings.Contains(r.Header.Get("Content-Disposition"), "attachment") || !strings.HasPrefix(string(b), "# 测试机房") {
		t.Fatalf("md: %d %s %.100s", r.StatusCode, r.Header.Get("Content-Disposition"), b)
	}
	// 非法 id 不能读到其他文件
	for _, bad := range []string{"..%2Fconfig", "../../config.yaml", "x"} {
		r, _ = c.Get(ts.URL + "/api/ai/reports/" + bad)
		r.Body.Close()
		if r.StatusCode == 200 {
			t.Fatalf("%s 不应读到内容", bad)
		}
	}
	// 删除
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/ai/reports/"+id, nil)
	r, _ = c.Do(req)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	r, _ = c.Get(ts.URL + "/api/ai/reports/" + id)
	r.Body.Close()
	if r.StatusCode != 404 {
		t.Fatalf("删除后应 404: %d", r.StatusCode)
	}
}

func TestReportTitle(t *testing.T) {
	if got := reportTitle("\n# 标题 A\n正文", "默认"); got != "标题 A" {
		t.Fatal(got)
	}
	if got := reportTitle("正文\n# 后面的标题", "默认"); got != "默认" {
		t.Fatal(got)
	}
}
