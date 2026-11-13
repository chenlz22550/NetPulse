// Package topology 定义机房拓扑（站点、交换机、链路、节点）及其 YAML 读写。
package topology

import (
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strings"
)

// Site 机房/网段信息。
type Site struct {
	Name        string `yaml:"name" json:"name"`
	Subnet      string `yaml:"subnet,omitempty" json:"subnet"`
	Gateway     string `yaml:"gateway,omitempty" json:"gateway"`
	Description string `yaml:"description,omitempty" json:"description"`
}

// Position 拓扑图坐标。
type Position struct {
	X float64 `yaml:"x" json:"x"`
	Y float64 `yaml:"y" json:"y"`
}

// MonitorSpec 单个节点的监控参数，零值表示继承 config.yaml 中的默认值。
type MonitorSpec struct {
	Enabled       *bool   `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Alert         bool    `yaml:"alert,omitempty" json:"alert,omitempty"` // 是否推送告警，默认关闭
	IntervalMs    int     `yaml:"interval_ms,omitempty" json:"interval_ms,omitempty"`
	TimeoutMs     int     `yaml:"timeout_ms,omitempty" json:"timeout_ms,omitempty"`
	LatencyWarnMs int     `yaml:"latency_warn_ms,omitempty" json:"latency_warn_ms,omitempty"`
	LossWarnPct   float64 `yaml:"loss_warn_pct,omitempty" json:"loss_warn_pct,omitempty"`
}

// IsEnabled 未设置时默认启用。
func (m MonitorSpec) IsEnabled() bool { return m.Enabled == nil || *m.Enabled }

// Switch 交换机。
type Switch struct {
	ID          string   `yaml:"id" json:"id"`
	Name        string   `yaml:"name" json:"name"`
	Role        string   `yaml:"role" json:"role"`
	Layer       string   `yaml:"layer,omitempty" json:"layer"`
	Vendor      string   `yaml:"vendor,omitempty" json:"vendor"`
	Model       string   `yaml:"model,omitempty" json:"model"`
	IP          string   `yaml:"ip,omitempty" json:"ip"`
	MAC         string   `yaml:"mac,omitempty" json:"mac"`
	Location    string   `yaml:"location,omitempty" json:"location"`
	Note        string   `yaml:"note,omitempty" json:"note"`
	Ports       []string `yaml:"ports,omitempty" json:"ports"`
	NodeSide    string   `yaml:"node_side,omitempty" json:"node_side"`
	NodeColumns int      `yaml:"node_columns,omitempty" json:"node_columns"`
	// PortLocations 端口 → 物理位置（如 "12": A03 机柜），端口定位时自动填入节点的 location。
	PortLocations map[string]string `yaml:"port_locations,omitempty" json:"port_locations"`
	Monitor       MonitorSpec       `yaml:"monitor,omitempty" json:"monitor"`
	Position      *Position         `yaml:"position,omitempty" json:"position"`
}

// Endpoint 交换机上的一个端口。
type Endpoint struct {
	Switch string `yaml:"switch" json:"switch"`
	Port   string `yaml:"port" json:"port"`
}

// Link 交换机之间的连线。
type Link struct {
	ID   string   `yaml:"id" json:"id"`
	From Endpoint `yaml:"from" json:"from"`
	To   Endpoint `yaml:"to" json:"to"`
	Kind string   `yaml:"kind,omitempty" json:"kind"`
	Note string   `yaml:"note,omitempty" json:"note"`
}

// Node 终端节点。
type Node struct {
	ID       string      `yaml:"id" json:"id"`
	Locked   bool        `yaml:"locked,omitempty" json:"locked"` // 上锁后不参与批量清理
	Name     string      `yaml:"name,omitempty" json:"name"`
	Hostname string      `yaml:"hostname,omitempty" json:"hostname"` // 网段扫描自动获得的主机名（NetBIOS / mDNS / 反向 DNS）
	IP       string      `yaml:"ip,omitempty" json:"ip"`
	MAC      string      `yaml:"mac,omitempty" json:"mac"`
	IPMode   string      `yaml:"ip_mode,omitempty" json:"ip_mode"` // "" 自动跟踪 / dhcp / static（锁定）
	Vendor   string      `yaml:"vendor,omitempty" json:"vendor"`
	Attach   Endpoint    `yaml:"attach" json:"attach"`
	Location string      `yaml:"location,omitempty" json:"location"`
	Flags    []string    `yaml:"flags,omitempty" json:"flags"`
	Tags     []string    `yaml:"tags,omitempty" json:"tags"`
	Note     string      `yaml:"note,omitempty" json:"note"`
	Monitor  MonitorSpec `yaml:"monitor,omitempty" json:"monitor"`
	Position *Position   `yaml:"position,omitempty" json:"position"`
}

// HasFlag 判断节点是否带某个标记。
func (n *Node) HasFlag(f string) bool {
	for _, x := range n.Flags {
		if x == f {
			return true
		}
	}
	return false
}

// DisplayName 名称为空时用 IP 或 MAC 显示。
func (n *Node) DisplayName() string {
	if n.Name != "" {
		return n.Name
	}
	if n.IP != "" {
		return n.IP
	}
	if n.MAC != "" {
		return n.MAC
	}
	return n.ID
}

// Topology 对应 topology.yaml。
type Topology struct {
	Version  int       `yaml:"version" json:"version"`
	Site     Site      `yaml:"site" json:"site"`
	Switches []*Switch `yaml:"switches" json:"switches"`
	Links    []*Link   `yaml:"links" json:"links"`
	Nodes    []*Node   `yaml:"nodes" json:"nodes"`
}

// 已知的取值，用于校验和网页下拉框。
var (
	Roles      = []string{"core", "aggregation", "access", "other"}
	NodeSides  = []string{"left", "right"}
	KnownFlags = []string{"gateway", "suspect", "unlocated", "arp_only", "jump_host", "no_ipv4", "critical", "discovered"}
	IPModes    = []string{"", "dhcp", "static"}
)

var idRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// ValidID 检查 id 是否合法（也用作历史数据目录名）。
func ValidID(id string) bool { return idRe.MatchString(id) }

// Clone 深拷贝。
func (t *Topology) Clone() *Topology {
	b, _ := json.Marshal(t)
	var c Topology
	_ = json.Unmarshal(b, &c)
	return &c
}

// FindSwitch 按 id 查找交换机。
func (t *Topology) FindSwitch(id string) *Switch {
	for _, s := range t.Switches {
		if s.ID == id {
			return s
		}
	}
	return nil
}

// FindNode 按 id 查找节点。
func (t *Topology) FindNode(id string) *Node {
	for _, n := range t.Nodes {
		if n.ID == id {
			return n
		}
	}
	return nil
}

// FindLink 按 id 查找链路。
func (t *Topology) FindLink(id string) *Link {
	for _, l := range t.Links {
		if l.ID == id {
			return l
		}
	}
	return nil
}

// AttachLabel 返回 “交换机名 端口” 形式的接线位置描述。
func (t *Topology) AttachLabel(e Endpoint) string {
	if e.Switch == "" {
		return "未定位"
	}
	name := e.Switch
	if s := t.FindSwitch(e.Switch); s != nil && s.Name != "" {
		name = s.Name
	}
	if e.Port == "" {
		return name
	}
	return name + " " + e.Port
}

func idFromIP(prefix, ip string) string {
	return prefix + strings.ReplaceAll(ip, ".", "-")
}

// NewNodeID 为节点生成一个不冲突的 id。
func (t *Topology) NewNodeID(n *Node) string {
	base := "node"
	switch {
	case n.IP != "":
		base = idFromIP("n-", n.IP)
	case n.MAC != "":
		base = "n-" + strings.ToLower(strings.NewReplacer(":", "", "-", "", ".", "").Replace(n.MAC))
	}
	return t.uniqueID(base)
}

// NewSwitchID 为交换机生成一个不冲突的 id。
func (t *Topology) NewSwitchID(s *Switch) string {
	base := "sw"
	if s.IP != "" {
		base = idFromIP("sw-", s.IP)
	}
	return t.uniqueID(base)
}

func (t *Topology) uniqueID(base string) string {
	used := t.usedIDs()
	if !used[base] {
		return base
	}
	for i := 2; ; i++ {
		id := fmt.Sprintf("%s-%d", base, i)
		if !used[id] {
			return id
		}
	}
}

func (t *Topology) usedIDs() map[string]bool {
	m := map[string]bool{}
	for _, s := range t.Switches {
		m[s.ID] = true
	}
	for _, n := range t.Nodes {
		m[n.ID] = true
	}
	for _, l := range t.Links {
		m[l.ID] = true
	}
	return m
}

// Normalize 补齐缺省值（空 id、版本号、链路 id 等），方便手写/AI 生成的 YAML 直接使用。
func (t *Topology) Normalize() {
	if t.Version == 0 {
		t.Version = 1
	}
	for _, s := range t.Switches {
		s.ID = strings.TrimSpace(s.ID)
		s.IP = strings.TrimSpace(s.IP)
		s.MAC = strings.ToUpper(strings.TrimSpace(s.MAC))
		if s.ID == "" {
			s.ID = t.NewSwitchID(s)
		}
		if s.Role == "" {
			s.Role = "access"
		}
		if s.NodeSide == "" {
			s.NodeSide = "right"
		}
		if s.NodeColumns <= 0 {
			s.NodeColumns = 2
		}
	}
	for _, n := range t.Nodes {
		n.ID = strings.TrimSpace(n.ID)
		n.IP = strings.TrimSpace(n.IP)
		n.MAC = strings.ToUpper(strings.TrimSpace(n.MAC))
		n.Hostname = strings.TrimSpace(n.Hostname)
		n.Attach.Switch = strings.TrimSpace(n.Attach.Switch)
		n.Attach.Port = strings.TrimSpace(n.Attach.Port)
		if n.ID == "" {
			n.ID = t.NewNodeID(n)
		}
	}
	for _, l := range t.Links {
		if l.ID == "" {
			l.ID = t.uniqueID("link-" + l.From.Switch + "-" + l.To.Switch)
		}
		if l.Kind == "" {
			l.Kind = "uplink"
		}
	}
	if t.Switches == nil {
		t.Switches = []*Switch{}
	}
	if t.Links == nil {
		t.Links = []*Link{}
	}
	if t.Nodes == nil {
		t.Nodes = []*Node{}
	}
}

func checkIP(field, ip string) error {
	if ip == "" {
		return nil
	}
	p := net.ParseIP(ip)
	if p == nil || p.To4() == nil {
		return fmt.Errorf("%s: %q 不是有效的 IPv4 地址", field, ip)
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Validate 检查拓扑一致性，返回所有发现的问题。
func (t *Topology) Validate() []string {
	var errs []string
	add := func(format string, a ...interface{}) { errs = append(errs, fmt.Sprintf(format, a...)) }
	ids := map[string]string{}
	claim := func(kind, id string) {
		if !ValidID(id) {
			add("%s id %q 不合法：只能包含字母、数字、_ . -，且以字母或数字开头，最长 64 字符", kind, id)
		}
		if prev, ok := ids[id]; ok {
			add("id %q 重复（%s 与 %s）", id, prev, kind)
		}
		ids[id] = kind
	}
	ips := map[string]string{}
	for _, s := range t.Switches {
		claim("交换机", s.ID)
		if s.IP != "" {
			if prev, ok := ips[s.IP]; ok {
				add("IP %s 同时出现在 %s 和 %s", s.IP, prev, s.ID)
			}
			ips[s.IP] = s.ID
		}
		if !contains(Roles, s.Role) {
			add("交换机 %s: role 必须是 %s 之一", s.ID, strings.Join(Roles, "/"))
		}
		if s.NodeSide != "" && !contains(NodeSides, s.NodeSide) {
			add("交换机 %s: node_side 必须是 left 或 right", s.ID)
		}
		if err := checkIP("交换机 "+s.ID+" ip", s.IP); err != nil {
			add("%v", err)
		}
	}
	for _, n := range t.Nodes {
		claim("节点", n.ID)
		if err := checkIP("节点 "+n.ID+" ip", n.IP); err != nil {
			add("%v", err)
		}
		if n.IP != "" {
			if prev, ok := ips[n.IP]; ok {
				add("IP %s 同时出现在节点 %s 和 %s", n.IP, prev, n.ID)
			}
			ips[n.IP] = n.ID
		}
		if n.Attach.Switch != "" && t.FindSwitch(n.Attach.Switch) == nil {
			add("节点 %s: attach.switch %q 不存在", n.ID, n.Attach.Switch)
		}
		if n.Attach.Switch == "" && n.Attach.Port != "" {
			add("节点 %s: 填写了 attach.port 但没有 attach.switch", n.ID)
		}
		if !contains(IPModes, n.IPMode) {
			add("节点 %s: ip_mode 只能是 dhcp、static 或留空（自动跟踪）", n.ID)
		}
		if n.Monitor.IntervalMs != 0 && n.Monitor.IntervalMs < 200 {
			add("节点 %s: monitor.interval_ms 不能小于 200", n.ID)
		}
	}
	for _, l := range t.Links {
		claim("链路", l.ID)
		for _, e := range []Endpoint{l.From, l.To} {
			if t.FindSwitch(e.Switch) == nil {
				add("链路 %s: 交换机 %q 不存在", l.ID, e.Switch)
			}
		}
		if l.From.Switch == l.To.Switch {
			add("链路 %s: 两端不能是同一台交换机", l.ID)
		}
	}
	return errs
}

// ValidationError 汇总多个校验问题。
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	return "拓扑校验失败:\n- " + strings.Join(e.Problems, "\n- ")
}
