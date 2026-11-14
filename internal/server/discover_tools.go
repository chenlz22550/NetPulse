package server

// 自主发现工具：让大模型（网页「AI 自主发现」或 MCP 客户端）自己找网段、探测、识别设备。
// 三个工具都只读：get_network_context 汇总已知网络线索；probe_subnet 只读扫描网段（不改拓扑）；
// fingerprint_host 对少量地址做服务指纹（TCP 端口、网页标题、SSH/Telnet 横幅、SNMP 系统描述、主机名、MAC 厂商）。
// 为防止模型被诱导去扫描公网，探测范围限制在私有地址与已配置的机房/扫描网段内。

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"netping/internal/discovery"
	"netping/internal/snmp"
	"netping/internal/topology"
)

const (
	seenTTL           = 6 * time.Hour // 探测结果作为 AI 建议依据的有效期
	fingerprintMaxIPs = 8
)

func discoverTools() []*mcpTool {
	return []*mcpTool{
		{
			Name: "get_network_context", Title: "网络线索汇总",
			Description: "自主发现的第一步：汇总 netPing 主机已知的网络线索——本机网卡地址与网段、机房网段和网关、已有扫描任务覆盖的网段及最近结果、" +
				"本机 ARP 表按 /24 分组（各组地址数、已登记数、未登记样例）、拓扑中设备按 /24 分组、交换机及其 SNMP 配置情况，" +
				"并列出“有线索但还没有扫描任务覆盖”的候选网段。只读，不发包。",
			Schema: obj(map[string]interface{}{}),
			Run:    toolNetworkContext,
		},
		{
			Name: "probe_subnet", Title: "只读探测网段", OpenWorld: true,
			Description: "只读探测一个或多个网段：并发 ping 每个地址、读取 ARP 得到 MAC 与厂商、解析主机名（NetBIOS/mDNS/反向 DNS），" +
				"并标注每个在线地址是否已在拓扑中登记（new 未登记、registered 已登记、moved 已登记设备换了 IP、switch 交换机、local 本机）。" +
				"与 scan_network 不同，它不修改拓扑、不自动添加设备。一次最多 1024 个地址（如 4 个 /24），只能探测私有地址或已配置的机房/扫描网段。/24 约 5~10 秒。",
			Schema: obj(map[string]interface{}{
				"subnets":       strArr("要探测的网段（CIDR），如 [\"192.168.2.0/24\"]"),
				"include_known": boolean("同时列出已登记的在线地址（默认 false，只给数量）"),
			}, "subnets"),
			Run: toolProbeSubnet,
		},
		{
			Name: "fingerprint_host", Title: "设备指纹识别", OpenWorld: true,
			Description: "识别设备是什么：对最多 8 个地址并行探测常见 TCP 端口（打印、视频、远程桌面、文件共享、网管等），读取网页标题/Server 头/认证域、" +
				"HTTPS 证书名称、SSH/FTP/Telnet 横幅、SNMP 系统描述（使用端口定位里设置的团体名）、主机名和 MAC 厂商，并给出类型线索（hints）。" +
				"用于给新发现的设备判断类别、起名，识别交换机/路由器/摄像头/打印机等。只探测私有地址或已配置网段，每台约 2~5 秒。",
			Schema: obj(map[string]interface{}{
				"ips": strArr("要识别的 IPv4 地址列表（最多 8 个）"),
				"id":  str("或：拓扑中设备的 id"),
				"ip":  str("或：单个 IPv4 地址"),
			}),
			Run: toolFingerprint,
		},
	}
}

// ---------------------------------------------------------------------------
// 探测范围限制与“已观察到的地址”缓存

var privateNets = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("169.254.0.0/16"),
}

// allowedNets 允许自主发现探测的范围：私有地址 + 机房网段 + 扫描任务网段（有些机房内网用的是公网地址段）。
func (s *Server) allowedNets() []netip.Prefix {
	out := append([]netip.Prefix(nil), privateNets...)
	t, _ := s.Topo.Snapshot()
	subs := []string{t.Site.Subnet}
	for _, j := range s.Cfg.Get().Discovery.Jobs {
		subs = append(subs, j.Subnets...)
	}
	for _, x := range subs {
		if p, err := netip.ParsePrefix(strings.TrimSpace(x)); err == nil && p.Addr().Is4() {
			out = append(out, p.Masked())
		}
	}
	return out
}

func prefixWithin(p netip.Prefix, nets []netip.Prefix) bool {
	for _, n := range nets {
		if n.Bits() <= p.Bits() && n.Contains(p.Addr()) {
			return true
		}
	}
	return false
}

