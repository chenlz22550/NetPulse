package locate

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"netping/internal/snmp"
	"netping/internal/topology"
)

// Sighting 表示某个 MAC 出现在某台交换机的某个端口。
type Sighting struct {
	Switch   string `json:"switch"`
	SwName   string `json:"switch_name"`
	Port     string `json:"port"`    // 对应到 topology.yaml 的端口名
	IfName   string `json:"if_name"` // 交换机上报的端口名
	VLAN     int    `json:"vlan,omitempty"`
	PortMACs int    `json:"port_macs"` // 该端口学到的 MAC 数
	Uplink   bool   `json:"uplink"`    // 级联/上联口
	Reason   string `json:"uplink_reason,omitempty"`
}

// Change 是定位对拓扑的一项修改。
type Change struct {
	Time     time.Time `json:"time"`
	Type     string    `json:"type"` // located 自动挂到端口 / moved 端口变化（已采纳）/ location 补全物理位置
	NodeID   string    `json:"node_id"`
	Name     string    `json:"name"`
	IP       string    `json:"ip,omitempty"`
	MAC      string    `json:"mac"`
	From     string    `json:"from,omitempty"`
	To       string    `json:"to"`
	Location string    `json:"location,omitempty"`
	Detail   string    `json:"detail"`
}

// Suggestion 是已定位设备的端口变化（需要确认）。
type Suggestion struct {
	NodeID   string `json:"node_id"`
	Name     string `json:"name"`
	IP       string `json:"ip,omitempty"`
	MAC      string `json:"mac"`
	From     string `json:"from"`
	To       string `json:"to"`
	Switch   string `json:"switch"`
	Port     string `json:"port"`
	Location string `json:"location,omitempty"`
}

// Unresolved 是看到了 MAC 但无法确定端口的设备。
type Unresolved struct {
	NodeID string     `json:"node_id"`
	Name   string     `json:"name"`
	MAC    string     `json:"mac"`
	Reason string     `json:"reason"`
	Seen   []Sighting `json:"seen"`
}

// Stranger 是边缘端口上学到、但拓扑中没有登记的 MAC。
type Stranger struct {
	MAC    string `json:"mac"`
	Switch string `json:"switch"`
	Port   string `json:"port"`
	VLAN   int    `json:"vlan,omitempty"`
}

// Placement 是一个 MAC 的最终位置。
type Placement struct {
	Sighting
	All []Sighting `json:"all"`
}

// Analysis 是对所有交换机 MAC 表的分析结果（不修改拓扑）。
type Analysis struct {
	Placed     map[string]Placement  // MAC → 位置
	Ambiguous  map[string][]Sighting // MAC → 无法确定的出现位置
	UplinkOnly map[string][]Sighting // 只出现在级联口上的 MAC
	Uplinks    map[string]map[string]string
	PortMACs   map[string]map[string]int
}

