package alert

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"netping/internal/config"
	"netping/internal/monitor"
	"netping/internal/topology"
)

func newTestEngine(t *testing.T) (*Engine, *[]string) {
	cfg := config.Default()
	cfg.Alert.Enabled = true
	cfg.Alert.WebhookURL = "http://example.invalid"
	e := NewEngine(func() config.Config { return cfg }, nil, "")
	var sent []string
	e.send = func(_ config.AlertConfig, content string) error { sent = append(sent, content); return nil }
	topo := topology.Seed()
	for _, n := range topo.Nodes {
		n.Monitor.Alert = true
	}
	for _, sw := range topo.Switches {
		sw.Monitor.Alert = true
	}
	e.SetTopology(topo)
	return e, &sent
}

func TestDefaultNoAlert(t *testing.T) {
	e, sent := newTestEngine(t)
	e.SetTopology(topology.Seed()) // 内置拓扑：所有设备默认不告警
	now := time.Now()
	e.Evaluate(now, []monitor.Status{st("n-192-168-101-254", "node", 10, 0)})
	e.flushIfDue(now.Add(time.Minute))
	if len(*sent) != 0 || len(e.History(10)) != 0 {
		t.Fatalf("alert sent for device with alert disabled: %v", *sent)
	}
	// 开启后，已离线的设备立即告警
	topo := topology.Seed()
	topo.FindNode("n-192-168-101-254").Monitor.Alert = true
	e.SetTopology(topo)
	e.Evaluate(now.Add(time.Minute), []monitor.Status{st("n-192-168-101-254", "node", 70, 0)})
	e.flushIfDue(now.Add(2 * time.Minute))
	if len(*sent) != 1 {
		t.Fatalf("expected alert after enabling, got %d", len(*sent))
	}
}

func st(id, kind string, fail, ok int) monitor.Status {
	return monitor.Status{ID: id, Kind: kind, IP: "192.168.101.x", ConsecFail: fail, ConsecOK: ok, LossWarnPct: 20, LatencyWarnMs: 100}
}

func TestDownRecover(t *testing.T) {
	e, sent := newTestEngine(t)
	now := time.Now()
	e.Evaluate(now, []monitor.Status{st("n-192-168-101-254", "node", 4, 0)})
	e.flushIfDue(now.Add(time.Minute))
	if len(*sent) != 0 {
		t.Fatal("should not alert before threshold")
	}
	e.Evaluate(now, []monitor.Status{st("n-192-168-101-254", "node", 5, 0)})
	e.flushIfDue(now.Add(5 * time.Second))
	if len(*sent) != 0 {
		t.Fatal("should wait for merge window")
	}
	e.flushIfDue(now.Add(11 * time.Second))
	if len(*sent) != 1 || !strings.Contains((*sent)[0], "离线") || !strings.Contains((*sent)[0], "Gi0/11") || !strings.Contains((*sent)[0], "[关键]") {
		t.Fatalf("down message: %v", *sent)
	}
	later := now.Add(2 * time.Minute)
	e.Evaluate(later, []monitor.Status{st("n-192-168-101-254", "node", 0, 3)})
	e.flushIfDue(later.Add(11 * time.Second))
	if len(*sent) != 2 || !strings.Contains((*sent)[1], "恢复") || !strings.Contains((*sent)[1], "2分0秒") {
		t.Fatalf("recover message: %v", *sent)
	}
	if h := e.History(10); len(h) != 2 || !h[0].Sent {
		t.Fatalf("history: %+v", h)
	}
}

func TestSwitchOutageMerged(t *testing.T) {
	e, sent := newTestEngine(t)
	now := time.Now()
	sts := []monitor.Status{st("access", "switch", 5, 0)}
	for _, n := range topology.Seed().Nodes {
		if n.Attach.Switch == "access" && n.IP != "" {
			sts = append(sts, st(n.ID, "node", 5, 0))
		}
	}
	e.Evaluate(now, sts)
	e.flushIfDue(now.Add(11 * time.Second))
	if len(*sent) != 1 {
		t.Fatalf("sent %d messages", len(*sent))
	}
	msg := (*sent)[0]
	if !strings.Contains(msg, "交换机 锐捷二层交换机 不可达") || strings.Count(msg, "● 离线") != 1 {
		t.Fatalf("message not merged:\n%s", msg)
	}
}

func TestLossSustained(t *testing.T) {
	e, sent := newTestEngine(t)
	now := time.Now()
	s := st("n-192-168-101-88", "node", 0, 1)
	s.Sent1m, s.Loss1m = 60, 35
	e.Evaluate(now, []monitor.Status{s})
	e.Evaluate(now.Add(time.Minute), []monitor.Status{s})
	e.flushIfDue(now.Add(2 * time.Minute))
	if len(*sent) != 0 {
		t.Fatal("loss alert fired too early")
	}
	e.Evaluate(now.Add(2*time.Minute+time.Second), []monitor.Status{s})
	e.flushIfDue(now.Add(3 * time.Minute))
	if len(*sent) != 1 || !strings.Contains((*sent)[0], "丢包 35%") {
		t.Fatalf("loss alert: %v", *sent)
	}
}

func TestSendWeCom(t *testing.T) {
	var got map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		if strings.Contains(r.URL.RawQuery, "bad") {
			_, _ = w.Write([]byte(`{"errcode":93000,"errmsg":"invalid webhook url"}`))
			return
		}
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer srv.Close()
	cfg := config.Default().Alert
	cfg.WebhookURL = srv.URL + "/cgi-bin/webhook/send?key=good"
	if err := SendWeCom(cfg, "hello"); err != nil {
		t.Fatal(err)
	}
	if got["msgtype"] != "markdown" {
		t.Fatalf("payload %v", got)
	}
	cfg.WebhookURL = srv.URL + "/cgi-bin/webhook/send?key=bad"
	if err := SendWeCom(cfg, "hello"); err == nil || !strings.Contains(err.Error(), "93000") {
		t.Fatalf("expected errcode error, got %v", err)
	}
}
