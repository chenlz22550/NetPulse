package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// RoomSettings holds per-room settings that are isolated across datacenter profiles.
// Global fields (listen, auth, topology_file, retention_days, mcp, ai) never belong here.
type RoomSettings struct {
	Schedule  Schedule     `yaml:"schedule" json:"schedule"`
	Discovery Discovery    `yaml:"discovery" json:"discovery"`
	Locate    Locate       `yaml:"locate" json:"locate"`
	Ping      PingDefaults `yaml:"ping" json:"ping"`
	Alert     AlertConfig  `yaml:"alert" json:"alert"`
}

// DefaultRoomSettings returns default per-room settings.
func DefaultRoomSettings() RoomSettings {
	return Default().RoomSettings()
}

// RoomSettings extracts a deep copy of the per-room subset from Config.
func (c Config) RoomSettings() RoomSettings {
	cc := c.Clone()
	return RoomSettings{
		Schedule:  cc.Schedule,
		Discovery: cc.Discovery,
		Locate:    cc.Locate,
		Ping:      cc.Ping,
		Alert:     cc.Alert,
	}
}

// ApplyRoomSettings applies a deep copy of rs onto c without touching global fields.
func (c *Config) ApplyRoomSettings(rs RoomSettings) {
	rs = rs.Clone()
	c.Schedule = rs.Schedule
	c.Discovery = rs.Discovery
	c.Locate = rs.Locate
	c.Ping = rs.Ping
	c.Alert = rs.Alert
}

// Clone returns a deep copy of RoomSettings.
func (rs RoomSettings) Clone() RoomSettings {
	c := Config{
		Schedule:  rs.Schedule,
		Discovery: rs.Discovery,
		Locate:    rs.Locate,
		Ping:      rs.Ping,
		Alert:     rs.Alert,
	}.Clone()
	return RoomSettings{
		Schedule:  c.Schedule,
		Discovery: c.Discovery,
		Locate:    c.Locate,
		Ping:      c.Ping,
		Alert:     c.Alert,
	}
}

// Normalize fills defaults and cleans fields for per-room settings.
func (rs *RoomSettings) Normalize() {
	d := Default()
	if rs.Ping.IntervalMs < 200 {
		rs.Ping.IntervalMs = d.Ping.IntervalMs
	}
	if rs.Ping.TimeoutMs < 100 {
		rs.Ping.TimeoutMs = d.Ping.TimeoutMs
	}
	if rs.Ping.PacketSize <= 0 || rs.Ping.PacketSize > 65500 {
		rs.Ping.PacketSize = d.Ping.PacketSize
	}
	if rs.Ping.LatencyWarnMs <= 0 {
		rs.Ping.LatencyWarnMs = d.Ping.LatencyWarnMs
	}
	if rs.Ping.LossWarnPct <= 0 || rs.Ping.LossWarnPct > 100 {
		rs.Ping.LossWarnPct = d.Ping.LossWarnPct
	}
	if rs.Schedule.Windows == nil {
		rs.Schedule.Windows = []Window{}
	}
	for i := range rs.Schedule.Windows {
		w := &rs.Schedule.Windows[i]
		w.Start, w.End = strings.TrimSpace(w.Start), strings.TrimSpace(w.End)
	}
	a := &rs.Alert
	if a.DownAfterFailures <= 0 {
		a.DownAfterFailures = d.Alert.DownAfterFailures
	}
	if a.UpAfterSuccesses <= 0 {
		a.UpAfterSuccesses = d.Alert.UpAfterSuccesses
	}
	if a.LossDurationMin <= 0 {
		a.LossDurationMin = d.Alert.LossDurationMin
	}
	if a.LatencyDurationMin <= 0 {
		a.LatencyDurationMin = d.Alert.LatencyDurationMin
	}
	if a.RepeatIntervalMin < 0 {
		a.RepeatIntervalMin = 0
	}
	if a.MergeWindowSec <= 0 {
		a.MergeWindowSec = d.Alert.MergeWindowSec
	}
	if a.SiteTitle == "" {
		a.SiteTitle = d.Alert.SiteTitle
	}
	a.WebhookURL = strings.TrimSpace(a.WebhookURL)
	a.DingTalkURL = strings.TrimSpace(a.DingTalkURL)
	a.DingTalkSecret = strings.TrimSpace(a.DingTalkSecret)
	a.FeishuURL = strings.TrimSpace(a.FeishuURL)
	a.FeishuSecret = strings.TrimSpace(a.FeishuSecret)
	a.SlackURL = strings.TrimSpace(a.SlackURL)
	a.GenericWebhookURL = strings.TrimSpace(a.GenericWebhookURL)
	a.ProxyURL = strings.TrimSpace(a.ProxyURL)

	dd := &rs.Discovery
	if dd.Jobs == nil {
		j := DefaultJob()
		if len(dd.LegacySubnets) > 0 {
			j.Subnets = dd.LegacySubnets
		}
		if dd.LegacyInterval > 0 {
			j.IntervalMin = dd.LegacyInterval
		}
		if dd.LegacyAutoAdd != nil {
			j.AutoAdd = *dd.LegacyAutoAdd
		}
		dd.Jobs = []ScanJob{j}
	}
	dd.LegacySubnets, dd.LegacyInterval, dd.LegacyAutoAdd = nil, 0, nil
	usedID := map[string]bool{}
	for i := range dd.Jobs {
		j := &dd.Jobs[i]
		j.ID = strings.TrimSpace(j.ID)
		j.Name = strings.TrimSpace(j.Name)
		j.Subnets = cleanList(j.Subnets)
		j.Tags = cleanList(j.Tags)
		if j.IntervalMin <= 0 {
			j.IntervalMin = 30
		}
		if j.ID == "" {
			for n := i + 1; ; n++ {
				if id := fmt.Sprintf("job-%d", n); !usedID[id] {
					j.ID = id
					break
				}
			}
		}
		usedID[j.ID] = true
		if j.Name == "" {
			if len(j.Subnets) > 0 {
				j.Name = strings.Join(j.Subnets, "、")
			} else {
				j.Name = "机房网段"
			}
		}
	}
	if dd.TimeoutMs < 100 {
		dd.TimeoutMs = d.Discovery.TimeoutMs
	}
	if dd.Concurrency <= 0 || dd.Concurrency > 256 {
		dd.Concurrency = d.Discovery.Concurrency
	}
	if dd.IPTracking == "" {
		dd.IPTracking = "all"
	}
	dd.Exclude = cleanList(dd.Exclude)
	if dd.OUI == nil {
		dd.OUI = map[string]string{}
	}

	lc := &rs.Locate
	if lc.IntervalMin <= 0 {
		lc.IntervalMin = d.Locate.IntervalMin
	}
	if lc.TimeoutMs < 200 {
		lc.TimeoutMs = d.Locate.TimeoutMs
	}
	lc.Community = strings.TrimSpace(lc.Community)
	if lc.Switches == nil {
		lc.Switches = map[string]LocateSwitch{}
	}
	for k, v := range lc.Switches {
		v.Host, v.Community = strings.TrimSpace(v.Host), strings.TrimSpace(v.Community)
		if v == (LocateSwitch{}) {
			delete(lc.Switches, k)
			continue
		}
		lc.Switches[k] = v
	}
}

