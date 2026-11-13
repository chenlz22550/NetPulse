package discovery

import (
	"fmt"
	"regexp"
	"strings"
)

// ARPEntry 是系统 ARP 表中的一条记录。
type ARPEntry struct {
	IP  string
	MAC string
}

// normalizeMAC 规范为大写冒号分隔，补齐前导 0；无效、全 0、广播或组播地址返回空字符串。
func normalizeMAC(s string) string {
	s = strings.TrimSpace(s)
	var parts []string
	switch {
	case strings.Contains(s, ":"):
		parts = strings.Split(s, ":")
	case strings.Contains(s, "-"):
		parts = strings.Split(s, "-")
	case len(s) == 12:
		for i := 0; i < 12; i += 2 {
			parts = append(parts, s[i:i+2])
		}
	}
	if len(parts) != 6 {
		return ""
	}
	var b [6]byte
	for i, p := range parts {
		var v int
		if _, err := fmt.Sscanf(p, "%x", &v); err != nil || v < 0 || v > 255 || len(p) == 0 || len(p) > 2 {
			return ""
		}
		b[i] = byte(v)
	}
	return macString(b[:])
}

func macString(b []byte) string {
	if len(b) != 6 {
		return ""
	}
	zero, bcast := true, true
	for _, x := range b {
		if x != 0 {
			zero = false
		}
		if x != 0xff {
			bcast = false
		}
	}
	if zero || bcast || b[0]&1 == 1 { // 组播位
		return ""
	}
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", b[0], b[1], b[2], b[3], b[4], b[5])
}

// arpEmptyHint 在有设备在线却读不到任何 ARP 记录时提示用户（macOS 本地网络权限）。
var arpEmptyHint = ""

var arpLineRe = regexp.MustCompile(`\(([0-9.]+)\) at ([0-9a-fA-F:]+)`)

// parseARPText 解析 `arp -an` 格式的文本。
func parseARPText(text string) []ARPEntry {
	var out []ARPEntry
	for _, m := range arpLineRe.FindAllStringSubmatch(text, -1) {
		if mac := normalizeMAC(m[2]); mac != "" {
			out = append(out, ARPEntry{IP: m[1], MAC: mac})
		}
	}
	return out
}
