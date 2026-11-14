package server

// MCP（Model Context Protocol）接口：Streamable HTTP 传输，单一端点 POST /mcp，
// 返回 application/json（不使用 SSE 流，也不建立会话），兼容 2024-11-05 ~ 2025-06-18 协议版本。
// AI 客户端可以通过它查询监控状态和历史、管理拓扑节点、开关告警、设置监控时段。

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"netping/internal/alert"
	"netping/internal/config"
	"netping/internal/discovery"
	"netping/internal/locate"
	"netping/internal/monitor"
	"netping/internal/topology"
)

const mcpLatestProtocol = "2025-06-18"

var mcpProtocols = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

const mcpInstructions = `netPing 是机房网络 Ping 监控工具。通过本接口可以：
1. 查询：get_overview（总体状态与异常设备）、list_devices（设备列表与实时延迟/丢包）、get_device、get_history（历史曲线统计）、list_alerts、get_topology、get_settings。
   分析（只读）：analyze_impact（异常设备按交换机/端口归类，找公共故障点）、get_uplink_path（设备到核心的逐跳路径）、find_outages（历史中断/丢包时段与可用率）、correlate_events（多设备同时异常）、rank_devices（历史稳定性排行）、compare_baseline（与昨天/上周同时段对比）、ping_now（立即 ping 测试）。
   现场核实（只读，从 netPing 主机实时探测）：get_switch_ports（SNMP 读交换机端口状态、速率、双工、错误计数增长、交换机运行时长）、tcp_check（TCP 端口探测，区分离线与禁 ping）、scan_ports（扫描最多 1024 个 TCP 端口）、get_tcp_monitoring（持续 TCPing 状态）、get_history 的 port 参数（端口历史）、arp_lookup（ARP 核对，发现 IP 冲突）、trace_route（逐跳路由追踪）、dns_lookup（DNS 解析）。
2. 端口定位：get_locate_status、get_switch_mac_table（交换机每个口学到的 MAC）、locate_device（设备实际插在哪个口）、auto_locate（未定位设备自动挂到端口）、apply_location、test_switch_snmp、set_locate_settings。
网段扫描（自动扫描模式）：get_discovery 查看扫描任务与结果；upsert_scan_job 新建/修改扫描任务（一个或多个网段，按间隔自动扫描、自动添加新设备）、delete_scan_job、scan_network 立即扫描（可传 subnets 临时扫描）、set_discovery_settings、add_discovered_devices、apply_ip_suggestion。
3. 管理（非只读模式）：upsert_device / delete_device（终端节点）、upsert_switch / delete_switch、upsert_link / delete_link、reset_layout、set_alerting（开关设备告警）、set_alert_config / send_test_alert（通知渠道）、set_tcp_monitoring（配置设备的持续 TCPing 端口）、set_ping_defaults、set_site、set_schedule（全局监控时段）、set_monitoring_paused、replace_topology_yaml（整体替换，先 dry_run 校验）、set_ai_settings（AI 诊断开关、协议格式、深度思考、回答前自检、模型名、超时）。
不能通过 MCP 修改：监听地址、网页登录账号、MCP 自身设置、AI 的 API Key / 接口地址 / 代理（需在网页或 config.yaml 中修改）。
约定：设备用 id 或 ip 指定；修改会立即写回 topology.yaml 并生效；设备默认只监控不告警；时间均为服务器本地时间。
字段说明见资源 netping://docs/topology-fields.md。批量迁移机房时优先使用 replace_topology_yaml 并先 dry_run=true。删除或整体替换前请先向用户确认。`

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type mcpTool struct {
	Name        string
	Title       string
	Description string
	Schema      map[string]interface{}
	Write       bool
	Destructive bool
	OpenWorld   bool // 会从 netPing 主机向外发包（探测任意地址、外网），对应 MCP openWorldHint
	Run         func(s *Server, args map[string]json.RawMessage, remote string) (interface{}, error)
}

// ---------------------------------------------------------------------------
// HTTP 入口

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	cfg := s.Cfg.Get().MCP
	if !cfg.Enabled {
		http.Error(w, "MCP 接口未开启（网页「MCP」页或 config.yaml 中 mcp.enabled）", http.StatusNotFound)
		return
	}
	if !originAllowed(r) {
		http.Error(w, "Origin 不被允许", http.StatusForbidden)
		return
	}
	if cfg.Token != "" {
		got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if subtle.ConstantTimeCompare([]byte(got), []byte(cfg.Token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="netPing MCP"`)
			http.Error(w, "需要有效的 MCP 访问令牌（Authorization: Bearer <token>）", http.StatusUnauthorized)
			return
		}
	}
	if r.Method != http.MethodPost {
		// 本服务不提供服务端主动推送的 SSE 流，也不维护会话。
		w.Header().Set("Allow", "POST")
		http.Error(w, "只支持 POST", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	body = bytes.TrimSpace(body)
	remote := clientAddr(r)

	var msgs []rpcMsg
	batch := len(body) > 0 && body[0] == '['
	if batch {
		err = json.Unmarshal(body, &msgs)
	} else {
		var m rpcMsg
		err = json.Unmarshal(body, &m)
		msgs = []rpcMsg{m}
	}
	if err != nil {
		writeRPC(w, rpcResp{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "JSON 解析失败: " + err.Error()}})
		return
	}

	var out []rpcResp
	for _, m := range msgs {
		if m.Method == "" { // 客户端发来的响应，忽略
			continue
		}
		isNotification := len(m.ID) == 0 || string(m.ID) == "null"
		result, rerr := s.mcpDispatch(m, remote)
		if isNotification {
			continue
		}
		resp := rpcResp{JSONRPC: "2.0", ID: m.ID}
		if rerr != nil {
			resp.Error = rerr
		} else {
			resp.Result = result
		}
		out = append(out, resp)
	}
	if len(out) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if batch {
		writeRPC(w, out)
	} else {
		writeRPC(w, out[0])
	}
}

func writeRPC(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// originAllowed 防止 DNS 重绑定：浏览器请求的 Origin 必须是本服务自身或本机。
func originAllowed(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" || o == "null" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	if u.Host == r.Host {
		return true
	}
	h := u.Hostname()
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------------------------------------------------------------------------
// 方法分发

func (s *Server) mcpDispatch(m rpcMsg, remote string) (interface{}, *rpcError) {
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
			ClientInfo      struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"clientInfo"`
		}
		_ = json.Unmarshal(m.Params, &p)
		ver := mcpLatestProtocol
		for _, v := range mcpProtocols {
			if v == p.ProtocolVersion {
				ver = v
			}
		}
		log.Printf("MCP 客户端连接：%s %s（%s，协议 %s）", p.ClientInfo.Name, p.ClientInfo.Version, remote, ver)
		return map[string]interface{}{
			"protocolVersion": ver,
			"capabilities": map[string]interface{}{
				"tools":     map[string]interface{}{"listChanged": false},
				"resources": map[string]interface{}{"listChanged": false, "subscribe": false},
				"prompts":   map[string]interface{}{"listChanged": false},
			},
			"serverInfo":   map[string]interface{}{"name": "netPing", "title": "netPing 机房网络监控", "version": Version},
			"instructions": mcpInstructions,
		}, nil
	case "ping":
		return map[string]interface{}{}, nil
	case "notifications/initialized", "notifications/cancelled", "notifications/progress", "notifications/roots/list_changed":
		return nil, nil
	case "tools/list":
		var list []map[string]interface{}
		for _, t := range s.visibleTools() {
			list = append(list, map[string]interface{}{
				"name":        t.Name,
				"title":       t.Title,
				"description": t.Description,
				"inputSchema": t.Schema,
				"annotations": map[string]interface{}{
					"title":           t.Title,
					"readOnlyHint":    !t.Write,
					"destructiveHint": t.Destructive,
					"openWorldHint":   t.OpenWorld,
				},
			})
		}
		return map[string]interface{}{"tools": list}, nil
	case "tools/call":
		var p struct {
			Name      string                     `json:"name"`
			Arguments map[string]json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: "参数无效: " + err.Error()}
		}
		var tool *mcpTool
		for _, t := range s.visibleTools() {
			if t.Name == p.Name {
				tool = t
			}
		}
		if tool == nil {
			if s.lookupTool(p.Name) != nil {
				return toolError("MCP 当前为只读模式，不能调用 " + p.Name + "（可在网页「MCP」页关闭只读）"), nil
			}
			return nil, &rpcError{Code: -32602, Message: "未知工具: " + p.Name}
		}
		if p.Arguments == nil {
			p.Arguments = map[string]json.RawMessage{}
		}
		res, err := tool.Run(s, p.Arguments, remote)
		if err != nil {
			return toolError(errText(err)), nil
		}
		text, _ := json.MarshalIndent(res, "", "  ")
		return map[string]interface{}{
			"content": []map[string]interface{}{{"type": "text", "text": string(text)}},
			"isError": false,
		}, nil
	case "resources/list":
		var list []map[string]interface{}
		for _, r := range s.mcpResources() {
			list = append(list, map[string]interface{}{"uri": r.uri, "name": r.name, "title": r.title, "description": r.desc, "mimeType": r.mime})
		}
		return map[string]interface{}{"resources": list}, nil
	case "resources/templates/list":
		return map[string]interface{}{"resourceTemplates": []interface{}{}}, nil
	case "resources/read":
		var p struct {
			URI string `json:"uri"`
		}
		_ = json.Unmarshal(m.Params, &p)
		for _, r := range s.mcpResources() {
			if r.uri == p.URI {
				text, err := r.read()
				if err != nil {
					return nil, &rpcError{Code: -32603, Message: err.Error()}
				}
				return map[string]interface{}{"contents": []map[string]interface{}{{"uri": r.uri, "mimeType": r.mime, "text": text}}}, nil
			}
		}
		return nil, &rpcError{Code: -32002, Message: "资源不存在: " + p.URI}
	case "prompts/list":
		var list []map[string]interface{}
		for _, p := range mcpPrompts {
			list = append(list, map[string]interface{}{"name": p.name, "title": p.title, "description": p.desc, "arguments": p.args})
		}
		return map[string]interface{}{"prompts": list}, nil
	case "prompts/get":
		var p struct {
			Name      string            `json:"name"`
			Arguments map[string]string `json:"arguments"`
		}
		_ = json.Unmarshal(m.Params, &p)
		for _, pr := range mcpPrompts {
			if pr.name == p.Name {
				for _, a := range pr.args {
					if a["required"] == true && strings.TrimSpace(p.Arguments[a["name"].(string)]) == "" {
						return nil, &rpcError{Code: -32602, Message: "缺少参数: " + a["name"].(string)}
					}
				}
				return map[string]interface{}{
					"description": pr.desc,
					"messages": []map[string]interface{}{{
						"role":    "user",
						"content": map[string]interface{}{"type": "text", "text": pr.build(s, p.Arguments)},
					}},
				}, nil
			}
		}
		return nil, &rpcError{Code: -32602, Message: "未知提示词: " + p.Name}
	case "logging/setLevel":
		return map[string]interface{}{}, nil
	}
	if strings.HasPrefix(m.Method, "notifications/") {
		return nil, nil
	}
	return nil, &rpcError{Code: -32601, Message: "不支持的方法: " + m.Method}
}

func toolError(msg string) map[string]interface{} {
	return map[string]interface{}{
		"content": []map[string]interface{}{{"type": "text", "text": msg}},
		"isError": true,
	}
}

func errText(err error) string {
	var ve *topology.ValidationError
	if errors.As(err, &ve) {
		return "拓扑校验失败，修改未生效：\n- " + strings.Join(ve.Problems, "\n- ")
	}
	return err.Error()
}

func (s *Server) visibleTools() []*mcpTool {
	ro := s.Cfg.Get().MCP.ReadOnly
	var out []*mcpTool
	for _, t := range mcpTools {
		if ro && t.Write {
			continue
		}
		out = append(out, t)
	}
	for _, t := range s.pluginTools() {
		if ro && t.Write {
			continue
		}
		out = append(out, t)
	}
	return out
}

