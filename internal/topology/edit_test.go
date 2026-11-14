package topology

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

func TestEditOperationsAndInvariants(t *testing.T) {
	mk := func() *Topology {
		tp := &Topology{
			Version: 1,
			Site:    Site{Name: "s", Subnet: "10.0.0.0/24", Gateway: "10.0.0.1"},
			Switches: []*Switch{
				{ID: "sw1", Name: "核心", Role: "core", IP: "10.0.0.2"},
				{ID: "sw2", Name: "接入", Role: "access", IP: "10.0.0.3"},
				{ID: "sw3", Name: "汇聚", Role: "aggregation", IP: "10.0.0.4"},
			},
			Links: []*Link{
				{ID: "l1", From: Endpoint{Switch: "sw1", Port: "1"}, To: Endpoint{Switch: "sw2", Port: "26"}},
				{ID: "l2", From: Endpoint{Switch: "sw1", Port: "2"}, To: Endpoint{Switch: "sw3", Port: "24"}},
			},
			Nodes: []*Node{
				{ID: "n1", Name: "终端1", IP: "10.0.0.10", Attach: Endpoint{Switch: "sw2", Port: "1"}},
				{ID: "n2", IP: "10.0.0.11", MAC: "AA:BB:CC:DD:EE:FF", Attach: Endpoint{Switch: "sw2", Port: "2"}, Flags: []string{"unlocated"}},
				{ID: "n3", Attach: Endpoint{Switch: "sw3", Port: "3"}},
			},
		}
		tp.Normalize()
		return tp
	}

	t.Run("RenameSwitchRefs", func(t *testing.T) {
		tp := mk()
		before := tp.Clone()
		tp.RenameSwitchRefs("sw2", "sw2")
		if !reflect.DeepEqual(tp, before) {
			t.Fatal("same-id rename should be a no-op")
		}
		tp.RenameSwitchRefs("sw2", "sw2-new")
		tp.Switches[1].ID = "sw2-new"
		if probs := tp.Validate(); len(probs) > 0 {
			t.Fatalf("unexpected validation errors: %v", probs)
		}
		if tp.FindNode("n1").Attach.Switch != "sw2-new" || tp.FindLink("l1").To.Switch != "sw2-new" {
			t.Fatal("references not renamed")
		}
		tp.RenameSwitchRefs("sw1", "sw1-new")
		tp.Switches[0].ID = "sw1-new"
		if tp.FindLink("l1").From.Switch != "sw1-new" || tp.FindLink("l2").From.Switch != "sw1-new" {
			t.Fatal("from switch refs not renamed")
		}
	})

	t.Run("DeleteSwitchDetachesAndRemovesLinks", func(t *testing.T) {
		tp := mk()
		if n, ok := tp.DeleteSwitch("missing"); ok || n != 0 {
			t.Fatalf("missing switch delete = %d, %v", n, ok)
		}
		detached, ok := tp.DeleteSwitch("sw2")
		if !ok || detached != 2 {
			t.Fatalf("detached=%d ok=%v", detached, ok)
		}
		if probs := tp.Validate(); len(probs) > 0 {
			t.Fatalf("topology invalid after DeleteSwitch: %v", probs)
		}
		if tp.FindSwitch("sw2") != nil || tp.FindLink("l1") != nil || tp.FindLink("l2") == nil {
			t.Fatal("switch or link state incorrect")
		}
		for _, id := range []string{"n1", "n2"} {
			n := tp.FindNode(id)
			if n.Attach != (Endpoint{}) || !n.HasFlag("unlocated") {
				t.Fatalf("node %s not properly detached: %+v", id, n)
			}
			// unlocated flag must not be duplicated
			cnt := 0
			for _, f := range n.Flags {
				if f == "unlocated" {
					cnt++
				}
			}
			if cnt != 1 {
				t.Fatalf("node %s duplicate unlocated flag: %v", id, n.Flags)
			}
		}
	})

	t.Run("DeleteNodeAndDeleteLink", func(t *testing.T) {
		tp := mk()
		if !tp.DeleteNode("n1") || tp.DeleteNode("n1") {
			t.Fatal("DeleteNode mismatch")
		}
		if !tp.DeleteLink("l1") || tp.DeleteLink("l1") {
			t.Fatal("DeleteLink mismatch")
		}
		if probs := tp.Validate(); len(probs) > 0 {
			t.Fatalf("invalid after delete: %v", probs)
		}
	})

	t.Run("SetAlertAndFindByIP", func(t *testing.T) {
		tp := mk()
		changed, missing := tp.SetAlert([]string{"n1", "sw1", "missing"}, true)
		if changed != 2 || len(missing) != 1 || missing[0] != "missing" {
			t.Fatalf("SetAlert enable: changed=%d missing=%v", changed, missing)
		}
		// Idempotent re-application changes 0
		changed, missing = tp.SetAlert([]string{"n1", "sw1"}, true)
		if changed != 0 || len(missing) != 0 {
			t.Fatalf("SetAlert idempotent: changed=%d missing=%v", changed, missing)
		}
		changed, _ = tp.SetAlert([]string{"n1", "sw1"}, false)
		if changed != 2 {
			t.Fatalf("SetAlert disable: changed=%d", changed)
		}
		if k, id := tp.FindByIP("10.0.0.10"); k != "node" || id != "n1" {
			t.Fatalf("FindByIP node = %q, %q", k, id)
		}
		if k, id := tp.FindByIP("10.0.0.2"); k != "switch" || id != "sw1" {
			t.Fatalf("FindByIP switch = %q, %q", k, id)
		}
		if k, id := tp.FindByIP("10.0.0.99"); k != "" || id != "" {
			t.Fatalf("FindByIP missing = %q, %q", k, id)
		}
	})
}

