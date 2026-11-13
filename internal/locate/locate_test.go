package locate

import (
	"fmt"
	"testing"

	"netping/internal/snmp"
	"netping/internal/topology"
)

func TestMatchPort(t *testing.T) {
	core := &topology.Switch{ID: "core", Ports: []string{"Gi0/1", "Gi0/2", "Te0/29"}}
	acc := &topology.Switch{ID: "access", Ports: []string{"1", "2", "26"}}
	bare := &topology.Switch{ID: "bare"}
	cases := []struct {
		sw     *topology.Switch
		ifName string
		used   []string
		want   string
	}{
		{core, "GigabitEthernet 0/1", nil, "Gi0/1"},
		{core, "TenGigabitEthernet 0/29", nil, "Te0/29"},
		{core, "GigabitEthernet 0/7", nil, "Gi0/7"}, // 未声明：短写
		{acc, "GigabitEthernet 0/26", nil, "26"},
		{acc, "Gi0/2", nil, "2"},
		{acc, "AggregatePort 1", nil, "Ag1"}, // 聚合口不能按编号对应到 1 口
		{bare, "GigabitEthernet1/0/5", []string{"3", "4"}, "5"},
		{bare, "XGigabitEthernet0/0/1", nil, "Xge0/0/1"},
		{bare, "Ten-GigabitEthernet1/0/49", nil, "Te1/0/49"},
	}
	for _, c := range cases {
		if got := MatchPort(c.sw, c.ifName, c.used); got != c.want {
			t.Errorf("MatchPort(%s, %q) = %q, want %q", c.sw.ID, c.ifName, got, c.want)
		}
	}
}

func mac(i int) string { return fmt.Sprintf("60:D7:E3:00:00:%02X", i) }

// 与默认拓扑一致：核心 Te0/29 ⇄ 接入 26。
func scenario() (*topology.Topology, map[string]*snmp.Table) {
	t := &topology.Topology{Version: 1, Site: topology.Site{Name: "t"},
		Switches: []*topology.Switch{
			{ID: "core", Name: "核心", Role: "core", Ports: []string{"Gi0/1", "Gi0/2", "Gi0/3", "Te0/29"}},
			{ID: "access", Name: "接入", Role: "access", MAC: "02:00:00:00:00:17", Ports: []string{"1", "7", "12", "26"},
				PortLocations: map[string]string{"12": "A03 机柜"}},
		},
		Links: []*topology.Link{{ID: "up", From: topology.Endpoint{Switch: "core", Port: "Te0/29"}, To: topology.Endpoint{Switch: "access", Port: "26"}}},
		Nodes: []*topology.Node{
			{ID: "pc-unlocated", IP: "10.0.0.20", MAC: mac(1), Flags: []string{"unlocated", "discovered"}, Position: &topology.Position{X: 1, Y: 2}},
			{ID: "srv-moved", IP: "10.0.0.21", MAC: mac(2), Attach: topology.Endpoint{Switch: "core", Port: "Gi0/3"}},
			{ID: "srv-same", IP: "10.0.0.22", MAC: mac(3), Attach: topology.Endpoint{Switch: "core", Port: "Gi0/1"}},
			{ID: "pc-hub", IP: "10.0.0.23", MAC: mac(4)},
			{ID: "pc-nomac", IP: "10.0.0.24"},
			{ID: "pc-far", IP: "10.0.0.25", MAC: mac(9)},
		},
	}
	t.Normalize()
	gi := func(p string) string { return "GigabitEthernet 0/" + p }
	core := &snmp.Table{Entries: []snmp.FDBEntry{
		{MAC: mac(3), IfName: gi("1"), VLAN: 1},
		{MAC: mac(1), IfName: "TenGigabitEthernet 0/29", VLAN: 1}, // 接入下的设备在核心上联口
		{MAC: mac(2), IfName: "TenGigabitEthernet 0/29", VLAN: 1},
		{MAC: mac(4), IfName: "TenGigabitEthernet 0/29", VLAN: 1},
		{MAC: mac(9), IfName: "TenGigabitEthernet 0/29", VLAN: 1},
		{MAC: "02:00:00:00:00:17", IfName: "TenGigabitEthernet 0/29", VLAN: 1},
		{MAC: mac(3), IfName: "VLAN 1", VLAN: 1}, // 非物理口忽略
	}}
	acc := &snmp.Table{Entries: []snmp.FDBEntry{
		{MAC: mac(1), IfName: gi("12"), VLAN: 1},
		{MAC: mac(1), IfName: gi("12"), VLAN: 20}, // 同口多 VLAN 去重
		{MAC: mac(2), IfName: gi("7"), VLAN: 1},
		{MAC: mac(4), IfName: gi("1"), VLAN: 1}, // 傻瓜交换机：1 口多个 MAC
		{MAC: mac(5), IfName: gi("1"), VLAN: 1}, // 未登记
		{MAC: mac(3), IfName: gi("26"), VLAN: 1},
		{MAC: mac(9), IfName: gi("26"), VLAN: 1}, // 两边都在上联口：设备在未读取的交换机下
	}}
	return t, map[string]*snmp.Table{"core": core, "access": acc}
}