// Validate checks per-room settings.
func (rs RoomSettings) Validate() error {
	if err := rs.Alert.ValidateChannels(); err != nil {
		return err
	}
	if err := rs.Schedule.Validate(); err != nil {
		return err
	}
	if err := rs.Discovery.Validate(); err != nil {
		return err
	}
	if rs.Locate.IntervalMin < 2 {
		return errors.New("端口定位间隔不能小于 2 分钟")
	}
	if rs.Alert.ProxyURL != "" {
		u, err := url.Parse(rs.Alert.ProxyURL)
		if err != nil || u.Host == "" {
			return errors.New("proxy_url 不是有效的代理地址")
		}
	}
	return nil
}

// LoadRoomSettings reads and normalizes per-room settings from path.
func LoadRoomSettings(path string) (RoomSettings, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return RoomSettings{}, err
	}
	rs := DefaultRoomSettings()
	if err := yaml.Unmarshal(data, &rs); err != nil {
		return RoomSettings{}, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	rs.Normalize()
	return rs, nil
}

// SaveRoomSettings validates and atomically writes per-room settings with 0600 permissions.
func SaveRoomSettings(path string, rs RoomSettings) error {
	rs = rs.Clone()
	rs.Normalize()
	if err := rs.Validate(); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(rs); err != nil {
		return err
	}
	_ = enc.Close()
	return WriteFileAtomicMode(path, buf.Bytes(), 0o600)
}

// ApplyRoom validates and updates only the per-room settings in memory without overwriting global fields.
func (m *Manager) ApplyRoom(rs RoomSettings) error {
	rs = rs.Clone()
	rs.Normalize()
	if err := rs.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	m.cfg.ApplyRoomSettings(rs)
	m.mu.Unlock()
	return nil
}

// SaveGlobal updates only global settings on disk while keeping current active room settings in memory.
func (m *Manager) SaveGlobal(c Config) error {
	c = c.Clone()
	c.Normalize()
	if err := c.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.save(c); err != nil {
		return err
	}
	m.cfg = c
	return nil
}

const (
	maskedURLSuffix  = "/***"
	maskedSecretHint = "（已设置）"
)

func maskSecretURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" && u.Host != "" {
		return u.Scheme + "://" + u.Host + maskedURLSuffix
	}
	return maskedSecretHint
}

func maskSecretText(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return maskedSecretHint
}

func isMaskedPlaceholder(v, current string) bool {
	if v == "" || current == "" {
		return false
	}
	return v == maskedSecretHint || v == "（已配置）" || v == maskSecretURL(current) || strings.Contains(v, "****")
}