func findTool(name string) *mcpTool {
	for _, t := range mcpTools {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 参数辅助

func argStr(a map[string]json.RawMessage, k string) string {
	var v string
	if raw, ok := a[k]; ok {
		_ = json.Unmarshal(raw, &v)
	}
	return strings.TrimSpace(v)
}

func argInt(a map[string]json.RawMessage, k string, def int) int {
	var v float64
	if raw, ok := a[k]; ok && json.Unmarshal(raw, &v) == nil {
		return int(v)
	}
	return def
}

func argBool(a map[string]json.RawMessage, k string) (bool, bool) {
	var v bool
	raw, ok := a[k]
	if !ok {
		return false, false
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return false, false
	}
	return v, true
}

func argStrings(a map[string]json.RawMessage, k string) []string {
	var v []string
	if raw, ok := a[k]; ok {
		_ = json.Unmarshal(raw, &v)
	}
	return v
}

// resolveDevice 用 id 或 ip 找到设备（节点或交换机）。
func resolveDevice(t *topology.Topology, id, ip string) (kind, rid string, err error) {
	if id != "" {
		if t.FindNode(id) != nil {
			return "node", id, nil
		}
		if t.FindSwitch(id) != nil {
			return "switch", id, nil
		}
		return "", "", fmt.Errorf("找不到 id 为 %q 的设备", id)
	}
	if ip != "" {
		if k, i := t.FindByIP(ip); k != "" {
			return k, i, nil
		}
		return "", "", fmt.Errorf("找不到 IP 为 %s 的设备", ip)
	}
	return "", "", errors.New("需要提供 id 或 ip")
}

// mergePatch 把 patch 中提供的字段覆盖到 base 上（attach、monitor 做一层合并），结果解码到 out，未知字段报错。
func mergePatch(base interface{}, patch map[string]json.RawMessage, out interface{}) error {
	b, _ := json.Marshal(base)
	m := map[string]json.RawMessage{}
	_ = json.Unmarshal(b, &m)
	for k, v := range patch {
		if (k == "attach" || k == "monitor" || k == "from" || k == "to") && string(v) != "null" {
			cur := map[string]json.RawMessage{}
			upd := map[string]json.RawMessage{}
			if json.Unmarshal(v, &upd) == nil {
				_ = json.Unmarshal(m[k], &cur)
				if cur == nil {
					cur = map[string]json.RawMessage{}
				}
				for kk, vv := range upd {
					cur[kk] = vv
				}
				nb, _ := json.Marshal(cur)
				m[k] = nb
				continue
			}
		}
		m[k] = v
	}
	merged, _ := json.Marshal(m)
	dec := json.NewDecoder(bytes.NewReader(merged))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("参数无效: %v", err)
	}
	return nil
}

func fmtLocal(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).Format("2006-01-02 15:04:05")
}

func r2(v float64) float64 { return math.Round(v*100) / 100 }

// ---------------------------------------------------------------------------
// 设备视图

type deviceRow struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	IP         string   `json:"ip,omitempty"`
	Name       string   `json:"name,omitempty"`
	Hostname   string   `json:"hostname,omitempty"`
	Vendor     string   `json:"vendor,omitempty"`
	MAC        string   `json:"mac,omitempty"`
	Attach     string   `json:"attach"`
	Location   string   `json:"location,omitempty"`
	Flags      []string `json:"flags,omitempty"`
	Alert      bool     `json:"alert"`
	State      string   `json:"state"`
	LastRTT    *float64 `json:"last_rtt_ms,omitempty"`
	Avg1m      *float64 `json:"avg_1m_ms,omitempty"`
	Jitter1m   *float64 `json:"jitter_1m_ms,omitempty"`
	Loss1m     *float64 `json:"loss_1m_pct,omitempty"`
	Loss5m     *float64 `json:"loss_5m_pct,omitempty"`
	Loss1h     *float64 `json:"loss_1h_pct,omitempty"`
	StateSince string   `json:"state_since,omitempty"`
}

func pf(v float64) *float64 { return &v }

func buildRows(t *topology.Topology, sts map[string]monitor.Status) []deviceRow {
	var rows []deviceRow
	fill := func(r *deviceRow, monitored bool) {
		st, ok := sts[r.ID]
		switch {
		case !monitored:
			r.State = "unmonitored"
		case !ok:
			r.State = "unknown"
		default:
			r.State = st.State
			if st.LastRTT >= 0 {
				r.LastRTT = pf(st.LastRTT)
			}
			if st.Avg1m > 0 {
				r.Avg1m = pf(st.Avg1m)
				r.Jitter1m = pf(st.Jitter1m)
			}
			r.Loss1m, r.Loss5m, r.Loss1h = pf(st.Loss1m), pf(st.Loss5m), pf(st.Loss1h)
			r.StateSince = fmtLocal(st.StateSince)
		}
	}
	for _, s := range t.Switches {
		r := deviceRow{ID: s.ID, Kind: "switch", IP: s.IP, Name: s.Name, Vendor: s.Vendor, MAC: s.MAC, Attach: s.TypeName() + "（" + s.Role + "）", Location: s.Location, Alert: s.Monitor.Alert}
		fill(&r, s.IP != "" && s.Monitor.IsEnabled())
		rows = append(rows, r)
	}
	for _, n := range t.Nodes {
		r := deviceRow{ID: n.ID, Kind: "node", IP: n.IP, Name: n.Name, Hostname: n.Hostname, Vendor: n.Vendor, MAC: n.MAC, Attach: t.AttachLabel(n.Attach), Location: n.Location, Flags: n.Flags, Alert: n.Monitor.Alert}
		fill(&r, n.IP != "" && n.Monitor.IsEnabled())
		rows = append(rows, r)
	}
	return rows
}

func ipKey(ip string) uint32 {
	p := net.ParseIP(ip).To4()
	if p == nil {
		return math.MaxUint32
	}
	return uint32(p[0])<<24 | uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3])
}

func (s *Server) statusMap() map[string]monitor.Status {
	m := map[string]monitor.Status{}
	for _, st := range s.Mon.Statuses(false) {
		m[st.ID] = st
	}
	return m
}

func isAbnormal(state string) bool { return state == "down" || state == "loss" || state == "slow" }

func maskWebhook(u string) string {
	if u == "" {
		return ""
	}
	if i := strings.Index(u, "key="); i >= 0 && len(u) > i+12 {
		return u[:i+8] + "****" + u[len(u)-4:]
	}
	return "（已配置）"
}

// ---------------------------------------------------------------------------
// 工具定义

func obj(props map[string]interface{}, required ...string) map[string]interface{} {
	o := map[string]interface{}{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}

func str(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "description": desc}
}
func boolean(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "boolean", "description": desc}
}
func integer(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "integer", "description": desc}
}
func enum(desc string, vals ...string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "enum": vals, "description": desc}
}
func strArr(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": desc}
}

var (
	endpointSchema = obj(map[string]interface{}{
		"switch": str("交换机 id；空字符串表示未定位"),
		"port":   str("端口名，如 Gi0/1、12"),
	})
	monitorSchema = obj(map[string]interface{}{
		"tcp_ports":       tcpPortsSchema(16),
		"enabled":         boolean("是否启用 Ping 和 TCPing 监控（默认 true）"),
		"alert":           boolean("是否推送告警通知（默认 false）"),
		"interval_ms":     integer("ping 间隔毫秒（>=200，0 表示用默认值）"),
		"timeout_ms":      integer("超时毫秒"),
		"latency_warn_ms": integer("1 分钟平均延迟告警阈值 ms"),
		"loss_warn_pct":   map[string]interface{}{"type": "number", "description": "1 分钟丢包率告警阈值 %"},
	})
	positionSchema = map[string]interface{}{
		"type": []string{"object", "null"}, "description": "拓扑图坐标；null 表示自动排布",
		"properties": map[string]interface{}{"x": map[string]interface{}{"type": "number"}, "y": map[string]interface{}{"type": "number"}},
	}
	idOrIP = map[string]interface{}{
		"id": str("设备 id（节点或交换机）"),
		"ip": str("设备 IP（未提供 id 时使用）"),
	}
	ranges6 = []string{"15m", "1h", "6h", "24h", "7d", "30d"}
)

