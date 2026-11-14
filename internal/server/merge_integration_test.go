package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"netping/internal/discovery"
	"netping/internal/topology"
)

// 跨 MCP、HTTP、YAML 持久化及 AI 上下文验证最近新增的所有设备类型。
func TestIntegrationDeviceTypesAcrossInterfaces(t *testing.T) {
	s, ts := newTestServer(t)
	var meta struct {
		Types []string          `json:"device_types"`
		Names map[string]string `json:"type_names"`
	}
	if err := json.Unmarshal(apiRequest(t, ts, "GET", "/api/meta", "", 200), &meta); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(meta.Types, topology.DeviceTypes) || !reflect.DeepEqual(meta.Names, topology.DeviceTypeNames) {
		t.Fatalf("设备类型元数据不一致: %+v", meta)
	}
	for i, typ := range topology.DeviceTypes {
		t.Run(typ, func(t *testing.T) {
			id := "merge-" + typ
			text, bad := call(t, ts, "upsert_switch", map[string]interface{}{"id": id, "name": topology.DeviceTypeNames[typ], "type": " " + strings.ToUpper(typ) + " ", "ip": fmt.Sprintf("192.168.77.%d", i+1), "ports": []string{"WAN1", "LAN1", "PON1"}})
			if bad {
				t.Fatal(text)
			}
			// 局部修改不能丢失 type、端口等已有字段。
			if text, bad = call(t, ts, "upsert_switch", map[string]interface{}{"id": id, "location": "测试机柜"}); bad {
				t.Fatal(text)
			}
			tp, rev := s.Topo.Snapshot()
			sw := tp.FindSwitch(id)
			if sw == nil || sw.DeviceType() != typ || len(sw.Ports) != 3 || sw.Location != "测试机柜" {
				t.Fatalf("类型或已有字段丢失: %+v", sw)
			}
			apiRequest(t, ts, "PUT", "/api/switches/"+id, fmtJSON(map[string]interface{}{"name": sw.Name, "type": typ, "role": "access", "ports": sw.Ports}), 200)
			before, rev := s.Topo.Snapshot()
			apiRequest(t, ts, "PUT", "/api/switches/"+id, `{"name":"非法类型","type":"invalid","role":"access"}`, 400)
			after, afterRev := s.Topo.Snapshot()
			if rev != afterRev || !reflect.DeepEqual(before, after) {
				t.Fatal("非法类型改变了拓扑")
			}
			text, bad = call(t, ts, "upsert_switch", map[string]interface{}{"id": id, "type": "invalid"})
			if !bad {
				t.Fatalf("MCP 接受非法类型: %s", text)
			}
		})
	}
	data := apiRequest(t, ts, "GET", "/api/topology/yaml", "", 200)
	parsed, err := topology.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := topology.Open(s.Topo.Path())
	if err != nil {
		t.Fatal(err)
	}
	disk, _ := reopened.Snapshot()
	if !reflect.DeepEqual(parsed, disk) {
		t.Fatal("YAML 下载与落盘数据不一致")
	}
	context, _ := s.optContext()
	for _, typ := range topology.DeviceTypes {
		if disk.FindSwitch("merge-"+typ).DeviceType() != typ || !strings.Contains(context, topology.DeviceTypeNames[typ]) {
			t.Fatalf("%s 未贯穿磁盘和 AI 优化上下文", typ)
		}
	}
}

// 由真实 MCP 探测入口记录证据，再经 HTTP 应用、重复应用和返回的撤销凭据走完闭环。
func TestIntegrationDiscoverTypedDevicesApplyUndo(t *testing.T) {
	s, ts := newTestServer(t)
	before, _ := s.Topo.Snapshot()
	var hosts []discovery.Host
	var devices []discDevice
	for i, typ := range topology.DeviceTypes {
		ip := fmt.Sprintf("192.168.78.%d", i+1)
		hosts = append(hosts, discovery.Host{IP: ip, MAC: fmt.Sprintf("AA:BB:CC:78:00:%02X", i+1), Ping: true})
		devices = append(devices, discDevice{IP: ip, Kind: "switch", Type: typ, Name: topology.DeviceTypeNames[typ], Alert: true})
	}
	stubProbe(t, hosts)
	if text, bad := call(t, ts, "probe_subnet", map[string]interface{}{"subnets": []string{"192.168.78.0/24"}}); bad {
		t.Fatal(text)
	}
	afterProbe, _ := s.Topo.Snapshot()
	if !reflect.DeepEqual(before, afterProbe) {
		t.Fatal("只读探测改变了拓扑")
	}
	// 走模型结果校验层，随后模拟用户确认。
	result := s.buildDiscResult(&discRaw{Devices: devices})
	body := fmtJSON(map[string]interface{}{"devices": result["devices"]})
	var applied struct {
		Switches int
		Undo     discUndo
	}
	if err := json.Unmarshal(apiRequest(t, ts, "POST", "/api/ai/discover/apply", body, 200), &applied); err != nil {
		t.Fatal(err)
	}
	if applied.Switches != len(devices) || len(applied.Undo.Switches) != len(devices) {
		t.Fatalf("新增数量不符: %+v", applied)
	}
	tp, _ := s.Topo.Snapshot()
	for i, d := range devices {
		_, id := tp.FindByIP(d.IP)
		sw := tp.FindSwitch(id)
		if sw == nil || sw.DeviceType() != d.Type || sw.MAC != hosts[i].MAC || !sw.Monitor.Alert {
			t.Fatalf("发现类型/证据丢失: %+v", sw)
		}
	}
	var duplicate struct {
		Switches int
		Skipped  []interface{}
	}
	if err := json.Unmarshal(apiRequest(t, ts, "POST", "/api/ai/discover/apply", body, 200), &duplicate); err != nil {
		t.Fatal(err)
	}
	if duplicate.Switches != 0 || len(duplicate.Skipped) != len(devices) {
		t.Fatalf("重复应用不幂等: %+v", duplicate)
	}
	apiRequest(t, ts, "POST", "/api/ai/discover/apply", fmtJSON(map[string]interface{}{"undo": applied.Undo}), http.StatusOK)
	reopened, err := topology.Open(s.Topo.Path())
	if err != nil {
		t.Fatal(err)
	}
	restored, _ := reopened.Snapshot()
	if !reflect.DeepEqual(before, restored) {
		t.Fatal("撤销后磁盘拓扑未恢复")
	}
}