// checkProbeSubnet 校验网段格式与探测范围，返回规范化的 CIDR。
func (s *Server) checkProbeSubnet(x string) (string, error) {
	p, err := netip.ParsePrefix(strings.TrimSpace(x))
	if err != nil || !p.Addr().Is4() {
		return "", fmt.Errorf("网段 %q 格式应为 CIDR，如 192.168.1.0/24", x)
	}
	p = p.Masked()
	if !prefixWithin(p, s.allowedNets()) {
		return "", fmt.Errorf("网段 %s 不是私有地址，也不在已配置的机房/扫描网段内，自主发现不会探测它", p)
	}
	return p.String(), nil
}

func (s *Server) checkProbeIP(ip string) (string, error) {
	a, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil || !a.Is4() {
		return "", fmt.Errorf("%q 不是 IPv4 地址", ip)
	}
	if a.IsUnspecified() || a.IsMulticast() || a == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return "", errors.New("不能探测广播、组播或全零地址")
	}
	if !prefixWithin(netip.PrefixFrom(a, 32), s.allowedNets()) {
		return "", fmt.Errorf("%s 不是私有地址，也不在已配置的机房/扫描网段内，自主发现不会探测它", a)
	}
	return a.String(), nil
}

// seenHost 自主发现中实际观察到的地址。
type seenHost struct {
	discovery.Host
	At       time.Time `json:"at"`
	Hints    []string  `json:"hints,omitempty"`
	Model    string    `json:"model,omitempty"` // SNMP 系统描述等可作为型号的信息
	Evidence string    `json:"evidence,omitempty"`
}

func (s *Server) rememberHost(h seenHost) {
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	if s.seen == nil {
		s.seen = map[string]seenHost{}
	}
	if old, ok := s.seen[h.IP]; ok && time.Since(old.At) < seenTTL {
		// 合并：保留之前得到的信息
		if h.MAC == "" {
			h.MAC, h.SharedMAC = old.MAC, old.SharedMAC
		}
		if h.Vendor == "" {
			h.Vendor = old.Vendor
		}
		if h.Hostname == "" {
			h.Hostname = old.Hostname
		}
		if !h.Ping {
			h.Ping, h.RTT = old.Ping, old.RTT
		}
		h.ARP = h.ARP || old.ARP
		if len(h.Hints) == 0 {
			h.Hints = old.Hints
		}
		if h.Model == "" {
			h.Model = old.Model
		}
		if h.Evidence == "" {
			h.Evidence = old.Evidence
		}
	}
	h.At = time.Now()
	s.seen[h.IP] = h
	if len(s.seen) > 8192 { // 清理过期记录
		for k, v := range s.seen {
			if time.Since(v.At) > seenTTL {
				delete(s.seen, k)
			}
		}
	}
}

// observed 返回最近实际看到过的地址信息：自主发现的探测结果，或网段扫描的最近结果。
func (s *Server) observed(ip string) (seenHost, bool) {
	s.seenMu.Lock()
	h, ok := s.seen[ip]
	s.seenMu.Unlock()
	if ok && time.Since(h.At) < seenTTL {
		return h, true
	}
	for _, r := range s.Disc.Results() {
		if time.Since(r.Finished) > seenTTL {
			continue
		}
		for _, x := range r.Hosts {
			if x.IP == ip {
				return seenHost{Host: x, At: r.Finished}, true
			}
		}
	}
	return seenHost{}, false
}

// ---------------------------------------------------------------------------
// get_network_context

func net24(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil || !a.Is4() {
		return ""
	}
	p, _ := a.Prefix(24)
	return p.String()
}