func withIDIP(extra map[string]interface{}) map[string]interface{} {
	m := map[string]interface{}{}
	for k, v := range idOrIP {
		m[k] = v
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

var mcpTools []*mcpTool

func init() {
	mcpTools = []*mcpTool{
		{
			Name: "get_overview", Title: "监控概况",
			Description: "返回机房信息、各状态设备数量、监控是否暂停/是否在监控时段、告警通道状态、网关延迟，以及当前所有异常（离线/丢包/延迟高）设备。排查问题时先调用它。",
			Schema:      obj(map[string]interface{}{}),
			Run:         toolOverview,
		},
		{
			Name: "list_devices", Title: "设备列表",
			Description: "列出交换机和终端节点及实时指标（当前延迟、1 分钟平均与抖动、1m/5m/1h 丢包率、状态、接线位置、是否开启告警）。可按状态、交换机、关键词筛选。",
			Schema: obj(map[string]interface{}{
				"state":      enum("按状态筛选；abnormal=离线+丢包+延迟高；unmonitored=无 IP 或停用监控", "ok", "slow", "loss", "down", "unknown", "paused", "unmonitored", "abnormal"),
				"switch":     str("只列出接在该交换机（id）上的节点"),
				"query":      str("关键词，匹配 IP、MAC、名称、厂商、位置、备注、标签"),
				"alert_only": boolean("只列出已开启告警的设备"),
				"sort":       enum("排序：ip（默认）、loss（1h 丢包率降序）、latency（1m 平均延迟降序）", "ip", "loss", "latency"),
				"limit":      integer("最多返回条数，默认 200"),
			}),
			Run: toolListDevices,
		},
		{
			Name: "get_device", Title: "设备详情",
			Description: "按 id 或 ip 返回单个设备的完整定义（接线位置、标记、监控参数等）、实时指标和最近告警。",
			Schema:      obj(withIDIP(nil)),
			Run:         toolGetDevice,
		},
		{
			Name: "get_history", Title: "历史延迟与丢包",
			Description: "返回设备在指定时间范围内的延迟（平均/最小/最大）与丢包率统计和按时间分桶的数据点。15m/1h 为秒级数据，更长范围为分钟级聚合（最多 30 天）。",
			Schema: obj(withIDIP(map[string]interface{}{
				"range":  enum("时间范围，默认 1h", ranges6...),
				"port":   integer("可选：查询该 TCP 端口的连接耗时和失败率；省略时查询 ICMP"),
				"points": integer("返回的数据点数量上限，默认 60，最大 360；0 表示只要统计"),
			})),
			Run: toolHistory,
		},
		{
			Name: "list_alerts", Title: "告警记录",
			Description: "返回最近的告警事件（离线、恢复、丢包、延迟高等）及通知发送结果，新的在前。",
			Schema: obj(withIDIP(map[string]interface{}{
				"limit": integer("返回条数，默认 50，最大 300"),
			})),
			Run: toolAlerts,
		},
		{
			Name: "get_topology", Title: "拓扑数据",
			Description: "返回完整拓扑：站点、交换机、链路、节点。format=yaml 时返回 topology.yaml 原文（含字段说明注释），适合整体修改后用 replace_topology_yaml 写回。",
			Schema:      obj(map[string]interface{}{"format": enum("json（默认）或 yaml", "json", "yaml")}),
			Run:         toolTopology,
		},
		{
			Name: "get_settings", Title: "监控设置",
			Description: "返回机房信息、全局监控时段、默认 ping 参数、告警规则（webhook 已脱敏）、历史保留天数、网段扫描设置与扫描任务、MCP 是否只读。",
			Schema:      obj(map[string]interface{}{}),
			Run:         toolSettings,
		},
		// ---------------- 写操作 ----------------
		{
			Name: "upsert_device", Title: "新增或修改终端节点", Write: true,
			Description: "新增或修改一个终端节点。用 id（或 ip）定位已有节点时只修改提供的字段（attach、monitor 内部字段逐项合并）；找不到时新建（id 可省略自动生成）。修改立即写回 topology.yaml。不能用于交换机（用 upsert_switch）。",
			Schema: obj(map[string]interface{}{
				"id":       str("节点 id；新建时可省略"),
				"ip":       str("IPv4；未提供 id 时也用于定位已有节点"),
				"name":     str("显示名"),
				"mac":      str("MAC 地址（网段扫描按 MAC 跟踪 IP 变化）"),
				"ip_mode":  enum("IP 分配方式：空=自动跟踪，dhcp=自动获取，static=固定 IP（锁定，扫描不自动修改）", topology.IPModes...),
				"vendor":   str("厂商"),
				"attach":   endpointSchema,
				"location": str("物理位置（机柜/U 位/房间）"),
				"flags":    map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string", "enum": topology.KnownFlags}, "description": "标记（整体替换）"},
				"tags":     strArr("自定义标签（整体替换）"),
				"note":     str("备注"),
				"monitor":  monitorSchema,
				"position": positionSchema,
			}),
			Run: toolUpsertDevice,
		},
		{
			Name: "delete_device", Title: "删除终端节点", Write: true, Destructive: true,
			Description: "按 id 或 ip 删除终端节点（历史数据保留到过期）。删除前请向用户确认。",
			Schema:      obj(withIDIP(nil)),
			Run:         toolDeleteDevice,
		},
		{
			Name: "upsert_switch", Title: "新增或修改网络设备（交换机/路由器/防火墙/光猫/AP 等）", Write: true,
			Description: "新增或修改带端口、可与其他设备连线的网络设备：交换机、路由器、防火墙、上网行为管理、光猫、无线 AP、无线控制器、负载均衡（用 type 区分，省略为交换机）。用 id 定位已有设备时只修改提供的字段；找不到时新建（新建必须提供 name）。ports 可用于端口下拉和排序。",
			Schema: obj(map[string]interface{}{
				"id":       str("设备 id；新建时可省略"),
				"name":     str("显示名"),
				"type":     enum("设备类型：switch 交换机 / router 路由器 / firewall 防火墙 / gateway 上网行为管理 / modem 光猫 / ap 无线AP / ac 无线控制器 / lb 负载均衡 / other 其他网络设备", topology.DeviceTypes...),
				"role":     enum("角色", topology.Roles...),
				"layer":    str("L2 / L3"),
				"vendor":   str("厂商"),
				"model":    str("型号"),
				"ip":       str("管理 IP（填写后交换机本身也会被 ping）"),
				"mac":      str("MAC"),
				"location": str("物理位置"),
				"note":     str("备注"),
				"ports":    strArr("端口名列表（整体替换）"),
				"port_locations": map[string]interface{}{"type": "object", "additionalProperties": map[string]interface{}{"type": "string"},
					"description": "端口 → 物理位置对照（整体替换），如 {\"12\": \"A03 机柜 配线架 2-12\"}；端口定位时自动填入设备的 location"},
				"node_side":    enum("终端卡片画在左侧或右侧", topology.NodeSides...),
				"node_columns": integer("每个端口下卡片列数 1~6"),
				"monitor":      monitorSchema,
				"position":     positionSchema,
			}),
			Run: toolUpsertSwitch,
		},
		{
			Name: "delete_switch", Title: "删除交换机", Write: true, Destructive: true,
			Description: "删除交换机：下挂节点变为未定位，相关链路一并删除。删除前请向用户确认。",
			Schema:      obj(map[string]interface{}{"id": str("交换机 id")}, "id"),
			Run:         toolDeleteSwitch,
		},
		{
			Name: "upsert_link", Title: "新增或修改交换机间链路", Write: true,
			Description: "新增或修改交换机之间的连线。from 为上游（靠近核心）一端，to 为下游一端；告警时据此合并下游交换机故障。提供已存在的 id 时修改，否则新建。",
			Schema: obj(map[string]interface{}{
				"id":   str("链路 id；新建时可省略"),
				"from": endpointSchema,
				"to":   endpointSchema,
				"kind": enum("链路类型", "uplink", "stack", "other"),
				"note": str("备注"),
			}),
			Run: toolUpsertLink,
		},
		{
			Name: "delete_link", Title: "删除链路", Write: true, Destructive: true,
			Description: "按 id 删除交换机间链路。",
			Schema:      obj(map[string]interface{}{"id": str("链路 id")}, "id"),
			Run:         toolDeleteLink,
		},
		{
			Name: "set_alerting", Title: "开关设备告警", Write: true,
			Description: "批量开启或关闭设备的告警通知（设备默认不告警）。用 ids 或 ips 指定设备，或用 switch 指定某交换机下的全部节点。",
			Schema: obj(map[string]interface{}{
				"ids":     strArr("设备 id 列表"),
				"ips":     strArr("设备 IP 列表"),
				"switch":  str("交换机 id：选中接在该交换机上的全部节点"),
				"enabled": boolean("true 开启，false 关闭"),
			}, "enabled"),
			Run: toolSetAlerting,
		},
		{
			Name: "set_schedule", Title: "设置全局监控时段", Write: true,
			Description: "设置全局监控时段：enabled=false 为全天候监控；enabled=true 时只在 windows 中任一时段内 ping 和告警，时段外暂停。时间为 HH:MM，end 不大于 start 表示跨午夜；days 1=周一…7=周日，省略为每天。",
			Schema: obj(map[string]interface{}{
				"enabled": boolean("是否启用时段限制"),
				"windows": map[string]interface{}{
					"type": "array", "description": "时段列表",
					"items": obj(map[string]interface{}{
						"days":  map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 7}, "description": "星期几，省略表示每天"},
						"start": str("开始 HH:MM"),
						"end":   str("结束 HH:MM"),
					}, "start", "end"),
				},
			}, "enabled"),
			Run: toolSetSchedule,
		},
		{
			Name: "set_monitoring_paused", Title: "暂停/恢复监控", Write: true,
			Description: "手动暂停或恢复全部 ping 监控（暂停期间不告警）。与监控时段相互独立。",
			Schema:      obj(map[string]interface{}{"paused": boolean("true 暂停，false 恢复")}, "paused"),
			Run:         toolSetPaused,
		},
		{
			Name: "replace_topology_yaml", Title: "整体替换拓扑", Write: true, Destructive: true,
			Description: "用完整的 topology.yaml 文本替换当前拓扑（迁移机房、批量导入时使用）。务必先 dry_run=true 校验并查看新增/删除/修改的设备，向用户确认后再 dry_run=false 写入。校验失败会返回逐条问题。",
			Schema: obj(map[string]interface{}{
				"yaml":    str("完整的 topology.yaml 内容"),
				"dry_run": boolean("true 只校验并返回差异，不写入（默认 true）"),
			}, "yaml"),
			Run: toolReplaceYAML,
		},
		{
			Name: "get_discovery", Title: "网段扫描状态与结果",
			Description: "返回自动扫描总开关、各扫描任务（网段、间隔、是否自动添加、上次/下次运行、在线数）、各任务最近一次扫描结果（自动添加的设备、按 MAC 跟踪到的 IP 变化等）、汇总的待确认 IP 变化（suggestions）与未登记设备（unknown），以及最近的变更记录。",
			Schema: obj(map[string]interface{}{
				"job":           str("只返回该任务 id 的结果（临时扫描为 adhoc）；省略返回全部任务"),
				"include_hosts": boolean("是否返回全部在线地址列表（默认 false，只返回统计与变更）"),
				"events":        integer("返回最近多少条变更记录，默认 30"),
			}),
			Run: toolGetDiscovery,
		},
		{
			Name: "scan_network", Title: "立即扫描网段", Write: true,
			Description: "立即 ping 扫描并读取 ARP：自动添加新设备（未定位）、按 MAC 更新变化的 IP、补全缺失的 MAC。提供 subnets 时临时扫描这些网段（不保存为任务）；提供 job 时运行该任务；都不提供时依次运行全部任务。/24 网段约 10 秒，返回变更与建议。",
			Schema: obj(map[string]interface{}{
				"job":      str("扫描任务 id"),
				"subnets":  strArr("临时扫描的网段列表（CIDR，每个最大 /20），如 [\"192.168.1.0/24\"]"),
				"auto_add": boolean("临时扫描时是否自动添加新设备，默认 true"),
			}),
			Run: toolScanNetwork,
		},
		{
			Name: "upsert_scan_job", Title: "新增或修改自动扫描任务", Write: true,
			Description: "自动扫描模式：新增或修改一个扫描任务，按间隔自动扫描一个或多个网段，并把新设备自动添加为“未定位”节点。用 id 定位已有任务时只修改提供的字段；找不到时新建（新建必须提供 subnets）。run_now=true 保存后立即扫描一次。需要自动扫描总开关（set_discovery_settings.enabled）为开。",
			Schema: obj(map[string]interface{}{
				"id":           str("任务 id（字母数字 - _）；新建时可省略自动生成"),
				"name":         str("显示名，如“办公区 3 楼”"),
				"enabled":      boolean("是否定时运行（新建默认 true）"),
				"subnets":      strArr("网段列表（CIDR，每个最大 /20）；空数组表示使用机房网段 site.subnet"),
				"interval_min": integer("扫描间隔分钟（>=5，默认 30）"),
				"auto_add":     boolean("新设备是否自动添加（默认 true）；false 时只列在未登记设备中"),
				"alert":        boolean("自动添加的设备是否同时开启告警通知（默认 false）"),
				"tags":         strArr("自动添加的设备附加的标签"),
				"run_now":      boolean("保存后立即扫描一次"),
			}),
			Run: toolUpsertScanJob,
		},
		{
			Name: "delete_scan_job", Title: "删除扫描任务", Write: true, Destructive: true,
			Description: "删除一个自动扫描任务（已添加的设备不受影响）。删除前请向用户确认。",
			Schema:      obj(map[string]interface{}{"id": str("任务 id")}, "id"),
			Run:         toolDeleteScanJob,
		},
		{
			Name: "set_discovery_settings", Title: "修改扫描全局设置", Write: true,
			Description: "修改网段扫描的全局设置（只修改提供的字段）：enabled 自动扫描总开关、ip_tracking 按 MAC 跟踪 IP 的范围、fast_resolve 离线时立即重新解析、timeout_ms/concurrency 扫描速度、exclude 排除地址、notify 推送变化、oui MAC 前缀厂商表。扫描任务用 upsert_scan_job 修改。",
			Schema: obj(map[string]interface{}{
				"enabled":      boolean("自动扫描总开关"),
				"ip_tracking":  enum("all=除 static 外的节点都按 MAC 跟踪 IP；dhcp=仅 ip_mode=dhcp；off=不自动改 IP", "all", "dhcp", "off"),
				"fast_resolve": boolean("可跟踪设备连续 ping 失败时立即扫描并暂缓离线告警 90 秒"),
				"timeout_ms":   integer("扫描单次 ping 超时毫秒（>=100）"),
				"concurrency":  integer("扫描并发数 1~256"),
				"exclude":      strArr("不扫描、不自动添加的 IP 或 CIDR（整体替换）"),
				"notify":       boolean("新增设备、IP 变化时推送通知"),
				"oui":          map[string]interface{}{"type": "object", "additionalProperties": map[string]interface{}{"type": "string"}, "description": "MAC 前缀→厂商（整体替换），如 {\"60:D7:E3\": \"深大智能\"}"},
			}),
			Run: toolSetDiscoverySettings,
		},
		{
			Name: "add_discovered_devices", Title: "添加扫描发现的设备", Write: true,
			Description: "把扫描发现、尚未登记的设备（get_discovery 的 unknown）添加为“未定位”节点。",
			Schema: obj(map[string]interface{}{
				"ips": strArr("要添加的 IP 列表"),
				"all": boolean("添加全部未登记设备"),
			}),
			Run: toolAddDiscovered,
		},
		{
			Name: "apply_ip_suggestion", Title: "采纳 IP 变化", Write: true,
			Description: "采纳扫描发现的 IP 变化（get_discovery 的 suggestions，通常是固定 IP 锁定的设备换了地址）：把节点 IP 改为新地址，可同时修改 IP 分配方式。",
			Schema: obj(map[string]interface{}{
				"node_id": str("节点 id"),
				"ip":      str("新 IP；省略时使用待确认建议中的地址"),
				"ip_mode": enum("同时修改 IP 分配方式（省略则不变）：空=自动跟踪，dhcp，static", topology.IPModes...),
			}, "node_id"),
			Run: toolApplySuggestion,
		},
		{
			Name: "set_alert_config", Title: "修改告警设置", Write: true,
			Description: "修改多渠道告警设置（只修改提供的字段）：开关、webhook、判定次数、持续时间、重复提醒、合并窗口、恢复通知、消息标题、代理。支持企业微信、钉钉、飞书、Slack、通用 Webhook；启用前至少配置一个地址。",
			Schema: obj(map[string]interface{}{
				"enabled":             boolean("是否启用告警推送"),
				"webhook_url":         str("企业微信群机器人 webhook 地址"),
				"dingtalk_url":        str("钉钉 Webhook 地址，空字符串停用"),
				"dingtalk_secret":     str("钉钉可选加签密钥，空字符串清除"),
				"feishu_url":          str("飞书 Webhook 地址，空字符串停用"),
				"feishu_secret":       str("飞书可选签名密钥，空字符串清除"),
				"slack_url":           str("Slack Incoming Webhook 地址，空字符串停用"),
				"generic_webhook_url": str("通用 Webhook 地址，POST JSON {title,text}，空字符串停用"),

				"down_after_failures":  integer("连续失败多少次判离线"),
				"up_after_successes":   integer("连续成功多少次判恢复"),
				"loss_duration_min":    integer("丢包持续多少分钟告警"),
				"latency_duration_min": integer("延迟高持续多少分钟告警"),
				"repeat_interval_min":  integer("故障持续时重复提醒间隔分钟，0 不重复"),
				"merge_window_sec":     integer("多少秒内的告警合并为一条"),
				"send_recovery":        boolean("是否发送恢复通知"),
				"site_title":           str("消息标题前缀"),
				"proxy_url":            str("HTTP 代理，可空"),
				"tls_skip_verify":      boolean("跳过 HTTPS 证书校验"),
			}),
			Run: toolSetAlertConfig,
		},
		{
			Name: "send_test_alert", Title: "发送测试告警", Write: true,
			Description: "按当前告警设置向所有已配置渠道发送一条测试消息，用于确认 webhook 可用。",
			Schema:      obj(map[string]interface{}{}),
			Run:         toolSendTestAlert,
		},
		{
			Name: "set_ping_defaults", Title: "修改默认 ping 参数", Write: true,
			Description: "修改节点未单独设置时的默认 ping 参数与历史保留天数（只修改提供的字段），立即生效。",
			Schema: obj(map[string]interface{}{
				"interval_ms":     integer("ping 间隔毫秒（>=200）"),
				"timeout_ms":      integer("超时毫秒（>=100）"),
				"packet_size":     integer("数据长度字节"),
				"latency_warn_ms": integer("1 分钟平均延迟阈值 ms"),
				"loss_warn_pct":   map[string]interface{}{"type": "number", "description": "1 分钟丢包率阈值 %"},
				"retention_days":  integer("历史数据保留天数"),
			}),
			Run: toolSetPingDefaults,
		},
		{
			Name: "set_site", Title: "修改机房信息", Write: true,
			Description: "修改机房信息（只修改提供的字段）：名称、网段（未单独配置网段的扫描任务会扫描它）、默认网关、说明。",
			Schema: obj(map[string]interface{}{
				"name":        str("机房名称"),
				"subnet":      str("网段 CIDR"),
				"gateway":     str("默认网关 IP"),
				"description": str("说明"),
			}),
			Run: toolSetSite,
		},
		{
			Name: "set_ai_settings", Title: "修改 AI 诊断设置", Write: true,
			Description: "修改网页「AI 诊断」的设置（只修改提供的字段）：是否启用、协议格式（openai / openai-responses / anthropic / gemini）、深度思考、回答前自检（模型写完回答后核对数据、必要时实测并修正）、模型名、单次诊断超时。出于安全考虑不能修改 API Key、接口地址和代理（需在网页中修改）。",
			Schema: obj(map[string]interface{}{
				"enabled":     boolean("是否启用 AI 诊断"),
				"protocol":    enum("协议格式：openai（OpenAI Chat Completions）、openai-responses（OpenAI Responses）、anthropic（Claude Messages）、gemini（Google Gemini）", "openai", "openai-responses", "anthropic", "gemini"),
				"thinking":    boolean("深度思考（更慢、更贵）"),
				"self_check":  boolean("回答前自检（默认开启）"),
				"model":       str("模型名，如 deepseek-flash"),
				"timeout_sec": integer("单次诊断最长秒数（10~600）"),
			}),
			Run: toolSetAISettings,
		},
		{
			Name: "get_locate_status", Title: "端口定位状态",
			Description: "返回端口定位（SNMP 读取交换机 MAC 地址表）的设置（团体名已脱敏）、要读取的交换机，以及最近一次定位结果：自动挂到端口的设备、端口变化待确认的设备、无法确定的设备、边缘端口上未登记的 MAC、各交换机读取情况和判定的级联口。",
			Schema:      obj(map[string]interface{}{}),
			Run:         toolLocateStatus,
		},
		{
			Name: "get_switch_mac_table", Title: "交换机 MAC 地址表",
			Description: "通过 SNMP 读取交换机的 MAC 地址表，按端口列出学到的 MAC 及对应的已登记设备，并标出级联口。用于查看“某个口后面接了什么”。",
			Schema: obj(map[string]interface{}{
				"switch":  str("交换机 id"),
				"port":    str("只返回该端口（topology.yaml 中的端口名，如 Gi0/1、12）"),
				"refresh": boolean("现场重新读取（默认 true；false 时使用最近一次读取的缓存）"),
			}, "switch"),
			Run: toolSwitchMACTable,
		},
		{
			Name: "locate_device", Title: "查询设备实际接线端口",
			Description: "按设备的 MAC 在所有交换机 MAC 表中查找，返回它实际接在哪台交换机的哪个端口（排除级联口、取 MAC 最少的端口），以及与拓扑中登记的位置是否一致。只查询不修改；要写入用 apply_location 或 upsert_device。",
			Schema: obj(withIDIP(map[string]interface{}{
				"mac":     str("MAC 地址（不是已登记设备时使用）"),
				"refresh": boolean("现场重新读取 MAC 表（默认 true）"),
			})),
			Run: toolLocateDevice,
		},
		{
			Name: "auto_locate", Title: "自动定位设备端口", Write: true,
			Description: "立即读取所有交换机的 MAC 表并定位：“未定位”的设备自动挂到查到的端口（按端口位置对照补全物理位置）；已定位但端口变化的设备默认只列出待确认，apply_moves=true 时一并修改。dry_run=true 只预览不写入。",
			Schema: obj(map[string]interface{}{
				"dry_run":     boolean("只预览不写入（默认 false）"),
				"apply_moves": boolean("已定位设备端口变化时也直接修改（默认 false，修改前请向用户确认）"),
			}),
			Run: toolAutoLocate,
		},
		{
			Name: "apply_location", Title: "采纳端口变化", Write: true,
			Description: "把一台已定位设备的接线位置改为 MAC 表中查到的新端口（auto_locate / get_locate_status 中的待确认项）。",
			Schema:      obj(withIDIP(nil)),
			Run:         toolApplyLocation,
		},
		{
			Name: "test_switch_snmp", Title: "测试交换机 SNMP",
			Description: "测试能否用 SNMP v2c 只读读取交换机：返回系统名称、型号描述、端口数和 MAC 表条数。不提供 host/community 时使用该交换机已保存的设置。",
			Schema: obj(map[string]interface{}{
				"switch":    str("交换机 id"),
				"host":      str("读取地址（默认交换机管理 IP）"),
				"community": str("团体名（默认已保存的设置）"),
			}),
			Run: toolTestSNMP,
		},
		{
			Name: "set_locate_settings", Title: "修改端口定位设置", Write: true,
			Description: "修改端口定位设置（只修改提供的字段）：enabled 定时定位、interval_min 间隔、community 默认只读团体名、timeout_ms、notify 端口变化推送通知、switches 单台交换机设置（按交换机 id 合并：host 读取地址、community 团体名、disabled 不读取）。团体名不会在任何返回中明文显示。交换机上需配置 snmp-server community <名称> ro。",
			Schema: obj(map[string]interface{}{
				"enabled":      boolean("是否定时定位"),
				"interval_min": integer("定位间隔分钟（>=2）"),
				"community":    str("默认 SNMP v2c 只读团体名"),
				"timeout_ms":   integer("SNMP 超时毫秒"),
				"notify":       boolean("端口变化推送通知"),
				"switches": map[string]interface{}{"type": "object", "description": "按交换机 id 的单独设置，如 {\"core\": {\"host\": \"192.168.101.250\"}}",
					"additionalProperties": obj(map[string]interface{}{"host": str("读取地址"), "community": str("团体名"), "disabled": boolean("不读取")})},
			}),
			Run: toolSetLocateSettings,
		},
		{
			Name: "reset_layout", Title: "恢复自动排布", Write: true,
			Description: "清除节点的手动坐标：已接线的回到交换机端口旁自动排布，未定位的按 IP 整齐排在拓扑图下方。可指定只处理某台交换机（此时不动未定位节点）。",
			Schema:      obj(map[string]interface{}{"switch": str("交换机 id；省略表示全部")}),
			Run:         toolResetLayout,
		},
		{
			Name: "reload_topology_from_disk", Title: "从磁盘重新加载拓扑", Write: true,
			Description: "topology.yaml 在程序外被修改后，重新读取并生效。文件校验失败时不会生效并返回问题列表。",
			Schema:      obj(map[string]interface{}{}),
			Run: func(s *Server, _ map[string]json.RawMessage, remote string) (interface{}, error) {
				if err := s.Topo.Reload(); err != nil {
					return nil, err
				}
				log.Printf("MCP(%s) 从磁盘重新加载拓扑", remote)
				t, rev := s.Topo.Snapshot()
				return map[string]interface{}{"ok": true, "rev": rev, "switches": len(t.Switches), "nodes": len(t.Nodes)}, nil
			},
		},
	}
	mcpTools = append(mcpTools, analysisTools()...)
	mcpTools = append(mcpTools, probeTools()...)
	mcpTools = append(mcpTools, tcpMonitoringTools()...)
	mcpTools = append(mcpTools, discoverTools()...)
}