func TestModelHelpersAndValidationEdgeCases(t *testing.T) {
	off := false
	on := true
	if !(MonitorSpec{}).IsEnabled() || !(MonitorSpec{Enabled: &on}).IsEnabled() || (MonitorSpec{Enabled: &off}).IsEnabled() {
		t.Fatal("MonitorSpec.IsEnabled mismatch")
	}
	if got := (&Node{ID: "id1"}).DisplayName(); got != "id1" {
		t.Fatalf("DisplayName id = %q", got)
	}
	if got := (&Node{ID: "id1", MAC: "AA:BB:CC:DD:EE:FF"}).DisplayName(); got != "AA:BB:CC:DD:EE:FF" {
		t.Fatalf("DisplayName mac = %q", got)
	}
	if got := (&Node{ID: "id1", IP: "1.2.3.4", MAC: "AA:BB"}).DisplayName(); got != "1.2.3.4" {
		t.Fatalf("DisplayName ip = %q", got)
	}
	if got := (&Node{ID: "id1", Name: "主机", IP: "1.2.3.4"}).DisplayName(); got != "主机" {
		t.Fatalf("DisplayName name = %q", got)
	}

	tp := &Topology{
		Switches: []*Switch{{ID: "sw1", Name: "核心", Role: "core"}, {ID: "sw2", Role: "access"}},
	}
	if got := tp.AttachLabel(Endpoint{}); got != "未定位" {
		t.Fatalf("AttachLabel empty = %q", got)
	}
	if got := tp.AttachLabel(Endpoint{Switch: "sw1"}); got != "核心" {
		t.Fatalf("AttachLabel no port = %q", got)
	}
	if got := tp.AttachLabel(Endpoint{Switch: "sw1", Port: "Gi0/1"}); got != "核心 Gi0/1" {
		t.Fatalf("AttachLabel full = %q", got)
	}
	if got := tp.AttachLabel(Endpoint{Switch: "sw2", Port: "1"}); got != "sw2 1" {
		t.Fatalf("AttachLabel unnamed switch = %q", got)
	}

	// uniqueID collision resolution
	tp.Nodes = []*Node{{ID: "n-10-0-0-1"}, {ID: "n-10-0-0-1-2"}}
	if got := tp.NewNodeID(&Node{IP: "10.0.0.1"}); got != "n-10-0-0-1-3" {
		t.Fatalf("NewNodeID collision = %q", got)
	}
	if got := tp.NewNodeID(&Node{}); got != "node" {
		t.Fatalf("NewNodeID empty = %q", got)
	}
	if got := tp.NewSwitchID(&Switch{}); got != "sw" {
		t.Fatalf("NewSwitchID empty = %q", got)
	}

	// Validation edge cases: bad ID, bad role, bad node_side, bad switch IP, bad ip_mode, interval < 200, self-link, missing link endpoints
	bad := &Topology{
		Switches: []*Switch{
			{ID: "-bad", Role: "badrole", NodeSide: "middle", IP: "not-ip"},
			{ID: "sw1", Role: "core", IP: "10.0.0.1"},
		},
		Links: []*Link{
			{ID: "sw1", From: Endpoint{Switch: "sw1"}, To: Endpoint{Switch: "sw1"}},
			{ID: "l2", From: Endpoint{Switch: "missing1"}, To: Endpoint{Switch: "missing2"}},
		},
		Nodes: []*Node{
			{ID: "n1", IP: "10.0.0.1", IPMode: "invalid", Monitor: MonitorSpec{IntervalMs: 100}},
		},
	}
	probs := bad.Validate()
	ve := &ValidationError{Problems: probs}
	msg := ve.Error()
	for _, want := range []string{
		"不合法", "role 必须是", "node_side 必须是", "不是有效的 IPv4",
		`id "sw1" 重复`, "两端不能是同一台交换机", `交换机 "missing1" 不存在`,
		"ip_mode 只能是", "interval_ms 不能小于 200", "同时出现在节点",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
	if !ValidID("A1_b.c-2") || ValidID("") || ValidID("_bad") || ValidID(strings.Repeat("a", 65)) {
		t.Fatal("ValidID boundary mismatch")
	}
}

func TestStoreEdgeCases(t *testing.T) {
	// Parse invalid YAML
	if _, err := Parse([]byte(":::")); err == nil {
		t.Fatal("expected yaml syntax error")
	}
	// Open non-existent directory path fails cleanly
	if _, err := Open("/dev/null/impossible/topology.yaml"); err == nil {
		t.Fatal("expected error opening invalid path")
	}
	s, err := Open(t.TempDir() + "/topology.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if s.Rev() != 1 || s.ChangedOnDisk() {
		t.Fatalf("initial rev=%d changed=%v", s.Rev(), s.ChangedOnDisk())
	}
	tp, _ := s.Snapshot()
	tp.Site.Name = "replaced"
	if err := s.Replace(tp); err != nil || s.Rev() != 2 {
		t.Fatalf("Replace err=%v rev=%d", err, s.Rev())
	}
	yml, err := s.YAML()
	if err != nil || !bytes.Contains(yml, []byte("name: replaced")) {
		t.Fatalf("YAML err=%v", err)
	}
}

// 性质测试：随机合法拓扑满足 Normalize 幂等、Marshal/Parse 往返不丢数据、DeleteSwitch 后拓扑仍始终有效。
func TestTopologyFormalProperties(t *testing.T) {
	cfg := &quick.Config{MaxCount: 100}
	if err := quick.Check(func(nSw, nNode uint8) bool {
		swCnt := int(nSw%5) + 1
		ndCnt := int(nNode % 12)
		tp := &Topology{Site: Site{Name: "prop-site", Subnet: "10.1.0.0/24"}}
		for i := 0; i < swCnt; i++ {
			tp.Switches = append(tp.Switches, &Switch{
				Name: fmt.Sprintf("sw-%d", i),
				IP:   fmt.Sprintf("10.1.0.%d", i+1),
				Role: Roles[i%len(Roles)],
			})
		}
		tp.Normalize()
		for i := 0; i+1 < len(tp.Switches); i++ {
			tp.Links = append(tp.Links, &Link{
				From: Endpoint{Switch: tp.Switches[i].ID, Port: "1"},
				To:   Endpoint{Switch: tp.Switches[i+1].ID, Port: "2"},
			})
		}
		for i := 0; i < ndCnt; i++ {
			sw := tp.Switches[i%len(tp.Switches)].ID
			tp.Nodes = append(tp.Nodes, &Node{
				Name:   fmt.Sprintf("node-%d", i),
				IP:     fmt.Sprintf("10.1.0.%d", 50+i),
				Attach: Endpoint{Switch: sw, Port: fmt.Sprintf("%d", i+1)},
			})
		}
		// 性质 1: Normalize 幂等
		tp.Normalize()
		snap1 := tp.Clone()
		tp.Normalize()
		if !reflect.DeepEqual(snap1, tp) {
			return false
		}
		// 性质 2: 校验通过且 Marshal/Parse 往返等价
		if probs := tp.Validate(); len(probs) > 0 {
			return false
		}
		raw, err := Marshal(tp)
		if err != nil {
			return false
		}
		parsed, err := Parse(raw)
		if err != nil || !reflect.DeepEqual(tp, parsed) {
			return false
		}
		// 性质 3: 任意删除交换机后拓扑始终保持合法，无悬空引用
		target := tp.Switches[0].ID
		tp.DeleteSwitch(target)
		if len(tp.Validate()) > 0 {
			return false
		}
		for _, n := range tp.Nodes {
			if n.Attach.Switch == target {
				return false
			}
		}
		for _, l := range tp.Links {
			if l.From.Switch == target || l.To.Switch == target {
				return false
			}
		}
		return true
	}, cfg); err != nil {
		t.Fatal(err)
	}
}
