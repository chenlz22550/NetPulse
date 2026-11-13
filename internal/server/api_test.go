package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"netping/internal/monitor"
	"netping/internal/store"
	"netping/internal/topology"
)

// 通过真实 HTTP 请求检查路由、认证、拓扑校验和磁盘保存，不访问实际设备。
func apiRequest(t *testing.T, ts *httptest.Server, method, path, body string, want int) []byte {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("", testPassword)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != want {
		t.Fatalf("%s %s: want %d, got %d: %s", method, path, want, resp.StatusCode, data)
	}
	return data
}

func TestAPITopologyLifecycle(t *testing.T) {
	s, ts := newTestServer(t)
	// 使用独立的最小拓扑，避免依赖内置机房数据。
	if err := s.Topo.Replace(&topology.Topology{Version: 1, Site: topology.Site{Name: "测试机房"}}); err != nil {
		t.Fatal(err)
	}
	apiRequest(t, ts, "POST", "/api/switches", `{"id":"core","name":"核心","role":"core","ports":["1","2"]}`, 200)
	apiRequest(t, ts, "POST", "/api/switches", `{"id":"edge","name":"接入","role":"access","ports":["1","2"]}`, 200)
	apiRequest(t, ts, "POST", "/api/links", `{"id":"uplink","from":{"switch":"core","port":"1"},"to":{"switch":"edge","port":"1"}}`, 200)
	apiRequest(t, ts, "POST", "/api/nodes", `{"id":"pc","name":"终端","ip":"10.0.0.10","hostname":"pc.local","locked":true,"attach":{"switch":"edge","port":"2"}}`, 200)

	// 重复 ID、无效引用和无效 IP 均不得改变已有拓扑或版本。
	before, rev := s.Topo.Snapshot()
	for _, body := range []string{
		`{"id":"pc","ip":"10.0.0.11"}`,
		`{"id":"bad","ip":"invalid"}`,
		`{"id":"bad","ip":"10.0.0.12","attach":{"switch":"missing","port":"1"}}`,
	} {
		apiRequest(t, ts, "POST", "/api/nodes", body, 400)
	}
	after, newRev := s.Topo.Snapshot()
	if !reflect.DeepEqual(before, after) || rev != newRev {
		t.Fatal("invalid request changed topology")
	}

	apiRequest(t, ts, "PUT", "/api/nodes/pc", `{"name":"更新终端","ip":"10.0.0.10","attach":{"switch":"edge","port":"2"}}`, 200)
	tp, _ := s.Topo.Snapshot()
	if n := tp.FindNode("pc"); n == nil || n.Name != "更新终端" || !n.Locked || n.Hostname != "pc.local" {
		t.Fatalf("node metadata lost: %+v", n)
	}
	apiRequest(t, ts, "PUT", "/api/switches/edge", `{"id":"edge-renamed","name":"接入","role":"access","ports":["1","2"]}`, 200)
	tp, _ = s.Topo.Snapshot()
	if tp.FindNode("pc").Attach.Switch != "edge-renamed" || tp.Links[0].To.Switch != "edge-renamed" {
		t.Fatal("rename did not update references")
	}

	// 坐标舍入、告警批量更新、暂停状态及回调。
	apiRequest(t, ts, "POST", "/api/positions", `{"nodes":{"pc":{"x":10.6,"y":-3.5}},"switches":{"core":{"x":42.4,"y":5.6}}}`, 200)
	tp, _ = s.Topo.Snapshot()
	if *tp.FindNode("pc").Position != (topology.Position{X: 11, Y: -4}) || *tp.FindSwitch("core").Position != (topology.Position{X: 42, Y: 6}) {
		t.Fatal("positions not rounded")
	}
	apiRequest(t, ts, "POST", "/api/alerting", `{"ids":["pc","core"],"enabled":true}`, 200)
	tp, _ = s.Topo.Snapshot()
	if !tp.FindNode("pc").Monitor.Alert || !tp.FindSwitch("core").Monitor.Alert {
		t.Fatal("alert flags not saved")
	}
	changes := make(chan bool, 2)
	s.OnPauseChange = func(paused bool) { changes <- paused }
	for _, paused := range []bool{false, true} {
		apiRequest(t, ts, "POST", "/api/monitor/pause", fmtJSON(map[string]bool{"paused": paused}), 200)
		if got := <-changes; got != paused || s.Mon.ManualPaused() != paused {
			t.Fatalf("pause state: %v", got)
		}
	}
	apiRequest(t, ts, "GET", "/api/status", "", 200)

	// 下载、删除交换机、解除接线，并从磁盘重开确认保存结果。
	yamlData := apiRequest(t, ts, "GET", "/api/topology/yaml?download=1", "", 200)
	if _, err := topology.Parse(yamlData); err != nil {
		t.Fatal(err)
	}
	apiRequest(t, ts, "DELETE", "/api/switches/edge-renamed", "", 200)
	tp, _ = s.Topo.Snapshot()
	if tp.FindSwitch("edge-renamed") != nil || len(tp.Links) != 0 || tp.FindNode("pc").Attach.Switch != "" || !tp.FindNode("pc").HasFlag("unlocated") {
		t.Fatal("switch removal did not detach node and remove link")
	}
	reopened, err := topology.Open(s.Topo.Path())
	if err != nil {
		t.Fatal(err)
	}
	disk, _ := reopened.Snapshot()
	if !reflect.DeepEqual(tp, disk) {
		t.Fatal("disk topology differs from API result")
	}
	apiRequest(t, ts, "DELETE", "/api/nodes/pc", "", 200)
	apiRequest(t, ts, "DELETE", "/api/nodes/pc", "", 404)
	apiRequest(t, ts, "DELETE", "/api/links/uplink", "", 404)
	apiRequest(t, ts, "GET", "/api/unknown", "", 404)
}