// ---------------------------------------------------------------------------
// 工具实现：查询

func toolOverview(s *Server, _ map[string]json.RawMessage, _ string) (interface{}, error) {
	t, rev := s.Topo.Snapshot()
	sts := s.statusMap()
	cfg := s.Cfg.Get()
	rows := buildRows(t, sts)
	counts := map[string]int{}
	alertOn := 0
	var abnormal []deviceRow
	for _, r := range rows {
		counts[r.State]++
		if r.Alert {
			alertOn++
		}
		if isAbnormal(r.State) {
			abnormal = append(abnormal, r)
		}
	}
	sort.Slice(abnormal, func(i, j int) bool { return ipKey(abnormal[i].IP) < ipKey(abnormal[j].IP) })
	gw := map[string]interface{}{"ip": t.Site.Gateway}
	if k, id := t.FindByIP(t.Site.Gateway); k != "" {
		if st, ok := sts[id]; ok {
			gw["state"], gw["avg_1m_ms"], gw["loss_1m_pct"] = st.State, st.Avg1m, st.Loss1m
		}
	}
	return map[string]interface{}{
		"site":              t.Site,
		"topology_rev":      rev,
		"switches":          len(t.Switches),
		"nodes":             len(t.Nodes),
		"state_counts":      counts,
		"monitoring":        map[string]interface{}{"paused": s.Mon.Paused(), "pause_reason": s.Mon.PauseReason(), "schedule": cfg.Schedule.Describe()},
		"alerting":          map[string]interface{}{"enabled": cfg.Alert.Enabled, "wecom_enabled": cfg.Alert.Enabled && cfg.Alert.WebhookURL != "", "webhook_configured": cfg.Alert.HasChannels(), "devices_with_alert": alertOn},
		"gateway":           gw,
		"abnormal_devices":  abnormal,
		"server_time":       time.Now().Format("2006-01-02 15:04:05"),
		"state_description": "ok 正常 / slow 延迟高 / loss 丢包 / down 离线 / unknown 等待数据 / paused 暂停 / unmonitored 未监控",
	}, nil
}

