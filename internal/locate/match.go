// Package locate 通过 SNMP 读取交换机 MAC 地址表，确定每台设备实际接在哪台交换机的哪个端口。
package locate

import (
	"regexp"
	"strings"

	"netping/internal/topology"
)

// 端口名长写 → 短写（按长度从长到短匹配）。覆盖锐捷、华为、H3C、思科常见命名。
var portPrefixes = [][2]string{
	{"hundredgigabitethernet", "hu"}, {"hundredgige", "hu"},
	{"fortygigabitethernet", "fo"}, {"fortygige", "fo"},
	{"twentyfivegigabitethernet", "tf"}, {"twentyfivegige", "tf"}, {"tfgigabitethernet", "tf"},
	{"ten-gigabitethernet", "te"}, {"tengigabitethernet", "te"}, {"tengige", "te"},
	{"xgigabitethernet", "xge"}, {"multigigabitethernet", "mgi"},
	{"gigabitethernet", "gi"}, {"gige", "gi"}, {"fastethernet", "fa"},
	{"aggregateport", "ag"}, {"port-channel", "po"}, {"eth-trunk", "eth-trunk"}, {"bridge-aggregation", "bagg"},
	{"ethernet", "eth"},
}

// 物理端口的短前缀（只有这些才允许按尾部编号匹配纯数字端口名）。
var physical = map[string]bool{"hu": true, "fo": true, "tf": true, "te": true, "xge": true, "mgi": true, "gi": true, "fa": true, "eth": true, "ge": true, "xe": true}

var shortRe = regexp.MustCompile(`^([a-z-]+)\s*([0-9].*)?$`)

// normPort 规范化端口名：小写、去空格、长写转短写。如 "GigabitEthernet 0/1" → "gi0/1"，"Gi0/1" → "gi0/1"。
func normPort(name string) string {
	s := strings.ToLower(strings.Join(strings.Fields(name), ""))
	for _, p := range portPrefixes {
		if strings.HasPrefix(s, p[0]) {
			return p[1] + s[len(p[0]):]
		}
	}
	// 已是短写：ge0/0/1、xe0/0/1、te0/1 等
	return s
}

// kindOf 返回规范化端口名的字母前缀，如 gi、te、ag。
func kindOf(norm string) string {
	if m := shortRe.FindStringSubmatch(norm); m != nil {
		return m[1]
	}
	return ""
}

// IsAggregate 判断是否为聚合口（通常是上联）。
func IsAggregate(ifName string) bool {
	k := kindOf(normPort(ifName))
	return k == "ag" || k == "po" || k == "eth-trunk" || k == "bagg"
}

// IsPhysical 判断是否为物理以太网口（排除 VLAN 接口、环回、管理口等）。
func IsPhysical(ifName string) bool {
	return physical[kindOf(normPort(ifName))]
}

var trailingNum = regexp.MustCompile(`(\d+)$`)

// MatchPort 把交换机上报的端口名对应到 topology.yaml 中该交换机的端口名：
//  1. 规范化后与已声明端口相同（"GigabitEthernet 0/1" ↔ "Gi0/1"）；
//  2. 已声明端口是纯数字（如二层交换机 1~26）时，按物理端口的尾部编号匹配（"Gi0/12" → "12"）；
//  3. 都不匹配时返回短写形式（"Gi0/12"、"Te0/25"），或在交换机未声明端口但已有节点使用纯数字端口时返回编号。
func MatchPort(sw *topology.Switch, ifName string, usedPorts []string) string {
	n := normPort(ifName)
	for _, p := range sw.Ports {
		if normPort(p) == n {
			return p
		}
	}
	for _, p := range usedPorts {
		if normPort(p) == n {
			return p
		}
	}
	numeric := func(list []string) bool {
		if len(list) == 0 {
			return false
		}
		for _, p := range list {
			if trailingNum.FindString(p) != p {
				return false
			}
		}
		return true
	}
	if IsPhysical(ifName) {
		if m := trailingNum.FindString(n); m != "" && (numeric(sw.Ports) || (len(sw.Ports) == 0 && numeric(usedPorts))) {
			return m
		}
	}
	// 短写：首字母大写
	if m := shortRe.FindStringSubmatch(n); m != nil && m[2] != "" {
		k := m[1]
		return strings.ToUpper(k[:1]) + k[1:] + m[2]
	}
	return strings.TrimSpace(ifName)
}
