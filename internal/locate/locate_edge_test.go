package locate

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"testing/quick"

	"netping/internal/config"
	"netping/internal/snmp"
	"netping/internal/topology"
)

func TestLocateServiceWithSimulatedSwitch(t *testing.T) {
	dir := t.TempDir()
	cfgMgr, err := config.Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	tpStore, err := topology.Open(filepath.Join(dir, "topology.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := tpStore.Update(func(tp *topology.Topology) error {
		tp.Switches = []*topology.Switch{
			{ID: "sw1", Name: "交换机1", Role: "access", IP: "127.0.0.1", Ports: []string{"1", "2", "3", "26"}, PortLocations: map[string]string{"2": "B02"}},
		}
		tp.Links = nil
		tp.Nodes = []*topology.Node{
			{ID: "n-unloc", IP: "10.0.0.1", MAC: mac(1), Flags: []string{"unlocated"}},
			{ID: "n-same", IP: "10.0.0.2", MAC: mac(2), Attach: topology.Endpoint{Switch: "sw1", Port: "2"}},
			{ID: "n-moved", IP: "10.0.0.3", MAC: mac(3), Attach: topology.Endpoint{Switch: "sw1", Port: "1"}},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Start simulated SNMP agent
	a := &snmp.Agent{Community: "ro"}
	a.SetString("1.3.6.1.2.1.1.1.0", "SimSwitch")
	a.SetString("1.3.6.1.2.1.1.5.0", "sw1")
	for i := 1; i <= 26; i++ {
		a.SetString(fmt.Sprintf("1.3.6.1.2.1.31.1.1.1.1.%d", i), fmt.Sprintf("GigabitEthernet 0/%d", i))
	}
	setMAC := func(m string, port int) {
		var b [6]int
		fmt.Sscanf(m, "%x:%x:%x:%x:%x:%x", &b[0], &b[1], &b[2], &b[3], &b[4], &b[5])
		a.SetInt(fmt.Sprintf("1.3.6.1.2.1.17.7.1.2.2.1.2.1.%d.%d.%d.%d.%d.%d", b[0], b[1], b[2], b[3], b[4], b[5]), int64(port))
	}
	setMAC(mac(1), 1)
	setMAC(mac(2), 2)
	setMAC(mac(3), 3)
	setMAC(mac(9), 1)
	addr, err := a.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	dataDir := filepath.Join(dir, "data")
	svc := New(cfgMgr, tpStore, dataDir)
	// Before community configured
	if _, err := svc.RunNow("test", false, false); err != ErrNoTargets {
		t.Fatalf("expected ErrNoTargets, got %v", err)
	}
	if _, err := svc.Dial("sw1"); err == nil {
		t.Fatal("Dial without community should fail")
	}
	if _, err := svc.Dial("missing"); err == nil {
		t.Fatal("Dial missing switch should fail")
	}
	if _, err := svc.Test("", ""); err == nil {
		t.Fatal("Test with empty host/comm should fail")
	}

	// Configure SNMP target
	c := cfgMgr.Get()
	c.Locate.Community = "ro"
	c.Locate.Notify = true
	c.Locate.Switches["sw1"] = config.LocateSwitch{Host: addr}
	if err := cfgMgr.Update(c); err != nil {
		t.Fatal(err)
	}
	svc.Trigger()
	if svc.Running() || len(svc.Targets()) != 1 {
		t.Fatalf("Targets/Running unexpected: %v %v", svc.Running(), svc.Targets())
	}
	// Test connection
	resTest, err := svc.Test(addr, "ro")
	if err != nil || resTest["mac_entries"] != 4 {
		t.Fatalf("Test = %+v err=%v", resTest, err)
	}
	// Dial directly
	cl, err := svc.Dial("sw1")
	if err != nil {
		t.Fatal(err)
	}
	cl.Close()

	var notifiedTitle string
	svc.Notify = func(title string, lines []string) { notifiedTitle = title + ":" + strings.Join(lines, ";") }

	// RunNow locates n-unloc, completes location for n-same, suggests move for n-moved
	rr, err := svc.RunNow("manual", false, false)
	if err != nil || len(rr.Changes) != 2 || len(rr.Suggestions) != 1 || len(rr.Strangers) != 1 {
		t.Fatalf("RunNow = %+v err=%v", rr, err)
	}
	if !strings.Contains(notifiedTitle, "端口定位") || !strings.Contains(notifiedTitle, "端口变化待确认") {
		t.Fatalf("Notify = %q", notifiedTitle)
	}
	if svc.Last() == nil {
		t.Fatal("Last() should be populated")
	}

	// Reload persisted locate_last.json
	svc2 := New(cfgMgr, tpStore, dataDir)
	if svc2.Last() == nil || svc2.Last().Reason != "manual" {
		t.Fatalf("svc2.Last = %+v", svc2.Last())
	}

	// MACTable & Find & ApplyMove
	rows, _, err := svc.MACTable("sw1", false)
	if err != nil || len(rows) != 3 {
		t.Fatalf("MACTable = %+v err=%v", rows, err)
	}
	if _, _, err := svc.MACTable("missing", false); err == nil {
		t.Fatal("MACTable missing switch should fail")
	}
	p, seen, why, err := svc.Find(mac(3), false)
	if err != nil || p == nil || p.Port != "3" || len(seen) == 0 || why != "" {
		t.Fatalf("Find moved = %+v seen=%v why=%q err=%v", p, seen, why, err)
	}
	p, _, why, err = svc.Find("AA:BB:CC:DD:EE:FF", false)
	if err != nil || p != nil || why == "" {
		t.Fatalf("Find absent = %+v why=%q err=%v", p, why, err)
	}
	if _, err := svc.ApplyMove("n-moved"); err != nil {
		t.Fatal(err)
	}
	tp, _ := tpStore.Snapshot()
	if tp.FindNode("n-moved").Attach.Port != "3" {
		t.Fatalf("ApplyMove failed: %+v", tp.FindNode("n-moved").Attach)
	}
}

func TestMatchPortAndAnalyzeEdgeCases(t *testing.T) {
	if IsPhysical("Vlan1") || IsAggregate("GigabitEthernet0/1") || kindOf("123") != "" {
		t.Fatal("IsPhysical/IsAggregate/kindOf mismatch")
	}
	sw := &topology.Switch{ID: "sw", Ports: []string{"1", "2"}}
	if got := MatchPort(sw, "Vlan-interface 1", nil); got != "Vlan-interface1" {
		t.Fatalf("MatchPort non-short = %q", got)
	}

	// Ambiguous MAC across two switches with same port MAC count
	tp := &topology.Topology{
		Switches: []*topology.Switch{{ID: "s1"}, {ID: "s2"}},
		Nodes:    []*topology.Node{{ID: "n1", MAC: mac(1)}},
	}
	tables := map[string]*snmp.Table{
		"s1": {Entries: []snmp.FDBEntry{{MAC: mac(1), IfName: "Gi0/1"}}},
		"s2": {Entries: []snmp.FDBEntry{{MAC: mac(1), IfName: "Gi0/2"}}},
	}
	a := Analyze(tp, tables)
	if len(a.Ambiguous[mac(1)]) != 2 {
		t.Fatalf("expected ambiguous sighting: %+v", a.Ambiguous)
	}
	res := Apply(tp, a, Options{})
	if len(res.Unresolved) != 1 {
		t.Fatalf("expected unresolved: %+v", res.Unresolved)
	}
}

// 性质测试：无论 MAC 地址表如何分布，Apply 后拓扑始终合法，且已定位的节点均指向存在的交换机，不会残留 unlocated 标记。
func TestLocateApplyPreservesTopologyValidity(t *testing.T) {
	if err := quick.Check(func(seed uint8, applyMoves bool) bool {
		tp := &topology.Topology{
			Version: 1,
			Site:    topology.Site{Name: "p"},
			Switches: []*topology.Switch{
				{ID: "sw1", Role: "core", Ports: []string{"1", "2", "24"}},
				{ID: "sw2", Role: "access", Ports: []string{"1", "2", "24"}},
			},
			Links: []*topology.Link{
				{ID: "l1", From: topology.Endpoint{Switch: "sw1", Port: "24"}, To: topology.Endpoint{Switch: "sw2", Port: "24"}},
			},
			Nodes: []*topology.Node{
				{ID: "n1", IP: "10.0.0.1", MAC: mac(1), Flags: []string{"unlocated"}},
				{ID: "n2", IP: "10.0.0.2", MAC: mac(2), Attach: topology.Endpoint{Switch: "sw1", Port: "1"}},
			},
		}
		tp.Normalize()
		tables := map[string]*snmp.Table{
			"sw1": {Entries: []snmp.FDBEntry{
				{MAC: mac(1), IfName: "Gi0/24"},
				{MAC: mac(2), IfName: fmt.Sprintf("Gi0/%d", int(seed%2)+1)},
			}},
			"sw2": {Entries: []snmp.FDBEntry{
				{MAC: mac(1), IfName: fmt.Sprintf("Gi0/%d", int(seed%2)+1)},
			}},
		}
		Apply(tp, Analyze(tp, tables), Options{ApplyMoves: applyMoves})
		if len(tp.Validate()) > 0 {
			return false
		}
		n1 := tp.FindNode("n1")
		return n1.Attach.Switch == "sw2" && !n1.HasFlag("unlocated")
	}, &quick.Config{MaxCount: 80}); err != nil {
		t.Fatal(err)
	}
}
