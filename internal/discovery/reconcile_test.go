package discovery

import (
	"strings"
	"testing"

	"netping/internal/config"
	"netping/internal/topology"
)

func topo(nodes ...*topology.Node) *topology.Topology {
	t := &topology.Topology{Version: 1, Site: topology.Site{Name: "t"},
		Switches: []*topology.Switch{{ID: "sw", Name: "sw", Role: "access", IP: "10.0.0.2"}}}
	t.Nodes = nodes
	t.Normalize()
	return t
}

func node(id, ip, mac string) *topology.Node {
	return &topology.Node{ID: id, IP: ip, MAC: mac, Attach: topology.Endpoint{Switch: "sw", Port: "1"}}
}

func host(ip, mac string) Host { return Host{IP: ip, MAC: mac, Ping: true, ARP: mac != ""} }

const (
	m1 = "60:D7:E3:00:00:01"
	m2 = "60:D7:E3:00:00:02"
	m3 = "60:D7:E3:00:00:03"
	m9 = "60:D7:E3:00:00:09"
)

func types(r Result) string {
	var s []string
	for _, c := range r.Changes {
		s = append(s, c.Type+":"+c.NodeID)
	}
	return strings.Join(s, ",")
}

func mustValid(t *testing.T, tp *topology.Topology) {
	t.Helper()
	if p := tp.Validate(); len(p) > 0 {
		t.Fatalf("topology invalid after reconcile: %v", p)
	}
}

func TestDHCPMoveAndSwap(t *testing.T) {
	tp := topo(node("a", "10.0.0.10", m1), node("b", "10.0.0.11", m2), node("c", "10.0.0.30", m3))
	r := Reconcile(tp, []Host{host("10.0.0.11", m1), host("10.0.0.10", m2), host("10.0.0.31", m3)}, Options{Tracking: "all", AutoAdd: true})
	mustValid(t, tp)
	if tp.FindNode("a").IP != "10.0.0.11" || tp.FindNode("b").IP != "10.0.0.10" || tp.FindNode("c").IP != "10.0.0.31" {
		t.Fatalf("moves wrong: %s", types(r))
	}
	if strings.Count(types(r), ChIPChanged) != 3 || len(r.Unknown) != 0 {
		t.Fatalf("changes: %s", types(r))
	}
}

func TestStaticLockedSuggestion(t *testing.T) {
	n := node("a", "10.0.0.10", m1)
	n.IPMode = "static"
	tp := topo(n)
	r := Reconcile(tp, []Host{host("10.0.0.12", m1)}, Options{Tracking: "all", AutoAdd: true})
	if tp.FindNode("a").IP != "10.0.0.10" || len(r.Suggestions) != 1 || r.Suggestions[0].SeenIP != "10.0.0.12" {
		t.Fatalf("static node must not move: %+v %s", r.Suggestions, types(r))
	}
	if len(tp.Nodes) != 1 {
		t.Fatal("registered MAC must not be auto-added again")
	}
	// dhcp 跟踪范围：未标记 dhcp 的节点只给建议
	tp2 := topo(node("b", "10.0.0.20", m2))
	r = Reconcile(tp2, []Host{host("10.0.0.21", m2)}, Options{Tracking: "dhcp"})
	if tp2.FindNode("b").IP != "10.0.0.20" || len(r.Suggestions) != 1 {
		t.Fatal("tracking=dhcp should only suggest")
	}
}

func TestReleaseAndAddNew(t *testing.T) {
	tp := topo(node("c", "10.0.0.20", m3))
	r := Reconcile(tp, []Host{host("10.0.0.20", m9)}, Options{Tracking: "all", AutoAdd: true})
	mustValid(t, tp)
	if tp.FindNode("c").IP != "" {
		t.Fatalf("stale IP should be released: %s", types(r))
	}
	nn := tp.FindNode("n-60d7e3000009")
	if nn == nil || nn.IP != "10.0.0.20" || !nn.HasFlag("unlocated") || !nn.HasFlag("discovered") || nn.Attach.Switch != "" {
		t.Fatalf("new node: %+v (%s)", nn, types(r))
	}
	// 之后设备 c 重新出现在新地址：自动恢复 IP
	r = Reconcile(tp, []Host{host("10.0.0.20", m9), host("10.0.0.25", m3)}, Options{Tracking: "all", AutoAdd: true})
	if tp.FindNode("c").IP != "10.0.0.25" {
		t.Fatalf("released node should come back: %s", types(r))
	}
}

func TestLearnMACAndVendor(t *testing.T) {
	known := node("k", "10.0.0.5", "60:D7:E3:AA:BB:CC")
	known.Vendor = "深大智能"
	tp := topo(known, node("x", "10.0.0.50", ""))
	r := Reconcile(tp, []Host{host("10.0.0.50", m1)}, Options{Tracking: "all"})
	x := tp.FindNode("x")
	if x.MAC != m1 || x.Vendor != "深大智能" || !strings.Contains(types(r), ChMACLearned) {
		t.Fatalf("learn: %+v %s", x, types(r))
	}
}

