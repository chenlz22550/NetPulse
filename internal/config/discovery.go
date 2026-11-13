package config

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"
)

var jobIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

// FindJob 按 id 查找扫描任务。
func (d Discovery) FindJob(id string) (ScanJob, bool) {
	for _, j := range d.Jobs {
		if j.ID == id {
			return j, true
		}
	}
	return ScanJob{}, false
}

// 每个网段最多 /20（4094 台），总计最多 8192 个地址，避免误配置导致长时间扫描。
const (
	maxHostsTotal = 8192
)

// Validate 检查扫描配置。
func (d Discovery) Validate() error {
	switch d.IPTracking {
	case "", "all", "dhcp", "off":
	default:
		return fmt.Errorf("discovery.ip_tracking 只能是 all、dhcp 或 off")
	}
	ids := map[string]bool{}
	for _, j := range d.Jobs {
		if !jobIDRe.MatchString(j.ID) {
			return fmt.Errorf("扫描任务 id %q 只能包含字母、数字、- 和 _", j.ID)
		}
		if ids[j.ID] {
			return fmt.Errorf("扫描任务 id %q 重复", j.ID)
		}
		ids[j.ID] = true
		if j.IntervalMin < 5 {
			return fmt.Errorf("扫描任务「%s」的间隔不能小于 5 分钟", j.Name)
		}
		if _, err := ExpandSubnets(j.Subnets, d.Exclude); err != nil {
			return fmt.Errorf("扫描任务「%s」：%v", j.Name, err)
		}
	}
	if _, err := parseExclude(d.Exclude); err != nil {
		return err
	}
	for k := range d.OUI {
		if NormalizeOUI(k) == "" {
			return fmt.Errorf("oui 前缀 %q 格式应为 AA:BB:CC", k)
		}
	}
	return nil
}

// NormalizeOUI 把 "60-d7-e3" / "60d7e3" / "60:D7:E3:xx..." 规范为 "60:D7:E3"，无效时返回空字符串。
func NormalizeOUI(s string) string {
	h := strings.ToUpper(strings.NewReplacer(":", "", "-", "", ".", "", " ", "").Replace(s))
	if len(h) < 6 {
		return ""
	}
	for _, c := range h[:6] {
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'F') {
			return ""
		}
	}
	return h[0:2] + ":" + h[2:4] + ":" + h[4:6]
}

func parseExclude(list []string) (func(netip.Addr) bool, error) {
	var prefixes []netip.Prefix
	for _, x := range list {
		if strings.Contains(x, "/") {
			p, err := netip.ParsePrefix(x)
			if err != nil {
				return nil, fmt.Errorf("排除项 %q 不是有效的网段", x)
			}
			prefixes = append(prefixes, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(x)
		if err != nil || !a.Is4() {
			return nil, fmt.Errorf("排除项 %q 不是有效的 IPv4 地址", x)
		}
		prefixes = append(prefixes, netip.PrefixFrom(a, 32))
	}
	return func(a netip.Addr) bool {
		for _, p := range prefixes {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}, nil
}

// ExpandSubnets 展开网段中的主机地址（去掉网络地址和广播地址、排除项），按顺序去重。
func ExpandSubnets(subnets, exclude []string) ([]string, error) {
	excluded, err := parseExclude(exclude)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range subnets {
		p, err := netip.ParsePrefix(strings.TrimSpace(s))
		if err != nil || !p.Addr().Is4() {
			return nil, fmt.Errorf("网段 %q 格式应为 CIDR，如 192.168.1.0/24", s)
		}
		p = p.Masked()
		bits := 32 - p.Bits()
		if bits > 12 {
			return nil, fmt.Errorf("网段 %s 太大（最多 /20）", s)
		}
		n := 1 << bits
		a := p.Addr()
		for i := 0; i < n; i++ {
			if bits >= 2 && (i == 0 || i == n-1) { // 跳过网络地址与广播地址
				a = a.Next()
				continue
			}
			if !excluded(a) && !seen[a.String()] {
				seen[a.String()] = true
				out = append(out, a.String())
			}
			a = a.Next()
		}
		if len(out) > maxHostsTotal {
			return nil, fmt.Errorf("扫描地址总数超过 %d", maxHostsTotal)
		}
	}
	return out, nil
}

// InSubnets 判断 ip 是否属于任一网段。
func InSubnets(ip string, subnets []string) bool {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	for _, s := range subnets {
		if p, err := netip.ParsePrefix(strings.TrimSpace(s)); err == nil && p.Masked().Contains(a) {
			return true
		}
	}
	return false
}

// Excluded 判断 ip 是否在排除列表中。
func Excluded(ip string, exclude []string) bool {
	f, err := parseExclude(exclude)
	if err != nil {
		return false
	}
	a, err := netip.ParseAddr(ip)
	return err == nil && f(a)
}
