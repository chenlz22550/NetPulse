package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"netping/internal/config"
	"netping/internal/monitor"
	"netping/internal/pinger"
	"netping/internal/store"
	"netping/internal/topology"
)

func TestMainHelpers(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0:8090":   "http://127.0.0.1:8090/",
		"[::]:9000":      "http://127.0.0.1:9000/",
		":8080":          "http://127.0.0.1:8080/",
		"192.168.1.2:80": "http://192.168.1.2:80/",
		"invalid":        "http://127.0.0.1:8090/",
	}
	for in, want := range cases {
		if got := localURL(in); got != want {
			t.Fatalf("localURL(%q) = %q, want %q", in, got, want)
		}
	}

	dir := t.TempDir()
	f := setupLog(dir)
	if f == nil {
		t.Fatal("setupLog returned nil")
	}
	f.Close()
	if _, err := os.Stat(filepath.Join(dir, "logs", "netping.log")); err != nil {
		t.Fatalf("log file missing: %v", err)
	}

	// trayStatus across manual pause, schedule pause, and active states
	mon := monitor.New(nil, func() int { return 2 })
	defer mon.Close()
	inSched := true
	mon.SetSchedule(func() bool { return inSched })
	mon.SetPaused(true)
	tip, lvl, paused := trayStatus(mon)
	if !paused || !strings.Contains(tip, "手动暂停") || lvl == 0 {
		t.Fatalf("manual paused trayStatus: %q %d %v", tip, lvl, paused)
	}
	mon.SetPaused(false)
	inSched = false
	tip, _, paused = trayStatus(mon)
	if paused || !strings.Contains(tip, "非监控时段") {
		t.Fatalf("schedule paused trayStatus: %q %v", tip, paused)
	}
	inSched = true
	mon.Sync(monitor.BuildTargets(&topology.Topology{
		Nodes: []*topology.Node{{ID: "n1", IP: "127.0.0.1"}},
	}, config.Default().Ping))
	tip, _, _ = trayStatus(mon)
	if !strings.Contains(tip, "netPing") {
		t.Fatalf("active trayStatus: %q", tip)
	}

	// retentionLoop removes expired history files and stops cleanly
	stDir := filepath.Join(dir, "hist")
	st, err := store.New(stDir)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Append("n1", store.Minute{T: time.Now().AddDate(0, 0, -40).Unix(), Sent: 1, Recv: 1})
	cfgMgr, err := config.Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		retentionLoop(st, cfgMgr, stop)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	close(stop)
	<-done
	_ = pinger.Result{}
}