func toolNetworkContext(s *Server, _ map[string]json.RawMessage, _ string) (interface{}, error) {
	t, _ := s.Topo.Snapshot()
	cfg := s.Cfg.Get()
	allowed := s.allowedNets()

	// 已有扫描任务覆盖的网段
	var covered []netip.Prefix
	var jobs []map[string]interface{}
	results := s.Disc.Results()
	for _, j := range cfg.Discovery.Jobs {
		eff := s.Disc.JobSubnets(j)
		for _, x := range eff {
			if p, err := netip.ParsePrefix(x); err == nil {
				covered = append(covered, p.Masked())
			}
		}
		jm := map[string]interface{}{"id": j.ID, "name": j.Name, "subnets": eff, "enabled": j.Enabled && cfg.Discovery.Enabled,
			"interval_min": j.IntervalMin, "auto_add": j.AutoAdd}
		if r := results[j.ID]; r != nil {
			jm["last_run"] = r.Finished.Format("2006-01-02 15:04")
			jm["alive"], jm["unknown"] = r.Alive, len(r.Unknown)
		}
		jobs = append(jobs, jm)
	}
	isCovered := func(cidr string) bool {
		p, err := netip.ParsePrefix(cidr)
		return err == nil && prefixWithin(p, covered)
	}

	type group struct {
		Subnet     string   `json:"subnet"`
		Count      int      `json:"count"`
		Registered int      `json:"registered"`
		Samples    []string `json:"unregistered_samples,omitempty"`
		Covered    bool     `json:"covered_by_job"`
	}
	cands := map[string][]string{} // 候选网段 → 线索来源
	addCand := func(cidr, why string) {
		p, err := netip.ParsePrefix(cidr)
		if err != nil || isCovered(cidr) || !prefixWithin(p.Masked(), allowed) {
			return
		}
		if !inList(cands[cidr], why) {
			cands[cidr] = append(cands[cidr], why)
		}
	}

	// 本机网卡
	var nics []map[string]interface{}
	if ifs, err := net.Interfaces(); err == nil {
		for _, inf := range ifs {
			if inf.Flags&net.FlagUp == 0 || inf.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, _ := inf.Addrs()
			var v4 []string
			for _, a := range addrs {
				ipn, ok := a.(*net.IPNet)
				if !ok || ipn.IP.To4() == nil {
					continue
				}
				ones, _ := ipn.Mask.Size()
				v4 = append(v4, fmt.Sprintf("%s/%d", ipn.IP.To4(), ones))
				if p, err := netip.ParsePrefix(fmt.Sprintf("%s/%d", ipn.IP.To4(), ones)); err == nil {
					p = p.Masked()
					if p.Bits() < 22 { // 太大的网段只建议本机所在的 /24
						p, _ = netip.MustParseAddr(ipn.IP.To4().String()).Prefix(24)
					}
					addCand(p.String(), "本机网卡 "+inf.Name)
				}
			}
			if len(v4) > 0 {
				nics = append(nics, map[string]interface{}{"name": inf.Name, "mac": strings.ToUpper(inf.HardwareAddr.String()), "ipv4": v4})
			}
		}
	}

	// 拓扑中已登记设备
	regIP := map[string]bool{}
	topoGroups := map[string]*group{}
	addTopo := func(ip string) {
		if ip == "" {
			return
		}
		regIP[ip] = true
		k := net24(ip)
		if k == "" {
			return
		}
		g := topoGroups[k]
		if g == nil {
			g = &group{Subnet: k, Covered: isCovered(k)}
			topoGroups[k] = g
		}
		g.Count++
		g.Registered++
	}
	for _, n := range t.Nodes {
		addTopo(n.IP)
	}
	var switches []map[string]interface{}
	snmpOK := map[string]bool{}
	for _, tg := range s.Loc.Targets() {
		snmpOK[tg.Switch] = true
	}
	for _, sw := range t.Switches {
		addTopo(sw.IP)
		switches = append(switches, map[string]interface{}{"id": sw.ID, "name": sw.Name, "role": sw.Role, "ip": sw.IP, "vendor": sw.Vendor, "snmp": snmpOK[sw.ID]})
	}
	for k := range topoGroups {
		addCand(k, "拓扑中已有设备")
	}
	if t.Site.Gateway != "" {
		if k := net24(t.Site.Gateway); k != "" {
			addCand(k, "机房网关所在网段")
		}
	}

	// ARP 表
	arpGroups := map[string]*group{}
	arpErr := ""
	if arp, err := readARP(); err != nil {
		arpErr = err.Error()
	} else {
		for _, e := range arp {
			a, err := netip.ParseAddr(e.IP)
			if err != nil || !a.Is4() || a.IsMulticast() || strings.HasSuffix(e.IP, ".255") {
				continue
			}
			k := net24(e.IP)
			g := arpGroups[k]
			if g == nil {
				g = &group{Subnet: k, Covered: isCovered(k)}
				arpGroups[k] = g
			}
			g.Count++
			if regIP[e.IP] {
				g.Registered++
			} else if len(g.Samples) < 8 {
				g.Samples = append(g.Samples, e.IP)
			}
		}
		for k := range arpGroups {
			addCand(k, "本机 ARP 表中有地址")
		}
	}
	sortGroups := func(m map[string]*group) []*group {
		out := make([]*group, 0, len(m))
		for _, g := range m {
			out = append(out, g)
		}
		sort.Slice(out, func(i, j int) bool { return ipLessStr(out[i].Subnet, out[j].Subnet) })
		return out
	}
	var candList []map[string]interface{}
	for k, why := range cands {
		candList = append(candList, map[string]interface{}{"subnet": k, "evidence": why})
	}
	sort.Slice(candList, func(i, j int) bool {
		return ipLessStr(candList[i]["subnet"].(string), candList[j]["subnet"].(string))
	})

	locCommunity := cfg.Locate.Community != ""
	out := map[string]interface{}{
		"site":                map[string]interface{}{"name": t.Site.Name, "subnet": t.Site.Subnet, "gateway": t.Site.Gateway},
		"netping_host_nics":   nics,
		"scan_jobs":           jobs,
		"arp_by_24":           sortGroups(arpGroups),
		"topology_by_24":      sortGroups(topoGroups),
		"switches":            switches,
		"candidate_subnets":   candList,
		"snmp_community_set":  locCommunity,
		"probe_limits":        fmt.Sprintf("probe_subnet 每次最多 %d 个地址；只能探测私有地址（10/8、172.16/12、192.168/16、100.64/10、169.254/16）或已配置网段", discovery.ProbeMaxHosts),
		"discovery_exclude":   cfg.Discovery.Exclude,
		"registered_counts":   map[string]int{"nodes": len(t.Nodes), "switches": len(t.Switches)},
		"note":                "ARP 只包含与 netPing 主机同一二层网段、且最近通信过的地址；跨网段设备要靠 ping 探测，得不到 MAC。",
		"generated":           time.Now().Format("2006-01-02 15:04:05"),
		"candidate_explained": "candidate_subnets：有线索但没有任何扫描任务覆盖的 /24（或本机网卡所在网段），是自主发现时优先探测的对象",
	}
	if arpErr != "" {
		out["arp_error"] = arpErr
	}
	return out, nil
}

