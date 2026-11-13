package discovery

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"netping/internal/topology"
)

func TestParseNBSTAT(t *testing.T) {
	b := binary.BigEndian.AppendUint16(nil, 0x1234)
	b = append(b, 0x84, 0, 0, 0, 0, 1, 0, 0, 0, 0) // response, AN=1
	b = append(b, 0x20)
	for i := 0; i < 32; i++ {
		b = append(b, 'A')
	}
	b = append(b, 0, 0, 0x21, 0, 1, 0, 0, 0, 0, 0, 0) // type class ttl rdlength(2，未校验)
	b = append(b, 3)                                  // 3 个名字
	entry := func(name string, suffix byte, flags uint16) {
		nm := []byte(name + strings.Repeat(" ", 15-len(name)))
		b = append(b, nm...)
		b = append(b, suffix)
		b = binary.BigEndian.AppendUint16(b, flags)
	}
	entry("WORKGROUP", 0x00, 0x8400) // 组名
	entry("DESKTOP-ABC", 0x20, 0x0400)
	entry("DESKTOP-ABC", 0x00, 0x0400)
	if n, err := parseNBSTAT(b, 0x1234); err != nil || n != "DESKTOP-ABC" {
		t.Fatalf("nbstat: %q %v", n, err)
	}
	if _, err := parseNBSTAT(b, 0x9999); err == nil {
		t.Fatal("wrong id accepted")
	}
}

func TestParsePTRWithCompression(t *testing.T) {
	b := binary.BigEndian.AppendUint16(nil, 1)
	b = append(b, 0x84, 0, 0, 1, 0, 1, 0, 0, 0, 0) // QD=1 AN=1
	qname := 12
	for _, l := range []string{"5", "3", "168", "192", "in-addr", "arpa"} {
		b = append(b, byte(len(l)))
		b = append(b, l...)
	}
	b = append(b, 0, 0, 12, 0, 1)
	b = append(b, 0xc0, byte(qname)) // 回答名指向问题名
	b = append(b, 0, 12, 0x80, 1, 0, 0, 0, 120)
	rdata := []byte{8}
	rdata = append(rdata, "iPhone-X"...)
	rdata = append(rdata, 5)
	rdata = append(rdata, "local"...)
	rdata = append(rdata, 0)
	b = binary.BigEndian.AppendUint16(b, uint16(len(rdata)))
	b = append(b, rdata...)
	if n, err := parsePTR(b); err != nil || n != "iPhone-X.local" {
		t.Fatalf("ptr: %q %v", n, err)
	}
	if _, err := parsePTR(b[:20]); err == nil {
		t.Fatal("truncated accepted")
	}
}

func TestCleanHostname(t *testing.T) {
	cases := map[string]string{
		"MacBook-Pro.local.":          "MacBook-Pro",
		"desktop-abc.lan":             "desktop-abc",
		"printer.home.arpa":           "printer",
		"192-168-3-5.dynamic.isp.net": "",
		"192.168.3.5":                 "",
		"localhost":                   "",
		"nas.example.com":             "nas.example.com",
		"  ":                          "",
	}
	for in, want := range cases {
		if got := CleanHostname(in, "192.168.3.5"); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func TestHostnameRecorded(t *testing.T) {
	named := node("pc", "10.0.0.10", m1)
	named.Name = "财务电脑"
	tp := topo(named, node("nas", "10.0.0.11", m2))
	h1, h2 := host("10.0.0.10", m1), host("10.0.0.11", m2)
	h1.Hostname, h2.Hostname = "DESKTOP-ABC", "NAS01"
	h3 := host("10.0.0.12", m3)
	h3.Hostname = "iPhone"
	r := Reconcile(tp, []Host{h1, h2, h3}, Options{Tracking: "all", AutoAdd: true, Now: time.Now()})
	mustValid(t, tp)
	got := map[string]*topology.Node{}
	for _, n := range tp.Nodes {
		got[n.IP] = n
	}
	if got["10.0.0.10"].Hostname != "DESKTOP-ABC" || got["10.0.0.10"].Name != "财务电脑" {
		t.Fatalf("named: %+v", got["10.0.0.10"])
	}
	if got["10.0.0.11"].Hostname != "NAS01" || got["10.0.0.11"].Name != "" {
		t.Fatalf("nas: %+v", got["10.0.0.11"])
	}
	if n := got["10.0.0.12"]; n == nil || n.Hostname != "iPhone" || n.Name != "" {
		t.Fatalf("new node: %+v", n)
	}
	if c := strings.Count(types(r), ChHostname+":"); c != 2 {
		t.Fatalf("hostname changes = %d (%s)", c, types(r))
	}
	// 再扫一次：大小写不同不算变化；查不到主机名时保留原值
	h1.Hostname, h2.Hostname = "desktop-abc", ""
	r = Reconcile(tp, []Host{h1, h2}, Options{Tracking: "all", AutoAdd: true, Now: time.Now()})
	if strings.Contains(types(r), ChHostname) || got["10.0.0.11"].Hostname != "NAS01" {
		t.Fatalf("second scan: %s %+v", types(r), got["10.0.0.11"])
	}
	// 改名
	h2.Hostname = "NAS02"
	r = Reconcile(tp, []Host{h2}, Options{Tracking: "all", AutoAdd: true, Now: time.Now()})
	if !strings.Contains(types(r), ChHostname+":nas") || got["10.0.0.11"].Hostname != "NAS02" {
		t.Fatalf("rename: %s", types(r))
	}
}
