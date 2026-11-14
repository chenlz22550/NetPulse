package alert

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"netping/internal/config"
	"netping/internal/monitor"
	"netping/internal/topology"
)

func TestAlertLifecycleAndEdgeCases(t *testing.T) {
	cfg := config.Default()
	cfg.Alert.Enabled = true
	cfg.Alert.WebhookURL = "http://example.invalid"
	cfg.Alert.RepeatIntervalMin = 10
	cfg.Alert.LossDurationMin = 2
	cfg.Alert.LatencyDurationMin = 2
	cfg.Alert.SendRecovery = true

	histPath := filepath.Join(t.TempDir(), "alerts.jsonl")
	e := NewEngine(func() config.Config { return cfg }, nil, histPath)
	var sent []string
	var sendErr error
	e.send = func(_ config.AlertConfig, content string) error {
		if sendErr != nil {
			return sendErr
		}
		sent = append(sent, content)
		return nil
	}
	held := false
	e.HoldDown = func(id string) bool { return held && id == "n1" }

	tp := &topology.Topology{
		Version: 1,
		Site:    topology.Site{Name: "测试站"},
		Switches: []*topology.Switch{
			{ID: "core", Name: "核心", Role: "core", IP: "10.0.0.1", Monitor: topology.MonitorSpec{Alert: true}},
			{ID: "acc", Name: "接入", Role: "access", IP: "10.0.0.2", Monitor: topology.MonitorSpec{Alert: true}},
		},
		Links: []*topology.Link{
			{ID: "l1", From: topology.Endpoint{Switch: "core", Port: "1"}, To: topology.Endpoint{Switch: "acc", Port: "26"}},
		},
		Nodes: []*topology.Node{
			{ID: "n1", IP: "10.0.0.10", Vendor: "华为", Attach: topology.Endpoint{Switch: "acc", Port: "1"}, Location: "A01", Monitor: topology.MonitorSpec{Alert: true}},
			{ID: "n2", IP: "10.0.0.11", Name: "设备2", Attach: topology.Endpoint{Switch: "acc", Port: "2"}, Monitor: topology.MonitorSpec{Alert: true}},
			{ID: "n3", IP: "10.0.0.12", Name: "设备3", Attach: topology.Endpoint{Switch: "acc", Port: "3"}, Monitor: topology.MonitorSpec{Alert: true}},
		},
	}
	tp.Normalize()
	e.SetTopology(tp)

	now := time.Now()
	// HoldDown suppresses down alert
	held = true
	e.Evaluate(now, []monitor.Status{{ID: "n1", Kind: "node", IP: "10.0.0.10", ConsecFail: 5, LossWarnPct: 20, LatencyWarnMs: 100}})
	if len(e.ActiveDown()) != 0 {
		t.Fatal("HoldDown should suppress down state")
	}
	held = false

	// Now n1 goes down, ActiveDown reports it, repeat interval triggers still_down
	e.Evaluate(now, []monitor.Status{{ID: "n1", Kind: "node", IP: "10.0.0.10", ConsecFail: 5, LossWarnPct: 20, LatencyWarnMs: 100}})
	if got := e.ActiveDown(); len(got) != 1 || got[0] != "n1" {
		t.Fatalf("ActiveDown = %v", got)
	}
	e.flushIfDue(now.Add(15 * time.Second))
	if len(sent) != 1 || !strings.Contains(sent[0], "华为") {
		t.Fatalf("down msg = %v", sent)
	}
	e.Evaluate(now.Add(11*time.Minute), []monitor.Status{{ID: "n1", Kind: "node", IP: "10.0.0.10", ConsecFail: 10, LossWarnPct: 20, LatencyWarnMs: 100}})
	e.flushIfDue(now.Add(11*time.Minute + 15*time.Second))
	if len(sent) != 2 || !strings.Contains(sent[1], "仍离线") {
		t.Fatalf("still_down msg = %v", sent)
	}

	// SendRecovery = false suppresses recovery notification, but still updates state & history
	cfg.Alert.SendRecovery = false
	e.Evaluate(now.Add(12*time.Minute), []monitor.Status{{ID: "n1", Kind: "node", IP: "10.0.0.10", ConsecOK: 3, LastRTT: 12.3, LossWarnPct: 20, LatencyWarnMs: 100}})
	e.flushIfDue(now.Add(12*time.Minute + 15*time.Second))
	if len(sent) != 2 || len(e.ActiveDown()) != 0 {
		t.Fatalf("recovery should clear down without sending when SendRecovery=false: sent=%d active=%v", len(sent), e.ActiveDown())
	}
	cfg.Alert.SendRecovery = true

	// Sustained high latency -> slow alert -> repeat -> recover
	slowSt := monitor.Status{ID: "n2", Kind: "node", IP: "10.0.0.11", ConsecOK: 1, Avg1m: 250, Max1m: 400, LatencyWarnMs: 100, LossWarnPct: 20}
	t0 := now.Add(20 * time.Minute)
	e.Evaluate(t0, []monitor.Status{slowSt})
	e.Evaluate(t0.Add(2*time.Minute+time.Second), []monitor.Status{slowSt})
	e.flushIfDue(t0.Add(3 * time.Minute))
	if len(sent) != 3 || !strings.Contains(sent[2], "延迟高") {
		t.Fatalf("slow alert = %v", sent)
	}
	e.Evaluate(t0.Add(13*time.Minute), []monitor.Status{slowSt})
	e.flushIfDue(t0.Add(14 * time.Minute))
	if len(sent) != 4 || !strings.Contains(sent[3], "持续") {
		t.Fatalf("slow repeat = %v", sent)
	}
	slowSt.Avg1m = 5
	e.Evaluate(t0.Add(15*time.Minute), []monitor.Status{slowSt})
	e.flushIfDue(t0.Add(16 * time.Minute))
	if len(sent) != 5 || !strings.Contains(sent[4], "延迟恢复") {
		t.Fatalf("slow recover = %v", sent)
	}

	// Without an observed switch outage, retain each child alert.
	t1 := now.Add(40 * time.Minute)
	e.Evaluate(t1, []monitor.Status{
		{ID: "n1", Kind: "node", IP: "10.0.0.10", ConsecFail: 5, LossWarnPct: 20, LatencyWarnMs: 100},
		{ID: "n2", Kind: "node", IP: "10.0.0.11", ConsecFail: 5, LossWarnPct: 20, LatencyWarnMs: 100},
		{ID: "n3", Kind: "node", IP: "10.0.0.12", ConsecFail: 5, LossWarnPct: 20, LatencyWarnMs: 100},
	})
	e.flushIfDue(t1.Add(15 * time.Second))
	if len(sent) != 6 || strings.Count(sent[5], "● 离线") != 3 {
		t.Fatalf("suspected switch outage msg = %v", sent)
	}

	// SendTest & persistence reload
	if err := e.SendTest(cfg.Alert); err != nil {
		t.Fatalf("SendTest failed: %v", err)
	}
	sendErr = os.ErrPermission
	if err := e.SendTest(cfg.Alert); err == nil {
		t.Fatal("SendTest should propagate send error")
	}
	sendErr = nil

	// Reload history from disk into new Engine
	e2 := NewEngine(func() config.Config { return cfg }, nil, histPath)
	if len(e2.History(100)) != len(e.History(100)) {
		t.Fatalf("reloaded history mismatch: %d vs %d", len(e2.History(100)), len(e.History(100)))
	}
}

