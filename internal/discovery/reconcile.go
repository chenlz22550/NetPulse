// Package discovery 扫描网段、读取 ARP，自动添加新设备并按 MAC 跟踪设备 IP 的变化。
package discovery

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"netping/internal/config"
	"netping/internal/topology"
)

// Host 是一次扫描中发现的在线地址。
type Host struct {
	IP       string  `json:"ip"`
	MAC      string  `json:"mac,omitempty"`
	Ping     bool    `json:"ping"`          // 是否响应 ping
	RTT      float64 `json:"rtt,omitempty"` // ms
	ARP      bool    `json:"arp"`           // 是否在 ARP 表中
	Hostname string  `json:"hostname,omitempty"`
	Vendor   string  `json:"vendor,omitempty"`
	Local    bool    `json:"local,omitempty"` // 运行 netPing 的本机
	// SharedMAC 表示该 MAC 同时对应多个 IP（无线中继 MAC-NAT、桥接、多地址主机），不能用 MAC 识别，按 IP 处理。
	SharedMAC []string `json:"shared_with,omitempty"`
	Status    string   `json:"status,omitempty"`
	NodeID    string   `json:"node_id,omitempty"`
}

// 变更类型。
const (
	ChAdded      = "added"       // 自动添加新设备
	ChIPChanged  = "ip_changed"  // 按 MAC 跟踪到新 IP 并已更新
	ChIPReleased = "ip_released" // 原 IP 已被其他 MAC 占用、本设备不在线，清空 IP
	ChMACLearned = "mac_learned" // 只登记了 IP 的节点补全了 MAC
	ChConflict   = "conflict"    // 无法自动处理的冲突
	ChHostname   = "hostname"    // 识别到或更新了主机名
)

// Change 是扫描对拓扑做出的一项修改（或冲突说明）。
type Change struct {
	Time   time.Time `json:"time"`
	Type   string    `json:"type"`
	NodeID string    `json:"node_id,omitempty"`
	Name   string    `json:"name,omitempty"`
	OldIP  string    `json:"old_ip,omitempty"`
	IP     string    `json:"ip,omitempty"`
	MAC    string    `json:"mac,omitempty"`
	Detail string    `json:"detail"`
	Job    string    `json:"job,omitempty"` // 产生该变更的扫描任务名
}

// Suggestion 是需要人工确认的 IP 变化（锁定为固定 IP 或未开启跟踪的节点）。
type Suggestion struct {
	NodeID    string `json:"node_id"`
	Name      string `json:"name"`
	CurrentIP string `json:"current_ip"`
	SeenIP    string `json:"seen_ip"`
	MAC       string `json:"mac"`
	Reason    string `json:"reason"`
}

// Options 控制自动处理的范围。
type Options struct {
	Tracking string // all | dhcp | off
	AutoAdd  bool
	Now      time.Time
	OUI      map[string]string // 额外的 MAC 前缀 → 厂商
	// 自动添加的设备：附加标签、是否开启告警、来源说明（扫描任务名）
	Tags   []string
	Alert  bool
	Source string
}

// Decorate 按扫描任务的设置补充新节点的标签、告警和来源说明。
func (o Options) Decorate(n *topology.Node) {
	for _, tg := range o.Tags {
		if !containsStr(n.Tags, tg) {
			n.Tags = append(n.Tags, tg)
		}
	}
	if o.Alert && n.Monitor.IsEnabled() {
		n.Monitor.Alert = true
	}
	if o.Source != "" {
		n.Note = strings.Replace(n.Note, "网段扫描自动发现于", "网段扫描（"+o.Source+"）自动发现于", 1)
	}
}

