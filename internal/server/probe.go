package server

// 现场核实类只读工具：从 netPing 主机实时探测，用来验证 AI 的判断，而不只看历史数据。
// 包括：交换机端口状态（SNMP）、TCP 端口探测、ARP 核对、路由追踪、DNS 解析。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"netping/internal/discovery"
	"netping/internal/locate"
	"netping/internal/pinger"
	"netping/internal/snmp"
	"netping/internal/topology"
)

func probeTools() []*mcpTool {
	return []*mcpTool{
		{
			Name: "get_switch_ports", Title: "交换机端口状态",
			Description: "通过 SNMP 实时读取交换机端口的管理/链路状态、协商速率、双工、输入输出错误与丢弃计数、CRC 错误、最近一次状态变化时间，以及交换机运行时长（判断是否重启过）；可间隔几秒采样两次，得到采样期间错误计数的增长和带宽占用。用于核实“网线/端口故障、双工不匹配、端口闪断、拥塞、交换机重启”等判断。可指定交换机，或直接给设备 id/ip（自动定位到它接的端口）。需要在端口定位中设置 SNMP 团体名。",
			Schema: obj(withIDIP(map[string]interface{}{
				"switch":     str("交换机 id（与 id/ip 二选一）"),
				"port":       str("只看该端口（拓扑中的端口名或交换机上的接口名，如 12、Gi0/12）"),
				"sample_sec": integer("两次采样的间隔秒数，默认 3（0 只读一次，最大 10）；采样可看出错误计数是否仍在增长和带宽占用"),
				"all":        boolean("列出全部物理端口；默认只列出有异常迹象的端口和接了设备的端口"),
			})),
			Run: toolSwitchPorts,
		},
		{
			Name: "tcp_check", Title: "TCP 端口探测", OpenWorld: true,
			Description: "从 netPing 主机对设备的若干 TCP 端口发起连接，报告每个端口开放、拒绝还是超时及连接耗时。ping 不通时用它核实设备是否其实在线（很多设备、Windows 防火墙默认禁 ping）：只要有端口开放或拒绝连接，就说明主机在线、网络可达；也可用来确认打印机（9100）、摄像头（554/80）、远程桌面（3389）等服务是否正常。",
			Schema: obj(withIDIP(map[string]interface{}{
				"ports":      map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "integer"}, "description": "端口列表，最多 16 个；默认 22,23,80,443,445,554,3389,8000,8080,9100"},
				"timeout_ms": integer("单个端口连接超时毫秒，默认 1500（200~5000）"),
			})),
			Run: toolTCPCheck,
		},
		{
			Name: "arp_lookup", Title: "ARP 核对",
			Description: "读取 netPing 主机的 ARP 表，查看某个 IP 当前对应的 MAC，并与拓扑中登记的 MAC 对比，同时列出同一 MAC 对应的其他 IP。用于核实：ping 不通但设备是否仍回应 ARP（在线但禁 ping）、IP 冲突、设备被更换或 IP 被别的设备占用。只对与 netPing 主机同网段的地址有效。",
			Schema: obj(withIDIP(map[string]interface{}{
				"refresh": boolean("先 ping 一次刷新 ARP 记录，默认 true"),
			})),
			Run: toolARPLookup,
		},
		{
			Name: "trace_route", Title: "路由追踪", OpenWorld: true,
			Description: "从 netPing 主机到目标地址逐跳追踪（类似 tracert），给出每一跳的回应地址、延迟和对应的拓扑设备。用于核实外网或跨网段不通时断在哪一跳（网关、出口路由器还是运营商）。同网段地址只有一跳。",
			Schema: obj(withIDIP(map[string]interface{}{
				"max_hops":   integer("最大跳数，默认 15（最大 30）"),
				"timeout_ms": integer("每跳超时毫秒，默认 1000（200~3000）"),
				"probes":     integer("每跳探测次数，默认 1（最大 3）"),
			})),
			Run: toolTraceRoute,
		},
		{
			Name: "dns_lookup", Title: "DNS 解析", OpenWorld: true,
			Description: "从 netPing 主机解析域名（或对 IP 做反向解析），可指定 DNS 服务器与系统默认解析结果对比，返回解析结果与耗时。用于核实“上不了网/打不开网站”是否是 DNS 问题。",
			Schema: obj(map[string]interface{}{
				"name":       str("要解析的域名，或要反向解析的 IPv4 地址"),
				"server":     str("指定 DNS 服务器 IPv4（可选，如网关或 223.5.5.5）；留空使用系统设置"),
				"timeout_ms": integer("超时毫秒，默认 3000（500~10000）"),
			}, "name"),
			Run: toolDNSLookup,
		},
	}
}

