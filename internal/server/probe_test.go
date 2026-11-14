package server

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"netping/internal/discovery"
	"netping/internal/snmp"
)

// fakePortAgent 模拟一台 26 口接入交换机的 IF-MIB：1 口半双工且 10 秒前刚变化，5 口没有链路。
func fakePortAgent(t *testing.T) (*snmp.Agent, string) {
	a := &snmp.Agent{Community: "s3cret-ro"}
	a.SetString("1.3.6.1.2.1.1.1.0", "Ruijie S2910 (simulated)")
	a.SetString("1.3.6.1.2.1.1.5.0", "access")
	a.Set(snmp.Var{OID: snmp.ParseOID("1.3.6.1.2.1.1.3.0"), Type: snmp.TypeTimeTicks, Int: 360000}) // 运行 1 小时
	for i := 1; i <= 26; i++ {
		a.SetString(fmt.Sprintf("1.3.6.1.2.1.31.1.1.1.1.%d", i), fmt.Sprintf("GigabitEthernet 0/%d", i))
		a.SetInt(fmt.Sprintf("1.3.6.1.2.1.2.2.1.7.%d", i), 1)
		oper := int64(1)
		if i == 5 {
			oper = 2
		}
		a.SetInt(fmt.Sprintf("1.3.6.1.2.1.2.2.1.8.%d", i), oper)
		a.Set(snmp.Var{OID: snmp.ParseOID(fmt.Sprintf("1.3.6.1.2.1.31.1.1.1.15.%d", i)), Type: snmp.TypeGauge32, Int: 1000})
		last := int64(100)
		if i == 1 {
			last = 359000
		}
		a.Set(snmp.Var{OID: snmp.ParseOID(fmt.Sprintf("1.3.6.1.2.1.2.2.1.9.%d", i)), Type: snmp.TypeTimeTicks, Int: last})
		a.Set(snmp.Var{OID: snmp.ParseOID(fmt.Sprintf("1.3.6.1.2.1.2.2.1.14.%d", i)), Type: snmp.TypeCounter32, Int: 0})
		a.Set(snmp.Var{OID: snmp.ParseOID(fmt.Sprintf("1.3.6.1.2.1.31.1.1.1.6.%d", i)), Type: snmp.TypeCounter64, Int: 1000})
		duplex := int64(3)
		if i == 1 {
			duplex = 2
		}
		a.SetInt(fmt.Sprintf("1.3.6.1.2.1.10.7.2.1.19.%d", i), duplex)
	}
	addr, err := a.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a, addr
}

func TestSwitchPorts(t *testing.T) {
	s, ts := newTestServer(t)
	a, addr := fakePortAgent(t)
	// 未设置团体名：明确提示
	if _, res := s.runAITool("get_switch_ports", `{"switch":"access","sample_sec":0}`, "test"); !strings.Contains(res, "团体名") {
		t.Fatalf("want no-snmp error: %s", res)
	}
	call(t, ts, "set_locate_settings", map[string]interface{}{"community": "s3cret-ro", "switches": map[string]interface{}{"access": map[string]interface{}{"host": addr}}})

	// 按设备查：自动定位到它接的 1 口
	out := aiCall(t, s, "get_switch_ports", `{"ip":"192.168.50.29","sample_sec":0}`)
	ports := out["ports"].([]interface{})
	if len(ports) != 1 {
		t.Fatalf("ports: %v", ports)
	}
	p1 := ports[0].(map[string]interface{})
	flags := fmtJSON(p1["flags"])
	if p1["port"] != "1" || p1["duplex"] != "half" || !strings.Contains(flags, "半双工") || !strings.Contains(flags, "状态变化") || p1["devices"] == nil {
		t.Fatalf("port 1: %v", p1)
	}
	if !strings.Contains(fmtJSON(out["switch_flags"]), "启动过") || out["device"] == nil {
		t.Fatalf("switch flags / device: %v", out)
	}
	if strings.Contains(fmtJSON(out), "s3cret") {
		t.Fatal("community leaked")
	}

	// 采样：2 口错误计数在采样期间增长
	go func() {
		time.Sleep(300 * time.Millisecond)
		a.Set(snmp.Var{OID: snmp.ParseOID("1.3.6.1.2.1.2.2.1.14.2"), Type: snmp.TypeCounter32, Int: 50})
		a.Set(snmp.Var{OID: snmp.ParseOID("1.3.6.1.2.1.31.1.1.1.6.2"), Type: snmp.TypeCounter64, Int: 1000 + 125000000})
	}()
	out = aiCall(t, s, "get_switch_ports", `{"switch":"access","port":"2","sample_sec":1}`)
	p2 := out["ports"].([]interface{})[0].(map[string]interface{})
	smp := p2["sample"].(map[string]interface{})
	if smp["new_errors"].(float64) != 50 || !strings.Contains(fmtJSON(p2["flags"]), "错误包增加 50") || smp["util_pct"].(float64) < 80 {
		t.Fatalf("port 2 sample: %v", p2)
	}

	// 整台交换机：默认只列出有异常或接了设备的端口，异常的排在前面
	out = aiCall(t, s, "get_switch_ports", `{"switch":"access","sample_sec":0}`)
	sum := out["summary"].(map[string]interface{})
	if sum["ports_read"].(float64) != 26 || sum["down"].(float64) != 1 {
		t.Fatalf("summary: %v", sum)
	}
	if first := out["ports"].([]interface{})[0].(map[string]interface{}); first["port"] != "1" {
		t.Fatalf("most severe first: %v", first)
	}
	all := aiCall(t, s, "get_switch_ports", `{"switch":"access","sample_sec":0,"all":true}`)
	if n := len(all["ports"].([]interface{})); n != 26 {
		t.Fatalf("all ports: %d", n)
	}
	if _, res := s.runAITool("get_switch_ports", `{"switch":"access","port":"99","sample_sec":0}`, "test"); !strings.Contains(res, "没有找到端口") {
		t.Fatalf("missing port: %s", res)
	}
}