func containsStr(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Result 是一次核对的结果。
type Result struct {
	Changes     []Change     `json:"changes"`
	Suggestions []Suggestion `json:"suggestions"`
	Unknown     []Host       `json:"unknown"` // 未自动添加的新设备
}

// Trackable 判断节点是否允许按 MAC 自动更新 IP。
func Trackable(n *topology.Node, tracking string) bool {
	if n.MAC == "" || n.IPMode == "static" {
		return false
	}
	switch tracking {
	case "all", "":
		return true
	case "dhcp":
		return n.IPMode == "dhcp"
	}
	return false
}

func nodeLabel(n *topology.Node) string {
	if n.Name != "" {
		return n.Name
	}
	if n.Hostname != "" {
		return n.Hostname
	}
	id := n.IP
	if id == "" {
		id = n.MAC
	}
	if id == "" {
		return n.ID
	}
	if n.Vendor != "" {
		return n.Vendor + " " + id
	}
	return id
}

// Reconcile 根据扫描结果修改拓扑 t（就地修改），返回所做的变更。
// 规则：
//  1. 只登记了 IP 的节点，若该 IP 在 ARP 中对应唯一 MAC 且 MAC 未被占用，补全 MAC/厂商；
//  2. 可跟踪节点的 MAC 出现在新 IP 上（且该 MAC 只对应一个 IP），把节点 IP 改为新 IP；
//     目标 IP 被另一个可跟踪、且已不在该 IP 上的节点占用时，该节点释放 IP；其他情况记为冲突；
//  3. 可跟踪节点原 IP 被别的 MAC 占用、自己的 MAC 在本次扫描中未出现，释放 IP；
//  4. 锁定/不可跟踪节点的 MAC 出现在新 IP 上，给出建议，不自动修改；
//  5. 剩余在线地址若拓扑中没有对应 IP/MAC，自动添加为未定位节点（或列入 Unknown）。
func Reconcile(t *topology.Topology, hosts []Host, opt Options) Result {
	var res Result
	now := opt.Now
	if now.IsZero() {
		now = time.Now()
	}
	add := func(c Change) {
		c.Time = now
		res.Changes = append(res.Changes, c)
	}

	macIPs := map[string][]string{}
	ipMAC := map[string]string{}
	for _, h := range hosts {
		if h.MAC != "" {
			macIPs[h.MAC] = append(macIPs[h.MAC], h.IP)
			ipMAC[h.IP] = h.MAC
		}
	}
	for i := range hosts {
		hosts[i].SharedMAC = nil
		if ips := macIPs[hosts[i].MAC]; hosts[i].MAC != "" && len(ips) > 1 {
			for _, ip := range ips {
				if ip != hosts[i].IP {
					hosts[i].SharedMAC = append(hosts[i].SharedMAC, ip)
				}
			}
		}
	}
	uniq := func(mac string) (string, bool) {
		ips := macIPs[mac]
		if len(ips) == 1 {
			return ips[0], true
		}
		return "", false
	}
	macOwner := func(mac string) *topology.Node {
		for _, n := range t.Nodes {
			if n.MAC == mac {
				return n
			}
		}
		return nil
	}
	switchMAC := map[string]bool{}
	switchIP := map[string]bool{}
	for _, s := range t.Switches {
		if s.MAC != "" {
			switchMAC[s.MAC] = true
		}
		if s.IP != "" {
			switchIP[s.IP] = true
		}
	}
	vendors := LearnOUI(t, opt.OUI)

	// 1. 补全 MAC
	for _, n := range t.Nodes {
		if n.MAC != "" || n.IP == "" {
			continue
		}
		mac := ipMAC[n.IP]
		if mac == "" || switchMAC[mac] || macOwner(mac) != nil {
			continue
		}
		if _, ok := uniq(mac); !ok {
			continue
		}
		n.MAC = mac
		if n.Vendor == "" {
			n.Vendor = VendorOf(mac, vendors)
		}
		add(Change{Type: ChMACLearned, NodeID: n.ID, Name: nodeLabel(n), IP: n.IP, MAC: mac, Detail: "从 ARP 补全 MAC"})
	}

	// 2/3/4. 跟踪 IP
	owner := map[string]*topology.Node{}
	for _, n := range t.Nodes {
		if n.IP != "" {
			owner[n.IP] = n
		}
	}
	moves := map[*topology.Node]string{}
	released := map[*topology.Node]bool{}
	var moveOrder []*topology.Node
	for _, n := range t.Nodes {
		if n.MAC == "" {
			continue
		}
		ip2, ok := uniq(n.MAC)
		if !ok || ip2 == n.IP {
			continue
		}
		if !Trackable(n, opt.Tracking) {
			reason := "固定 IP（ip_mode: static）设备出现在新地址"
			if n.IPMode != "static" {
				reason = "当前跟踪范围不包含该设备（ip_tracking: " + opt.Tracking + "）"
			}
			res.Suggestions = append(res.Suggestions, Suggestion{NodeID: n.ID, Name: nodeLabel(n), CurrentIP: n.IP, SeenIP: ip2, MAC: n.MAC, Reason: reason})
			continue
		}
		moves[n] = ip2
		moveOrder = append(moveOrder, n)
	}
	// 原 IP 被其他 MAC 占用，且本设备 MAC 未出现：释放
	for _, n := range t.Nodes {
		if n.IP == "" || n.MAC == "" || !Trackable(n, opt.Tracking) {
			continue
		}
		if _, moving := moves[n]; moving {
			continue
		}
		if m := ipMAC[n.IP]; m != "" && m != n.MAC && len(macIPs[n.MAC]) == 0 {
			released[n] = true
		}
	}
	// 反复检查直到稳定：某个移动被取消后，依赖它腾出地址的其他移动也要重新判断。
	for again := true; again; {
		again = false
		for _, n := range moveOrder {
			ip2, ok := moves[n]
			if !ok {
				continue
			}
			if switchIP[ip2] {
				add(Change{Type: ChConflict, NodeID: n.ID, Name: nodeLabel(n), OldIP: n.IP, IP: ip2, MAC: n.MAC, Detail: "新地址与交换机管理 IP 相同，未修改"})
				delete(moves, n)
				again = true
				continue
			}
			o := owner[ip2]
			if o == nil || o == n {
				continue
			}
			if _, oMoving := moves[o]; oMoving || released[o] {
				continue
			}
			if Trackable(o, opt.Tracking) && ipMAC[ip2] != o.MAC {
				released[o] = true
				continue
			}
			detail := fmt.Sprintf("新地址 %s 已登记给 %s（%s）", ip2, nodeLabel(o), o.ID)
			if o.MAC == "" {
				detail += "，该节点没有 MAC，可能是同一台设备重复登记"
			} else if o.IPMode == "static" {
				detail += "，该节点为固定 IP"
			}
			add(Change{Type: ChConflict, NodeID: n.ID, Name: nodeLabel(n), OldIP: n.IP, IP: ip2, MAC: n.MAC, Detail: detail + "，未修改"})
			delete(moves, n)
			again = true
		}
	}
	for _, n := range t.Nodes {
		if released[n] {
			if _, moving := moves[n]; moving {
				continue
			}
			add(Change{Type: ChIPReleased, NodeID: n.ID, Name: nodeLabel(n), OldIP: n.IP, MAC: n.MAC,
				Detail: fmt.Sprintf("原地址已被 %s 使用，本设备未在线，暂时清空 IP（再次出现时自动恢复）", ipMAC[n.IP])})
			n.IP = ""
		}
	}
	for _, n := range moveOrder {
		ip2, ok := moves[n]
		if !ok {
			continue
		}
		add(Change{Type: ChIPChanged, NodeID: n.ID, Name: nodeLabel(n), OldIP: n.IP, IP: ip2, MAC: n.MAC,
			Detail: fmt.Sprintf("按 MAC 跟踪：%s → %s", orDash(n.IP), ip2)})
		n.IP = ip2
	}

	// 5. 新设备
	ipUsed := map[string]bool{}
	for _, n := range t.Nodes {
		if n.IP != "" {
			ipUsed[n.IP] = true
		}
	}
	for ip := range switchIP {
		ipUsed[ip] = true
	}
	for _, h := range hosts {
		if ipUsed[h.IP] {
			continue
		}
		if h.MAC != "" && (switchMAC[h.MAC] || len(h.SharedMAC) == 0 && macOwner(h.MAC) != nil) {
			continue // 已登记的设备（锁定/冲突），已在建议或冲突中说明
		}
		if h.Vendor == "" && h.MAC != "" && len(h.SharedMAC) == 0 {
			h.Vendor = VendorOf(h.MAC, vendors)
		}
		if !opt.AutoAdd {
			res.Unknown = append(res.Unknown, h)
			continue
		}
		n := NewNode(t, h, now)
		opt.Decorate(n)
		t.Nodes = append(t.Nodes, n)
		ipUsed[h.IP] = true
		add(Change{Type: ChAdded, NodeID: n.ID, Name: nodeLabel(n), IP: h.IP, MAC: h.MAC, Detail: "自动添加为未定位设备"})
	}

	// 6. 主机名：记录到 hostname 字段（不改动用户填写的 name）
	byIP := map[string]*topology.Node{}
	for _, n := range t.Nodes {
		if n.IP != "" {
			byIP[n.IP] = n
		}
	}
	for _, h := range hosts {
		if h.Hostname == "" {
			continue
		}
		n := byIP[h.IP]
		if n == nil || (n.MAC != "" && h.MAC != "" && n.MAC != h.MAC && len(h.SharedMAC) == 0) {
			continue
		}
		if strings.EqualFold(n.Hostname, h.Hostname) {
			continue
		}
		detail := "识别到主机名 " + h.Hostname
		if n.Hostname != "" {
			detail = "主机名：" + n.Hostname + " → " + h.Hostname
		}
		label := nodeLabel(n)
		n.Hostname = h.Hostname
		add(Change{Type: ChHostname, NodeID: n.ID, Name: label, IP: n.IP, MAC: n.MAC, Detail: detail})
	}
	return res
}

// NewNode 根据扫描结果创建一个“未定位”节点（有 MAC 时 id 按 MAC 生成，IP 变化后 id 不变）。
func NewNode(t *topology.Topology, h Host, now time.Time) *topology.Node {
	n := &topology.Node{
		IP:       h.IP,
		MAC:      h.MAC,
		Vendor:   h.Vendor,
		Hostname: h.Hostname,
		Flags:    []string{"unlocated", "discovered"},
		Note:     "网段扫描自动发现于 " + now.Format("2006-01-02 15:04"),
	}
	if len(h.SharedMAC) > 0 { // MAC 不唯一：不记录为本设备 MAC，按 IP 识别
		n.MAC, n.Vendor = "", ""
		n.Note = fmt.Sprintf("ARP 中与 %s 共用 MAC %s（无线中继/桥接/多地址设备），按 IP 识别；%s",
			strings.Join(h.SharedMAC, "、"), h.MAC, n.Note)
	}
	if h.Local {
		n.Note = "运行 netPing 的本机；" + n.Note
	}
	if !h.Ping && !h.Local { // 只在 ARP 中出现、不响应 ping：ping 监控只会一直显示离线，默认关闭
		off := false
		n.Monitor.Enabled = &off
		n.Flags = append(n.Flags, "arp_only")
		n.Note = "不响应 ping（仅出现在 ARP 中），已关闭 ping 监控；" + n.Note
	}
	if n.MAC != "" {
		n.ID = t.NewNodeID(&topology.Node{MAC: n.MAC})
	} else {
		n.ID = t.NewNodeID(n)
	}
	return n
}

func orDash(s string) string {
	if s == "" {
		return "（无）"
	}
	return s
}

// ---------------------------------------------------------------------------
// 厂商识别

// 少量确定的常见前缀；站点内其余厂商从拓扑已有设备中学习，或在 config.yaml discovery.oui 中补充。
var builtinOUI = map[string]string{
	"00:0C:29": "VMware", "00:50:56": "VMware", "00:05:69": "VMware",
	"00:15:5D": "Microsoft Hyper-V", "08:00:27": "VirtualBox",
	"B8:27:EB": "Raspberry Pi", "DC:A6:32": "Raspberry Pi", "E4:5F:01": "Raspberry Pi",
	"00:0E:C6": "ASIX",
}

// LearnOUI 合并内置前缀、拓扑中已有设备的前缀（多数投票）和配置中的前缀（优先级最高）。
func LearnOUI(t *topology.Topology, extra map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range builtinOUI {
		out[k] = v
	}
	votes := map[string]map[string]int{}
	note := func(mac, vendor string) {
		if mac == "" || vendor == "" || vendor == "本地私有地址" || isLocalAdmin(mac) {
			return
		}
		p := config.NormalizeOUI(mac)
		if votes[p] == nil {
			votes[p] = map[string]int{}
		}
		votes[p][vendor]++
	}
	for _, n := range t.Nodes {
		note(n.MAC, n.Vendor)
	}
	for _, s := range t.Switches {
		note(s.MAC, s.Vendor)
	}
	for p, vs := range votes {
		best, bestN := "", 0
		keys := make([]string, 0, len(vs))
		for v := range vs {
			keys = append(keys, v)
		}
		sort.Strings(keys)
		for _, v := range keys {
			if vs[v] > bestN {
				best, bestN = v, vs[v]
			}
		}
		out[p] = best
	}
	for k, v := range extra {
		if p := config.NormalizeOUI(k); p != "" && strings.TrimSpace(v) != "" {
			out[p] = strings.TrimSpace(v)
		}
	}
	return out
}

func isLocalAdmin(mac string) bool {
	hw, err := net.ParseMAC(mac)
	return err == nil && len(hw) > 0 && hw[0]&0x02 != 0
}

// VendorOf 按 MAC 前缀判断厂商；本地管理位 MAC（随机化 MAC 等）返回“本地私有地址”。
func VendorOf(mac string, table map[string]string) string {
	if mac == "" {
		return ""
	}
	if isLocalAdmin(mac) {
		return "本地私有地址"
	}
	return table[config.NormalizeOUI(mac)]
}