func toolListDevices(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	t, _ := s.Topo.Snapshot()
	rows := buildRows(t, s.statusMap())
	state, sw, q := argStr(a, "state"), argStr(a, "switch"), strings.ToLower(argStr(a, "query"))
	alertOnly, _ := argBool(a, "alert_only")
	out := []deviceRow{}
	for _, r := range rows {
		if state == "abnormal" && !isAbnormal(r.State) || state != "" && state != "abnormal" && r.State != state {
			continue
		}
		if sw != "" {
			n := t.FindNode(r.ID)
			if n == nil || n.Attach.Switch != sw {
				continue
			}
		}
		if alertOnly && !r.Alert {
			continue
		}
		if q != "" {
			hay := strings.ToLower(strings.Join([]string{r.ID, r.IP, r.MAC, r.Name, r.Hostname, r.Vendor, r.Attach, r.Location}, " "))
			if n := t.FindNode(r.ID); n != nil {
				hay += " " + strings.ToLower(n.Note+" "+strings.Join(n.Tags, " "))
				if n.Attach.Port != "" {
					hay += " port " + strings.ToLower(n.Attach.Port) + " port" + strings.ToLower(n.Attach.Port)
				}
			}
			if !strings.Contains(hay, q) {
				continue
			}
		}
		out = append(out, r)
	}
	val := func(p *float64) float64 {
		if p == nil {
			return -1
		}
		return *p
	}
	switch argStr(a, "sort") {
	case "loss":
		sort.SliceStable(out, func(i, j int) bool { return val(out[i].Loss1h) > val(out[j].Loss1h) })
	case "latency":
		sort.SliceStable(out, func(i, j int) bool { return val(out[i].Avg1m) > val(out[j].Avg1m) })
	default:
		sort.SliceStable(out, func(i, j int) bool { return ipKey(out[i].IP) < ipKey(out[j].IP) })
	}
	total := len(out)
	limit := argInt(a, "limit", 200)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return map[string]interface{}{"total": total, "returned": len(out), "devices": out}, nil
}

func toolGetDevice(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	t, _ := s.Topo.Snapshot()
	kind, id, err := resolveDevice(t, argStr(a, "id"), argStr(a, "ip"))
	if err != nil {
		return nil, err
	}
	res := map[string]interface{}{"kind": kind}
	if kind == "node" {
		n := t.FindNode(id)
		res["definition"], res["attach_label"] = n, t.AttachLabel(n.Attach)
	} else {
		sw := t.FindSwitch(id)
		var attached []string
		for _, n := range t.Nodes {
			if n.Attach.Switch == id {
				attached = append(attached, fmt.Sprintf("%s %s", n.Attach.Port, n.DisplayName()))
			}
		}
		var links []*topology.Link
		for _, l := range t.Links {
			if l.From.Switch == id || l.To.Switch == id {
				links = append(links, l)
			}
		}
		res["definition"], res["attached_nodes"], res["links"] = sw, attached, links
	}
	if st, ok := s.Mon.Status(id); ok {
		res["status"] = st
		res["state_since"] = fmtLocal(st.StateSince)
		res["spark_note"] = "spark 为最近 60 次 ping 的延迟 ms，-1 表示丢包"
	} else {
		res["status"] = "未监控（没有 IP 或已停用监控）"
	}
	res["tcp_monitoring"], _ = toolGetTCPMonitoring(s, a, "")
	var alerts []alert.Event
	for _, ev := range s.Alerts.History(300) {
		if ev.TargetID == id && len(alerts) < 10 {
			alerts = append(alerts, ev)
		}
	}
	res["recent_alerts"] = alerts
	return res, nil
}

func toolHistory(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	t, _ := s.Topo.Snapshot()
	_, id, err := resolveDevice(t, argStr(a, "id"), argStr(a, "ip"))
	if err != nil {
		return nil, err
	}
	if raw, ok := a["port"]; ok {
		var port int
		if err := json.Unmarshal(raw, &port); err != nil || port < 1 || port > 65535 {
			return nil, badRequest("port 必须为 1–65535 的整数")
		}
		id = monitor.TCPID(id, port)
	}
	rg := argStr(a, "range")
	if rg == "" {
		rg = "1h"
	}
	maxPts := argInt(a, "points", 60)
	if maxPts > 360 {
		maxPts = 360
	}
	bucketPts := maxPts
	if bucketPts <= 0 {
		bucketPts = 360
	}
	h, err := s.history(id, rg, bucketPts)
	if err != nil {
		return nil, err
	}
	var sent, lost int
	var sum float64
	var recvTotal int
	mn, mx := math.Inf(1), 0.0
	var worst []map[string]interface{}
	pts := make([]map[string]interface{}, 0, len(h.Points))
	for _, p := range h.Points {
		sent += p.Sent
		recv := int(math.Round(float64(p.Sent) * (100 - p.Loss) / 100))
		lost += p.Sent - recv
		if p.Avg != nil {
			sum += *p.Avg * float64(recv)
			recvTotal += recv
		}
		if p.Min != nil && *p.Min < mn {
			mn = *p.Min
		}
		if p.Max != nil && *p.Max > mx {
			mx = *p.Max
		}
		row := map[string]interface{}{"time": fmtLocal(p.T), "loss_pct": p.Loss}
		if p.Avg != nil {
			row["avg_ms"], row["min_ms"], row["max_ms"] = *p.Avg, *p.Min, *p.Max
		}
		pts = append(pts, row)
		if p.Loss > 0 {
			worst = append(worst, row)
		}
	}
	sort.SliceStable(worst, func(i, j int) bool { return worst[i]["loss_pct"].(float64) > worst[j]["loss_pct"].(float64) })
	if len(worst) > 10 {
		worst = worst[:10]
	}
	summary := map[string]interface{}{"samples": sent, "lost": lost}
	if sent > 0 {
		summary["loss_pct"] = r2(float64(lost) * 100 / float64(sent))
	}
	if recvTotal > 0 {
		summary["avg_ms"], summary["min_ms"], summary["max_ms"] = r2(sum/float64(recvTotal)), mn, mx
	}
	res := map[string]interface{}{
		"id": id, "range": rg, "from": h.From.Format("2006-01-02 15:04:05"), "to": h.To.Format("2006-01-02 15:04:05"),
		"bucket": h.Bucket.String(), "summary": summary, "worst_loss_buckets": worst,
	}
	if maxPts > 0 {
		res["points"] = pts
	}
	if sent == 0 {
		res["note"] = "该时间范围内没有数据（可能未监控、处于暂停/非监控时段，或程序刚启动）"
	}
	return res, nil
}

func toolAlerts(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	limit := argInt(a, "limit", 50)
	if limit <= 0 || limit > 300 {
		limit = 300
	}
	id := argStr(a, "id")
	if ip := argStr(a, "ip"); id == "" && ip != "" {
		t, _ := s.Topo.Snapshot()
		_, id, _ = resolveDevice(t, "", ip)
		if id == "" {
			return nil, fmt.Errorf("找不到 IP 为 %s 的设备", ip)
		}
	}
	var out []map[string]interface{}
	for _, ev := range s.Alerts.History(300) {
		if id != "" && ev.TargetID != id {
			continue
		}
		out = append(out, map[string]interface{}{
			"time": ev.Time.Format("2006-01-02 15:04:05"), "type": ev.Type, "id": ev.TargetID, "ip": ev.IP,
			"name": ev.Name, "location": ev.Location, "detail": ev.Detail, "sent": ev.Sent, "error": ev.Error,
		})
		if len(out) >= limit {
			break
		}
	}
	return map[string]interface{}{
		"alerts":     out,
		"type_names": "down 离线 / still_down 仍离线 / recover 恢复 / loss 丢包 / loss_recover 丢包恢复 / slow 延迟高 / slow_recover 延迟恢复 / test 测试",
	}, nil
}

func toolTopology(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	if argStr(a, "format") == "yaml" {
		data, err := s.Topo.YAML()
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"yaml": string(data)}, nil
	}
	t, rev := s.Topo.Snapshot()
	return map[string]interface{}{"rev": rev, "topology": t}, nil
}

func toolSettings(s *Server, _ map[string]json.RawMessage, _ string) (interface{}, error) {
	c := s.Cfg.Get()
	t, _ := s.Topo.Snapshot()
	site := t.Site
	al := c.Alert
	al = maskAlertChannels(al)
	return map[string]interface{}{
		"schedule":             c.Schedule,
		"schedule_description": c.Schedule.Describe(),
		"in_schedule":          s.Mon.InSchedule(),
		"manual_paused":        s.Mon.ManualPaused(),
		"ping_defaults":        c.Ping,
		"alert":                al,
		"retention_days":       c.RetentionDays,
		"mcp_read_only":        c.MCP.ReadOnly,
		"discovery":            c.Discovery,
		"locate":               maskLocate(c.Locate),
		"site":                 site,
		"ai":                   aiPublicSettings(c.AI),
	}, nil
}

// ---------------------------------------------------------------------------
// 工具实现：修改

func toolUpsertDevice(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	id, ip := argStr(a, "id"), argStr(a, "ip")
	var action string
	var saved topology.Node
	err := s.Topo.Update(func(t *topology.Topology) error {
		var target *topology.Node
		if id != "" {
			target = t.FindNode(id)
			if target == nil && t.FindSwitch(id) != nil {
				return fmt.Errorf("%s 是交换机，请使用 upsert_switch", id)
			}
		} else if ip != "" {
			if k, rid := t.FindByIP(ip); k == "node" {
				target = t.FindNode(rid)
			} else if k == "switch" {
				return fmt.Errorf("IP %s 属于交换机 %s，请使用 upsert_switch", ip, rid)
			}
		}
		if target != nil {
			var n topology.Node
			patch := map[string]json.RawMessage{}
			for k, v := range a {
				if k != "id" {
					patch[k] = v
				}
			}
			if err := mergePatch(target, patch, &n); err != nil {
				return err
			}
			n.ID = target.ID
			if n.Attach.Switch != "" {
				n.Flags = removeFlag(n.Flags, "unlocated")
			}
			*target = n
			action, saved = "updated", n
			return nil
		}
		var n topology.Node
		if err := mergePatch(&topology.Node{}, a, &n); err != nil {
			return err
		}
		if n.ID == "" {
			n.ID = t.NewNodeID(&n)
		}
		t.Nodes = append(t.Nodes, &n)
		action, saved = "created", n
		return nil
	})
	if err != nil {
		return nil, err
	}
	log.Printf("MCP(%s) %s 节点 %s %s", remote, action, saved.ID, saved.IP)
	t, _ := s.Topo.Snapshot()
	return map[string]interface{}{"action": action, "device": t.FindNode(saved.ID), "attach_label": t.AttachLabel(saved.Attach)}, nil
}

func removeFlag(flags []string, f string) []string {
	out := flags[:0]
	for _, x := range flags {
		if x != f {
			out = append(out, x)
		}
	}
	return out
}