func TestTCPCheck(t *testing.T) {
	s, _ := newTestServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	tmp, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := tmp.Addr().(*net.TCPAddr).Port
	tmp.Close()
	open := ln.Addr().(*net.TCPAddr).Port

	out := aiCall(t, s, "tcp_check", fmt.Sprintf(`{"ip":"127.0.0.1","ports":[%d,%d]}`, open, closed))
	ps := out["ports"].([]interface{})
	if ps[0].(map[string]interface{})["state"] != "open" || ps[1].(map[string]interface{})["state"] != "refused" {
		t.Fatalf("ports: %v", ps)
	}
	if v := out["summary"].(map[string]interface{})["verdict"].(string); !strings.Contains(v, "主机在线") {
		t.Fatalf("verdict: %s", v)
	}
	for _, bad := range []string{`{"ip":"127.0.0.1","ports":[0]}`, `{"ip":"255.255.255.255"}`, `{"ip":"224.0.0.1"}`, `{}`,
		`{"ip":"127.0.0.1","ports":[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17]}`} {
		if _, res := s.runAITool("tcp_check", bad, "test"); !strings.Contains(res, "error") {
			t.Fatalf("%s should fail: %s", bad, res)
		}
	}
}

func TestARPLookup(t *testing.T) {
	s, _ := newTestServer(t)
	old := readARP
	defer func() { readARP = old }()
	readARP = func() ([]discovery.ARPEntry, error) {
		return []discovery.ARPEntry{
			{IP: "192.168.50.29", MAC: "02:00:00:00:00:12"},
			{IP: "192.168.50.10", MAC: "AA:BB:CC:DD:EE:01"},
		}, nil
	}
	out := aiCall(t, s, "arp_lookup", `{"ip":"192.168.50.29","refresh":false}`)
	if out["in_arp_table"] != true || out["mac_matches_registered"] != true {
		t.Fatalf("match: %v", out)
	}
	// 登记的 MAC 与 ARP 不同：提示 IP 冲突或设备更换
	out = aiCall(t, s, "arp_lookup", `{"ip":"192.168.50.10","refresh":false}`)
	if out["mac_matches_registered"] != false || !strings.Contains(out["verdict"].(string), "IP 冲突") {
		t.Fatalf("mismatch: %v", out)
	}
	out = aiCall(t, s, "arp_lookup", `{"ip":"192.168.50.250","refresh":false}`)
	if out["in_arp_table"] != false {
		t.Fatalf("absent: %v", out)
	}
}

func TestDNSLookup(t *testing.T) {
	s, _ := newTestServer(t)
	out := aiCall(t, s, "dns_lookup", `{"name":"localhost"}`)
	if out["ok"] != true || out["server"] != "系统默认" {
		t.Fatalf("localhost: %v", out)
	}
	for _, bad := range []string{`{"name":""}`, `{"name":"bad name!"}`, `{"name":"example.com","server":"dns.example"}`} {
		if _, res := s.runAITool("dns_lookup", bad, "test"); !strings.Contains(res, "error") {
			t.Fatalf("%s should fail: %s", bad, res)
		}
	}
}

func TestTraceRouteLoopback(t *testing.T) {
	s, _ := newTestServer(t)
	out := aiCall(t, s, "trace_route", `{"ip":"127.0.0.1","max_hops":3,"timeout_ms":500}`)
	if out["reached"] != true {
		t.Skipf("系统 ping 不可用或不支持 TTL：%v", out)
	}
	hops := out["hops"].([]interface{})
	if len(hops) != 1 || hops[0].(map[string]interface{})["ip"] != "127.0.0.1" {
		t.Fatalf("hops: %v", hops)
	}
}

func TestProbeToolsRegistered(t *testing.T) {
	for _, tl := range probeTools() {
		if tl.Write || findTool(tl.Name) == nil || !aiAllowed(tl.Name) {
			t.Fatalf("%s 未正确注册为 AI 可用的只读工具", tl.Name)
		}
	}
}