// ---------------------------------------------------------------------------
// 公共：解析探测目标

// probeTarget 按 id 或 ip 得到要探测的 IPv4 地址（以及它在拓扑中的设备 id，可能为空），拒绝广播、组播、全零地址。
func probeTarget(t *topology.Topology, a map[string]json.RawMessage) (id, ip string, err error) {
	id, ip = argStr(a, "id"), argStr(a, "ip")
	if id != "" {
		_, rid, err := resolveDevice(t, id, "")
		if err != nil {
			return "", "", err
		}
		id = rid
		_, ip, _ = deviceLabel(t, id)
		if ip == "" {
			return "", "", errors.New("该设备没有 IP，无法探测")
		}
	} else if ip == "" {
		return "", "", errors.New("需要提供 id 或 ip")
	} else if _, rid := t.FindByIP(ip); rid != "" {
		id = rid
	}
	p4 := net.ParseIP(ip).To4()
	if p4 == nil {
		return "", "", fmt.Errorf("%q 不是 IPv4 地址", ip)
	}
	if p4.IsUnspecified() || p4.IsMulticast() || p4.Equal(net.IPv4bcast) {
		return "", "", errors.New("不能探测广播、组播或全零地址")
	}
	if _, nw, err := net.ParseCIDR(t.Site.Subnet); err == nil && len(nw.Mask) == 4 {
		bc := make(net.IP, 4)
		for i := range bc {
			bc[i] = nw.IP.To4()[i] | ^nw.Mask[i]
		}
		if p4.Equal(bc) {
			return "", "", errors.New("不能探测网段广播地址")
		}
	}
	return id, p4.String(), nil
}

// deviceRef 探测结果里附带的设备信息与监控状态。
func (s *Server) deviceRef(t *topology.Topology, id string) interface{} {
	if id == "" {
		return "不在拓扑中的地址"
	}
	name, _, attach := deviceLabel(t, id)
	d := map[string]interface{}{"id": id, "name": name, "attach": attach}
	if st, ok := s.Mon.Status(id); ok {
		d["monitor"] = map[string]interface{}{"state": st.State, "avg_1m_ms": st.Avg1m, "loss_1m_pct": st.Loss1m, "loss_1h_pct": st.Loss1h}
	}
	return d
}

func agoText(sec int64) string {
	switch {
	case sec < 0:
		return ""
	case sec < 90:
		return fmt.Sprintf("%d 秒前", sec)
	case sec < 5400:
		return fmt.Sprintf("%d 分钟前", sec/60)
	case sec < 2*86400:
		return fmt.Sprintf("%.1f 小时前", float64(sec)/3600)
	}
	return fmt.Sprintf("%.1f 天前", float64(sec)/86400)
}

func durText(sec int64) string {
	d, h, m := sec/86400, sec%86400/3600, sec%3600/60
	switch {
	case d > 0:
		return fmt.Sprintf("%d 天 %d 小时", d, h)
	case h > 0:
		return fmt.Sprintf("%d 小时 %d 分钟", h, m)
	}
	return fmt.Sprintf("%d 分钟", m)
}

// ---------------------------------------------------------------------------
// get_switch_ports

var ifStatusText = map[int]string{1: "up", 2: "down", 3: "testing", 4: "unknown", 5: "dormant", 6: "notPresent", 7: "lowerLayerDown"}

