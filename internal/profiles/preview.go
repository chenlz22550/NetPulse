package profiles

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"netping/internal/config"
	"netping/internal/monitor"
	"netping/internal/pinger"
	"netping/internal/topology"
)

const (
	previewMaxProbeTargets = 5
	previewProbeTimeout    = 800 * time.Millisecond
)

// PreviewSummary summarizes the topology and runtime configuration of a target profile.
type PreviewSummary struct {
	Switches        int    `json:"switches"`
	Nodes           int    `json:"nodes"`
	Links           int    `json:"links"`
	Monitored       int    `json:"monitored"`
	SiteName        string `json:"site_name"`
	Subnet          string `json:"subnet"`
	Gateway         string `json:"gateway"`
	AlertEnabled    bool   `json:"alert_enabled"`
	ScheduleEnabled bool   `json:"schedule_enabled"`
	InScheduleNow   bool   `json:"in_schedule_now"`
	DiscoveryJobs   int    `json:"discovery_jobs"`
	LocateSwitches  int    `json:"locate_switches"`
}

// ConnectivityCheck describes the pre-activation probe outcome for a key host in the target room.
type ConnectivityCheck struct {
	IP     string `json:"ip"`
	Status string `json:"status"` // "reachable" | "unreachable" | "unknown"
	Detail string `json:"detail"`
}

// PreviewResult contains pre-activation validation, scan ranges, masked alert destinations, and connectivity checks.
type PreviewResult struct {
	ID                       string                           `json:"id"`
	Name                     string                           `json:"name"`
	AlreadyActive            bool                             `json:"already_active"`
	CanActivate              bool                             `json:"can_activate"`
	Busy                     bool                             `json:"busy"`
	BusyReason               string                           `json:"busy_reason"`
	Summary                  PreviewSummary                   `json:"summary"`
	ScanRanges               []string                         `json:"scan_ranges"`
	NotificationDestinations []config.NotificationDestination `json:"notification_destinations"`
	Connectivity             []ConnectivityCheck              `json:"connectivity"`
	Errors                   []string                         `json:"errors"`
	Warnings                 []string                         `json:"warnings"`
}

// Preview evaluates whether profile id is ready to activate without mutating runtime state.
func (m *Manager) Preview(id string, probe bool) (*PreviewResult, error) {
	m.mu.RLock()
	idx := m.findIdxLocked(id)
	if idx < 0 {
		m.mu.RUnlock()
		return nil, ErrNotFound
	}
	meta := m.reg.Profiles[idx]
	activeID := m.reg.ActiveID
	m.mu.RUnlock()

	res := &PreviewResult{
		ID:                       meta.ID,
		Name:                     meta.Name,
		AlreadyActive:            meta.ID == activeID,
		ScanRanges:               []string{},
		NotificationDestinations: []config.NotificationDestination{},
		Connectivity:             []ConnectivityCheck{},
		Errors:                   []string{},
		Warnings:                 []string{},
	}

	if m.disc != nil && m.disc.Running() {
		res.Busy = true
		res.BusyReason = "正在执行网段扫描，请等待完成后再激活"
	} else if m.loc != nil && m.loc.Running() {
		res.Busy = true
		res.BusyReason = "正在执行端口定位，请等待完成后再激活"
	}

	t, err := m.loadRoomTopology(id)
	if err != nil {
		res.Errors = append(res.Errors, "拓扑配置无效: "+err.Error())
		return res, nil
	}

	rs, err := m.loadRoomSettings(id)
	if err != nil {
		res.Errors = append(res.Errors, "机房设置无效: "+err.Error())
	} else {
		rs.Normalize()
		if verr := rs.Validate(); verr != nil {
			res.Errors = append(res.Errors, "机房设置校验失败: "+verr.Error())
		}
	}

	targets := monitor.BuildTargets(t, rs.Ping)
	monitoredCount := 0
	for _, tg := range targets {
		if tg.Kind != "tcp" {
			monitoredCount++
		}
	}

	res.Summary = PreviewSummary{
		Switches:        len(t.Switches),
		Nodes:           len(t.Nodes),
		Links:           len(t.Links),
		Monitored:       monitoredCount,
		SiteName:        t.Site.Name,
		Subnet:          t.Site.Subnet,
		Gateway:         t.Site.Gateway,
		AlertEnabled:    rs.Alert.Enabled,
		ScheduleEnabled: rs.Schedule.Enabled,
		InScheduleNow:   rs.Schedule.Active(time.Now()),
		DiscoveryJobs:   len(rs.Discovery.Jobs),
		LocateSwitches:  len(rs.Locate.Switches),
	}

	res.ScanRanges = config.ScanRanges(rs.Discovery, t.Site.Subnet)
	res.NotificationDestinations = config.NotificationDestinations(rs.Alert)

	if len(res.ScanRanges) == 0 && rs.Discovery.Enabled {
		res.Warnings = append(res.Warnings, "未配置扫描网段且拓扑缺少 site.subnet，自动扫描将不会覆盖任何地址")
	}
	if rs.Alert.Enabled && len(res.NotificationDestinations) == 0 {
		res.Warnings = append(res.Warnings, "已开启告警但未配置通知渠道")
	}
	if !rs.Alert.Enabled && len(res.NotificationDestinations) > 0 {
		res.Warnings = append(res.Warnings, "已配置通知渠道但告警总开关当前处于关闭状态")
	}
	if rs.Schedule.Enabled && !res.Summary.InScheduleNow {
		res.Warnings = append(res.Warnings, "当前不在该机房的监控时段内，激活后将暂不发送 ping 与告警")
	}
	if rs.Locate.Enabled && rs.Locate.Community == "" && len(rs.Locate.Switches) == 0 && len(t.Switches) > 0 {
		res.Warnings = append(res.Warnings, "未配置 SNMP 只读团体名，端口定位将跳过交换机")
	}

	if probe {
		res.Connectivity = m.probeRoomConnectivity(t)
		for _, ck := range res.Connectivity {
			if ck.Status == "unreachable" {
				res.Warnings = append(res.Warnings, fmt.Sprintf("连通性预检：%s 不可达（%s）", ck.IP, ck.Detail))
			}
		}
	}

	res.CanActivate = !res.Busy && len(res.Errors) == 0
	return res, nil
}

