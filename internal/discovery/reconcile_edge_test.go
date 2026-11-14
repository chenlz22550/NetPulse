package discovery

import (
	"net/netip"
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

func TestARPTextAndMACNormalizationEdgeCases(t *testing.T) {
	// 12-hex format, invalid lengths, zero/broadcast, parseARPText
	if got := normalizeMAC("60d7e3000001"); got != "60:D7:E3:00:00:01" {
		t.Fatalf("12-hex normalizeMAC = %q", got)
	}
	for _, bad := range []string{"", "00:00:00:00:00:00", "ff:ff:ff:ff:ff:ff", "00:11:22:33:44", "zz:11:22:33:44:55", "001:11:22:33:44:55"} {
		if got := normalizeMAC(bad); got != "" {
			t.Fatalf("bad MAC %q normalized to %q", bad, got)
		}
	}
	if macString([]byte{1, 2, 3}) != "" {
		t.Fatal("short macString should be empty")
	}
	text := "? (192.168.1.1) at 0:e:c6:1:2:3 on en0 ifscope [ethernet]\n? (192.168.1.2) at (incomplete) on en0\n? (192.168.1.3) at ff:ff:ff:ff:ff:ff on en0\n"
	entries := parseARPText(text)
	if len(entries) != 1 || entries[0].IP != "192.168.1.1" || entries[0].MAC != "00:0E:C6:01:02:03" {
		t.Fatalf("parseARPText = %+v", entries)
	}
}

func TestNamesEdgeCases(t *testing.T) {
	if _, err := reverseName("not-ip"); err == nil {
		t.Fatal("invalid IP reverseName should fail")
	}
	if got, err := reverseName("192.168.1.5"); err != nil || got != "5.1.168.192.in-addr.arpa" {
		t.Fatalf("reverseName = %q, %v", got, err)
	}
	if _, err := parsePTR([]byte{1, 2, 3}); err == nil {
		t.Fatal("short packet should fail")
	}
	// Too many compression pointers (cycle detection)
	cyc := []byte{0xc0, 0x00}
	if _, err := readName(cyc, 0); err == nil {
		t.Fatal("pointer loop should fail")
	}
	if got := CleanHostname(strings.Repeat("a", 150)+".local", "10.0.0.1"); len(got) != 100 {
		t.Fatalf("CleanHostname truncation = %d", len(got))
	}
	_ = localHostname()
}

func TestServiceMethodsWithoutNetwork(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfgMgr, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	c := cfgMgr.Get()
	c.Discovery.Jobs = []config.ScanJob{
		{ID: "j1", Name: "网段1", Enabled: true, Subnets: []string{"192.168.10.0/24"}, IntervalMin: 30, AutoAdd: false, Alert: true, Tags: []string{"办公"}},
		{ID: "j2", Name: "机房", Enabled: false, Subnets: []string{}, IntervalMin: 30, AutoAdd: true},
	}
	if err := cfgMgr.Update(c); err != nil {
		t.Fatal(err)
	}
	tpStore, err := topology.Open(filepath.Join(dir, "topology.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	mon := monitor.New(nil, nil)
	defer mon.Close()
	dataDir := filepath.Join(dir, "data")
	_ = os.MkdirAll(dataDir, 0o755)

	// Pre-populate legacy discovery_last.json to verify migration in load()
	legacy := `{"finished":"2026-11-01T10:00:00Z","alive":1,"scanned":254,"unknown":[{"ip":"192.168.10.50","mac":"60:D7:E3:AA:00:01","ping":true,"arp":true}]}`
	_ = os.WriteFile(filepath.Join(dataDir, "discovery_last.json"), []byte(legacy), 0o644)

	svc := New(cfgMgr, tpStore, mon, dataDir)
	if svc.Last() == nil || svc.Running() || svc.HoldDown("any") {
		t.Fatalf("initial svc state unexpected: last=%v running=%v", svc.Last(), svc.Running())
	}
	if subs := svc.Subnets(); len(subs) != 1 || subs[0] != "192.168.10.0/24" {
		t.Fatalf("Subnets = %v", subs)
	}
	if eff := svc.JobSubnets(c.Discovery.Jobs[1]); len(eff) != 1 {
		t.Fatalf("JobSubnets fallback to site.subnet = %v", eff)
	}
	if st := svc.Status(); st["enabled"] != true || len(st["jobs"].([]JobStatus)) != 2 {
		t.Fatalf("Status = %+v", st)
	}

	// Inject scan result for j1 with Unknown & Suggestion, test AddUnknown / ApplySuggestion / Events / notify
	now := time.Now()
	svc.mu.Lock()
	svc.results["j1"] = &ScanResult{
		Job: "j1", JobName: "网段1", Finished: now, Scanned: 254, Alive: 2,
		Unknown:     []Host{{IP: "192.168.10.88", MAC: m1, Ping: true, ARP: true}, {IP: "192.168.10.9", MAC: m2, Ping: true, ARP: true}},
		Suggestions: []Suggestion{{NodeID: "n-192-168-101-254", CurrentIP: "192.168.101.254", SeenIP: "192.168.101.253", MAC: "AA:BB:CC:11:22:33"}},
	}
	svc.pending["n1"] = now.Add(time.Minute)
	svc.mu.Unlock()
	if !svc.HoldDown("n1") {
		t.Fatal("HoldDown should be true")
	}

	unk := svc.Unknown()
	if len(unk) < 2 || !ipLess(unk[0].IP, unk[1].IP) {
		t.Fatalf("Unknown not sorted by IP: %+v", unk)
	}
	if sgs := svc.Suggestions(); len(sgs) != 1 {
		t.Fatalf("Suggestions = %+v", sgs)
	}

	addedCalled := false
	svc.OnAdded = func() { addedCalled = true }
	added, err := svc.AddUnknown([]string{"192.168.10.88"})
	if err != nil || len(added) != 1 || !addedCalled {
		t.Fatalf("AddUnknown = %+v err=%v addedCalled=%v", added, err, addedCalled)
	}
	if _, err := svc.AddUnknown([]string{"192.168.10.200"}); err == nil {
		t.Fatal("AddUnknown missing IP should fail")
	}

	ch, err := svc.ApplySuggestion("n-192-168-101-254", "192.168.101.250", "dhcp")
	if err != nil || ch.IP != "192.168.101.250" {
		t.Fatalf("ApplySuggestion = %+v err=%v", ch, err)
	}
	if _, err := svc.ApplySuggestion("missing", "10.0.0.1", "-"); err == nil {
		t.Fatal("ApplySuggestion missing node should fail")
	}
	if evs := svc.Events(10); len(evs) < 2 {
		t.Fatalf("Events = %+v", evs)
	}

	// MarkKnown & annotate
	tp, _ := tpStore.Snapshot()
	hs := []Host{
		{IP: tp.Switches[1].IP, Ping: true},
		{IP: "192.168.101.250", Ping: true},
		{IP: "192.168.99.99", MAC: m1, Ping: true}, // moved
		{IP: "192.168.99.100", Local: true, Ping: true},
		{IP: "192.168.99.101", Ping: true},
	}
	MarkKnown(hs, tp, nil)
	if hs[0].Status != "switch" || hs[1].Status != "registered" || hs[2].Status != "moved" || hs[3].Status != "local" || hs[4].Status != "new" {
		t.Fatalf("MarkKnown statuses = %+v", hs)
	}
	annotate(hs, tp, Result{Changes: []Change{{Type: ChIPChanged, IP: "192.168.101.250"}}, Unknown: []Host{{IP: "192.168.99.101"}}})
	if hs[1].Status != ChIPChanged || hs[4].Status != "unknown" {
		t.Fatalf("annotate statuses = %+v", hs)
	}

	// ScanJobs / ScanAdhoc validation failures (without network)
	if _, err := svc.ScanJobs([]string{"missing"}, "test"); err == nil {
		t.Fatal("missing job should fail")
	}
	if _, err := svc.ScanAdhoc(nil, false, "test"); err == nil {
		t.Fatal("empty adhoc subnets should fail")
	}
	if _, err := svc.Probe([]string{"10.0.0.0/20"}, "test"); err == nil {
		t.Fatal("Probe >1024 hosts should fail")
	}
	if got := trimList([]string{"10.0.0.0/24, 10.0.1.0/24，10.0.2.0/24"}); len(got) != 3 {
		t.Fatalf("trimList = %v", got)
	}

	// notify formats message when changes/suggestions exist
	c2 := cfgMgr.Get()
	c2.Discovery.Notify = true
	if err := cfgMgr.Update(c2); err != nil {
		t.Fatal(err)
	}
	var notifiedTitle string
	svc.Notify = func(title string, lines []string) { notifiedTitle = title }
	svc.notify(&ScanResult{
		JobName:     "j1",
		Changes:     []Change{{Type: ChIPChanged, OldIP: "1.1.1.1", IP: "1.1.1.2", MAC: m1}},
		Suggestions: []Suggestion{{CurrentIP: "1.1.1.3", SeenIP: "1.1.1.4", MAC: m2, Name: "dev"}},
	})
	if !strings.Contains(notifiedTitle, "j1") {
		t.Fatalf("notify title = %q", notifiedTitle)
	}
}

// 性质测试：任意在线主机列表经 Reconcile 处理后，拓扑永远保持 Validate() 通过（无重复 IP、无重复 ID）。
func TestReconcilePreservesTopologyValidity(t *testing.T) {
	if err := quick.Check(func(seed uint8, autoAdd bool, trackingIdx uint8) bool {
		trackings := []string{"all", "dhcp", "off"}
		tracking := trackings[int(trackingIdx)%len(trackings)]
		tp := topo(
			node("n1", "10.0.0.10", m1),
			node("n2", "10.0.0.11", m2),
			node("n3", "10.0.0.12", ""),
		)
		tp.Nodes[1].IPMode = "static"
		var hosts []Host
		cnt := int(seed%5) + 1
		for i := 0; i < cnt; i++ {
			ip := netip.AddrFrom4([4]byte{10, 0, 0, byte(10 + ((int(seed) + i) % 10))}).String()
			mac := ""
			switch i % 4 {
			case 0:
				mac = m1
			case 1:
				mac = m2
			case 2:
				mac = m3
			case 3:
				mac = m9
			}
			hosts = append(hosts, Host{IP: ip, MAC: mac, Ping: i%2 == 0, ARP: mac != ""})
		}
		Reconcile(tp, hosts, Options{Tracking: tracking, AutoAdd: autoAdd})
		return len(tp.Validate()) == 0
	}, &quick.Config{MaxCount: 150}); err != nil {
		t.Fatal(err)
	}
}
