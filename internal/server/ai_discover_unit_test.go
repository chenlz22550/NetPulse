package server

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"netping/internal/discovery"
)

func TestDiscoverDeviceValidation(t *testing.T) {
	s, _ := newTestServer(t)
	tp, _ := s.Topo.Snapshot()
	s.rememberHost(seenHost{Host: discovery.Host{IP: "192.168.79.10", MAC: "AA:BB:CC:79:00:10", Hostname: "observed", Ping: true}})
	tests := []struct {
		name                      string
		d                         discDevice
		kind, typ, category, port string
		rejected                  bool
	}{
		{name: "规范设备类型", d: discDevice{Kind: "switch", Type: " FIREWALL ", Category: "打印机", Switch: "access", Port: "5"}, kind: "switch", typ: "firewall"},
		{name: "默认交换机", d: discDevice{Kind: "switch", Type: "switch"}, kind: "switch"},
		{name: "未知网络类型回退", d: discDevice{Kind: "switch", Type: "unknown"}, kind: "switch"},
		{name: "终端清除网络类型", d: discDevice{Kind: "node", Type: "router", Category: "光猫", Switch: "access", Port: "5"}, kind: "node", category: "光猫", port: "5"},
		{name: "非法分类和接线", d: discDevice{Kind: "anything", Category: "invalid", Switch: "missing", Port: "5"}, kind: "node"},
		{name: "不存在的端口", d: discDevice{Switch: "access", Port: "99999"}, kind: "node"},
		{name: "部分接线", d: discDevice{Switch: "access"}, kind: "node"},
		{name: "未观察地址", d: discDevice{IP: "192.168.79.99"}, rejected: true},
		{name: "非法地址", d: discDevice{IP: "bad"}, rejected: true},
		{name: "IPv6", d: discDevice{IP: "::1"}, rejected: true},
		{name: "已登记", d: discDevice{IP: "192.168.101.232"}, rejected: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := tt.d
			if d.IP == "" {
				d.IP = " 192.168.79.10 "
			}
			d.MAC = "forged"
			d.Name = strings.Repeat("中", 45)
			d.Evidence = strings.Repeat("证", 350)
			d.Confidence = "invalid"
			why := s.checkDiscDevice(tp, &d)
			if tt.rejected {
				if why == "" {
					t.Fatal("应拒绝")
				}
				return
			}
			if why != "" {
				t.Fatal(why)
			}
			if d.Kind != tt.kind || d.Type != tt.typ || d.Category != tt.category || d.Port != tt.port {
				t.Fatalf("校验结果: %+v", d)
			}
			if d.MAC != "AA:BB:CC:79:00:10" || d.Hostname != "observed" || utf8.RuneCountInString(d.Name) != 40 || d.Evidence != strings.Repeat("证", 300)+"…（已截断）" || d.Confidence != "medium" {
				t.Fatalf("证据或长度限制不符: %+v", d)
			}
			if (d.Attach != "") != (tt.port != "") {
				t.Fatalf("接线说明: %s", d.Attach)
			}
		})
	}
}

func TestDiscoverSharedMACAndExpiredEvidence(t *testing.T) {
	s, _ := newTestServer(t)
	tp, _ := s.Topo.Snapshot()
	s.rememberHost(seenHost{Host: discovery.Host{IP: "192.168.79.20", MAC: "AA:BB:CC:79:00:20", SharedMAC: []string{"192.168.79.21"}, ARP: true}})
	d := discDevice{IP: "192.168.79.20"}
	if why := s.checkDiscDevice(tp, &d); why != "" {
		t.Fatal(why)
	}
	if d.MAC != "" || d.Ping {
		t.Fatalf("共享 MAC 或 ARP-only 处理不符: %+v", d)
	}
	s.seenMu.Lock()
	h := s.seen[d.IP]
	h.At = time.Now().Add(-seenTTL - time.Second)
	s.seen[d.IP] = h
	s.seenMu.Unlock()
	if why := s.checkDiscDevice(tp, &d); why == "" {
		t.Fatal("过期探测证据仍可添加")
	}
}

func TestDiscoverNotesAndSubnetInput(t *testing.T) {
	for _, tt := range []struct {
		raw  string
		want []string
	}{
		{`["一","二"]`, []string{"一", "二"}},
		{`[{"text":"一"},{"note":"二"},{"reason":"三"},{"unknown":"忽略"}]`, []string{"一", "二", "三"}},
		{`invalid`, nil},
		{`["一",42]`, nil}, {`null`, nil},
	} {
		if got := notesOf(json.RawMessage(tt.raw)); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: %v", tt.raw, got)
		}
	}
	got := trimSubnets([]string{" 192.168.1.0/24，192.168.2.0/24\n10.0.0.0/24、10.0.1.0/24", "", "172.16.0.0/24,172.17.0.0/24"})
	if len(got) != 6 {
		t.Fatalf("网段拆分: %v", got)
	}
}

func TestDiscoverJobValidation(t *testing.T) {
	s, _ := newTestServer(t)
	for _, tt := range []struct {
		name     string
		subnets  []string
		interval int
		want     []string
		rejected bool
	}{
		{"规范化并去重", []string{"192.168.80.123/24", "192.168.80.0/24", "8.8.8.0/24"}, 1, []string{"192.168.80.0/24"}, false},
		{"已覆盖", []string{"192.168.101.0/25"}, 30, nil, true},
		{"公网", []string{"8.8.8.0/24"}, 30, nil, true},
		{"超过网段大小", []string{"10.0.0.0/16"}, 30, nil, true},
		{"有效间隔", []string{"192.168.81.0/24"}, 5, []string{"192.168.81.0/24"}, false},
		{"过大间隔回退", []string{"192.168.82.0/24"}, 1441, []string{"192.168.82.0/24"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			j := discJob{Subnets: tt.subnets, IntervalMin: tt.interval}
			why := s.checkDiscJob(s.Cfg.Get(), &j)
			if tt.rejected {
				if why == "" {
					t.Fatal("应拒绝")
				}
				return
			}
			if why != "" {
				t.Fatal(why)
			}
			if !reflect.DeepEqual(j.Subnets, tt.want) || j.Name != strings.Join(tt.want, "、") {
				t.Fatalf("网段规范化不符: %+v", j)
			}
			wantInterval := tt.interval
			if wantInterval < 5 || wantInterval > 1440 {
				wantInterval = 30
			}
			if j.IntervalMin != wantInterval {
				t.Fatalf("扫描间隔: %d", j.IntervalMin)
			}
		})
	}
}