func ipLessStr(a, b string) bool {
	pa, ea := netip.ParsePrefix(a)
	pb, eb := netip.ParsePrefix(b)
	if ea == nil && eb == nil {
		return pa.Addr().Less(pb.Addr())
	}
	return a < b
}

// ---------------------------------------------------------------------------
// probe_subnet

var probeSubnets = func(s *Server, subnets []string, reason string) ([]discovery.Host, error) { // 测试中可替换
	return s.Disc.Probe(subnets, reason)
}

func toolProbeSubnet(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	raw := argStrings(a, "subnets")
	if len(raw) == 0 {
		if one := argStr(a, "subnet"); one != "" {
			raw = []string{one}
		}
	}
	if len(raw) == 0 {
		return nil, errors.New("需要 subnets（CIDR 列表），如 [\"192.168.2.0/24\"]")
	}
	var subs []string
	for _, x := range raw {
		for _, y := range strings.FieldsFunc(x, func(r rune) bool { return r == ',' || r == '，' || r == ' ' }) {
			c, err := s.checkProbeSubnet(y)
			if err != nil {
				return nil, err
			}
			subs = append(subs, c)
		}
	}
	start := time.Now()
	hosts, err := probeSubnets(s, subs, "AI 自主发现探测")
	if err != nil {
		return nil, err
	}
	log.Printf("%s 只读探测 %v：在线 %d 个，用时 %dms", remote, subs, len(hosts), time.Since(start).Milliseconds())
	incKnown, _ := argBool(a, "include_known")
	type row struct {
		IP       string   `json:"ip"`
		MAC      string   `json:"mac,omitempty"`
		Vendor   string   `json:"vendor,omitempty"`
		Hostname string   `json:"hostname,omitempty"`
		Ping     bool     `json:"ping"`
		RTT      float64  `json:"rtt_ms,omitempty"`
		Status   string   `json:"status"`
		NodeID   string   `json:"registered_id,omitempty"`
		Shared   []string `json:"mac_shared_with,omitempty"`
	}
	counts := map[string]int{}
	newRows, moved, known := []row{}, []row{}, []row{}
	for _, h := range hosts {
		s.rememberHost(seenHost{Host: h})
		r := row{IP: h.IP, MAC: h.MAC, Vendor: h.Vendor, Hostname: h.Hostname, Ping: h.Ping, RTT: h.RTT, Status: h.Status, NodeID: h.NodeID, Shared: h.SharedMAC}
		counts[h.Status]++
		switch h.Status {
		case "new":
			newRows = append(newRows, r)
		case "moved":
			moved = append(moved, r)
		default:
			if incKnown {
				known = append(known, r)
			}
		}
	}
	out := map[string]interface{}{
		"subnets": subs, "alive": len(hosts), "counts": counts, "new": newRows, "duration_ms": time.Since(start).Milliseconds(),
		"note": "只读探测，未修改拓扑。status：new 未登记；moved 已登记设备（registered_id）出现在新 IP，正常扫描会自动跟踪；ping=false 表示只在 ARP 中出现（禁 ping）。",
	}
	if len(moved) > 0 {
		out["moved"] = moved
	}
	if incKnown {
		out["known"] = known
	}
	if len(hosts) == 0 {
		out["hint"] = "没有发现在线地址：网段可能不存在或不可达（可用 trace_route 看路由），也可能设备都禁 ping 且不在同一二层网段。"
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// fingerprint_host

// 识别用的端口：服务名 → 用于推断类型。
var fpPorts = []int{21, 22, 23, 53, 80, 81, 135, 139, 443, 445, 515, 554, 631, 1883, 3389, 5000, 5060, 5900, 8000, 8080, 8291, 8443, 9100, 37777, 34567, 3306, 1433, 902}

var fpPortNames = map[int]string{81: "HTTP(备用)", 1883: "MQTT", 5000: "HTTP(群晖/UPnP)", 8291: "MikroTik Winbox", 34567: "雄迈 NVR", 902: "VMware ESXi"}

func portName(p int) string {
	if n := tcpPortNames[p]; n != "" {
		return n
	}
	return fpPortNames[p]
}

var (
	titleRe   = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	spaceRe   = regexp.MustCompile(`\s+`)
	nonPrintR = regexp.MustCompile(`[^\x20-\x7e\p{Han}]+`)
)

type fpResult struct {
	IP       string            `json:"ip"`
	Error    string            `json:"error,omitempty"`
	Online   bool              `json:"online"`
	Open     []string          `json:"open_ports"`
	MAC      string            `json:"mac,omitempty"`
	Vendor   string            `json:"vendor,omitempty"`
	Hostname string            `json:"hostname,omitempty"`
	HTTP     []map[string]any  `json:"http,omitempty"`
	Banners  map[string]string `json:"banners,omitempty"`
	SNMP     map[string]string `json:"snmp,omitempty"`
	Hints    []string          `json:"hints"`
	Device   interface{}       `json:"registered,omitempty"`
	ms       int64
}

func toolFingerprint(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	t, _ := s.Topo.Snapshot()
	ips := argStrings(a, "ips")
	if id := argStr(a, "id"); id != "" {
		_, rid, err := resolveDevice(t, id, "")
		if err != nil {
			return nil, err
		}
		_, ip, _ := deviceLabel(t, rid)
		if ip == "" {
			return nil, errors.New("该设备没有 IP")
		}
		ips = append(ips, ip)
	}
	if ip := argStr(a, "ip"); ip != "" {
		ips = append(ips, ip)
	}
	if len(ips) == 0 {
		return nil, errors.New("需要 ips、ip 或 id")
	}
	var uniq []string
	seen := map[string]bool{}
	for _, ip := range ips {
		c, err := s.checkProbeIP(ip)
		if err != nil {
			return nil, err
		}
		if !seen[c] {
			seen[c] = true
			uniq = append(uniq, c)
		}
	}
	if len(uniq) > fingerprintMaxIPs {
		return nil, fmt.Errorf("一次最多识别 %d 个地址", fingerprintMaxIPs)
	}
	cfg := s.Cfg.Get()
	arp := map[string]string{}
	if es, err := readARP(); err == nil {
		for _, e := range es {
			arp[e.IP] = e.MAC
		}
	}
	vendors := discovery.LearnOUI(t, cfg.Discovery.OUI)
	out := make([]*fpResult, len(uniq))
	var wg sync.WaitGroup
	for i, ip := range uniq {
		wg.Add(1)
		go func(i int, ip string) {
			defer wg.Done()
			start := time.Now()
			r := fingerprintOne(ip, cfg.Locate.Community, time.Duration(cfg.Locate.TimeoutMs)*time.Millisecond)
			r.ms = time.Since(start).Milliseconds()
			out[i] = r
		}(i, ip)
	}
	wg.Wait()
	for _, r := range out {
		if mac := arp[r.IP]; mac != "" { // 探测后 ARP 才会有记录，所以最后再读一次
			r.MAC = mac
		}
		if r.MAC == "" {
			if h, ok := s.observed(r.IP); ok {
				r.MAC = h.MAC
			}
		}
		if r.MAC == "" {
			if es, err := readARP(); err == nil {
				for _, e := range es {
					if e.IP == r.IP {
						r.MAC = e.MAC
					}
				}
			}
		}
		r.Vendor = discovery.VendorOf(r.MAC, vendors)
		r.Hints = append(r.Hints, inferHints(r)...)
		if len(r.Hints) == 0 {
			r.Hints = []string{}
		}
		if _, id := t.FindByIP(r.IP); id != "" {
			r.Device = s.deviceRef(t, id)
		}
		model := r.SNMP["sys_descr"]
		ev := strings.Join(r.Open, ",")
		if r.Online {
			s.rememberHost(seenHost{Host: discovery.Host{IP: r.IP, MAC: r.MAC, Vendor: r.Vendor, Hostname: r.Hostname, ARP: r.MAC != ""},
				Hints: r.Hints, Model: truncRunes(model, 80), Evidence: ev})
		}
	}
	log.Printf("%s 设备指纹识别 %v", remote, uniq)
	return map[string]interface{}{"results": out, "time": time.Now().Format("2006-01-02 15:04:05"),
		"note": "hints 是按端口、网页标题、横幅、SNMP 描述和 MAC 厂商推断的线索，不一定准确；online=false 表示所有探测都没有回应（可能离线或防火墙全部拦截）。"}, nil
}

// fingerprintOne 对单个地址做服务指纹。
func fingerprintOne(ip, community string, snmpTimeout time.Duration) *fpResult {
	r := &fpResult{IP: ip, Open: []string{}, Banners: map[string]string{}}
	open := map[int]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	refused := false
	for _, p := range fpPorts {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, fmt.Sprint(p)), 900*time.Millisecond)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				conn.Close()
				open[p] = true
			} else if isRefused(err) {
				refused = true
			}
		}(p)
	}
	// 主机名与 SNMP 和端口探测并行
	wg.Add(1)
	go func() {
		defer wg.Done()
		name := discovery.LookupName(ip)
		mu.Lock()
		r.Hostname = name
		mu.Unlock()
	}()
	if community != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if m := snmpSysInfo(ip, community, snmpTimeout); len(m) > 0 {
				mu.Lock()
				r.SNMP = m
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	for _, p := range fpPorts {
		if open[p] {
			r.Open = append(r.Open, fmt.Sprintf("%d/%s", p, portName(p)))
		}
	}
	r.Online = len(open) > 0 || refused || len(r.SNMP) > 0

	// 网页与横幅
	var wg2 sync.WaitGroup
	web := 0
	for _, p := range []int{80, 443, 8080, 8000, 8443, 81, 5000} {
		if !open[p] || web >= 3 {
			continue
		}
		web++
		wg2.Add(1)
		go func(p int) {
			defer wg2.Done()
			if h := httpInfo(ip, p); h != nil {
				mu.Lock()
				r.HTTP = append(r.HTTP, h)
				mu.Unlock()
			}
		}(p)
	}
	for _, p := range []int{22, 21, 23} {
		if !open[p] {
			continue
		}
		wg2.Add(1)
		go func(p int) {
			defer wg2.Done()
			if b := readBanner(ip, p); b != "" {
				mu.Lock()
				r.Banners[portName(p)] = b
				mu.Unlock()
			}
		}(p)
	}
	wg2.Wait()
	sort.Slice(r.HTTP, func(i, j int) bool { return fmt.Sprint(r.HTTP[i]["port"]) < fmt.Sprint(r.HTTP[j]["port"]) })
	if len(r.Banners) == 0 {
		r.Banners = nil
	}
	return r
}

