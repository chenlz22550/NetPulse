package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"

	"netping/internal/topology"
)

func TestTCPMonitoringMCPAndAPI(t *testing.T) {
	s, ts := newTestServer(t)
	apiRequest(t, ts, "POST", "/api/nodes", `{"id":"tcp-test","ip":"127.0.0.1","name":"保留名称","monitor":{"interval_ms":500,"alert":true}}`, 200)
	for _, ports := range []interface{}{[]int{0}, []int{65536}, []int{22, 22}, nil, "22", []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17}} {
		if _, bad := call(t, ts, "set_tcp_monitoring", map[string]interface{}{"id": "tcp-test", "ports": ports}); !bad {
			t.Fatalf("accepted invalid ports: %v", ports)
		}
	}
	if txt, bad := call(t, ts, "set_tcp_monitoring", map[string]interface{}{"id": "tcp-test", "ports": []int{22, 443}}); bad {
		t.Fatal(txt)
	}
	topo, _ := s.Topo.Snapshot()
	n := topo.FindNode("tcp-test")
	if n.Name != "保留名称" || n.Monitor.IntervalMs != 500 || !n.Monitor.Alert || !reflect.DeepEqual(n.Monitor.TCPPorts, []int{22, 443}) {
		t.Fatalf("config changed: %+v", n)
	}
	reloaded, err := topology.Open(s.Topo.Path())
	if err != nil {
		t.Fatal(err)
	}
	disk, _ := reloaded.Snapshot()
	if !reflect.DeepEqual(disk.FindNode("tcp-test").Monitor.TCPPorts, []int{22, 443}) {
		t.Fatal("ports not persisted")
	}
	if txt, bad := call(t, ts, "get_tcp_monitoring", map[string]interface{}{"id": "tcp-test"}); bad || !strings.Contains(txt, "443") {
		t.Fatal(txt)
	}
	if txt, bad := call(t, ts, "get_history", map[string]interface{}{"id": "tcp-test", "port": 443, "range": "1h"}); bad {
		t.Fatal(txt)
	}
	apiRequest(t, ts, "GET", "/api/history/tcp-test?port=443&range=24h", "", 200)
	apiRequest(t, ts, "GET", "/api/history/tcp-test?port=0", "", 400)
	cfg := s.Cfg.Get()
	cfg.MCP.ReadOnly = true
	if err := s.Cfg.Update(cfg); err != nil {
		t.Fatal(err)
	}
	_, blocked := rpc(t, ts, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"set_tcp_monitoring","arguments":{"id":"tcp-test","ports":[80]}}}`)
	if !strings.Contains(fmtJSON(blocked), "只读") {
		t.Fatalf("read-only MCP allowed writes: %v", blocked)
	}
	apiRequest(t, ts, "PUT", "/api/ports/monitoring", `{"id":"tcp-test","ports":[]}`, 200)
	topo, _ = s.Topo.Snapshot()
	if len(topo.FindNode("tcp-test").Monitor.TCPPorts) != 0 {
		t.Fatal("ports not removed")
	}
}

func TestScanPorts(t *testing.T) {
	s, ts := newTestServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	apiRequest(t, ts, "POST", "/api/nodes", `{"id":"scan-test","ip":"127.0.0.1"}`, 200)
	args := map[string]interface{}{"id": "scan-test", "ports": []int{port}}
	txt, bad := call(t, ts, "scan_ports", args)
	if bad || !strings.Contains(txt, `"open"`) {
		t.Fatal(txt)
	}
	apiRequest(t, ts, "POST", "/api/ports/scan", fmt.Sprintf(`{"id":"scan-test","ports":[%d]}`, port), 200)
	apiRequest(t, ts, "POST", "/api/ports/scan", `{"id":"scan-test","ports":[-1]}`, 400)
	topo, _ := s.Topo.Snapshot()
	if len(topo.FindNode("scan-test").Monitor.TCPPorts) != 0 {
		t.Fatal("scan silently changed monitoring")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	raw := map[string]json.RawMessage{"id": json.RawMessage(`"scan-test"`), "ports": json.RawMessage(fmt.Sprintf(`[%d]`, port))}
	if _, err := scanTCP(ctx, s, raw, 1024); err == nil {
		t.Fatal("cancelled scan succeeded")
	}
}