func toolDeleteDevice(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	var deleted string
	err := s.Topo.Update(func(t *topology.Topology) error {
		kind, id, err := resolveDevice(t, argStr(a, "id"), argStr(a, "ip"))
		if err != nil {
			return err
		}
		if kind == "switch" {
			return fmt.Errorf("%s 是交换机，请使用 delete_switch", id)
		}
		t.DeleteNode(id)
		deleted = id
		return nil
	})
	if err != nil {
		return nil, err
	}
	log.Printf("MCP(%s) 删除节点 %s", remote, deleted)
	return map[string]interface{}{"ok": true, "deleted": deleted}, nil
}

func toolUpsertSwitch(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	id := argStr(a, "id")
	var action, savedID string
	err := s.Topo.Update(func(t *topology.Topology) error {
		if cur := t.FindSwitch(id); id != "" && cur != nil {
			var sw topology.Switch
			if err := mergePatch(cur, a, &sw); err != nil {
				return err
			}
			sw.ID = cur.ID
			*cur = sw
			action, savedID = "updated", sw.ID
			return nil
		}
		if id != "" && t.FindNode(id) != nil {
			return fmt.Errorf("%s 是终端节点，请使用 upsert_device", id)
		}
		if argStr(a, "name") == "" {
			return errors.New("新建交换机需要提供 name")
		}
		var sw topology.Switch
		if err := mergePatch(&topology.Switch{}, a, &sw); err != nil {
			return err
		}
		if sw.ID == "" {
			sw.ID = t.NewSwitchID(&sw)
		}
		if sw.Position == nil {
			sw.Position = freeSpot(t)
		}
		t.Switches = append(t.Switches, &sw)
		action, savedID = "created", sw.ID
		return nil
	})
	if err != nil {
		return nil, err
	}
	log.Printf("MCP(%s) %s 交换机 %s", remote, action, savedID)
	t, _ := s.Topo.Snapshot()
	return map[string]interface{}{"action": action, "switch": t.FindSwitch(savedID)}, nil
}

func toolDeleteSwitch(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	id := argStr(a, "id")
	detached := 0
	err := s.Topo.Update(func(t *topology.Topology) error {
		n, ok := t.DeleteSwitch(id)
		if !ok {
			return fmt.Errorf("交换机 %q 不存在", id)
		}
		detached = n
		return nil
	})
	if err != nil {
		return nil, err
	}
	log.Printf("MCP(%s) 删除交换机 %s，%d 个节点变为未定位", remote, id, detached)
	return map[string]interface{}{"ok": true, "deleted": id, "detached_nodes": detached}, nil
}

func toolUpsertLink(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	id := argStr(a, "id")
	var action string
	var saved topology.Link
	err := s.Topo.Update(func(t *topology.Topology) error {
		if cur := t.FindLink(id); id != "" && cur != nil {
			var l topology.Link
			if err := mergePatch(cur, a, &l); err != nil {
				return err
			}
			l.ID = cur.ID
			*cur = l
			action, saved = "updated", l
			return nil
		}
		if _, ok := a["from"]; !ok {
			return errors.New("新建链路需要提供 from 和 to")
		}
		var l topology.Link
		if err := mergePatch(&topology.Link{}, a, &l); err != nil {
			return err
		}
		t.Links = append(t.Links, &l)
		action, saved = "created", l
		return nil
	})
	if err != nil {
		return nil, err
	}
	log.Printf("MCP(%s) %s 链路 %s:%s -> %s:%s", remote, action, saved.From.Switch, saved.From.Port, saved.To.Switch, saved.To.Port)
	return map[string]interface{}{"action": action, "link": saved}, nil
}