func toolSwitchPorts(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	t, _ := s.Topo.Snapshot()
	swID, port := argStr(a, "switch"), argStr(a, "port")
	var focus string
	if argStr(a, "id") != "" || argStr(a, "ip") != "" {
		kind, id, err := resolveDevice(t, argStr(a, "id"), argStr(a, "ip"))
		if err != nil {
			return nil, err
		}
		if kind == "switch" {
			swID = id
		} else {
			n := t.FindNode(id)
			if n.Attach.Switch == "" {
				return nil, fmt.Errorf("设备 %s 未定位到交换机端口，可先用 locate_device 查询实际端口", n.DisplayName())
			}
			swID, focus = n.Attach.Switch, id
			if port == "" {
				port = n.Attach.Port
			}
		}
	}
	if swID == "" {
		return nil, errors.New("需要提供 switch，或设备的 id/ip")
	}
	sw := t.FindSwitch(swID)
	if sw == nil {
		return nil, fmt.Errorf("交换机 %q 不存在", swID)
	}
	sample := clampInt(argInt(a, "sample_sec", 3), 0, 10)
	all, _ := argBool(a, "all")

	// 拓扑端口 → 接在上面的设备 / 链路
	var used []string
	devs := map[string][]map[string]interface{}{}
	sts := s.statusMap()
	for _, n := range t.Nodes {
		if n.Attach.Switch == swID && n.Attach.Port != "" {
			used = append(used, n.Attach.Port)
			d := map[string]interface{}{"id": n.ID, "name": n.DisplayName(), "ip": n.IP}
			if st, ok := sts[n.ID]; ok {
				d["state"] = st.State
			}
			devs[strings.ToLower(n.Attach.Port)] = append(devs[strings.ToLower(n.Attach.Port)], d)
		}
	}
	links := map[string]string{}
	for _, l := range t.Links {
		if l.From.Switch == swID && l.From.Port != "" {
			links[strings.ToLower(l.From.Port)] = "级联到 " + l.To.Switch + " " + l.To.Port
		}
		if l.To.Switch == swID && l.To.Port != "" {
			links[strings.ToLower(l.To.Port)] = "级联到 " + l.From.Switch + " " + l.From.Port
		}
	}
	mapped := map[int]string{}
	keep := func(idx int, name string) bool {
		p := locate.MatchPort(sw, name, used)
		if port != "" {
			if !strings.EqualFold(p, port) && !strings.EqualFold(strings.ReplaceAll(name, " ", ""), strings.ReplaceAll(port, " ", "")) {
				return false
			}
		} else if !locate.IsPhysical(name) && !locate.IsAggregate(name) {
			return false
		}
		mapped[idx] = p
		return true
	}

	c, err := s.Loc.Dial(swID)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	first, err := c.ReadPorts(keep)
	if err != nil {
		return nil, fmt.Errorf("读取交换机端口失败：%v", err)
	}
	if len(first.Order) == 0 {
		if port != "" {
			return nil, fmt.Errorf("交换机上没有找到端口 %q（可不带 port 查看全部端口名）", port)
		}
		return nil, errors.New("交换机没有返回任何物理端口")
	}
	var second *snmp.PortSnapshot
	if sample > 0 {
		sel := map[int]bool{}
		for _, idx := range first.Order {
			sel[idx] = true
		}
		time.Sleep(time.Duration(sample) * time.Second)
		if second, err = c.ReadPorts(func(idx int, _ string) bool { return sel[idx] }); err != nil {
			second = nil // 第二次读取失败时仍返回第一次的结果
		}
	}

	upSec := first.UpTime / 100
	res := map[string]interface{}{
		"switch": map[string]interface{}{"id": sw.ID, "name": sw.Name, "ip": sw.IP, "role": sw.Role},
		"time":   first.At.Format("2006-01-02 15:04:05"),
	}
	var swFlags []string
	if first.UpTime > 0 {
		res["switch"].(map[string]interface{})["uptime"] = durText(upSec)
		res["switch"].(map[string]interface{})["booted_at"] = first.At.Add(-time.Duration(upSec) * time.Second).Format("2006-01-02 15:04")
		if upSec < 86400 {
			swFlags = append(swFlags, fmt.Sprintf("交换机（SNMP 进程）%s前启动过，可能重启/掉电过", durText(upSec)))
		}
	}
	type row struct {
		m     map[string]interface{}
		score int
	}
	var rows []row
	up, down := 0, 0
	for _, idx := range first.Order {
		p := first.Ports[idx]
		name := mapped[idx]
		key := strings.ToLower(name)
		m := map[string]interface{}{"port": name, "if_name": p.Name, "admin": ifStatusText[p.Admin], "oper": ifStatusText[p.Oper]}
		if p.Alias != "" {
			m["alias"] = p.Alias
		}
		if p.SpeedMbps > 0 && p.Oper == 1 {
			m["speed_mbps"] = p.SpeedMbps
		}
		switch p.Duplex {
		case 2:
			m["duplex"] = "half"
		case 3:
			m["duplex"] = "full"
		}
		if d := devs[key]; len(d) > 0 {
			m["devices"] = d
		}
		if l := links[key]; l != "" {
			m["link"] = l
		}
		var flags []string
		score := 0
		switch {
		case p.Admin == 2:
			flags = append(flags, "端口被管理性关闭（shutdown）")
			score += 2
		case p.Oper == 1:
			up++
		default:
			down++
			if len(devs[key]) > 0 || links[key] != "" {
				flags = append(flags, "端口没有链路（对端设备关机、网线断开或松动）")
				score += 3
			}
		}
		if p.Oper == 1 {
			if p.Duplex == 2 {
				flags = append(flags, "半双工运行（常见于双工不匹配，会造成丢包和 CRC 错误）")
				score += 3
			}
			if p.SpeedMbps > 0 && p.SpeedMbps <= 100 {
				flags = append(flags, fmt.Sprintf("只协商到 %dM（网线/水晶头不良时常见，或对端只支持百兆）", p.SpeedMbps))
				score++
			}
		}
		if p.LastChange > 0 && first.UpTime >= p.LastChange {
			ago := (first.UpTime - p.LastChange) / 100
			m["last_change"] = agoText(ago)
			if ago < 3600 {
				flags = append(flags, "最近 "+agoText(ago)+"端口状态变化过（可能闪断、重新协商或刚插拔）")
				score += 2
			}
		}
		cnt := map[string]interface{}{}
		addCnt := func(k string, v int64) {
			if v > 0 {
				cnt[k] = v
			}
		}
		addCnt("in_errors", p.InErrors)
		addCnt("out_errors", p.OutErrors)
		addCnt("in_discards", p.InDiscards)
		addCnt("out_discards", p.OutDiscard)
		addCnt("crc_errors", p.FCSErrors)
		if len(cnt) > 0 {
			m["counters_since_boot"] = cnt
			if p.InErrors+p.OutErrors+p.FCSErrors > 0 && second == nil {
				flags = append(flags, "开机以来有错误包累计（未采样，无法确认是否仍在增长）")
				score++
			}
		}
		if second != nil {
			if q := second.Ports[idx]; q != nil {
				secs := second.At.Sub(first.At).Seconds()
				d32 := func(a, b int64) int64 { // 32 位计数器回绕
					if b < a {
						return b + (1 << 32) - a
					}
					return b - a
				}
				errs := d32(p.InErrors, q.InErrors) + d32(p.OutErrors, q.OutErrors)
				if q.HasFCS && p.HasFCS {
					if f := d32(p.FCSErrors, q.FCSErrors); f > errs {
						errs = f
					}
				}
				disc := d32(p.InDiscards, q.InDiscards) + d32(p.OutDiscard, q.OutDiscard)
				w := map[string]interface{}{"seconds": math.Round(secs*10) / 10}
				if errs > 0 {
					w["new_errors"] = errs
					flags = append(flags, fmt.Sprintf("采样 %.0f 秒内错误包增加 %d 个（链路质量问题：网线、水晶头、光模块或双工不匹配）", secs, errs))
					score += 4
				}
				if disc > 0 {
					w["new_discards"] = disc
					flags = append(flags, fmt.Sprintf("采样 %.0f 秒内丢弃 %d 个包（端口拥塞或缓存不足）", secs, disc))
					score += 2
				}
				if secs > 0 && p.Oper == 1 {
					var din, dout uint64
					if q.InOctets >= p.InOctets {
						din = q.InOctets - p.InOctets
					} else if !q.HCOctets {
						din = q.InOctets + (1 << 32) - p.InOctets
					}
					if q.OutOctets >= p.OutOctets {
						dout = q.OutOctets - p.OutOctets
					} else if !q.HCOctets {
						dout = q.OutOctets + (1 << 32) - p.OutOctets
					}
					inM, outM := float64(din)*8/secs/1e6, float64(dout)*8/secs/1e6
					w["in_mbps"], w["out_mbps"] = r2(inM), r2(outM)
					if p.SpeedMbps > 0 {
						util := math.Max(inM, outM) * 100 / float64(p.SpeedMbps)
						w["util_pct"] = r2(util)
						if util >= 80 {
							flags = append(flags, fmt.Sprintf("采样期间带宽占用 %.0f%%（接近满载，可能拥塞）", util))
							score += 3
						}
					}
				}
				if q.Oper != p.Oper {
					flags = append(flags, fmt.Sprintf("采样期间端口状态从 %s 变为 %s", ifStatusText[p.Oper], ifStatusText[q.Oper]))
					score += 4
				}
				m["sample"] = w
			}
		}
		if len(flags) > 0 {
			m["flags"] = flags
		}
		if port != "" || all || score > 0 || len(devs[key]) > 0 || links[key] != "" {
			rows = append(rows, row{m, score})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].score > rows[j].score })
	limit := 64
	out := make([]map[string]interface{}, 0, len(rows))
	flagged := 0
	for i, r := range rows {
		if r.score > 0 {
			flagged++
		}
		if i < limit {
			out = append(out, r.m)
		}
	}
	res["summary"] = map[string]interface{}{"ports_read": len(first.Order), "up": up, "down": down, "ports_with_issues": flagged, "sampled_sec": sample}
	if second == nil && sample > 0 {
		res["summary"].(map[string]interface{})["sample_failed"] = "第二次读取失败，只有一次读数"
	}
	if len(swFlags) > 0 {
		res["switch_flags"] = swFlags
	}
	res["ports"] = out
	if len(rows) > limit {
		res["note"] = fmt.Sprintf("端口较多，只列出前 %d 个（异常的在前）", limit)
	}
	if focus != "" {
		res["device"] = s.deviceRef(t, focus)
	}
	res["hint"] = "counters_since_boot 为交换机启动以来的累计值，判断是否仍有问题看 sample 中的增长；down 的端口若没接设备属正常"
	return res, nil
}

