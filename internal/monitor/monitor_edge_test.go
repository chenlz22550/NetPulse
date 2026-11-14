package monitor

import (
	"testing"
	"testing/quick"
	"time"

	"netping/internal/config"
	"netping/internal/pinger"
	"netping/internal/store"
	"netping/internal/topology"
)

func TestBuildTargetsAndMonitorQueries(t *testing.T) {
	off := false
	tp := &topology.Topology{
		Switches: []*topology.Switch{
			{ID: "sw1", IP: "10.0.0.1", Monitor: topology.MonitorSpec{IntervalMs: 500, TimeoutMs: 800, LatencyWarnMs: 50, LossWarnPct: 10}},
			{ID: "sw-noip"},
			{ID: "sw-off", IP: "10.0.0.2", Monitor: topology.MonitorSpec{Enabled: &off}},
		},
		Nodes: []*topology.Node{
			{ID: "n1", IP: "10.0.0.10"},
			{ID: "n-off", IP: "10.0.0.11", Monitor: topology.MonitorSpec{Enabled: &off}},
		},
	}
	defs := config.PingDefaults{IntervalMs: 1000, TimeoutMs: 1000, PacketSize: 32, LatencyWarnMs: 100, LossWarnPct: 20}
	tgs := BuildTargets(tp, defs)
	if len(tgs) != 2 {
		t.Fatalf("BuildTargets = %d, want 2", len(tgs))
	}
	if tgs[0].ID != "sw1" || tgs[0].Interval != 500*time.Millisecond || tgs[0].Timeout != 800*time.Millisecond || tgs[0].LatencyWarnMs != 50 || tgs[0].LossWarnPct != 10 {
		t.Fatalf("sw1 target override mismatch: %+v", tgs[0])
	}
	if tgs[1].ID != "n1" || tgs[1].Interval != time.Second || tgs[1].LatencyWarnMs != 100 || tgs[1].LossWarnPct != 20 {
		t.Fatalf("n1 target default mismatch: %+v", tgs[1])
	}

	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := New(st, nil)
	defer m.Close()

	// Status/Recent on missing target
	if _, ok := m.Status("missing"); ok {
		t.Fatal("missing status should be false")
	}
	if got := m.Recent("missing", 0); got != nil {
		t.Fatalf("missing recent = %v", got)
	}

	// Sync targets with paused monitor (safe without real network ping)
	m.SetPaused(true)
	m.Sync(tgs)
	if sts := m.Statuses(false); len(sts) != 2 || sts[0].ID != "n1" || sts[1].ID != "sw1" {
		t.Fatalf("Statuses sorted mismatch: %+v", sts)
	}
	if s1, ok := m.Status("n1"); !ok || s1.State != StatePaused {
		t.Fatalf("Status(n1) = %+v, %v", s1, ok)
	}
	// Update target IP triggers worker replacement
	tgs[1].IP = "10.0.0.99"
	tgs[1].Interval = 200 * time.Millisecond
	m.Sync(tgs)
	if s1, ok := m.Status("n1"); !ok || s1.IP != "10.0.0.99" {
		t.Fatalf("updated IP mismatch: %+v", s1)
	}
	// Remove one target
	m.Sync(tgs[:1])
	if sts := m.Statuses(true); len(sts) != 1 || sts[0].ID != "sw1" {
		t.Fatalf("after remove = %+v", sts)
	}
}

func TestWorkerClassificationAndMarkPaused(t *testing.T) {
	m := New(nil, func() int { return 3 })
	defer m.Close()
	w := newWorker(Target{ID: "x", IP: "1.1.1.1", Interval: time.Second, LatencyWarnMs: 50, LossWarnPct: 25}, m)
	if st := w.status(false); st.State != StateUnknown {
		t.Fatalf("initial state = %s, want unknown", st.State)
	}
	now := time.Now()
	w.record(now, pinger.Result{OK: true, RTT: 80 * time.Millisecond})
	if st := w.status(false); st.State != StateSlow || st.Avg1m != 80 {
		t.Fatalf("slow state = %+v", st)
	}
	w.markPaused()
	if w.state != StatePaused {
		t.Fatalf("markPaused failed: %s", w.state)
	}
	// Queue full drops cleanly without blocking
	full := &Monitor{flushCh: make(chan flushItem, 1)}
	full.enqueue(flushItem{id: "a"})
	full.enqueue(flushItem{id: "b"})
}

// 性质测试：任意样本序列计算出的指标满足 Sent1m >= Recv 约束、0 <= Loss <= 100、Min1m <= Avg1m <= Max1m（有成功样本时）且 Jitter1m >= 0。
func TestMonitorStatsInvariants(t *testing.T) {
	m := New(nil, func() int { return 5 })
	defer m.Close()
	if err := quick.Check(func(pattern uint64, rttBase uint8) bool {
		w := newWorker(Target{ID: "p", Interval: time.Second, LatencyWarnMs: 100, LossWarnPct: 20}, m)
		now := time.Now()
		hasOK := false
		for i := 0; i < 30; i++ {
			ts := now.Add(time.Duration(i-30) * time.Second)
			if (pattern>>uint(i%64))&1 == 1 {
				hasOK = true
				rtt := time.Duration(int(rttBase%150)+1) * time.Millisecond
				w.record(ts, pinger.Result{OK: true, RTT: rtt})
			} else {
				w.record(ts, pinger.Result{OK: false})
			}
		}
		st := w.status(true)
		if st.Sent1m != 30 || st.Loss1m < 0 || st.Loss1m > 100 || st.Jitter1m < 0 {
			return false
		}
		if hasOK && (st.Min1m > st.Avg1m+0.01 || st.Avg1m > st.Max1m+0.01) {
			return false
		}
		return len(st.Spark) == 30
	}, &quick.Config{MaxCount: 100}); err != nil {
		t.Fatal(err)
	}
}