func toolDeleteLink(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	id := argStr(a, "id")
	err := s.Topo.Update(func(t *topology.Topology) error {
		if !t.DeleteLink(id) {
			return fmt.Errorf("链路 %q 不存在", id)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	log.Printf("MCP(%s) 删除链路 %s", remote, id)
	return map[string]interface{}{"ok": true, "deleted": id}, nil
}

func toolSetAlerting(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	enabled, ok := argBool(a, "enabled")
	if !ok {
		return nil, errors.New("需要提供 enabled（true/false）")
	}
	ids := argStrings(a, "ids")
	ips := argStrings(a, "ips")
	sw := argStr(a, "switch")
	if len(ids) == 0 && len(ips) == 0 && sw == "" {
		return nil, errors.New("需要提供 ids、ips 或 switch 之一")
	}
	var changed int
	var missing []string
	err := s.Topo.Update(func(t *topology.Topology) error {
		all := append([]string{}, ids...)
		for _, ip := range ips {
			if _, id := t.FindByIP(ip); id != "" {
				all = append(all, id)
			} else {
				missing = append(missing, ip)
			}
		}
		if sw != "" {
			if t.FindSwitch(sw) == nil {
				return fmt.Errorf("交换机 %q 不存在", sw)
			}
			for _, n := range t.Nodes {
				if n.Attach.Switch == sw && n.IP != "" {
					all = append(all, n.ID)
				}
			}
		}
		var miss []string
		changed, miss = t.SetAlert(all, enabled)
		missing = append(missing, miss...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	log.Printf("MCP(%s) %s告警：%d 台设备", remote, map[bool]string{true: "开启", false: "关闭"}[enabled], changed)
	return map[string]interface{}{"ok": true, "enabled": enabled, "changed": changed, "not_found": missing}, nil
}

func toolSetSchedule(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	var sch config.Schedule
	raw, _ := json.Marshal(a)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sch); err != nil {
		return nil, fmt.Errorf("参数无效: %v", err)
	}
	c := s.Cfg.Get()
	c.Schedule = sch
	if err := s.Cfg.Update(c); err != nil {
		return nil, err
	}
	log.Printf("MCP(%s) 设置监控时段：%s", remote, sch.Describe())
	return map[string]interface{}{"ok": true, "schedule": s.Cfg.Get().Schedule.Describe(), "in_schedule_now": s.Mon.InSchedule()}, nil
}

func toolSetPaused(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	p, ok := argBool(a, "paused")
	if !ok {
		return nil, errors.New("需要提供 paused（true/false）")
	}
	s.Mon.SetPaused(p)
	if s.OnPauseChange != nil {
		s.OnPauseChange(p)
	}
	log.Printf("MCP(%s) %s监控", remote, map[bool]string{true: "手动暂停", false: "恢复"}[p])
	return map[string]interface{}{"ok": true, "manual_paused": p, "pause_reason": s.Mon.PauseReason()}, nil
}

func toolReplaceYAML(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	text := argStr(a, "yaml")
	if text == "" {
		return nil, errors.New("yaml 不能为空")
	}
	dry := true
	if v, ok := argBool(a, "dry_run"); ok {
		dry = v
	}
	next, err := topology.Parse([]byte(text))
	if err != nil {
		return nil, err
	}
	cur, _ := s.Topo.Snapshot()
	diff := topologyDiff(cur, next)
	res := map[string]interface{}{
		"valid": true, "dry_run": dry,
		"switches": len(next.Switches), "links": len(next.Links), "nodes": len(next.Nodes),
		"diff": diff,
	}
	if dry {
		res["next_step"] = "校验通过。向用户确认差异后，用 dry_run=false 再次调用以写入。"
		return res, nil
	}
	if err := s.Topo.Replace(next); err != nil {
		return nil, err
	}
	log.Printf("MCP(%s) 整体替换拓扑：%d 交换机 %d 节点", remote, len(next.Switches), len(next.Nodes))
	res["written"] = true
	return res, nil
}

// topologyDiff 按 id 比较两份拓扑。
func topologyDiff(a, b *topology.Topology) map[string]interface{} {
	idx := func(t *topology.Topology) map[string]string {
		m := map[string]string{}
		for _, x := range t.Switches {
			j, _ := json.Marshal(x)
			m["switch:"+x.ID] = string(j)
		}
		for _, x := range t.Nodes {
			j, _ := json.Marshal(x)
			m["node:"+x.ID] = string(j)
		}
		for _, x := range t.Links {
			j, _ := json.Marshal(x)
			m["link:"+x.ID] = string(j)
		}
		return m
	}
	ma, mb := idx(a), idx(b)
	var added, removed, changed []string
	for k, v := range mb {
		if old, ok := ma[k]; !ok {
			added = append(added, k)
		} else if old != v {
			changed = append(changed, k)
		}
	}
	for k := range ma {
		if _, ok := mb[k]; !ok {
			removed = append(removed, k)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)
	return map[string]interface{}{"added": added, "removed": removed, "changed": changed, "site_changed": a.Site != b.Site}
}

// ---------------------------------------------------------------------------
// 资源与提示词

type mcpResource struct {
	uri, name, title, desc, mime string
	read                         func() (string, error)
}

func (s *Server) mcpResources() []mcpResource {
	return []mcpResource{
		{"netping://topology.yaml", "topology.yaml", "当前拓扑文件", "当前生效的 topology.yaml（含字段说明注释）", "text/yaml", func() (string, error) {
			b, err := s.Topo.YAML()
			return string(b), err
		}},
		{"netping://docs/topology-fields.md", "topology-fields.md", "拓扑字段说明", "topology.yaml 全部字段说明、示例与迁移机房的方法", "text/markdown", func() (string, error) {
			return string(s.DocsMD), nil
		}},
		{"netping://schema/topology.schema.json", "topology.schema.json", "拓扑 JSON Schema", "topology.yaml 的 JSON Schema", "application/schema+json", func() (string, error) {
			return string(s.SchemaJSON), nil
		}},
		{"netping://docs/plugin-development.md", "plugin-development.md", "插件开发指南", "netPing 插件的目录结构、plugin.yaml 清单、标准输入输出 JSON 协议、各钩子的数据格式与示例，可据此编写插件", "text/markdown", func() (string, error) {
			return string(s.PluginDocMD), nil
		}},
		{"netping://status", "status", "实时状态", "所有设备的实时状态（与 list_devices 相同的数据）", "application/json", func() (string, error) {
			t, _ := s.Topo.Snapshot()
			b, err := json.MarshalIndent(buildRows(t, s.statusMap()), "", "  ")
			return string(b), err
		}},
	}
}

type mcpPrompt struct {
	name, title, desc string
	args              []map[string]interface{}
	build             func(s *Server, a map[string]string) string
}

func promptArg(name, desc string, required bool) map[string]interface{} {
	return map[string]interface{}{"name": name, "description": desc, "required": required}
}

var mcpPrompts = []mcpPrompt{
	{
		name: "generate_topology", title: "根据 MAC 表 / ARP 表生成机房拓扑",
		desc: "迁移到新机房时，根据交换机 MAC 地址表和 ARP 表生成 topology.yaml 并导入",
		args: []map[string]interface{}{
			promptArg("mac_table", "交换机 MAC 地址表（如 show mac-address-table 输出，多台交换机请注明各自名称和上联口）", true),
			promptArg("arp_table", "ARP 表（如 show arp 输出）", true),
			promptArg("site_name", "机房名称", false),
			promptArg("alert_devices", "需要开启告警的设备说明（如：网关和所有服务器）", false),
		},
		build: func(s *Server, a map[string]string) string {
			return fmt.Sprintf(`请为 netPing 生成新机房的 topology.yaml 并导入。

字段说明如下（也可读取资源 netping://docs/topology-fields.md）：
-----
%s
-----

要求：
1. 每台交换机一个 switches 条目；核心交换机 role: core、node_side: left、node_columns: 1；接入交换机 role: access。
2. 交换机之间的上联口写入 links，from 为上游（靠近核心）。
3. 用 MAC 表确定每个 MAC 的接入交换机和端口（忽略上联口学到的 MAC），用 ARP 表把 MAC 对应到 IP，写成 nodes，attach 填交换机 id 和端口。
4. ARP 中有、MAC 表中没有的 IP：attach.switch 留空，flags: [unlocated, arp_only]。
5. 按 MAC 前缀判断厂商填 vendor；本地管理位 MAC 写“本地私有地址”。默认网关加 flags: [gateway]。
6. 设备默认不告警；需要告警的设备加 monitor: {alert: true}。需要告警的设备：%s
7. id 与 position 留空；纯数字端口加引号。
8. 生成后先调用 replace_topology_yaml（dry_run=true）校验，如有问题修正后重试；把差异（新增/删除/修改）汇总给我确认后，再用 dry_run=false 写入。

机房名称：%s

MAC 地址表：
%s

ARP 表：
%s`, string(s.DocsMD), orDefault(a["alert_devices"], "（未指定，全部不开启）"), orDefault(a["site_name"], "（请根据数据命名）"), a["mac_table"], a["arp_table"])
		},
	},
	{
		name: "health_report", title: "网络健康报告",
		desc: "汇总当前异常设备和一段时间内的丢包/延迟情况，生成中文报告",
		args: []map[string]interface{}{
			promptArg("range", "统计范围：1h、6h、24h（默认）、7d、30d", false),
		},
		build: func(s *Server, a map[string]string) string {
			rg := orDefault(a["range"], "24h")
			return fmt.Sprintf(`请使用 netPing 的工具生成一份机房网络健康报告（统计范围 %s）：
1. 调用 get_overview 了解总体状态、监控时段和当前异常设备。
2. 调用 list_devices(sort="loss", limit=10) 找出近 1 小时丢包最多的设备，对其中丢包率大于 0 的设备以及所有当前异常设备调用 get_history(range="%s", points=0) 获取统计。
3. 调用 list_alerts(limit=50) 查看该时间段内的告警。
4. 按接线位置（交换机/端口）归纳问题：同一交换机或同一端口下多台设备同时异常，说明可能是交换机、上联或口后集线器问题。
5. 输出：总体结论、异常设备表（IP、名称、位置、丢包率、平均/最大延迟）、可能原因与建议；如发现重要设备未开启告警，一并提示。`, rg, rg)
		},
	},
	{
		name: "discover_devices", title: "自主发现设备",
		desc: "由 AI 自己找网段、探测、识别新设备并查出接线端口，确认后添加到拓扑",
		args: []map[string]interface{}{
			promptArg("focus", "重点（可空），如：二楼的摄像头、打印机；有没有私接路由器", false),
			promptArg("subnets", "指定要探测的网段（可空，逗号分隔）", false),
		},
		build: func(s *Server, a map[string]string) string {
			return fmt.Sprintf(`请用 netPing 的工具自主发现网络中还没有登记的设备：
1. 调用 get_network_context，根据本机网卡、ARP 分组、拓扑分组和 candidate_subnets 决定要探测的网段（有依据，不要盲目遍历）。指定网段：%s
2. 用 probe_subnet 只读探测（每次最多 4 个 /24），只关注 status=new 的地址。
3. 用 fingerprint_host（每次最多 8 个）识别值得识别的新设备；有 MAC 的用 locate_device 查接在哪个交换机端口。
4. 把结果整理成表格给我确认：IP、MAC、厂商、建议名称、类别、依据、接线、是否建议告警；以及建议新增的扫描任务和其他发现（IP 冲突、私接路由器等）。
5. 我确认后，再用 upsert_device 逐台添加（有端口的填 attach，类别写成标签），用 upsert_scan_job 新建扫描任务。确认前不要做任何修改。
重点：%s`, orDefault(a["subnets"], "（无，由你决定）"), orDefault(a["focus"], "（无，全面发现）"))
		},
	},
}

func orDefault(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return v
}

// ---------------------------------------------------------------------------
// 供网页「MCP」页展示

func (s *Server) handleMCPInfo(w http.ResponseWriter) {
	c := s.Cfg.Get().MCP
	var tools []map[string]interface{}
	for _, t := range mcpTools {
		tools = append(tools, map[string]interface{}{"name": t.Name, "title": t.Title, "description": t.Description, "write": t.Write, "destructive": t.Destructive, "probe": t.OpenWorld || probeToolNames[t.Name]})
	}
	for _, t := range s.pluginTools() {
		tools = append(tools, map[string]interface{}{"name": t.Name, "title": t.Title, "description": t.Description, "write": t.Write, "destructive": false, "probe": !t.Write, "plugin": true})
	}
	var prompts []map[string]interface{}
	for _, p := range mcpPrompts {
		prompts = append(prompts, map[string]interface{}{"name": p.name, "title": p.title, "description": p.desc})
	}
	var res []map[string]interface{}
	for _, r := range s.mcpResources() {
		res = append(res, map[string]interface{}{"uri": r.uri, "title": r.title, "description": r.desc})
	}
	writeJSON(w, map[string]interface{}{
		"enabled": c.Enabled, "read_only": c.ReadOnly, "token_set": c.Token != "",
		"auth_basic": s.Cfg.Get().Auth.HasPassword(),
		"path":       "/mcp", "protocol": mcpLatestProtocol, "server_name": "netPing", "version": Version,
		"tools": tools, "prompts": prompts, "resources": res,
	})
}

// ---------------------------------------------------------------------------
// 网段扫描

func discoverySummary(r *discovery.ScanResult, includeHosts bool) map[string]interface{} {
	if r == nil {
		return map[string]interface{}{"note": "还没有扫描结果，可调用 scan_network"}
	}
	counts := map[string]int{}
	for _, c := range r.Changes {
		counts[c.Type]++
	}
	out := map[string]interface{}{
		"job": r.Job, "job_name": r.JobName,
		"started": r.Started.Format("2006-01-02 15:04:05"), "duration_ms": r.DurationMs, "reason": r.Reason,
		"subnets": r.Subnets, "scanned": r.Scanned, "alive": r.Alive, "ping_alive": r.PingAlive,
		"change_counts": counts, "changes": r.Changes, "suggestions": r.Suggestions, "unknown": r.Unknown,
	}
	if r.Error != "" {
		out["error"] = r.Error
	}
	if r.ARPError != "" {
		out["arp_error"] = r.ARPError
	}
	if len(r.ProxyMACs) > 0 {
		out["proxy_arp_macs"] = r.ProxyMACs
	}
	if includeHosts {
		out["hosts"] = r.Hosts
	}
	return out
}

func toolGetDiscovery(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	inc, _ := argBool(a, "include_hosts")
	res := map[string]interface{}{
		"status":        s.Disc.Status(),
		"suggestions":   s.Disc.Suggestions(),
		"unknown":       s.Disc.Unknown(),
		"recent_events": s.Disc.Events(argInt(a, "events", 30)),
		"change_types":  "added 自动添加 / ip_changed 按 MAC 更新 IP / ip_released 原 IP 被占用已清空 / mac_learned 补全 MAC / conflict 冲突未处理",
	}
	results := s.Disc.Results()
	if job := argStr(a, "job"); job != "" {
		r, ok := results[job]
		if !ok {
			return nil, fmt.Errorf("任务 %q 还没有扫描结果", job)
		}
		res["result"] = discoverySummary(r, inc)
		return res, nil
	}
	per := map[string]interface{}{}
	for k, r := range results {
		per[k] = discoverySummary(r, inc)
	}
	res["results"] = per
	return res, nil
}

func toolScanNetwork(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	if subnets := argStrings(a, "subnets"); len(subnets) > 0 {
		autoAdd, ok := argBool(a, "auto_add")
		if !ok {
			autoAdd = true
		}
		log.Printf("MCP(%s) 临时扫描 %v（自动添加 %v）", remote, subnets, autoAdd)
		r, err := s.Disc.ScanAdhoc(subnets, autoAdd, "MCP 临时扫描")
		if err != nil {
			return nil, err
		}
		return discoverySummary(r, false), nil
	}
	var ids []string
	if job := argStr(a, "job"); job != "" {
		ids = []string{job}
	}
	log.Printf("MCP(%s) 触发扫描任务 %v", remote, ids)
	rs, err := s.Disc.ScanJobs(ids, "MCP 触发")
	if err != nil {
		return nil, err
	}
	out := []interface{}{}
	for _, r := range rs {
		out = append(out, discoverySummary(r, false))
	}
	return map[string]interface{}{"results": out}, nil
}

// updateConfig 读取当前配置、修改并保存。
func (s *Server) updateConfig(fn func(c *config.Config) error) (config.Config, error) {
	c := s.Cfg.Get()
	if err := fn(&c); err != nil {
		return c, err
	}
	if err := s.Cfg.Update(c); err != nil {
		return c, err
	}
	return s.Cfg.Get(), nil
}

// patchStruct 把 a 中提供的字段覆盖到 *ptr 上（未提供的保持不变，未知字段报错）。
func patchStruct(ptr interface{}, a map[string]json.RawMessage, skip ...string) error {
	p := map[string]json.RawMessage{}
	for k, v := range a {
		p[k] = v
	}
	for _, k := range skip {
		delete(p, k)
	}
	return mergePatch(ptr, p, ptr)
}

func toolUpsertScanJob(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	id := argStr(a, "id")
	var action string
	var saved config.ScanJob
	c, err := s.updateConfig(func(c *config.Config) error {
		jobs := c.Discovery.Jobs
		idx := -1
		for i, j := range jobs {
			if id != "" && j.ID == id {
				idx = i
			}
		}
		var j config.ScanJob
		if idx >= 0 {
			j = jobs[idx]
			action = "updated"
		} else {
			j = config.DefaultJob()
			j.ID, j.Name, j.Subnets = id, "", nil
			action = "created"
			if _, ok := a["subnets"]; !ok {
				return errors.New("新建扫描任务需要提供 subnets（一个或多个 CIDR 网段）")
			}
		}
		if err := patchStruct(&j, a, "run_now"); err != nil {
			return err
		}
		if idx >= 0 {
			jobs[idx] = j
		} else {
			jobs = append(jobs, j)
		}
		c.Discovery.Jobs = jobs
		return nil
	})
	if err != nil {
		return nil, err
	}
	// 新建时 id 可能是自动生成的：按顺序取回
	for _, j := range c.Discovery.Jobs {
		if (id != "" && j.ID == id) || (id == "" && action == "created") {
			saved = j
		}
	}
	log.Printf("MCP(%s) %s 扫描任务 %s %v", remote, action, saved.ID, saved.Subnets)
	out := map[string]interface{}{"action": action, "job": saved, "discovery_enabled": c.Discovery.Enabled}
	if !c.Discovery.Enabled {
		out["note"] = "自动扫描总开关已关闭，任务不会定时运行；可用 set_discovery_settings 打开"
	}
	if run, _ := argBool(a, "run_now"); run {
		rs, err := s.Disc.ScanJobs([]string{saved.ID}, "MCP 新建/修改任务后立即扫描")
		if err != nil {
			out["scan_error"] = err.Error()
		} else if len(rs) > 0 {
			out["scan"] = discoverySummary(rs[0], false)
		}
	}
	return out, nil
}

func toolDeleteScanJob(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	id := argStr(a, "id")
	_, err := s.updateConfig(func(c *config.Config) error {
		jobs := c.Discovery.Jobs[:0]
		found := false
		for _, j := range c.Discovery.Jobs {
			if j.ID == id {
				found = true
				continue
			}
			jobs = append(jobs, j)
		}
		if !found {
			return fmt.Errorf("扫描任务 %q 不存在", id)
		}
		c.Discovery.Jobs = jobs
		return nil
	})
	if err != nil {
		return nil, err
	}
	log.Printf("MCP(%s) 删除扫描任务 %s", remote, id)
	return map[string]interface{}{"ok": true, "deleted": id}, nil
}

func toolSetDiscoverySettings(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	if _, ok := a["jobs"]; ok {
		return nil, errors.New("扫描任务请用 upsert_scan_job / delete_scan_job 修改")
	}
	c, err := s.updateConfig(func(c *config.Config) error {
		if raw, ok := a["oui"]; ok { // oui 整体替换，不做合并
			var m map[string]string
			if err := json.Unmarshal(raw, &m); err != nil {
				return fmt.Errorf("oui 格式应为 {\"AA:BB:CC\": \"厂商\"}")
			}
			c.Discovery.OUI = m
		}
		return patchStruct(&c.Discovery, a, "oui")
	})
	if err != nil {
		return nil, err
	}
	log.Printf("MCP(%s) 修改扫描设置", remote)
	return map[string]interface{}{"ok": true, "discovery": c.Discovery}, nil
}

func toolAddDiscovered(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	ips := argStrings(a, "ips")
	if all, _ := argBool(a, "all"); all {
		ips = nil
		for _, h := range s.Disc.Unknown() {
			ips = append(ips, h.IP)
		}
	}
	if len(ips) == 0 {
		return nil, errors.New("需要提供 ips，或 all=true 添加全部未登记设备")
	}
	added, err := s.Disc.AddUnknown(ips)
	if err != nil {
		return nil, err
	}
	log.Printf("MCP(%s) 添加扫描发现的设备 %d 台", remote, len(added))
	return map[string]interface{}{"ok": true, "added": added, "count": len(added)}, nil
}

func toolApplySuggestion(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	id, ip := argStr(a, "node_id"), argStr(a, "ip")
	var sg *discovery.Suggestion
	for _, x := range s.Disc.Suggestions() {
		x := x
		if x.NodeID == id {
			sg = &x
		}
	}
	if ip == "" {
		if sg == nil {
			return nil, fmt.Errorf("节点 %q 没有待确认的 IP 变化，请提供 ip", id)
		}
		ip = sg.SeenIP
	}
	mode := "-"
	if raw, ok := a["ip_mode"]; ok {
		if err := json.Unmarshal(raw, &mode); err != nil {
			return nil, errors.New("ip_mode 只能是 dhcp、static 或空字符串")
		}
		if mode != "" && mode != "dhcp" && mode != "static" {
			return nil, errors.New("ip_mode 只能是 dhcp、static 或空字符串")
		}
	}
	ch, err := s.Disc.ApplySuggestion(id, ip, mode)
	if err != nil {
		return nil, err
	}
	log.Printf("MCP(%s) 采纳 IP 变化 %s %s → %s", remote, id, ch.OldIP, ip)
	return map[string]interface{}{"ok": true, "change": ch}, nil
}

func toolSetAlertConfig(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	c, err := s.updateConfig(func(c *config.Config) error { return patchStruct(&c.Alert, a) })
	if err != nil {
		return nil, err
	}
	log.Printf("MCP(%s) 修改告警设置", remote)
	al := c.Alert
	al = maskAlertChannels(al)
	return map[string]interface{}{"ok": true, "alert": al}, nil
}

func toolSendTestAlert(s *Server, _ map[string]json.RawMessage, remote string) (interface{}, error) {
	if err := s.Alerts.SendTest(s.Cfg.Get().Alert); err != nil {
		return nil, err
	}
	log.Printf("MCP(%s) 发送测试告警", remote)
	return map[string]interface{}{"ok": true, "note": "测试消息已发送到所有已配置渠道"}, nil
}

func toolSetPingDefaults(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	c, err := s.updateConfig(func(c *config.Config) error {
		if raw, ok := a["retention_days"]; ok {
			if err := json.Unmarshal(raw, &c.RetentionDays); err != nil || c.RetentionDays < 1 {
				return errors.New("retention_days 应为大于 0 的整数")
			}
		}
		return patchStruct(&c.Ping, a, "retention_days")
	})
	if err != nil {
		return nil, err
	}
	log.Printf("MCP(%s) 修改默认 ping 参数", remote)
	return map[string]interface{}{"ok": true, "ping_defaults": c.Ping, "retention_days": c.RetentionDays}, nil
}

func toolSetSite(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	var site topology.Site
	err := s.Topo.Update(func(t *topology.Topology) error {
		site = t.Site
		if err := patchStruct(&site, a); err != nil {
			return err
		}
		t.Site = site
		return nil
	})
	if err != nil {
		return nil, err
	}
	log.Printf("MCP(%s) 修改机房信息 %s %s", remote, site.Name, site.Subnet)
	return map[string]interface{}{"ok": true, "site": site}, nil
}

func toolResetLayout(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	sw := argStr(a, "switch")
	count := 0
	err := s.Topo.Update(func(t *topology.Topology) error {
		if sw != "" && t.FindSwitch(sw) == nil {
			return fmt.Errorf("交换机 %q 不存在", sw)
		}
		for _, n := range t.Nodes {
			if n.Position != nil && (sw == "" || n.Attach.Switch == sw) {
				n.Position = nil
				count++
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	log.Printf("MCP(%s) 重置 %d 个节点为自动排布", remote, count)
	return map[string]interface{}{"ok": true, "reset": count}, nil
}

// ---------------------------------------------------------------------------
// 端口定位

func maskLocate(l config.Locate) map[string]interface{} {
	m := func(c string) string {
		if c == "" {
			return ""
		}
		return "（已设置）"
	}
	sw := map[string]interface{}{}
	for k, v := range l.Switches {
		sw[k] = map[string]interface{}{"host": v.Host, "community": m(v.Community), "disabled": v.Disabled}
	}
	return map[string]interface{}{"enabled": l.Enabled, "interval_min": l.IntervalMin, "community": m(l.Community),
		"timeout_ms": l.TimeoutMs, "notify": l.Notify, "switches": sw}
}

func toolLocateStatus(s *Server, _ map[string]json.RawMessage, _ string) (interface{}, error) {
	st := s.locateStatus()
	st["settings"] = maskLocate(s.Cfg.Get().Locate)
	if len(s.Loc.Targets()) == 0 {
		st["note"] = locate.ErrNoTargets.Error()
	}
	return st, nil
}

func toolSwitchMACTable(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	refresh, ok := argBool(a, "refresh")
	if !ok {
		refresh = true
	}
	rows, st, err := s.Loc.MACTable(argStr(a, "switch"), refresh)
	if err != nil {
		return nil, err
	}
	if port := argStr(a, "port"); port != "" {
		var keep []locate.PortRow
		for _, r := range rows {
			if strings.EqualFold(r.Port, port) || strings.EqualFold(r.IfName, port) {
				keep = append(keep, r)
			}
		}
		rows = keep
	}
	return map[string]interface{}{"switch": argStr(a, "switch"), "ports": rows, "read": st}, nil
}

func toolLocateDevice(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	mac := strings.ToUpper(argStr(a, "mac"))
	t, _ := s.Topo.Snapshot()
	var node *topology.Node
	if argStr(a, "id") != "" || argStr(a, "ip") != "" {
		kind, id, err := resolveDevice(t, argStr(a, "id"), argStr(a, "ip"))
		if err != nil {
			return nil, err
		}
		if kind != "node" {
			return nil, errors.New("只能查询终端节点")
		}
		node = t.FindNode(id)
		mac = strings.ToUpper(node.MAC)
		if mac == "" {
			return nil, fmt.Errorf("节点 %s 没有登记 MAC，无法按 MAC 表定位（可先运行网段扫描补全 MAC）", node.ID)
		}
	}
	if mac == "" {
		return nil, errors.New("需要提供 id、ip 或 mac")
	}
	refresh, ok := argBool(a, "refresh")
	if !ok {
		refresh = true
	}
	p, seen, why, err := s.Loc.Find(mac, refresh)
	if err != nil {
		return nil, err
	}
	out := map[string]interface{}{"mac": mac, "seen": seen}
	if seen == nil {
		out["seen"] = []interface{}{}
	}
	if node != nil {
		out["node_id"] = node.ID
		out["registered"] = t.AttachLabel(node.Attach)
	}
	if p == nil {
		out["located"] = false
		out["reason"] = why
		return out, nil
	}
	ep := topology.Endpoint{Switch: p.Switch, Port: p.Port}
	out["located"] = true
	out["switch"], out["port"], out["if_name"], out["label"] = p.Switch, p.Port, p.IfName, t.AttachLabel(ep)
	out["port_macs"] = p.PortMACs
	if sw := t.FindSwitch(p.Switch); sw != nil && sw.PortLocations[p.Port] != "" {
		out["port_location"] = sw.PortLocations[p.Port]
	}
	if node != nil {
		out["matches_registered"] = node.Attach.Switch == p.Switch && strings.EqualFold(node.Attach.Port, p.Port)
	}
	return out, nil
}

func toolAutoLocate(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	dry, _ := argBool(a, "dry_run")
	moves, _ := argBool(a, "apply_moves")
	r, err := s.Loc.RunNow("MCP 触发", dry, moves)
	if err != nil {
		return nil, err
	}
	log.Printf("MCP(%s) 端口定位（预览 %v，含端口变化 %v）：%d 项修改", remote, dry, moves, len(r.Changes))
	return r, nil
}

func toolApplyLocation(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	t, _ := s.Topo.Snapshot()
	kind, id, err := resolveDevice(t, argStr(a, "id"), argStr(a, "ip"))
	if err != nil {
		return nil, err
	}
	if kind != "node" {
		return nil, errors.New("只能修改终端节点")
	}
	r, err := s.Loc.ApplyMove(id)
	if err != nil {
		return nil, err
	}
	if len(r.Changes) == 0 {
		return map[string]interface{}{"ok": false, "note": "MAC 表中的位置与登记一致，或无法确定位置，未修改", "unresolved": r.Unresolved}, nil
	}
	log.Printf("MCP(%s) 采纳端口变化 %s", remote, id)
	return map[string]interface{}{"ok": true, "changes": r.Changes}, nil
}

func toolTestSNMP(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	host, comm, id := argStr(a, "host"), argStr(a, "community"), argStr(a, "switch")
	if id != "" {
		c := s.Cfg.Get().Locate
		o := c.Switches[id]
		t, _ := s.Topo.Snapshot()
		sw := t.FindSwitch(id)
		if sw == nil {
			return nil, fmt.Errorf("交换机 %q 不存在", id)
		}
		if host == "" {
			host = o.Host
			if host == "" {
				host = sw.IP
			}
		}
		if comm == "" {
			comm = o.Community
			if comm == "" {
				comm = c.Community
			}
		}
	}
	if host == "" {
		return nil, errors.New("没有读取地址：交换机未填写管理 IP，请提供 host")
	}
	if comm == "" {
		return nil, errors.New("没有团体名：请先用 set_locate_settings 设置，或提供 community")
	}
	return s.Loc.Test(host, comm)
}

func toolSetLocateSettings(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	c, err := s.updateConfig(func(c *config.Config) error {
		if raw, ok := a["switches"]; ok {
			var m map[string]map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				return errors.New("switches 格式应为 {\"交换机id\": {\"host\": …, \"community\": …, \"disabled\": …}}")
			}
			t, _ := s.Topo.Snapshot()
			for id, patch := range m {
				if t.FindSwitch(id) == nil {
					return fmt.Errorf("交换机 %q 不存在", id)
				}
				cur := c.Locate.Switches[id]
				if err := patchStruct(&cur, patch); err != nil {
					return err
				}
				c.Locate.Switches[id] = cur
			}
		}
		rest := map[string]json.RawMessage{}
		for k, v := range a {
			if k != "switches" {
				rest[k] = v
			}
		}
		sw := c.Locate.Switches
		if err := patchStruct(&c.Locate, rest); err != nil {
			return err
		}
		c.Locate.Switches = sw
		return nil
	})
	if err != nil {
		return nil, err
	}
	log.Printf("MCP(%s) 修改端口定位设置", remote)
	out := map[string]interface{}{"ok": true, "locate": maskLocate(c.Locate), "targets": len(s.Loc.Targets())}
	if len(s.Loc.Targets()) == 0 {
		out["note"] = locate.ErrNoTargets.Error()
	}
	return out, nil
}

func maskAlertChannels(a config.AlertConfig) config.AlertConfig {
	a.WebhookURL = maskWebhook(a.WebhookURL)
	a.DingTalkURL = maskWebhook(a.DingTalkURL)
	a.FeishuURL = maskWebhook(a.FeishuURL)
	a.SlackURL = maskWebhook(a.SlackURL)
	a.GenericWebhookURL = maskWebhook(a.GenericWebhookURL)
	a.DingTalkSecret = maskWebhook(a.DingTalkSecret)
	a.FeishuSecret = maskWebhook(a.FeishuSecret)
	return a
}