// ---------------------------------------------------------------------------
// tcp_check

var defaultTCPPorts = []int{22, 23, 80, 443, 445, 554, 3389, 8000, 8080, 9100}

var tcpPortNames = map[int]string{
	21: "FTP", 22: "SSH", 23: "Telnet", 25: "SMTP", 53: "DNS", 80: "HTTP", 135: "RPC", 139: "NetBIOS", 443: "HTTPS",
	445: "SMB 文件共享", 515: "LPD 打印", 554: "RTSP 视频流", 631: "IPP 打印", 1433: "SQL Server", 3306: "MySQL",
	3389: "远程桌面", 5900: "VNC", 8000: "HTTP(备用)/海康", 8080: "HTTP(备用)", 8443: "HTTPS(备用)", 9100: "打印机 RAW",
	37777: "大华", 5060: "SIP",
}

// isRefused 判断连接被拒绝（RST）：说明主机在线但端口未开放。兼容中文 Windows 的本地化错误信息。
func isRefused(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) && uintptr(errno) == 10061 { // WSAECONNREFUSED
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "refused") || strings.Contains(s, "积极拒绝")
}

func toolTCPCheck(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	return scanTCP(context.Background(), s, a, 16)
}

func scanTCP(ctx context.Context, s *Server, a map[string]json.RawMessage, limit int) (interface{}, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	t, _ := s.Topo.Snapshot()
	id, ip, err := probeTarget(t, a)
	if err != nil {
		return nil, err
	}
	var ports []int
	if raw, ok := a["ports"]; ok && len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &ports); err != nil {
			return nil, errors.New("ports 必须是整数数组")
		}
	}
	if len(ports) == 0 {
		ports = defaultTCPPorts
	}
	if len(ports) > limit {
		return nil, fmt.Errorf("ports 最多 %d 个", limit)
	}
	seen := map[int]bool{}
	var uniq []int
	for _, p := range ports {
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("端口 %d 无效", p)
		}
		if !seen[p] {
			seen[p] = true
			uniq = append(uniq, p)
		}
	}
	timeout := time.Duration(clampInt(argInt(a, "timeout_ms", 1500), 200, 5000)) * time.Millisecond

	type result struct {
		Port    int     `json:"port"`
		Service string  `json:"service,omitempty"`
		State   string  `json:"state"`
		MS      float64 `json:"ms,omitempty"`
		Error   string  `json:"error,omitempty"`
	}
	out := make([]result, len(uniq))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 32)
	for i, p := range uniq {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return nil, ctx.Err()
		}
		wg.Add(1)
		go func(i, p int) {
			defer wg.Done()
			defer func() { <-slots }()
			r := result{Port: p, Service: tcpPortNames[p]}
			start := time.Now()
			conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", net.JoinHostPort(ip, fmt.Sprint(p)))
			el := r2(float64(time.Since(start).Microseconds()) / 1000)
			switch {
			case err == nil:
				conn.Close()
				r.State, r.MS = "open", el
			case isRefused(err):
				r.State, r.MS = "refused", el
			default:
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					r.State = "timeout"
				} else {
					r.State = "error"
					r.Error = err.Error()
				}
			}
			out[i] = r
		}(i, p)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	open, refused := 0, 0
	for _, r := range out {
		switch r.State {
		case "open":
			open++
		case "refused":
			refused++
		}
	}
	var verdict string
	switch {
	case open > 0:
		verdict = fmt.Sprintf("主机在线：%d 个端口开放", open)
	case refused > 0:
		verdict = "主机在线（端口拒绝连接说明主机回应了 TCP），但这些端口没有开放服务"
	default:
		verdict = "未发现可连接端口：主机可能不在线、端口被过滤或探测出错，请结合各端口结果判断（不能据此断定离线，可结合 arp_lookup）"
	}
	return map[string]interface{}{
		"ip": ip, "timeout_ms": timeout.Milliseconds(), "ports": out,
		"summary": map[string]interface{}{"open": open, "refused": refused, "no_response": len(out) - open - refused, "verdict": verdict},
		"device":  s.deviceRef(t, id), "time": time.Now().Format("2006-01-02 15:04:05"),
		"note": "从 netPing 所在主机发起连接",
	}, nil
}

