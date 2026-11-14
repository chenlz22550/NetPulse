package topology

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSeedIsSynthetic(t *testing.T) {
	topo := Seed()
	if len(topo.Switches) != 2 || len(topo.Links) != 1 {
		t.Fatalf("switches=%d links=%d", len(topo.Switches), len(topo.Links))
	}
	if len(topo.Nodes) != 44 {
		t.Fatalf("nodes = %d, want 44", len(topo.Nodes))
	}
	for _, n := range topo.Nodes {
		if n.Vendor != "示例厂商" || !strings.HasPrefix(n.MAC, "02:00:00:") {
			t.Fatalf("seed must contain synthetic devices: %+v", n)
		}
	}
	gw := topo.FindNode("n-192-168-50-1")
	if gw == nil || !gw.HasFlag("gateway") || gw.Attach.Port != "Gi0/11" {
		t.Fatalf("gateway node wrong: %+v", gw)
	}
}

func TestMarshalRoundTripKeepsHeader(t *testing.T) {
	topo := Seed()
	data, err := Marshal(topo)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "# ====") || !strings.Contains(string(data), "attach {switch, port}") {
		t.Fatal("header comment missing")
	}
	again, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	data2, _ := Marshal(again)
	if !bytes.Equal(data, data2) {
		t.Fatal("round trip not stable")
	}
}

func TestValidate(t *testing.T) {
	yml := `
version: 1
site: {name: x}
switches:
  - {id: sw1, name: a, role: core}
nodes:
  - {id: a, ip: 10.0.0.1, attach: {switch: sw1, port: "1"}}
  - {id: a, ip: 10.0.0.300, attach: {switch: nope, port: "1"}}
  - {ip: 10.0.0.1, attach: {switch: "", port: "2"}}
`
	_, err := Parse([]byte(yml))
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("expected ValidationError, got %v", err)
	}
	joined := strings.Join(ve.Problems, "\n")
	for _, want := range []string{`id "a" 重复`, "不是有效的 IPv4", `"nope" 不存在`, "同时出现在节点", "没有 attach.switch"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing problem %q in:\n%s", want, joined)
		}
	}
}

func TestDeviceTypes(t *testing.T) {
	topo, err := Parse([]byte(`
site: {name: x}
switches:
  - {id: fw, name: 防火墙, type: " Firewall ", role: core}
  - {id: ont, name: 光猫, type: modem}
  - {id: sw, name: 交换机, type: switch}
  - {id: old, name: 旧交换机}
links:
  - {from: {switch: ont, port: LAN1}, to: {switch: fw, port: WAN1}}
  - {from: {switch: fw, port: LAN1}, to: {switch: sw, port: "1"}}
`))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range topo.Switches {
		got[s.ID] = s.Type + "|" + s.DeviceType() + "|" + s.TypeName()
	}
	want := map[string]string{"fw": "firewall|firewall|防火墙", "ont": "modem|modem|光猫", "sw": "|switch|交换机", "old": "|switch|交换机"}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: got %q want %q", id, got[id], w)
		}
	}
	out, err := Marshal(topo)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(out), "type: ") != 2 {
		t.Errorf("默认类型 switch 不应写入文件:\n%s", out)
	}
	if _, err := Parse([]byte("site: {name: x}\nswitches:\n  - {id: a, name: a, type: toaster}\n")); err == nil || !strings.Contains(err.Error(), "type 必须是") {
		t.Fatalf("expected type error, got %v", err)
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	_, err := Parse([]byte("version: 1\nsite: {name: x}\nnodes:\n  - {ip: 10.0.0.1, adress: x}\n"))
	if err == nil || !strings.Contains(err.Error(), "adress") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestNormalizeGeneratesIDs(t *testing.T) {
	topo, err := Parse([]byte(`
site: {name: x}
switches:
  - {name: s, ip: 10.0.0.2}
links: []
nodes:
  - {ip: 10.0.0.9, attach: {switch: "", port: ""}}
  - {mac: aa:bb:cc:dd:ee:ff, attach: {switch: "", port: ""}}
`))
	if err != nil {
		t.Fatal(err)
	}
	if topo.Switches[0].ID != "sw-10-0-0-2" || topo.Nodes[0].ID != "n-10-0-0-9" || topo.Nodes[1].ID != "n-aabbccddeeff" {
		t.Fatalf("ids: %s %s %s", topo.Switches[0].ID, topo.Nodes[0].ID, topo.Nodes[1].ID)
	}
	if topo.Nodes[1].MAC != "AA:BB:CC:DD:EE:FF" {
		t.Fatal("mac not upper-cased")
	}
}

func TestStoreUpdateAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "topology.yaml")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := 0
	s.OnChange(func(*Topology) { changed++ })
	if err := s.Update(func(t *Topology) error {
		t.Nodes = append(t.Nodes, &Node{IP: "192.168.50.250"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// 非法修改不能落盘
	if err := s.Update(func(t *Topology) error {
		t.Nodes = append(t.Nodes, &Node{IP: "192.168.50.250"})
		return nil
	}); err == nil {
		t.Fatal("duplicate IP should fail")
	}
	data, _ := os.ReadFile(path)
	if strings.Count(string(data), "ip: 192.168.50.250") != 1 {
		t.Fatalf("file content unexpected")
	}
	// 外部修改后重新加载
	edited := strings.Replace(string(data), "name: 演示机房", "name: 新机房", 1)
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	snap, _ := s.Snapshot()
	if snap.Site.Name != "新机房" || changed != 2 {
		t.Fatalf("reload failed: %s changed=%d", snap.Site.Name, changed)
	}
}

// 文档中的示例必须能通过校验。
func TestDocsExampleParses(t *testing.T) {
	data, err := os.ReadFile("../../docs/topology-fields.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(data)
	i := strings.Index(doc, "## 示例")
	start := strings.Index(doc[i:], "```yaml\n") + i + len("```yaml\n")
	end := strings.Index(doc[start:], "```") + start
	topo, err := Parse([]byte(doc[start:end]))
	if err != nil {
		t.Fatal(err)
	}
	if len(topo.Nodes) != 2 || topo.Nodes[1].Monitor.IntervalMs != 500 {
		t.Fatalf("unexpected: %+v", topo.Nodes)
	}
}
