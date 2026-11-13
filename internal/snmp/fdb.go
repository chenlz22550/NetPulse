package snmp

import (
	"fmt"
	"sort"
	"strings"
)

// 标准 MIB（BRIDGE-MIB / Q-BRIDGE-MIB / IF-MIB），锐捷、华为、H3C、思科等交换机均支持。
var (
	oidSysDescr        = ParseOID("1.3.6.1.2.1.1.1.0")
	oidSysName         = ParseOID("1.3.6.1.2.1.1.5.0")
	OidIfName          = ParseOID("1.3.6.1.2.1.31.1.1.1.1")
	OidIfDescr         = ParseOID("1.3.6.1.2.1.2.2.1.2")
	OidBasePortIfIndex = ParseOID("1.3.6.1.2.1.17.1.4.1.2")
	OidQFdbPort        = ParseOID("1.3.6.1.2.1.17.7.1.2.2.1.2")
	OidQFdbStatus      = ParseOID("1.3.6.1.2.1.17.7.1.2.2.1.3")
	OidFdbPort         = ParseOID("1.3.6.1.2.1.17.4.3.1.2")
	OidFdbStatus       = ParseOID("1.3.6.1.2.1.17.4.3.1.3")
)

// FDBEntry 是 MAC 地址表的一条记录。
type FDBEntry struct {
	MAC     string `json:"mac"`
	VLAN    int    `json:"vlan,omitempty"`
	IfIndex int    `json:"if_index"`
	IfName  string `json:"if_name"`
}

// Table 是从一台交换机读到的信息。
type Table struct {
	SysName  string         `json:"sys_name"`
	SysDescr string         `json:"sys_descr"`
	IfNames  map[int]string `json:"if_names"`
	Entries  []FDBEntry     `json:"entries"`
	Source   string         `json:"source"` // q-bridge / bridge
	Warnings []string       `json:"warnings,omitempty"`
}

func macFromOID(o OID) string {
	if len(o) < 6 {
		return ""
	}
	m := o[len(o)-6:]
	for _, x := range m {
		if x > 255 {
			return ""
		}
	}
	if m[0]&1 == 1 { // 组播
		return ""
	}
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", m[0], m[1], m[2], m[3], m[4], m[5])
}

func keyOf(o OID, root OID) string { return o[len(root):].String() }

// Probe 只读取系统名称与描述，用于测试连接。
func (c *Client) Probe() (name, descr string, err error) {
	vars, err := c.Get(oidSysName, oidSysDescr)
	if err != nil {
		return "", "", err
	}
	for _, v := range vars {
		if v.OID.cmp(oidSysName) == 0 {
			name = v.String()
		} else if v.OID.cmp(oidSysDescr) == 0 {
			descr = strings.TrimSpace(v.String())
		}
	}
	return name, descr, nil
}

// ReadTable 读取端口名和 MAC 地址表（优先 Q-BRIDGE-MIB，按 VLAN；否则 BRIDGE-MIB）。
func (c *Client) ReadTable() (*Table, error) {
	t := &Table{IfNames: map[int]string{}}
	var err error
	if t.SysName, t.SysDescr, err = c.Probe(); err != nil {
		return nil, err
	}
	walkInts := func(root OID) (map[string]int64, error) {
		m := map[string]int64{}
		err := c.Walk(root, func(v Var) bool { m[keyOf(v.OID, root)] = v.Int; return true })
		return m, err
	}
	walkStrs := func(root OID) (map[int]string, error) {
		m := map[int]string{}
		err := c.Walk(root, func(v Var) bool {
			if len(v.OID) == len(root)+1 {
				m[int(v.OID[len(root)])] = strings.TrimSpace(v.String())
			}
			return true
		})
		return m, err
	}
	if t.IfNames, err = walkStrs(OidIfName); err != nil {
		return nil, fmt.Errorf("读取端口名失败: %w", err)
	}
	if len(t.IfNames) == 0 {
		if t.IfNames, err = walkStrs(OidIfDescr); err != nil {
			return nil, fmt.Errorf("读取端口名失败: %w", err)
		}
	}
	basePort, err := walkInts(OidBasePortIfIndex)
	if err != nil {
		return nil, fmt.Errorf("读取网桥端口失败: %w", err)
	}
	ifIndexOf := func(bp int64) int {
		if v, ok := basePort[fmt.Sprint(bp)]; ok {
			return int(v)
		}
		return int(bp) // 部分设备网桥端口号即 ifIndex
	}
	usable := func(status map[string]int64, k string) bool {
		s, ok := status[k]
		return !ok || s == 1 || s == 3 // other / learned；排除 invalid、self、mgmt
	}

	ports, err := walkInts(OidQFdbPort)
	if err != nil {
		return nil, fmt.Errorf("读取 MAC 地址表失败: %w", err)
	}
	status := map[string]int64{}
	if len(ports) > 0 {
		t.Source = "q-bridge"
		status, _ = walkInts(OidQFdbStatus)
	} else {
		t.Source = "bridge"
		if ports, err = walkInts(OidFdbPort); err != nil {
			return nil, fmt.Errorf("读取 MAC 地址表失败: %w", err)
		}
		status, _ = walkInts(OidFdbStatus)
	}
	for k, bp := range ports {
		if bp <= 0 || !usable(status, k) {
			continue
		}
		o := ParseOID(k)
		mac := macFromOID(o)
		if mac == "" {
			continue
		}
		e := FDBEntry{MAC: mac, IfIndex: ifIndexOf(bp)}
		if t.Source == "q-bridge" && len(o) == 7 {
			e.VLAN = int(o[0])
		}
		e.IfName = t.IfNames[e.IfIndex]
		if e.IfName == "" {
			e.IfName = fmt.Sprintf("ifIndex %d", e.IfIndex)
		}
		t.Entries = append(t.Entries, e)
	}
	if len(t.Entries) == 0 {
		t.Warnings = append(t.Warnings, "MAC 地址表为空：交换机可能未开放 BRIDGE-MIB，或该团体名的视图（view）不包含 1.3.6.1.2.1.17")
	}
	sort.Slice(t.Entries, func(i, j int) bool {
		if t.Entries[i].IfIndex != t.Entries[j].IfIndex {
			return t.Entries[i].IfIndex < t.Entries[j].IfIndex
		}
		return t.Entries[i].MAC < t.Entries[j].MAC
	})
	return t, nil
}