var fpHTTPClient = &http.Client{
	Timeout: 3 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true}, // 内网设备多为自签名证书，只读取标题不传敏感数据
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: 3 * time.Second,
		Proxy:                 nil,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func httpInfo(ip string, port int) map[string]interface{} {
	scheme := "http"
	if port == 443 || port == 8443 {
		scheme = "https"
	}
	u := fmt.Sprintf("%s://%s/", scheme, net.JoinHostPort(ip, fmt.Sprint(port)))
	if (scheme == "http" && port == 80) || (scheme == "https" && port == 443) {
		u = fmt.Sprintf("%s://%s/", scheme, ip)
	}
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("User-Agent", "netPing-discovery")
	resp, err := fpHTTPClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	m := map[string]interface{}{"port": port, "status": resp.StatusCode}
	if v := resp.Header.Get("Server"); v != "" {
		m["server"] = truncRunes(v, 80)
	}
	if v := resp.Header.Get("WWW-Authenticate"); v != "" {
		m["auth"] = truncRunes(v, 80)
	}
	if v := resp.Header.Get("Location"); v != "" {
		m["redirect"] = truncRunes(v, 120)
	}
	if mm := titleRe.FindSubmatch(body); mm != nil {
		if tt := strings.TrimSpace(spaceRe.ReplaceAllString(string(mm[1]), " ")); tt != "" {
			m["title"] = truncRunes(tt, 80)
		}
	}
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		c := resp.TLS.PeerCertificates[0]
		cert := c.Subject.CommonName
		if len(c.Subject.Organization) > 0 {
			cert += " / " + c.Subject.Organization[0]
		}
		if strings.TrimSpace(cert) != "" {
			m["cert"] = truncRunes(cert, 80)
		}
	}
	return m
}