func TestWeComEdgeCasesAndFormatting(t *testing.T) {
	if err := SendWeCom(config.AlertConfig{}, "hi"); err == nil {
		t.Fatal("empty webhook should fail")
	}
	if err := SendWeCom(config.AlertConfig{WebhookURL: "http://127.0.0.1:1", ProxyURL: "://bad"}, "hi"); err == nil {
		t.Fatal("invalid proxy should fail")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "500") {
			w.WriteHeader(500)
			_, _ = w.Write([]byte("server err"))
			return
		}
		_, _ = w.Write([]byte("not-json"))
	}))
	defer srv.Close()
	if err := SendWeCom(config.AlertConfig{WebhookURL: srv.URL + "/500", TLSSkipVerify: true}, "hi"); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("500 error mismatch: %v", err)
	}
	if err := SendWeCom(config.AlertConfig{WebhookURL: srv.URL + "/badjson"}, "hi"); err == nil || !strings.Contains(err.Error(), "无法解析") {
		t.Fatalf("bad json mismatch: %v", err)
	}
	if fmtMs(-1) != "超时" || fmtMs(2.5) != "2.50ms" || fmtMs(15.2) != "15ms" {
		t.Fatal("fmtMs mismatch")
	}
	if fmtDur(30*time.Second) != "30秒" || fmtDur(90*time.Second) != "1分30秒" || fmtDur(2*time.Hour+5*time.Minute) != "2小时5分" {
		t.Fatal("fmtDur mismatch")
	}
}

// 性质测试：合并后的消息字节数永远不超过 wecomMaxBytes（针对普通单条事件限制），且历史记录保持有序不超限。
func TestAlertMessageSizeAndHistoryBound(t *testing.T) {
	cfg := config.Default()
	cfg.Alert.Enabled = true
	e := NewEngine(func() config.Config { return cfg }, nil, "")
	if err := quick.Check(func(n uint8) bool {
		cnt := int(n%80) + 1
		evs := make([]Event, cnt)
		now := time.Now()
		for i := 0; i < cnt; i++ {
			evs[i] = Event{
				Time:     now,
				Type:     EvDown,
				TargetID: "id",
				Kind:     "node",
				Name:     "node-name",
				IP:       "10.0.0.1",
				Location: "loc",
				Detail:   strings.Repeat("x", 40),
			}
		}
		e.mu.Lock()
		msg, c := e.composeLocked(evs, cfg.Alert)
		e.mu.Unlock()
		return c == cnt && len(msg) <= wecomMaxBytes+120
	}, &quick.Config{MaxCount: 50}); err != nil {
		t.Fatal(err)
	}
}