func TestAPIHistoryAndValidation(t *testing.T) {
	s, ts := newTestServer(t)
	now := time.Now().Add(-2 * time.Minute).Truncate(time.Minute).Unix()
	if err := s.Store.Append("history-test", store.Minute{T: now, Sent: 10, Recv: 8, Avg: 12.5, Min: 5, Max: 20}); err != nil {
		t.Fatal(err)
	}
	data := apiRequest(t, ts, "GET", "/api/history/history-test?range=6h", "", 200)
	var out struct {
		ID, Range string
		Points    []Point `json:"points"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != "history-test" || out.Range != "6h" || len(out.Points) != 1 {
		t.Fatalf("history: %s", data)
	}
	p := out.Points[0]
	if p.T != now*1000 || p.Sent != 10 || p.Loss != 20 || p.Avg == nil || *p.Avg != 12.5 {
		t.Fatalf("point: %+v", p)
	}
	apiRequest(t, ts, "GET", "/api/history/history-test?range=bad", "", 400)
	apiRequest(t, ts, "GET", "/api/history/invalid%20id", "", 400)
	apiRequest(t, ts, "POST", "/api/nodes", `{"id":`, 400)
	apiRequest(t, ts, "POST", "/api/alerting", `{"ids":[]}`, 400)
}

func TestBucketSamples(t *testing.T) {
	for _, tc := range []struct {
		name    string
		samples []monitor.Sample
		want    []Point
	}{
		{"empty", nil, nil},
		{"mixed and boundary", []monitor.Sample{{T: 1000, RTT: 10}, {T: 1500, RTT: -1}, {T: 1999, RTT: 20}, {T: 2000, RTT: -1}, {T: 4000, RTT: 0}}, []Point{
			{T: 1000, Avg: fp(15), Min: fp(10), Max: fp(20), Loss: 33.33, Sent: 3},
			{T: 2000, Loss: 100, Sent: 1},
			{T: 4000, Avg: fp(0), Min: fp(0), Max: fp(0), Sent: 1},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := bucketSamples(tc.samples, time.Second); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v; want %+v", got, tc.want)
			}
		})
	}
}

func TestBucketMinutes(t *testing.T) {
	// 输入乱序；均值必须按接收次数加权，完全丢包不能混入延迟范围。
	recs := []store.Minute{
		{T: 240, Sent: 2, Recv: 0},
		{T: 60, Sent: 4, Recv: 1, Avg: 10, Min: 10, Max: 10},
		{T: 0, Sent: 3, Recv: 3, Avg: 30, Min: 20, Max: 40},
		{T: 120},
	}
	want := []Point{
		{T: 0, Sent: 7, Avg: fp(25), Min: fp(10), Max: fp(40), Loss: 42.86},
		{T: 120000},
		{T: 240000, Sent: 2, Loss: 100},
	}
	if got := bucketMinutes(recs, 120); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v; want %+v", got, want)
	}
	if got := bucketMinutes(nil, 60); len(got) != 0 {
		t.Fatalf("empty: %+v", got)
	}
}

// 使用正式 Start 的路由和内嵌资源，覆盖测试辅助服务器无法验证的启动路径。
func TestHTTPServerStartAndStream(t *testing.T) {
	s, _ := newTestServer(t)
	cfg := s.Cfg.Get()
	cfg.Listen = "127.0.0.1:0"
	if err := s.Cfg.Update(cfg); err != nil {
		t.Fatal(err)
	}
	addr, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, tc := range []struct {
		path, contains string
		auth           bool
		status         int
	}{
		{"/", "", false, 302},
		{"/login.html", "<html", false, 200},
		{"/", "<html", true, 200},
		{"/app.js", "", true, 200},
		{"/style.css", "", true, 200},
		{"/api/meta", `"version"`, true, 200},
	} {
		req, err := http.NewRequest("GET", "http://"+addr+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if tc.auth {
			req.SetBasicAuth("", testPassword)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if resp.StatusCode != tc.status || !strings.Contains(string(data), tc.contains) {
			t.Fatalf("%s: status %d, body %s", tc.path, resp.StatusCode, data)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+addr+"/api/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("", testPassword)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream response: %d %v", resp.StatusCode, resp.Header)
	}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var payload struct {
			Paused     bool
			Summary    summary
			ServerTime int64 `json:"server_time"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
			t.Fatal(err)
		}
		if !payload.Paused || payload.Summary.ByState == nil || payload.ServerTime <= 0 {
			t.Fatalf("invalid stream payload: %s", line)
		}
		cancel()
		return
	}
	t.Fatalf("stream ended without status: %v", scanner.Err())
}