func readBanner(ip string, port int) string {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, fmt.Sprint(port)), time.Second)
	if err != nil {
		return ""
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	buf := make([]byte, 256)
	n, _ := conn.Read(buf)
	b := strings.TrimSpace(nonPrintR.ReplaceAllString(string(buf[:n]), " "))
	return truncRunes(b, 100)
}

var snmpGetSys = func(ip, community string, timeout time.Duration) ([]snmp.Var, error) { // 测试中可替换
	if timeout <= 0 || timeout > 2*time.Second {
		timeout = 1500 * time.Millisecond
	}
	c := &snmp.Client{Host: ip, Community: community, Timeout: timeout, Retries: 0}
	if err := c.Connect(); err != nil {
		return nil, err
	}
	defer c.Close()
	return c.Get(snmp.ParseOID("1.3.6.1.2.1.1.1.0"), snmp.ParseOID("1.3.6.1.2.1.1.2.0"), snmp.ParseOID("1.3.6.1.2.1.1.5.0"))
}

func snmpSysInfo(ip, community string, timeout time.Duration) map[string]string {
	vars, err := snmpGetSys(ip, community, timeout)
	if err != nil {
		return nil
	}
	m := map[string]string{}
	keys := []string{"sys_descr", "sys_object_id", "sys_name"}
	for i, v := range vars {
		if i < len(keys) {
			if x := strings.TrimSpace(nonPrintR.ReplaceAllString(v.String(), " ")); x != "" {
				m[keys[i]] = truncRunes(x, 160)
			}
		}
	}
	return m
}