// Analyze 根据各交换机的 MAC 地址表确定每个 MAC 所在端口：
//   - 级联口不算设备位置：topology.yaml 中交换机间链路的端口、聚合口、学到其他交换机 MAC 的端口；
//   - 同一 MAC 出现在多个非级联口时，取学到 MAC 最少的端口（离设备最近）；数量相同则无法确定。
func Analyze(t *topology.Topology, tables map[string]*snmp.Table) *Analysis {
	a := &Analysis{Placed: map[string]Placement{}, Ambiguous: map[string][]Sighting{}, UplinkOnly: map[string][]Sighting{},
		Uplinks: map[string]map[string]string{}, PortMACs: map[string]map[string]int{}}
	swMAC := map[string]string{}
	for _, s := range t.Switches {
		if s.MAC != "" {
			swMAC[strings.ToUpper(s.MAC)] = s.ID
		}
	}
	used := map[string][]string{}
	for _, n := range t.Nodes {
		if n.Attach.Switch != "" && n.Attach.Port != "" {
			used[n.Attach.Switch] = append(used[n.Attach.Switch], n.Attach.Port)
		}
	}
	byMAC := map[string][]Sighting{}
	seen := map[string]bool{}
	ids := make([]string, 0, len(tables))
	for id := range tables {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		tb := tables[id]
		sw := t.FindSwitch(id)
		if sw == nil || tb == nil {
			continue
		}
		up := map[string]string{}
		for _, l := range t.Links {
			if l.From.Switch == id {
				up[l.From.Port] = "交换机间链路"
			}
			if l.To.Switch == id {
				up[l.To.Port] = "交换机间链路"
			}
		}
		type hit struct {
			mac string
			s   Sighting
		}
		count := map[string]int{}
		var local []hit
		for _, e := range tb.Entries {
			if !IsPhysical(e.IfName) && !IsAggregate(e.IfName) {
				continue // VLAN 接口、CPU 等
			}
			port := MatchPort(sw, e.IfName, used[id])
			k := id + "|" + port + "|" + e.MAC
			if seen[k] {
				continue // 同一端口多个 VLAN
			}
			seen[k] = true
			count[port]++
			if IsAggregate(e.IfName) {
				up[port] = "聚合口"
			}
			if other, ok := swMAC[e.MAC]; ok && other != id {
				up[port] = "学到交换机 " + other + " 的 MAC"
			}
			local = append(local, hit{e.MAC, Sighting{Switch: id, SwName: sw.Name, Port: port, IfName: e.IfName, VLAN: e.VLAN}})
		}
		for _, h := range local {
			h.s.PortMACs = count[h.s.Port]
			if r, ok := up[h.s.Port]; ok {
				h.s.Uplink, h.s.Reason = true, r
			}
			byMAC[h.mac] = append(byMAC[h.mac], h.s)
		}
		a.Uplinks[id] = up
		a.PortMACs[id] = count
	}
	for mac, list := range byMAC {
		var edge []Sighting
		for _, s := range list {
			if !s.Uplink {
				edge = append(edge, s)
			}
		}
		if len(edge) == 0 {
			a.UplinkOnly[mac] = list
			continue
		}
		sort.Slice(edge, func(i, j int) bool { return edge[i].PortMACs < edge[j].PortMACs })
		if len(edge) > 1 && edge[0].PortMACs == edge[1].PortMACs && edge[0].Switch != edge[1].Switch {
			a.Ambiguous[mac] = list
			continue
		}
		a.Placed[mac] = Placement{Sighting: edge[0], All: list}
	}
	return a
}

// Options 控制写入范围。
type Options struct {
	ApplyMoves bool     // 已定位设备的端口变化也直接修改
	OnlyNodes  []string // 只处理这些节点（为空表示全部）
	Now        time.Time
}

// Result 是一次定位的结果。
type Result struct {
	Changes     []Change     `json:"changes"`
	Suggestions []Suggestion `json:"suggestions"`
	Unresolved  []Unresolved `json:"unresolved"`
	Strangers   []Stranger   `json:"strangers"`
	Matched     int          `json:"matched"` // 在 MAC 表中找到的已登记设备数
}

func label(t *topology.Topology, e topology.Endpoint) string {
	if e.Switch == "" {
		return "未定位"
	}
	return t.AttachLabel(e)
}

func nodeName(n *topology.Node) string {
	if n.Name != "" {
		return n.Name
	}
	if n.IP != "" {
		return n.IP
	}
	return n.ID
}

