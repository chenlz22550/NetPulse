package monitor

import (
	"net"
	"testing"
	"time"

	"netping/internal/config"
	"netping/internal/topology"
)

func TestTCPMonitoringLifecycle(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	m := New(nil, func() int { return 2 })
	defer m.Close()
	target := Target{ID: TCPID("test", port), ParentID: "test", Kind: "tcp", IP: "127.0.0.1", Port: port, Interval: 200 * time.Millisecond, Timeout: 100 * time.Millisecond, LatencyWarnMs: 100, LossWarnPct: 20}
	m.Sync([]Target{target})
	wait := func(check func(Status) bool) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			st, _ := m.Status(target.ID)
			if check(st) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("TCP state did not converge")
	}
	wait(func(st Status) bool {
		return st.ConsecOK >= 2 && st.LastRTT >= 0 && st.Port == port && st.ParentID == "test"
	})
	if len(m.Statuses(false)) != 0 || len(m.TCPStatuses(false)) != 1 {
		t.Fatal("TCP targets changed ICMP inventory")
	}
	m.SetPaused(true)
	wait(func(st Status) bool { return st.State == StatePaused })
	m.SetPaused(false)
	ln.Close()
	wait(func(st Status) bool { return st.State == StateDown && st.LastRTT < 0 && st.Loss1m > 0 })
	if len(m.Recent(target.ID, 0)) < 4 {
		t.Fatal("TCP samples not retained")
	}
	m.Sync(nil)
	if _, ok := m.Status(target.ID); ok {
		t.Fatal("removed port still monitored")
	}
}

func TestBuildTCPAndICMPTargets(t *testing.T) {
	topo := &topology.Topology{Nodes: []*topology.Node{{ID: "n_1", IP: "127.0.0.1", Monitor: topology.MonitorSpec{TCPPorts: []int{22, 443}}}}}
	targets := BuildTargets(topo, config.Default().Ping)
	if len(targets) != 3 || targets[0].Kind != "node" || targets[1].Port != 22 || targets[2].Port != 443 {
		t.Fatalf("targets: %+v", targets)
	}
	for _, p := range []int{22, 443} {
		if !ValidTCPID(TCPID("n_1", p)) {
			t.Fatal("invalid generated history id")
		}
	}
	for _, id := range []string{"_tcp_0_n", "_tcp_65536_n", "_tcp_22_../x", "_tcp_022_n"} {
		if ValidTCPID(id) {
			t.Fatal("unsafe id", id)
		}
	}
	disabled := false
	topo.Nodes[0].Monitor.Enabled = &disabled
	if len(BuildTargets(topo, config.Default().Ping)) != 0 {
		t.Fatal("disabled node has active targets")
	}
}