// 推断规则：关键词（不区分大小写）→ 线索。
var fpTextRules = []struct {
	re   *regexp.Regexp
	hint string
}{
	{regexp.MustCompile(`(?i)hikvision|DNVRS|App-webs|\bDS-\d`), "海康威视 → 摄像头或录像机"},
	{regexp.MustCompile(`(?i)dahua|\bDH-|DHI-`), "大华 → 摄像头或录像机"},
	{regexp.MustCompile(`(?i)uniview|宇视`), "宇视 → 摄像头或录像机"},
	{regexp.MustCompile(`(?i)\bNVR\b|DVR|录像`), "录像机（NVR/DVR）"},
	{regexp.MustCompile(`(?i)ipcam|network camera|IP Camera|webcam`), "摄像头"},
	{regexp.MustCompile(`(?i)laserjet|officejet|deskjet|\bHP .*(MFP|Printer)|brother|canon|epson|ricoh|kyocera|konica|lexmark|xerox|printer|打印`), "打印机"},
	{regexp.MustCompile(`(?i)synology|diskstation|qnap|truenas|freenas|\bNAS\b`), "网络存储"},
	{regexp.MustCompile(`(?i)routeros|mikrotik`), "MikroTik 路由器"},
	{regexp.MustCompile(`(?i)fortigate|fortinet|sangfor|深信服|hillstone|山石|pfsense|opnsense|sophos|firewall|防火墙|\bUSG\d`), "防火墙/出口网关"},
	{regexp.MustCompile(`(?i)switch|交换机|comware|\bVRP\b|ruijie|锐捷|catalyst|\bIOS\b.*cisco|cisco.*\bIOS\b|\bS\d{4}|H3C|TL-SG|procurve|aruba.*switch|netgear.*GS\d`), "可能是交换机（网管型）"},
	{regexp.MustCompile(`(?i)unifi|ubiquiti|access point|\bAP\b|wireless|ruckus|aruba`), "无线 AP / 无线控制器"},
	{regexp.MustCompile(`(?i)vmware esxi|esxi`), "VMware ESXi 宿主机 → 服务器"},
	{regexp.MustCompile(`(?i)idrac|ilo\b|integrated lights|\bIPMI\b|\bBMC\b|xclarity|supermicro`), "服务器带外管理口（iDRAC/iLO/BMC）"},
	{regexp.MustCompile(`(?i)yealink|grandstream|fanvil|polycom|avaya|cisco.*phone|\bSIP\b`), "IP 电话"},
	{regexp.MustCompile(`(?i)zkteco|中控|门禁|考勤|attendance|access control`), "门禁考勤"},
	{regexp.MustCompile(`(?i)windows|microsoft-iis|microsoft-httpapi`), "Windows 主机"},
	{regexp.MustCompile(`(?i)openssh.*ubuntu|debian|centos|linux`), "Linux 主机"},
	{regexp.MustCompile(`(?i)raspberry`), "树莓派"},
	{regexp.MustCompile(`(?i)tp-?link|mercury|水星|fast\b|迅捷|tenda|腾达|xiaomi|小米路由|router`), "家用/小型路由器或无线设备"},
	{regexp.MustCompile(`(?i)网关|光猫|\bONT\b|\bONU\b|gpon|epon|gateway|中国电信|中国移动|中国联通|天翼`), "运营商光猫/出口网关"},
	{regexp.MustCompile(`(?i)\bUPS\b|apc|eaton|山特|santak`), "UPS 电源"},
	{regexp.MustCompile(`(?i)vmware|virtualbox|hyper-v|qemu`), "虚拟机"},
}

