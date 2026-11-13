package monitor

import (
	"reflect"
	"testing"
	"time"

	"netping/internal/pinger"
)

func TestIntervalChangePreservesHistory(t *testing.T) {
	m := New(nil, nil)
	defer m.Close()
	target := Target{ID: "x", Interval: 10 * time.Second, LatencyWarnMs: 100, LossWarnPct: 20}
	w := newWorker(target, m)
	base := time.Now().Add(-2 * time.Hour)
	// 先让环形缓冲绕回起点，验证扩容时不会打乱样本顺序。
	for i := 0; i < 400; i++ {
		w.record(base.Add(time.Duration(i)*10*time.Second), pinger.Result{OK: true, RTT: time.Millisecond})
	}
	before := w.recent(0)
	target.Interval = time.Second
	w.setTarget(target)
	if got := w.recent(0); !reflect.DeepEqual(got, before) {
		t.Fatal("resizing lost or reordered existing samples")
	}
	start := base.Add(4000 * time.Second)
	for i := 0; i < 3600; i++ {
		w.record(start.Add(time.Duration(i)*time.Second), pinger.Result{OK: i%2 == 0, RTT: 2 * time.Millisecond})
	}
	samples := w.recent(start.UnixMilli())
	if len(samples) != 3600 {
		t.Fatalf("one-hour history has %d samples, want 3600", len(samples))
	}
	for i, sample := range samples {
		if sample.T != start.Add(time.Duration(i)*time.Second).UnixMilli() {
			t.Fatalf("sample %d out of order", i)
		}
	}
	w.mu.Lock()
	st := w.computeLocked(start.Add(time.Hour).UnixMilli(), false)
	w.mu.Unlock()
	if st.Loss1h != 50 || st.Avg1h != 2 {
		t.Fatalf("one-hour stats: loss=%v avg=%v", st.Loss1h, st.Avg1h)
	}
	before = w.recent(0)
	target.Interval = time.Minute
	w.setTarget(target)
	if got := w.recent(0); !reflect.DeepEqual(got, before) {
		t.Fatal("slower sampling discarded existing history")
	}
}

func TestStatsAndClassify(t *testing.T) {
	m := New(nil, func() int { return 3 })
	w := newWorker(Target{ID: "x", IP: "1.1.1.1", Interval: time.Second, LatencyWarnMs: 100, LossWarnPct: 20}, m)
	base := time.Now().Add(-30 * time.Second)
	rtts := []float64{10, 20, -1, 10, 20, 10, 20, 10, -1, 10}
	for i, r := range rtts {
		res := pinger.Result{OK: r >= 0, RTT: time.Duration(r * float64(time.Millisecond))}
		w.record(base.Add(time.Duration(i)*time.Second), res)
	}
	st := w.status(false)
	if st.Sent1m != 10 || st.Loss1m != 20 {
		t.Fatalf("sent=%d loss=%v", st.Sent1m, st.Loss1m)
	}
	if st.Avg1m != 13.75 || st.Min1m != 10 || st.Max1m != 20 {
		t.Fatalf("avg=%v min=%v max=%v", st.Avg1m, st.Min1m, st.Max1m)
	}
	if st.State != StateLoss {
		t.Fatalf("state = %s", st.State)
	}
	for i := 0; i < 3; i++ {
		w.record(base.Add(time.Duration(10+i)*time.Second), pinger.Result{})
	}
	if st := w.status(true); st.State != StateDown || st.ConsecFail != 3 || len(st.Spark) != 13 {
		t.Fatalf("state=%s fail=%d spark=%d", st.State, st.ConsecFail, len(st.Spark))
	}
}

func TestMinuteFlush(t *testing.T) {
	// 不启动后台写入协程，直接从队列读取，避免竞争。
	m := &Monitor{flushCh: make(chan flushItem, 8), downAfter: func() int { return 5 }}
	w := newWorker(Target{ID: "x", IP: "1.1.1.1", Interval: time.Second, LatencyWarnMs: 100, LossWarnPct: 20}, m)
	t0 := time.Now().Truncate(time.Minute).Add(-2 * time.Minute)
	w.record(t0.Add(10*time.Second), pinger.Result{OK: true, RTT: 2 * time.Millisecond})
	w.record(t0.Add(20*time.Second), pinger.Result{})
	w.record(t0.Add(70*time.Second), pinger.Result{OK: true, RTT: time.Millisecond})
	select {
	case it := <-m.flushCh:
		if it.m.Sent != 2 || it.m.Recv != 1 || it.m.Avg != 2 || it.m.T != t0.Unix() {
			t.Fatalf("flushed %+v", it.m)
		}
	default:
		t.Fatal("minute not flushed")
	}
}

func TestPauseReason(t *testing.T) {
	m := New(nil, nil)
	in := true
	m.SetSchedule(func() bool { return in })
	if m.Paused() || m.PauseReason() != "" {
		t.Fatal("should run")
	}
	in = false
	if !m.Paused() || m.PauseReason() != "schedule" || m.ManualPaused() {
		t.Fatal("should pause by schedule")
	}
	m.SetPaused(true)
	if m.PauseReason() != "manual" {
		t.Fatal("manual pause takes precedence")
	}
}