// Apply 按分析结果修改拓扑 t（就地修改）：未定位设备挂到端口；已定位设备端口变化时给出建议（或 ApplyMoves 时直接修改）。
func Apply(t *topology.Topology, a *Analysis, opt Options) Result {
	res := Result{Changes: []Change{}, Suggestions: []Suggestion{}, Unresolved: []Unresolved{}, Strangers: []Stranger{}}
	if opt.Now.IsZero() {
		opt.Now = time.Now()
	}
	only := map[string]bool{}
	for _, id := range opt.OnlyNodes {
		only[id] = true
	}
	known := map[string]bool{}
	for _, s := range t.Switches {
		if s.MAC != "" {
			known[strings.ToUpper(s.MAC)] = true
		}
	}
	for _, n := range t.Nodes {
		mac := strings.ToUpper(n.MAC)
		if mac == "" {
			continue
		}
		known[mac] = true
		if len(only) > 0 && !only[n.ID] {
			continue
		}
		p, ok := a.Placed[mac]
		if !ok {
			if s := a.Ambiguous[mac]; s != nil {
				res.Unresolved = append(res.Unresolved, Unresolved{NodeID: n.ID, Name: nodeName(n), MAC: mac, Seen: s,
					Reason: "同时出现在多台交换机的普通端口上且 MAC 数相同，无法判断（请在拓扑中补全交换机间链路）"})
			} else if s := a.UplinkOnly[mac]; s != nil {
				res.Unresolved = append(res.Unresolved, Unresolved{NodeID: n.ID, Name: nodeName(n), MAC: mac, Seen: s,
					Reason: "只出现在级联口上：设备可能接在未读取 MAC 表的交换机下"})
			}
			continue
		}
		res.Matched++
		sw := t.FindSwitch(p.Switch)
		to := topology.Endpoint{Switch: p.Switch, Port: p.Port}
		loc := ""
		if sw != nil && sw.PortLocations != nil {
			loc = sw.PortLocations[p.Port]
		}
		cur := n.Attach
		switch {
		case cur.Switch == "":
			n.Attach = to
			n.Flags = removeFlag(n.Flags, "unlocated")
			n.Position = nil
			ch := Change{Time: opt.Now, Type: "located", NodeID: n.ID, Name: nodeName(n), IP: n.IP, MAC: mac, From: "未定位", To: label(t, to),
				Detail: fmt.Sprintf("按 MAC 地址表挂到 %s（该口 %d 个 MAC）", label(t, to), p.PortMACs)}
			if loc != "" && n.Location == "" {
				n.Location = loc
				ch.Location = loc
			}
			res.Changes = append(res.Changes, ch)
		case cur.Switch == to.Switch && normPort(cur.Port) == normPort(to.Port):
			if loc != "" && n.Location == "" {
				n.Location = loc
				res.Changes = append(res.Changes, Change{Time: opt.Now, Type: "location", NodeID: n.ID, Name: nodeName(n), IP: n.IP, MAC: mac,
					To: label(t, cur), Location: loc, Detail: "按端口位置对照补全物理位置：" + loc})
			}
		default:
			if opt.ApplyMoves {
				from := label(t, cur)
				n.Attach = to
				n.Position = nil
				ch := Change{Time: opt.Now, Type: "moved", NodeID: n.ID, Name: nodeName(n), IP: n.IP, MAC: mac, From: from, To: label(t, to),
					Detail: fmt.Sprintf("端口变化：%s → %s", from, label(t, to))}
				if loc != "" {
					n.Location = loc
					ch.Location = loc
				}
				res.Changes = append(res.Changes, ch)
			} else {
				res.Suggestions = append(res.Suggestions, Suggestion{NodeID: n.ID, Name: nodeName(n), IP: n.IP, MAC: mac,
					From: label(t, cur), To: label(t, to), Switch: to.Switch, Port: to.Port, Location: loc})
			}
		}
	}
	for mac, p := range a.Placed {
		if !known[mac] {
			res.Strangers = append(res.Strangers, Stranger{MAC: mac, Switch: p.Switch, Port: p.Port, VLAN: p.VLAN})
		}
	}
	sort.Slice(res.Strangers, func(i, j int) bool {
		a, b := res.Strangers[i], res.Strangers[j]
		if a.Switch != b.Switch {
			return a.Switch < b.Switch
		}
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		return a.MAC < b.MAC
	})
	return res
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