// MaskRoomSettings returns a copy of rs with webhook URLs, secrets, and SNMP communities redacted.
func MaskRoomSettings(rs RoomSettings) RoomSettings {
	out := rs.Clone()
	out.Alert.WebhookURL = maskSecretURL(out.Alert.WebhookURL)
	out.Alert.DingTalkURL = maskSecretURL(out.Alert.DingTalkURL)
	out.Alert.FeishuURL = maskSecretURL(out.Alert.FeishuURL)
	out.Alert.SlackURL = maskSecretURL(out.Alert.SlackURL)
	out.Alert.GenericWebhookURL = maskSecretURL(out.Alert.GenericWebhookURL)
	out.Alert.DingTalkSecret = maskSecretText(out.Alert.DingTalkSecret)
	out.Alert.FeishuSecret = maskSecretText(out.Alert.FeishuSecret)
	out.Locate.Community = maskSecretText(out.Locate.Community)
	for k, sw := range out.Locate.Switches {
		sw.Community = maskSecretText(sw.Community)
		out.Locate.Switches[k] = sw
	}
	return out
}

// NotificationDestination describes a configured notification channel without exposing secrets.
type NotificationDestination struct {
	Channel     string `json:"channel"`
	Destination string `json:"destination"`
}

// NotificationDestinations returns human-readable channel names and masked host destinations without tokens or paths.
func NotificationDestinations(a AlertConfig) []NotificationDestination {
	type ch struct {
		label string
		raw   string
	}
	channels := []ch{
		{"企业微信", a.WebhookURL},
		{"钉钉", a.DingTalkURL},
		{"飞书", a.FeishuURL},
		{"Slack", a.SlackURL},
		{"通用 Webhook", a.GenericWebhookURL},
	}
	out := []NotificationDestination{}
	for _, c := range channels {
		raw := strings.TrimSpace(c.raw)
		if raw == "" {
			continue
		}
		dest := "已配置"
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			if u.Scheme != "" {
				dest = u.Scheme + "://" + u.Host + "/***"
			} else {
				dest = u.Host
			}
		}
		out = append(out, NotificationDestination{Channel: c.label, Destination: dest})
	}
	return out
}

// ScanRanges returns the deduplicated list of CIDR scan ranges configured for a room.
func ScanRanges(d Discovery, siteSubnet string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, j := range d.Jobs {
		if len(j.Subnets) > 0 {
			for _, sub := range j.Subnets {
				add(sub)
			}
		} else if siteSubnet != "" {
			add(siteSubnet)
		}
	}
	if out == nil {
		return []string{}
	}
	return out
}

// PatchRoomSettings applies a JSON patch (full or partial) onto existing RoomSettings,
// preserving existing secrets whenever a secret field is omitted or matches a masked placeholder.
func PatchRoomSettings(existing RoomSettings, raw []byte) (RoomSettings, error) {
	next := existing.Clone()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&next); err != nil {
		return RoomSettings{}, fmt.Errorf("JSON 格式错误: %v", err)
	}
	var rawMap map[string]json.RawMessage
	_ = json.Unmarshal(raw, &rawMap)
	var rawLocate map[string]json.RawMessage
	if lb, ok := rawMap["locate"]; ok {
		_ = json.Unmarshal(lb, &rawLocate)
	}
	var rawSwitches map[string]map[string]json.RawMessage
	if sb, ok := rawLocate["switches"]; ok {
		_ = json.Unmarshal(sb, &rawSwitches)
	}
	if isMaskedPlaceholder(next.Alert.WebhookURL, existing.Alert.WebhookURL) {
		next.Alert.WebhookURL = existing.Alert.WebhookURL
	}
	if isMaskedPlaceholder(next.Alert.DingTalkURL, existing.Alert.DingTalkURL) {
		next.Alert.DingTalkURL = existing.Alert.DingTalkURL
	}
	if isMaskedPlaceholder(next.Alert.DingTalkSecret, existing.Alert.DingTalkSecret) {
		next.Alert.DingTalkSecret = existing.Alert.DingTalkSecret
	}
	if isMaskedPlaceholder(next.Alert.FeishuURL, existing.Alert.FeishuURL) {
		next.Alert.FeishuURL = existing.Alert.FeishuURL
	}
	if isMaskedPlaceholder(next.Alert.FeishuSecret, existing.Alert.FeishuSecret) {
		next.Alert.FeishuSecret = existing.Alert.FeishuSecret
	}
	if isMaskedPlaceholder(next.Alert.SlackURL, existing.Alert.SlackURL) {
		next.Alert.SlackURL = existing.Alert.SlackURL
	}
	if isMaskedPlaceholder(next.Alert.GenericWebhookURL, existing.Alert.GenericWebhookURL) {
		next.Alert.GenericWebhookURL = existing.Alert.GenericWebhookURL
	}
	if isMaskedPlaceholder(next.Locate.Community, existing.Locate.Community) {
		next.Locate.Community = existing.Locate.Community
	}
	for k, sw := range next.Locate.Switches {
		if prev, ok := existing.Locate.Switches[k]; ok {
			_, hasCommunity := rawSwitches[k]["community"]
			if !hasCommunity || isMaskedPlaceholder(sw.Community, prev.Community) {
				sw.Community = prev.Community
				next.Locate.Switches[k] = sw
			}
		}
	}
	next.Normalize()
	if err := next.Validate(); err != nil {
		return RoomSettings{}, err
	}
	return next, nil
}
