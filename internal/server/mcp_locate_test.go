package server

import (
	"fmt"
	"strings"
	"testing"

	"netping/internal/snmp"
)

func fakeAccess(t *testing.T, macs map[string]int) string {
	a := &snmp.Agent{Community: "s3cret-ro"}
	a.SetString("1.3.6.1.2.1.1.1.0", "Ruijie S2910 (simulated)")
	a.SetString("1.3.6.1.2.1.1.5.0", "access")
	for i := 1; i <= 26; i++ {
		a.SetString(fmt.Sprintf("1.3.6.1.2.1.31.1.1.1.1.%d", i), fmt.Sprintf("GigabitEthernet 0/%d", i))
	}
	for m, port := range macs {
		var b [6]int
		fmt.Sscanf(m, "%x:%x:%x:%x:%x:%x", &b[0], &b[1], &b[2], &b[3], &b[4], &b[5])
		a.SetInt(fmt.Sprintf("1.3.6.1.2.1.17.7.1.2.2.1.2.1.%d.%d.%d.%d.%d.%d", b[0], b[1], b[2], b[3], b[4], b[5]), int64(port))
	}
	addr, err := a.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return addr
}

func TestMCPLocate(t *testing.T) {
	s, ts := newTestServer(t)
	addr := fakeAccess(t, map[string]int{
		"AA:BB:CC:00:00:01": 12, // 新电脑（未定位）
		"60:D7:E3:C0:45:43": 7,  // 默认拓扑中接在接入交换机 1 口的设备，换到了 7 口
		"00:0E:C6:DB:04:53": 1,  // 位置不变
		"DE:AD:BE:EF:00:01": 3,  // 未登记
	})
	// 未设置团体名：明确提示
	if txt, isErr := call(t, ts, "auto_locate", map[string]interface{}{}); !isErr || !strings.Contains(txt, "团体名") {
		t.Fatalf("want no-target error: %s", txt)
	}
	txt, isErr := call(t, ts, "set_locate_settings", map[string]interface{}{"community": "s3cret-ro", "switches": map[string]interface{}{"access": map[string]interface{}{"host": addr}}})
	if isErr || strings.Contains(txt, "s3cret") {
		t.Fatalf("settings (community must be masked): %s", txt)
	}
	if txt, isErr := call(t, ts, "test_switch_snmp", map[string]interface{}{"switch": "access"}); isErr || !strings.Contains(txt, `"mac_entries": 4`) {
		t.Fatalf("test: %s", txt)
	}
	// 新电脑：先登记为未定位
	call(t, ts, "upsert_device", map[string]interface{}{"ip": "192.168.101.77", "mac": "AA:BB:CC:00:00:01", "name": "新电脑", "flags": []string{"unlocated"}})
	call(t, ts, "upsert_switch", map[string]interface{}{"id": "access", "port_locations": map[string]string{"12": "A03 机柜"}})

	if txt, _ := call(t, ts, "locate_device", map[string]interface{}{"ip": "192.168.101.77"}); !strings.Contains(txt, `"port": "12"`) || !strings.Contains(txt, "A03") {
		t.Fatalf("locate_device: %s", txt)
	}
	// 预览不写入
	call(t, ts, "auto_locate", map[string]interface{}{"dry_run": true})
	tp, _ := s.Topo.Snapshot()
	if _, id := tp.FindByIP("192.168.101.77"); tp.FindNode(id).Attach.Switch != "" {
		t.Fatal("dry_run must not write")
	}
	txt, isErr = call(t, ts, "auto_locate", map[string]interface{}{})
	if isErr || !strings.Contains(txt, `"located"`) || !strings.Contains(txt, `"suggestions"`) {
		t.Fatalf("auto_locate: %s", txt)
	}
	tp, _ = s.Topo.Snapshot()
	_, id := tp.FindByIP("192.168.101.77")
	if n := tp.FindNode(id); n.Attach.Switch != "access" || n.Attach.Port != "12" || n.Location != "A03 机柜" {
		t.Fatalf("new pc: %+v", n)
	}
	_, moved := tp.FindByIP("192.168.101.30")
	if n := tp.FindNode(moved); n.Attach.Port != "1" {
		t.Fatalf("located device must not move without confirmation: %+v", n.Attach)
	}
	if txt, _ := call(t, ts, "get_locate_status", map[string]interface{}{}); !strings.Contains(txt, "DE:AD:BE:EF:00:01") || strings.Contains(txt, "s3cret") {
		t.Fatalf("status: %s", txt)
	}
	if txt, isErr := call(t, ts, "apply_location", map[string]interface{}{"ip": "192.168.101.30"}); isErr || !strings.Contains(txt, `"moved"`) {
		t.Fatalf("apply_location: %s", txt)
	}
	tp, _ = s.Topo.Snapshot()
	if n := tp.FindNode(moved); n.Attach.Port != "7" {
		t.Fatalf("apply_location: %+v", n.Attach)
	}
	if txt, _ := call(t, ts, "get_switch_mac_table", map[string]interface{}{"switch": "access", "port": "12"}); !strings.Contains(txt, "新电脑") {
		t.Fatalf("mac table: %s", txt)
	}
	if txt, _ := call(t, ts, "get_settings", map[string]interface{}{}); strings.Contains(txt, "s3cret") {
		t.Fatal("get_settings leaks community")
	}
	if txt, _ := call(t, ts, "get_topology", map[string]interface{}{"format": "yaml"}); strings.Contains(txt, "s3cret") {
		t.Fatal("topology leaks community")
	}
}