func TestConflictsAndAmbiguous(t *testing.T) {
	// 目标地址登记给了一个没有 MAC 的节点：冲突，不修改
	tp := topo(node("e", "10.0.0.60", m1), node("f", "10.0.0.61", ""))
	r := Reconcile(tp, []Host{host("10.0.0.61", m1)}, Options{Tracking: "all", AutoAdd: true})
	mustValid(t, tp)
	if tp.FindNode("e").IP != "10.0.0.60" || !strings.Contains(types(r), ChConflict) {
		t.Fatalf("conflict expected: %s", types(r))
	}
	// 一个 MAC 对应多个 IP：不跟踪
	tp = topo(node("g", "10.0.0.70", m2))
	Reconcile(tp, []Host{host("10.0.0.71", m2), host("10.0.0.72", m2)}, Options{Tracking: "all"})
	if tp.FindNode("g").IP != "10.0.0.70" {
		t.Fatal("ambiguous MAC must not move node")
	}
	// 交换机地址
	tp = topo(node("h", "10.0.0.80", m3))
	r = Reconcile(tp, []Host{host("10.0.0.2", m3)}, Options{Tracking: "all"})
	if tp.FindNode("h").IP != "10.0.0.80" || !strings.Contains(types(r), ChConflict) {
		t.Fatalf("switch IP conflict: %s", types(r))
	}
}

func TestAutoAddOff(t *testing.T) {
	tp := topo()
	r := Reconcile(tp, []Host{host("10.0.0.9", "02:00:00:00:00:01"), {IP: "10.0.0.8", Ping: true}}, Options{Tracking: "all", AutoAdd: false})
	if len(tp.Nodes) != 0 || len(r.Unknown) != 2 || r.Unknown[0].Vendor != "本地私有地址" {
		t.Fatalf("unknown: %+v", r.Unknown)
	}
}

func TestMergeHostsProxyARP(t *testing.T) {
	ips, _ := config.ExpandSubnets([]string{"10.0.0.0/27"}, nil)
	var arp []ARPEntry
	for _, ip := range ips[:12] {
		arp = append(arp, ARPEntry{IP: ip, MAC: "00:11:22:33:44:55"})
	}
	arp = append(arp, ARPEntry{IP: ips[20], MAC: m1})
	res := &ScanResult{}
	hosts := mergeHosts(ips, map[string]float64{ips[0]: 1}, arp, nil, res)
	if len(hosts) != 2 || hosts[0].MAC != "" || hosts[1].MAC != m1 || len(res.ProxyMACs) != 1 {
		t.Fatalf("hosts: %+v proxy=%v", hosts, res.ProxyMACs)
	}
}

func TestNormalizeMAC(t *testing.T) {
	for in, want := range map[string]string{
		"2:0:0:0:0:6":   "02:00:00:00:00:06",
		"02-00-00-00-00-06": "02:00:00:00:00:06",
		"ff:ff:ff:ff:ff:ff": "",
		"01:00:5e:00:00:01": "",
		"(incomplete)":      "",
	} {
		if got := normalizeMAC(in); got != want {
			t.Errorf("%s -> %q want %q", in, got, want)
		}
	}
}

func TestSharedMACAddedByIP(t *testing.T) {
	// 无线中继 MAC-NAT：一个 MAC 对应多个 IP。已有节点按该 MAC 登记，其余地址应按 IP 添加，且不记录该 MAC。
	tp := topo(node("rep", "10.0.0.130", m9))
	hosts := []Host{host("10.0.0.130", m9), host("10.0.0.156", m9), host("10.0.0.183", m9)}
	r := Reconcile(tp, hosts, Options{Tracking: "all", AutoAdd: true})
	mustValid(t, tp)
	if tp.FindNode("rep").IP != "10.0.0.130" {
		t.Fatalf("shared MAC must not move IP: %s", types(r))
	}
	if strings.Count(types(r), ChAdded) != 2 {
		t.Fatalf("want 2 added, got %s", types(r))
	}
	for _, n := range tp.Nodes[1:] {
		if n.MAC != "" || !strings.Contains(n.Note, m9) || !strings.HasPrefix(n.ID, "n-10-0-0-") {
			t.Fatalf("bad shared node: %+v", n)
		}
	}
	if len(hosts[1].SharedMAC) != 2 {
		t.Fatalf("SharedMAC not annotated: %+v", hosts[1])
	}
	// 再扫一次：稳定，无变化
	if r2 := Reconcile(tp, hosts, Options{Tracking: "all", AutoAdd: true}); len(r2.Changes) != 0 {
		t.Fatalf("second pass not stable: %s", types(r2))
	}
}

func TestARPOnlyAddedUnmonitored(t *testing.T) {
	tp := topo()
	h := Host{IP: "10.0.0.90", MAC: m1, ARP: true}
	Reconcile(tp, []Host{h, host("10.0.0.91", m2)}, Options{Tracking: "all", AutoAdd: true})
	mustValid(t, tp)
	a, b := tp.Nodes[0], tp.Nodes[1]
	if a.Monitor.IsEnabled() || !strings.Contains(strings.Join(a.Flags, ","), "arp_only") {
		t.Fatalf("arp-only node should be unmonitored: %+v", a)
	}
	if !b.Monitor.IsEnabled() || strings.Contains(strings.Join(b.Flags, ","), "arp_only") {
		t.Fatalf("pingable node should be monitored: %+v", b)
	}
}

func TestJobOptionsDecorateNewNodes(t *testing.T) {
	tp := topo()
	opt := Options{Tracking: "all", AutoAdd: true, Tags: []string{"办公区"}, Alert: true, Source: "办公区"}
	Reconcile(tp, []Host{host("10.0.0.50", m1), {IP: "10.0.0.51", MAC: m2, ARP: true}}, opt)
	mustValid(t, tp)
	a, b := tp.Nodes[0], tp.Nodes[1]
	if !a.Monitor.Alert || len(a.Tags) != 1 || a.Tags[0] != "办公区" || !strings.Contains(a.Note, "（办公区）") {
		t.Fatalf("decorated node: %+v", a)
	}
	if b.Monitor.Alert { // 不响应 ping、已关闭监控的设备不开告警
		t.Fatalf("arp-only node must not alert: %+v", b)
	}
}