func inferHints(r *fpResult) []string {
	var hints []string
	add := func(h string) {
		if !inList(hints, h) {
			hints = append(hints, h)
		}
	}
	has := func(p int) bool {
		for _, o := range r.Open {
			if strings.HasPrefix(o, fmt.Sprint(p)+"/") {
				return true
			}
		}
		return false
	}
	if has(9100) || has(515) || has(631) {
		add("打印端口开放（9100/515/631）→ 打印机")
	}
	if has(554) {
		add("RTSP 554 开放 → 摄像头或录像机")
	}
	if has(554) && has(8000) {
		add("554 + 8000 → 海康威视设备的典型组合")
	}
	if has(37777) {
		add("37777 开放 → 大华设备")
	}
	if has(34567) {
		add("34567 开放 → 雄迈 NVR/摄像头")
	}
	if has(3389) {
		add("远程桌面 3389 → Windows 主机（台式机/服务器）")
	}
	if has(445) && has(135) {
		add("445 + 135 → Windows 主机")
	}
	if has(5000) && has(445) {
		add("5000 + 445 → 可能是群晖等网络存储")
	}
	if has(8291) {
		add("Winbox 8291 → MikroTik 路由器")
	}
	if has(5060) {
		add("SIP 5060 → IP 电话或语音网关")
	}
	if has(902) {
		add("902 开放 → VMware ESXi 宿主机")
	}
	if has(3306) || has(1433) {
		add("数据库端口开放 → 服务器")
	}
	if has(1883) {
		add("MQTT 1883 → 物联网设备或网关")
	}
	if has(53) {
		add("DNS 53 开放 → 网关/路由器/DNS 服务器")
	}
	if len(r.SNMP) > 0 {
		add("SNMP 有回应 → 网管型设备（交换机、路由器、打印机、UPS 等）")
	}
	var texts []string
	for _, h := range r.HTTP {
		for _, k := range []string{"title", "server", "auth", "cert"} {
			if v, ok := h[k].(string); ok {
				texts = append(texts, v)
			}
		}
	}
	for _, v := range r.Banners {
		texts = append(texts, v)
	}
	for _, v := range r.SNMP {
		texts = append(texts, v)
	}
	texts = append(texts, r.Hostname, r.Vendor)
	all := strings.Join(texts, " | ")
	for _, rule := range fpTextRules {
		if rule.re.MatchString(all) {
			add(rule.hint)
		}
	}
	if r.Vendor == "本地私有地址" {
		add("随机/本地管理 MAC → 多为手机、平板或开启了随机 MAC 的笔记本")
	}
	if host := strings.ToUpper(r.Hostname); host != "" {
		switch {
		case strings.HasPrefix(host, "DESKTOP-"):
			add("主机名 DESKTOP- → Windows 台式机")
		case strings.HasPrefix(host, "LAPTOP-") || strings.Contains(host, "MACBOOK"):
			add("主机名 → 笔记本")
		case strings.Contains(host, "IPHONE") || strings.Contains(host, "IPAD") || strings.Contains(host, "ANDROID"):
			add("主机名 → 手机平板")
		case strings.HasPrefix(host, "NPI") || strings.Contains(host, "PRN") || strings.Contains(host, "PRINTER"):
			add("主机名 → 打印机")
		}
	}
	if !r.Online {
		add("无任何回应：可能已离线，或防火墙拦截了所有探测")
	}
	return hints
}

// 供 ai_discover 使用：拓扑中是否已有该 IP 或 MAC。
func registeredBy(t *topology.Topology, ip, mac string) string {
	if _, id := t.FindByIP(ip); id != "" {
		return id
	}
	if mac != "" {
		for _, n := range t.Nodes {
			if n.MAC == mac {
				return n.ID
			}
		}
		for _, sw := range t.Switches {
			if sw.MAC == mac {
				return sw.ID
			}
		}
	}
	return ""
}
