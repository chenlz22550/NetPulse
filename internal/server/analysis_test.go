package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"netping/internal/store"
)

// aiCall 通过 AI 工具入口调用并解析 JSON 结果。
func aiCall(t *testing.T, s *Server, name, args string) map[string]interface{} {
	t.Helper()
	_, res := s.runAITool(name, args, "test")
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(res), &out); err != nil {
		t.Fatalf("%s: 结果不是 JSON: %.300s", name, res)
	}
	if e, ok := out["error"]; ok {
		t.Fatalf("%s: %v", name, e)
	}
	return out
}

// fillMinutes 写入 [now-n 分钟, now) 的分钟数据，bad 指定哪些分钟（距今分钟数）收不到或部分收到。
func fillMinutes(t *testing.T, s *Server, id string, end time.Time, n int, avg float64, recvAt func(ago int) int) {
	t.Helper()
	base := end.Unix() / 60 * 60
	for ago := n; ago >= 1; ago-- {
		recv := 60
		if recvAt != nil {
			recv = recvAt(ago)
		}
		m := store.Minute{T: base - int64(ago)*60, Sent: 60, Recv: recv}
		if recv > 0 {
			m.Min, m.Avg, m.Max = avg*0.8, avg, avg*1.5
		}
		if err := s.Store.Append(id, m); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAnalysisToolsRegistered(t *testing.T) {
	for _, tl := range analysisTools() {
		if tl.Write || findTool(tl.Name) == nil || !aiAllowed(tl.Name) {
			t.Fatalf("%s 未正确注册为 AI 可用的只读工具", tl.Name)
		}
	}
}

func TestFindOutages(t *testing.T) {
	s, _ := newTestServer(t)
	id := "n-192-168-101-232"
	fillMinutes(t, s, id, time.Now(), 120, 2, func(ago int) int {
		switch {
		case ago >= 30 && ago <= 32:
			return 0 // 3 分钟完全中断
		case ago == 10:
			return 54 // 丢包 10%
		}
		return 60
	})
	out := aiCall(t, s, "find_outages", `{"id":"`+id+`","range":"24h"}`)
	sum := out["summary"].(map[string]interface{})
	if sum["episodes"].(float64) != 2 || sum["down_episodes"].(float64) != 1 || sum["total_down_sec"].(float64) != 180 {
		t.Fatalf("summary: %v", sum)
	}
	eps := out["episodes"].([]interface{})
	if eps[0].(map[string]interface{})["type"] != "loss" || eps[1].(map[string]interface{})["type"] != "down" {
		t.Fatalf("episodes: %v", eps)
	}
	if out["abnormal_minutes_by_hour"] == nil {
		t.Fatal("缺少按小时分布")
	}
	// 门槛 20% 时 10% 丢包的那分钟不算
	out = aiCall(t, s, "find_outages", `{"id":"`+id+`","range":"24h","min_loss_pct":20}`)
	if n := out["summary"].(map[string]interface{})["episodes"].(float64); n != 1 {
		t.Fatalf("min_loss_pct: %v", n)
	}
}

func TestCorrelateAndRank(t *testing.T) {
	s, _ := newTestServer(t)
	now := time.Now()
	a, b, c := "n-192-168-101-232", "n-192-168-101-219", "n-192-168-101-223"
	fillMinutes(t, s, a, now, 60, 2, func(ago int) int {
		if ago == 20 || ago == 21 {
			return 0
		}
		return 60
	})
	fillMinutes(t, s, b, now, 60, 3, func(ago int) int {
		if ago == 20 {
			return 0
		}
		return 60
	})
	fillMinutes(t, s, c, now, 60, 40, func(ago int) int {
		if ago == 45 {
			return 10
		}
		return 60
	})
	out := aiCall(t, s, "correlate_events", `{"ids":["`+a+`","`+b+`","`+c+`"],"range":"6h"}`)
	evs := out["common_events"].([]interface{})
	if len(evs) != 1 {
		t.Fatalf("common events: %v", evs)
	}
	ev := evs[0].(map[string]interface{})
	if ev["devices_involved"].(float64) != 2 || ev["peak_down_devices"].(float64) != 2 {
		t.Fatalf("event: %v", ev)
	}
	if out["pairwise_overlap"] == nil {
		t.Fatal("缺少两两重合度")
	}
	for _, d := range out["devices"].([]interface{}) {
		m := d.(map[string]interface{})
		if m["id"] == c && m["shared_minutes"].(float64) != 0 || m["id"] == b && m["shared_pct"].(float64) != 100 {
			t.Fatalf("device row: %v", m)
		}
	}

	out = aiCall(t, s, "rank_devices", `{"range":"6h","metric":"latency","limit":3}`)
	rk := out["ranking"].([]interface{})
	if len(rk) == 0 || rk[0].(map[string]interface{})["id"] != c {
		t.Fatalf("latency ranking: %v", rk)
	}
	out = aiCall(t, s, "rank_devices", `{"range":"6h","metric":"downtime"}`)
	if top := out["ranking"].([]interface{})[0].(map[string]interface{}); top["id"] != a || top["down_minutes"].(float64) != 2 {
		t.Fatalf("downtime ranking: %v", top)
	}
}

func TestCompareBaseline(t *testing.T) {
	s, _ := newTestServer(t)
	id := "n-192-168-101-232"
	now := time.Now()
	fillMinutes(t, s, id, now.Add(-24*time.Hour), 60, 5, nil) // 昨天同时段 5ms
	fillMinutes(t, s, id, now, 60, 50, nil)                   // 现在 50ms
	out := aiCall(t, s, "compare_baseline", `{"id":"`+id+`"}`)
	vs := out["vs_yesterday"].(map[string]interface{})
	if vs["avg_change_pct"].(float64) < 500 {
		t.Fatalf("vs yesterday: %v", vs)
	}
	if out["vs_last_week"].(map[string]interface{})["note"] == nil {
		t.Fatalf("上周无数据时应提示: %v", out["vs_last_week"])
	}
}

func TestUplinkPathAndImpact(t *testing.T) {
	s, _ := newTestServer(t)
	tp, _ := s.Topo.Snapshot()
	var id string
	for _, n := range tp.Nodes {
		if n.Attach.Switch == "access" && n.Attach.Port == "1" {
			id = n.ID
			break
		}
	}
	out := aiCall(t, s, "get_uplink_path", `{"id":"`+id+`"}`)
	path := out["path"].([]interface{})
	if len(path) != 2 || path[0].(map[string]interface{})["uplink_port"] != "26" || path[1].(map[string]interface{})["switch"] != "core" {
		t.Fatalf("path: %v", path)
	}
	if out["note"] != nil || len(out["same_port_devices"].([]interface{})) == 0 {
		t.Fatalf("path result: %v", out)
	}
	out = aiCall(t, s, "analyze_impact", `{}`)
	if out["abnormal_total"].(float64) != 0 || out["summary"] == nil {
		t.Fatalf("impact: %v", out)
	}
}

func TestPingNowValidation(t *testing.T) {
	s, _ := newTestServer(t)
	for _, args := range []string{`{"ip":"example.com"}`, `{"ip":"224.0.0.1"}`, `{"ip":"192.168.101.255"}`, `{}`} {
		if _, res := s.runAITool("ping_now", args, "test"); !strings.Contains(res, "error") {
			t.Fatalf("%s 应被拒绝: %s", args, res)
		}
	}
}