func TestLocate(t *testing.T) {
	tp, tables := scenario()
	a := Analyze(tp, tables)
	res := Apply(tp, a, Options{})
	if p := tp.Validate(); len(p) > 0 {
		t.Fatalf("invalid: %v", p)
	}
	n := tp.FindNode("pc-unlocated")
	if n.Attach.Switch != "access" || n.Attach.Port != "12" || n.Location != "A03 机柜" || n.Position != nil {
		t.Fatalf("unlocated not placed: %+v", n)
	}
	for _, f := range n.Flags {
		if f == "unlocated" {
			t.Fatal("unlocated flag should be removed")
		}
	}
	if h := tp.FindNode("pc-hub"); h.Attach.Switch != "access" || h.Attach.Port != "1" {
		t.Fatalf("hub port: %+v", h.Attach)
	}
	// 已定位但端口变化：只建议，不修改
	if m := tp.FindNode("srv-moved"); m.Attach.Port != "Gi0/3" {
		t.Fatalf("moved node must not change without ApplyMoves: %+v", m.Attach)
	}
	if len(res.Suggestions) != 1 || res.Suggestions[0].NodeID != "srv-moved" || res.Suggestions[0].Port != "7" {
		t.Fatalf("suggestions: %+v", res.Suggestions)
	}
	if len(res.Changes) != 2 {
		t.Fatalf("changes: %+v", res.Changes)
	}
	if len(res.Unresolved) != 1 || res.Unresolved[0].NodeID != "pc-far" {
		t.Fatalf("unresolved: %+v", res.Unresolved)
	}
	if len(res.Strangers) != 1 || res.Strangers[0].MAC != mac(5) || res.Strangers[0].Port != "1" {
		t.Fatalf("strangers: %+v", res.Strangers)
	}
	if a.Uplinks["core"]["Te0/29"] == "" || a.Uplinks["access"]["26"] == "" {
		t.Fatalf("uplinks: %+v", a.Uplinks)
	}

	// 采纳端口变化
	tp2, tables2 := scenario()
	Apply(tp2, Analyze(tp2, tables2), Options{ApplyMoves: true})
	if m := tp2.FindNode("srv-moved"); m.Attach.Switch != "access" || m.Attach.Port != "7" {
		t.Fatalf("ApplyMoves: %+v", m.Attach)
	}
	// 再次运行：结果稳定
	if r := Apply(tp2, Analyze(tp2, tables2), Options{}); len(r.Changes) != 0 || len(r.Suggestions) != 0 {
		t.Fatalf("second pass not stable: %+v", r)
	}
}

func TestLocateWithoutLinkUsesSwitchMAC(t *testing.T) {
	// 没有登记链路时：学到其他交换机 MAC 的端口视为级联口
	tp, tables := scenario()
	tp.Links = nil
	tables["core"].Entries = append(tables["core"].Entries, snmp.FDBEntry{MAC: mac(7), IfName: "GigabitEthernet 0/2"})
	tp.Nodes = append(tp.Nodes, &topology.Node{ID: "x", IP: "10.0.0.30", MAC: mac(7)})
	res := Apply(tp, Analyze(tp, tables), Options{})
	if n := tp.FindNode("pc-unlocated"); n.Attach.Switch != "access" {
		t.Fatalf("should use edge port on access: %+v (%+v)", n.Attach, res)
	}
	if n := tp.FindNode("x"); n.Attach.Switch != "core" || n.Attach.Port != "Gi0/2" {
		t.Fatalf("x: %+v", n.Attach)
	}
}