// ---------------------------------------------------------------------------
// arp_lookup

var readARP = discovery.ReadARP // 测试中可替换

func toolARPLookup(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	t, _ := s.Topo.Snapshot()
	id, ip, err := probeTarget(t, a)
	if err != nil {
		return nil, err
	}
	refresh, ok := argBool(a, "refresh")
	if !ok {
		refresh = true
	}
	res := map[string]interface{}{"ip": ip, "time": time.Now().Format("2006-01-02 15:04:05")}
	if refresh {
		if p, err := pinger.New(); err == nil {
			r, _ := p.Ping(ip, time.Second, 32)
			p.Close()
			res["ping_ok"] = r.OK
		}
	}
	entries, err := readARP()
	if err != nil {
		return nil, fmt.Errorf("读取 ARP 表失败：%v", err)
	}
	var mac string
	for _, e := range entries {
		if e.IP == ip {
			mac = e.MAC
		}
	}
	var reg string
	if id != "" {
		if n := t.FindNode(id); n != nil {
			reg = strings.ToUpper(n.MAC)
		}
	}
	res["device"] = s.deviceRef(t, id)
	if reg != "" {
		res["registered_mac"] = reg
	}
	if mac == "" {
		res["in_arp_table"] = false
		res["verdict"] = "ARP 表中没有该地址：设备不在线、与 netPing 主机不在同一网段，或记录已老化"
		if p, ok := res["ping_ok"].(bool); ok && p {
			res["verdict"] = "ping 通但 ARP 表中没有该地址：目标与 netPing 主机不在同一网段（经网关转发），ARP 核对不适用"
		}
		return res, nil
	}
	res["in_arp_table"], res["arp_mac"] = true, mac
	var others []map[string]interface{}
	for _, e := range entries {
		if e.MAC == mac && e.IP != ip {
			o := map[string]interface{}{"ip": e.IP}
			if _, oid := t.FindByIP(e.IP); oid != "" {
				o["name"], _, _ = deviceLabel(t, oid)
			}
			others = append(others, o)
		}
	}
	if len(others) > 0 {
		res["same_mac_other_ips"] = others
	}
	var verdict []string
	if reg != "" && reg != mac {
		res["mac_matches_registered"] = false
		holder := ""
		for _, n := range t.Nodes {
			if strings.EqualFold(n.MAC, mac) {
				holder = n.DisplayName() + "（" + n.IP + "）"
			}
		}
		v := "ARP 中的 MAC 与登记不一致：IP 可能被其他设备占用（IP 冲突）、设备已更换，或登记有误"
		if holder != "" {
			v += "；该 MAC 在拓扑中属于 " + holder
		}
		verdict = append(verdict, v)
	} else if reg != "" {
		res["mac_matches_registered"] = true
		verdict = append(verdict, "MAC 与登记一致")
	}
	if p, ok := res["ping_ok"].(bool); ok && !p {
		verdict = append(verdict, "ping 不通但仍有 ARP 记录：设备二层在线，多半是禁 ping/防火墙（若记录为刚才 ping 前残留，可稍后再查一次）")
	}
	if len(others) > 0 && len(others) >= 3 {
		verdict = append(verdict, "同一 MAC 对应多个 IP：可能是网关/路由器代理 ARP，或该设备配置了多个地址")
	}
	res["verdict"] = strings.Join(verdict, "；")
	return res, nil
}