func (m *Manager) probeRoomConnectivity(t *topology.Topology) []ConnectivityCheck {
	type candidate struct {
		ip    string
		label string
	}
	seen := map[string]bool{}
	var cands []candidate
	add := func(ip, label string) {
		ip = strings.TrimSpace(ip)
		if ip == "" || seen[ip] || len(cands) >= previewMaxProbeTargets {
			return
		}
		seen[ip] = true
		cands = append(cands, candidate{ip: ip, label: label})
	}
	if t.Site.Gateway != "" {
		add(t.Site.Gateway, "网关")
	}
	for _, sw := range t.Switches {
		if sw.Role == "core" && sw.IP != "" {
			add(sw.IP, "核心交换机 "+sw.Name)
		}
	}
	for _, sw := range t.Switches {
		if sw.IP != "" {
			add(sw.IP, "交换机 "+sw.Name)
		}
	}
	if len(cands) == 0 {
		return []ConnectivityCheck{{
			IP:     "",
			Status: "unknown",
			Detail: "拓扑未配置网关或带管理 IP 的交换机",
		}}
	}

	probeFn := m.probeFunc
	if probeFn == nil {
		probeFn = defaultPingProbe
	}

	out := make([]ConnectivityCheck, len(cands))
	var wg sync.WaitGroup
	for i, c := range cands {
		wg.Add(1)
		go func(idx int, cand candidate) {
			defer wg.Done()
			status, detail := probeFn(cand.ip, previewProbeTimeout)
			if cand.label != "" {
				detail = cand.label + "：" + detail
			}
			out[idx] = ConnectivityCheck{
				IP:     cand.ip,
				Status: status,
				Detail: detail,
			}
		}(i, c)
	}
	wg.Wait()
	return out
}

func defaultPingProbe(ip string, timeout time.Duration) (string, string) {
	p, err := pinger.New()
	if err != nil {
		return "unknown", "无法创建 ICMP 探测（" + err.Error() + "）"
	}
	defer p.Close()
	res, err := p.Ping(ip, timeout, 32)
	if err != nil {
		return "unknown", err.Error()
	}
	if !res.OK {
		return "unreachable", fmt.Sprintf("超时（>%dms）", timeout.Milliseconds())
	}
	return "reachable", fmt.Sprintf("延迟 %.2f ms", float64(res.RTT.Microseconds())/1000.0)
}
