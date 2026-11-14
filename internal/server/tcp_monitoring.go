package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	"netping/internal/monitor"
	"netping/internal/topology"
)

func tcpPortsSchema(max int) map[string]interface{} {
	return map[string]interface{}{"type": "array", "maxItems": max, "uniqueItems": true, "items": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 65535}, "description": "TCP 端口列表"}
}

func tcpMonitoringTools() []*mcpTool {
	return []*mcpTool{
		{Name: "scan_ports", Title: "扫描 TCP 端口", OpenWorld: true,
			Description: "扫描指定设备 id/ip 的 TCP 端口（最多 1024 个，并发最多 32；省略 ports 扫描常用端口）。返回开放、拒绝、超时和连接耗时。仅探测，不修改监控；使用 set_tcp_monitoring 添加持续监控。",
			Schema:      obj(withIDIP(map[string]interface{}{"ports": tcpPortsSchema(1024), "timeout_ms": integer("连接超时，默认 1500ms，范围 200–5000")})),
			Run: func(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
				return scanTCP(context.Background(), s, a, 1024)
			}},
		{Name: "set_tcp_monitoring", Title: "设置 TCPing 监控", Write: true,
			Description: "替换指定已有设备的持续 TCPing 端口列表（最多 16 个）；空数组清除端口监控。保留 ICMP 及其余配置，沿用设备监控间隔、超时、阈值、启停、告警和全局监控时段。每个端口独立记录连接耗时及失败率，连接被拒绝也算失败。",
			Schema:      obj(withIDIP(map[string]interface{}{"ports": tcpPortsSchema(16)}), "ports"), Run: toolSetTCPMonitoring},
		{Name: "get_tcp_monitoring", Title: "查询 TCPing 监控", Description: "查询设备已配置的 TCP 端口与独立实时状态、延迟和失败率；历史使用 get_history 的 port 参数。", Schema: obj(withIDIP(nil)), Run: toolGetTCPMonitoring},
	}
}

func deviceMonitor(t *topology.Topology, a map[string]json.RawMessage) (string, *topology.MonitorSpec, error) {
	kind, id, err := resolveDevice(t, argStr(a, "id"), argStr(a, "ip"))
	if err != nil {
		return "", nil, err
	}
	if kind == "node" {
		return id, &t.FindNode(id).Monitor, nil
	}
	return id, &t.FindSwitch(id).Monitor, nil
}

func toolSetTCPMonitoring(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	var ports []int
	raw, ok := a["ports"]
	if !ok || string(raw) == "null" || json.Unmarshal(raw, &ports) != nil {
		return nil, badRequest("ports 必须是整数数组；清除监控请传 []")
	}
	if err := topology.ValidateTCPPorts(ports); err != nil {
		return nil, badRequest(err.Error())
	}
	var id string
	err := s.Topo.Update(func(t *topology.Topology) error {
		rid, spec, err := deviceMonitor(t, a)
		if err != nil {
			return err
		}
		id = rid
		spec.TCPPorts = append([]int(nil), ports...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	log.Printf("TCPing(%s) %s ports=%v", remote, id, ports)
	return toolGetTCPMonitoring(s, a, remote)
}

func toolGetTCPMonitoring(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	t, _ := s.Topo.Snapshot()
	id, spec, err := deviceMonitor(t, a)
	if err != nil {
		return nil, err
	}
	states := make([]monitor.Status, 0, len(spec.TCPPorts))
	for _, port := range spec.TCPPorts {
		if st, ok := s.Mon.Status(monitor.TCPID(id, port)); ok {
			states = append(states, st)
		}
	}
	ports := append([]int{}, spec.TCPPorts...)
	return map[string]interface{}{"id": id, "ports": ports, "enabled": spec.IsEnabled(), "statuses": states}, nil
}

func (s *Server) handlePortScan(w http.ResponseWriter, r *http.Request) error {
	var args map[string]json.RawMessage
	if err := decode(r, &args); err != nil {
		return err
	}
	// The UI scan is always scoped to an existing device.
	t, _ := s.Topo.Snapshot()
	if _, _, err := deviceMonitor(t, args); err != nil {
		return err
	}
	result, err := scanTCP(r.Context(), s, args, 1024)
	if err != nil {
		return badRequest(err.Error())
	}
	writeJSON(w, result)
	return nil
}
func (s *Server) handleTCPMonitoring(w http.ResponseWriter, r *http.Request) error {
	args := map[string]json.RawMessage{}
	var result interface{}
	var err error
	if r.Method == http.MethodGet {
		args["id"], _ = json.Marshal(r.URL.Query().Get("id"))
		result, err = toolGetTCPMonitoring(s, args, r.RemoteAddr)
	} else {
		if err = decode(r, &args); err != nil {
			return err
		}
		result, err = toolSetTCPMonitoring(s, args, r.RemoteAddr)
	}
	if err != nil {
		return err
	}
	writeJSON(w, result)
	return nil
}