// ---------------------------------------------------------------------------
// trace_route

func toolTraceRoute(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	t, _ := s.Topo.Snapshot()
	id, ip, err := probeTarget(t, a)
	if err != nil {
		return nil, err
	}
	maxHops := clampInt(argInt(a, "max_hops", 15), 1, 30)
	timeout := time.Duration(clampInt(argInt(a, "timeout_ms", 1000), 200, 3000)) * time.Millisecond
	probes := clampInt(argInt(a, "probes", 1), 1, 3)
	p, err := pinger.New()
	if err != nil {
		return nil, fmt.Errorf("无法创建 ping：%v", err)
	}
	defer p.Close()
	tp, ok := p.(pinger.TTLPinger)
	if !ok {
		return nil, errors.New("当前平台不支持路由追踪")
	}
	const budget = 45 * time.Second
	start := time.Now()
	var hops []map[string]interface{}
	reached, silent := false, 0
	stop := ""
	for ttl := 1; ttl <= maxHops && !reached; ttl++ {
		if time.Since(start) > budget {
			stop = "超过 45 秒时间上限，停止追踪"
			break
		}
		hop := map[string]interface{}{"hop": ttl}
		var rtts []interface{}
		from, status := "", ""
		for i := 0; i < probes; i++ {
			h, err := tp.PingTTL(ip, ttl, timeout)
			if err != nil {
				return nil, err
			}
			if h.From == "" {
				rtts = append(rtts, "*")
				continue
			}
			from = h.From
			rtts = append(rtts, r2(float64(h.RTT.Microseconds())/1000))
			if h.Status != "" {
				status = h.Status
			}
			if h.Reached {
				reached = true
			}
		}
		hop["rtt_ms"] = rtts
		if from == "" {
			hop["ip"] = "*"
			silent++
		} else {
			silent = 0
			hop["ip"] = from
			if _, did := t.FindByIP(from); did != "" {
				hop["name"], _, _ = deviceLabel(t, did)
			} else if from == t.Site.Gateway {
				hop["name"] = "网关"
			}
		}
		if status != "" {
			hop["status"] = status
		}
		hops = append(hops, hop)
		if status != "" {
			stop = "第 " + fmt.Sprint(ttl) + " 跳回应" + status + "，停止追踪"
			break
		}
		if silent >= 5 {
			stop = "连续 5 跳无回应，停止追踪"
			break
		}
	}
	res := map[string]interface{}{"target": ip, "reached": reached, "hops": hops, "device": s.deviceRef(t, id),
		"seconds": r2(time.Since(start).Seconds()), "note": "从 netPing 所在主机发出；中间某跳显示 * 但后面能到达，通常只是该路由器不回应 ICMP，不代表故障"}
	if stop != "" {
		res["stopped"] = stop
	}
	if !reached {
		last := ""
		for _, h := range hops {
			if h["ip"] != "*" {
				last = fmt.Sprintf("第 %v 跳 %v", h["hop"], h["ip"])
			}
		}
		if last == "" {
			res["verdict"] = "第一跳就没有回应：netPing 主机到网关可能不通，或网关不回应 ICMP"
		} else {
			res["verdict"] = "未到达目标，最后有回应的是" + last + "，问题可能在它之后的链路或设备（也可能目标禁 ping）"
		}
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// dns_lookup

var hostnameRe = regexp.MustCompile(`^[A-Za-z0-9_]([A-Za-z0-9_-]{0,62})(\.[A-Za-z0-9_]([A-Za-z0-9_-]{0,62}))*\.?$`)

func toolDNSLookup(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	name := strings.TrimSpace(argStr(a, "name"))
	if name == "" {
		return nil, errors.New("需要提供 name")
	}
	reverse := net.ParseIP(name) != nil
	if !reverse && (len(name) > 253 || !hostnameRe.MatchString(name)) {
		return nil, fmt.Errorf("%q 不是有效的域名", name)
	}
	timeout := time.Duration(clampInt(argInt(a, "timeout_ms", 3000), 500, 10000)) * time.Millisecond
	r := net.DefaultResolver
	server := strings.TrimSpace(argStr(a, "server"))
	if server != "" {
		sip := net.ParseIP(server).To4()
		if sip == nil {
			return nil, fmt.Errorf("DNS 服务器 %q 不是 IPv4 地址", server)
		}
		addr := net.JoinHostPort(sip.String(), "53")
		r = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: timeout}
			return d.DialContext(ctx, network, addr)
		}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	start := time.Now()
	res := map[string]interface{}{"name": name, "server": server}
	if server == "" {
		res["server"] = "系统默认"
	}
	var answers []string
	if reverse {
		res["type"] = "PTR（反向解析）"
		answers, err := r.LookupAddr(ctx, name)
		res["ms"] = r2(float64(time.Since(start).Microseconds()) / 1000)
		if err != nil {
			res["ok"], res["error"] = false, dnsErrText(err)
			return res, nil
		}
		res["ok"], res["answers"] = true, answers
		return res, nil
	}
	addrs, err := r.LookupIPAddr(ctx, name)
	res["ms"] = r2(float64(time.Since(start).Microseconds()) / 1000)
	if err != nil {
		res["ok"], res["error"] = false, dnsErrText(err)
		return res, nil
	}
	for _, ad := range addrs {
		answers = append(answers, ad.IP.String())
	}
	res["ok"], res["answers"] = true, answers
	if ms := res["ms"].(float64); ms > 1000 {
		res["note"] = "解析耗时超过 1 秒，DNS 服务器响应慢或到 DNS 的链路有丢包"
	}
	return res, nil
}

func dnsErrText(err error) string {
	var de *net.DNSError
	if errors.As(err, &de) {
		switch {
		case de.IsNotFound:
			return "域名不存在（NXDOMAIN）：" + de.Error()
		case de.IsTimeout:
			return "解析超时（DNS 服务器无响应或不可达）：" + de.Error()
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "解析超时（DNS 服务器无响应或不可达）"
	}
	return err.Error()
}
