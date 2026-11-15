package profiles

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"time"

	"netping/internal/config"
	"netping/internal/topology"
)

// 按机房导入导出配置：导出文件只包含一个机房的拓扑和机房级设置（不含监听地址、登录、MCP、AI 等全局配置，
// 也不含历史数据、告警记录与版本快照）。导入只作用于指定的一个机房，导入前自动创建版本备份，可回滚。

const (
	ExportFormat        = "netpulse-room-config"
	ExportVersion       = 1
	MaxImportBytes      = 8 << 20
	SecretsMasked       = "masked"
	SecretsIncluded     = "included"
	importVersionReason = "pre_import"
)

// ExportSource 记录导出文件来自哪个机房。
type ExportSource struct {
	ProfileID   string `json:"profile_id"`
	ProfileName string `json:"profile_name"`
}

// RoomExport 机房配置导出文件。
type RoomExport struct {
	Format     string              `json:"format"`
	Version    int                 `json:"version"`
	ExportedAt time.Time           `json:"exported_at"`
	AppVersion string              `json:"app_version,omitempty"`
	Source     ExportSource        `json:"source"`
	Secrets    string              `json:"secrets"` // masked | included
	Topology   *topology.Topology  `json:"topology"`
	Settings   config.RoomSettings `json:"settings"`
}

// ImportError 导入文件不合法（格式、版本、内容校验失败），现有配置未被修改。
type ImportError struct {
	Msg      string
	Problems []string
}

func (e *ImportError) Error() string {
	if len(e.Problems) == 0 {
		return e.Msg
	}
	return e.Msg + "：" + strings.Join(e.Problems, "；")
}

func importErr(format string, a ...interface{}) error {
	return &ImportError{Msg: fmt.Sprintf(format, a...)}
}

// Export 生成 profileID 的配置导出；includeSecrets 为 false 时 Webhook 地址、签名密钥、SNMP 团体名与代理账号会被脱敏。
func (m *Manager) Export(profileID string, includeSecrets bool, appVersion string) (*RoomExport, error) {
	m.mu.RLock()
	idx := m.findIdxLocked(profileID)
	var meta ProfileMeta
	if idx >= 0 {
		meta = m.reg.Profiles[idx]
	}
	m.mu.RUnlock()
	if idx < 0 {
		return nil, ErrNotFound
	}
	t, err := m.loadRoomTopology(profileID)
	if err != nil {
		return nil, err
	}
	rs, err := m.loadRoomSettings(profileID)
	if err != nil {
		return nil, err
	}
	out := &RoomExport{
		Format:     ExportFormat,
		Version:    ExportVersion,
		ExportedAt: time.Now(),
		AppVersion: appVersion,
		Source:     ExportSource{ProfileID: meta.ID, ProfileName: meta.Name},
		Secrets:    SecretsMasked,
		Topology:   t.Clone(),
		Settings:   rs.Clone(),
	}
	if includeSecrets {
		out.Secrets = SecretsIncluded
	} else {
		out.Settings = config.MaskRoomSettingsForExport(out.Settings)
	}
	return out, nil
}

// ParseImport 严格解析并校验导入文件：格式标识、版本、未知字段（例如全局配置字段）、拓扑与设置合法性。
func ParseImport(raw []byte) (*RoomExport, error) {
	if len(raw) == 0 {
		return nil, importErr("导入文件为空")
	}
	if len(raw) > MaxImportBytes {
		return nil, importErr("导入文件过大（上限 %d MB）", MaxImportBytes>>20)
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	// 先只看格式与版本，给出更明确的错误。
	var head struct {
		Format  string `json:"format"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, importErr("不是有效的 JSON 文件：%v", err)
	}
	if head.Format != ExportFormat {
		return nil, importErr("文件格式不正确：需要网脉机房配置导出文件（format=%s），实际为 %q", ExportFormat, head.Format)
	}
	if head.Version <= 0 {
		return nil, importErr("文件缺少版本号")
	}
	if head.Version > ExportVersion {
		return nil, importErr("文件版本 %d 高于当前程序支持的版本 %d，请升级后再导入", head.Version, ExportVersion)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, importErr("不是有效的 JSON 对象：%v", err)
	}
	allowed := map[string]bool{"format": true, "version": true, "exported_at": true, "app_version": true, "source": true, "secrets": true, "topology": true, "settings": true}
	var unknown []string
	for k := range top {
		if !allowed[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		return nil, &ImportError{Msg: "文件包含不属于机房配置的字段（全局配置不能通过机房导入修改）", Problems: unknown}
	}
	if !isJSONObject(top["topology"]) {
		return nil, importErr("文件缺少 topology（拓扑），或它不是 JSON 对象")
	}
	if !isJSONObject(top["settings"]) {
		return nil, importErr("文件缺少 settings（机房设置），或它不是 JSON 对象")
	}
	if src, ok := top["source"]; ok && !isJSONObject(src) {
		return nil, importErr("source 必须是 JSON 对象")
	}
	// 机房设置的各部分必须完整给出，避免缺失或 null 的部分被静默重置为默认值。
	var sections map[string]json.RawMessage
	_ = json.Unmarshal(top["settings"], &sections)
	var bad []string
	for _, k := range []string{"schedule", "discovery", "locate", "ping", "alert"} {
		if !isJSONObject(sections[k]) {
			bad = append(bad, "settings."+k)
		}
	}
	if len(bad) > 0 {
		return nil, &ImportError{Msg: "机房设置不完整（以下部分缺失或不是 JSON 对象），未做任何修改", Problems: bad}
	}

	var exp RoomExport
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&exp); err != nil {
		return nil, importErr("文件内容不符合机房配置格式：%v", err)
	}
	if dec.More() {
		return nil, importErr("文件末尾有多余内容")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, importErr("文件末尾有多余内容")
	}
	if exp.Secrets == "" {
		exp.Secrets = SecretsMasked
	}
	if exp.Secrets != SecretsMasked && exp.Secrets != SecretsIncluded {
		return nil, importErr("secrets 字段只能是 masked 或 included")
	}
	if exp.Topology == nil {
		return nil, importErr("文件中的拓扑为空")
	}
	exp.Topology.Normalize()
	if probs := exp.Topology.Validate(); len(probs) > 0 {
		return nil, &ImportError{Msg: "拓扑校验失败，未做任何修改", Problems: probs}
	}
	exp.Settings.Normalize()
	if err := exp.Settings.Validate(); err != nil {
		return nil, &ImportError{Msg: "机房设置校验失败，未做任何修改", Problems: []string{err.Error()}}
	}
	return &exp, nil
}

// isJSONObject 判断原始 JSON 是否为对象（null、数组、字符串、数字等都不是）。
func isJSONObject(raw json.RawMessage) bool {
	b := bytes.TrimSpace(raw)
	return len(b) > 0 && b[0] == '{'
}

// ImportPreview 说明导入会如何覆盖目标机房，供用户确认。
type ImportPreview struct {
	TargetID       string                    `json:"target_id"`
	TargetName     string                    `json:"target_name"`
	TargetActive   bool                      `json:"target_active"`
	NewProfile     bool                      `json:"new_profile,omitempty"`
	SourceName     string                    `json:"source_name"`
	SourceID       string                    `json:"source_id"`
	ExportedAt     time.Time                 `json:"exported_at"`
	FileSecrets    string                    `json:"file_secrets"`
	Mode           string                    `json:"mode"` // replace：整体替换目标机房的拓扑与机房设置
	Current        ImportCounts              `json:"current"`
	Incoming       ImportCounts              `json:"incoming"`
	ChangedSection []string                  `json:"changed_sections"`
	Secrets        []config.SecretResolution `json:"secrets"`
	Warnings       []string                  `json:"warnings"`
	Unaffected     []string                  `json:"unaffected"`
	BackupVersion  string                    `json:"backup_version,omitempty"`
}

// ImportCounts 拓扑规模。
type ImportCounts struct {
	SiteName string `json:"site_name"`
	Switches int    `json:"switches"`
	Nodes    int    `json:"nodes"`
	Links    int    `json:"links"`
	ScanJobs int    `json:"scan_jobs"`
}

func countsOf(t *topology.Topology, rs config.RoomSettings) ImportCounts {
	c := ImportCounts{ScanJobs: len(rs.Discovery.Jobs)}
	if t != nil {
		c.SiteName, c.Switches, c.Nodes, c.Links = t.Site.Name, len(t.Switches), len(t.Nodes), len(t.Links)
	}
	return c
}

var sectionNames = []struct{ key, label string }{
	{"schedule", "监控时段"}, {"discovery", "网段扫描"}, {"locate", "端口定位"}, {"ping", "Ping 参数"}, {"alert", "告警与通知"},
}

func changedSections(a, b config.RoomSettings) []string {
	pick := func(rs config.RoomSettings, k string) interface{} {
		switch k {
		case "schedule":
			return rs.Schedule
		case "discovery":
			return rs.Discovery
		case "locate":
			return rs.Locate
		case "ping":
			return rs.Ping
		default:
			return rs.Alert
		}
	}
	out := []string{}
	for _, s := range sectionNames {
		if !reflect.DeepEqual(pick(a, s.key), pick(b, s.key)) {
			out = append(out, s.label)
		}
	}
	return out
}

func topoEqual(a, b *topology.Topology) bool {
	if a == nil || b == nil {
		return a == b
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

func (m *Manager) buildPreview(profileID string, exp *RoomExport) (*ImportPreview, *topology.Topology, config.RoomSettings, error) {
	m.mu.RLock()
	idx := m.findIdxLocked(profileID)
	var meta ProfileMeta
	if idx >= 0 {
		meta = m.reg.Profiles[idx]
	}
	active := profileID == m.reg.ActiveID
	m.mu.RUnlock()
	if idx < 0 {
		return nil, nil, config.RoomSettings{}, ErrNotFound
	}
	curTopo, err := m.loadRoomTopology(profileID)
	if err != nil {
		return nil, nil, config.RoomSettings{}, err
	}
	curRS, err := m.loadRoomSettings(profileID)
	if err != nil {
		return nil, nil, config.RoomSettings{}, err
	}
	nextRS, notes := config.ResolveImportedSecrets(curRS, exp.Settings, exp.Secrets == SecretsMasked)
	nextRS.Normalize()
	alertOff := disableAlertIfChannelsCleared(&nextRS, notes)
	if err := nextRS.Validate(); err != nil {
		return nil, nil, config.RoomSettings{}, &ImportError{Msg: "机房设置校验失败，未做任何修改", Problems: []string{err.Error()}}
	}
	nextTopo := exp.Topology.Clone()

	p := &ImportPreview{
		TargetID: meta.ID, TargetName: meta.Name, TargetActive: active,
		SourceName: exp.Source.ProfileName, SourceID: exp.Source.ProfileID,
		ExportedAt: exp.ExportedAt, FileSecrets: exp.Secrets, Mode: "replace",
		Current: countsOf(curTopo, curRS), Incoming: countsOf(nextTopo, nextRS),
		ChangedSection: changedSections(curRS, nextRS),
		Secrets:        notes,
		Warnings:       []string{},
		Unaffected: []string{
			"其他机房的拓扑、设置、历史数据与版本",
			"全局配置（监听地址、登录密码、MCP、AI 设置、数据保留天数）",
			"本机房的历史监控数据、告警记录与已有版本快照",
		},
	}
	if p.Secrets == nil {
		p.Secrets = []config.SecretResolution{}
	}
	if !topoEqual(curTopo, nextTopo) {
		p.ChangedSection = append([]string{"拓扑（交换机、链路、设备）"}, p.ChangedSection...)
	}
	if active {
		p.Warnings = append(p.Warnings, "目标是当前激活机房：导入后立即按新拓扑和设置监控、告警")
	}
	if alertOff != "" {
		p.Warnings = append(p.Warnings, alertOff)
	}
	if exp.Source.ProfileName != "" && exp.Source.ProfileName != meta.Name {
		p.Warnings = append(p.Warnings, fmt.Sprintf("文件来自机房「%s」，将覆盖到机房「%s」", exp.Source.ProfileName, meta.Name))
	}
	for _, n := range notes {
		if n.Action == "cleared" {
			p.Warnings = append(p.Warnings, "文件中的凭据已脱敏且目标机房没有原值，导入后需要重新填写："+n.Field)
		}
	}
	if p.Current.Nodes > 0 && p.Incoming.Nodes == 0 {
		p.Warnings = append(p.Warnings, fmt.Sprintf("导入后设备将从 %d 台变为 0 台", p.Current.Nodes))
	}
	return p, nextTopo, nextRS, nil
}

// PreviewImport 只做校验与对比，不修改任何数据。
func (m *Manager) PreviewImport(profileID string, raw []byte) (*ImportPreview, error) {
	exp, err := ParseImport(raw)
	if err != nil {
		return nil, err
	}
	p, _, _, err := m.buildPreview(profileID, exp)
	return p, err
}

// Import 用导入文件整体替换 profileID 的拓扑与机房设置；导入前自动创建 pre_import 版本，失败时恢复原状。
func (m *Manager) Import(profileID string, raw []byte) (*ImportPreview, error) {
	exp, err := ParseImport(raw)
	if err != nil {
		return nil, err
	}
	m.switchMu.Lock()
	defer m.switchMu.Unlock()
	return m.importLocked(profileID, exp)
}

func (m *Manager) importLocked(profileID string, exp *RoomExport) (*ImportPreview, error) {
	active := profileID == m.ActiveID()
	if active {
		unlockTopo, ok := m.topo.TryLockProfile()
		if !ok {
			return nil, ErrBusy
		}
		defer unlockTopo()
		if (m.disc != nil && m.disc.Running()) || (m.loc != nil && m.loc.Running()) {
			return nil, ErrBusy
		}
	}
	p, nextTopo, nextRS, err := m.buildPreview(profileID, exp)
	if err != nil {
		return nil, err
	}
	src := exp.Source.ProfileName
	if src == "" {
		src = "导入文件"
	}
	backup, err := m.CreateVersion(profileID, importVersionReason, "导入「"+src+"」前自动备份")
	if err != nil {
		return nil, fmt.Errorf("创建导入前备份失败，未做任何修改: %w", err)
	}
	p.BackupVersion = backup.ID
	if err := m.applyRoomState(profileID, active, nextTopo, nextRS); err != nil {
		return nil, err
	}
	m.touchProfile(profileID)
	return p, nil
}

// ImportAsNew 用导入文件创建一个新的（未激活）机房；name 为空时使用文件中的机房名，重名时自动加后缀。
// 新机房没有需要保护的原配置，因此不创建导入前备份版本。
func (m *Manager) ImportAsNew(name string, raw []byte) (*ImportPreview, error) {
	exp, err := ParseImport(raw)
	if err != nil {
		return nil, err
	}
	m.switchMu.Lock()
	defer m.switchMu.Unlock()
	name = strings.TrimSpace(name)
	if name == "" {
		name = m.defaultImportName(exp)
	}
	p, rs, err := newProfilePreview(name, exp)
	if err != nil {
		return nil, err
	}
	info, err := m.Create(name, false, "")
	if err != nil {
		return nil, err
	}
	if err := m.applyRoomState(info.ID, false, exp.Topology.Clone(), rs); err != nil {
		m.removeProfileLocked(info.ID)
		return nil, err
	}
	m.touchProfile(info.ID)
	p.TargetID = info.ID
	return p, nil
}

func (m *Manager) uniqueName(base string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if r := []rune(base); len(r) > 70 {
		base = string(r[:70])
	}
	taken := func(n string) bool {
		for _, p := range m.reg.Profiles {
			if p.Name == n {
				return true
			}
		}
		return false
	}
	if !taken(base) {
		return base
	}
	for i := 2; i < 1000; i++ {
		if n := fmt.Sprintf("%s (导入%d)", base, i); !taken(n) {
			return n
		}
	}
	return base + " (导入)"
}

// removeProfileLocked 删除刚创建但导入失败的机房（调用方持有 switchMu）。
func (m *Manager) removeProfileLocked(id string) {
	m.mu.Lock()
	if idx := m.findIdxLocked(id); idx >= 0 && id != m.reg.ActiveID {
		m.reg.Profiles = append(m.reg.Profiles[:idx], m.reg.Profiles[idx+1:]...)
		_ = m.saveRegistryLocked()
		m.mu.Unlock()
		_ = os.RemoveAll(m.RoomDir(id))
		return
	}
	m.mu.Unlock()
}

// defaultImportName 导入为新机房时的默认名称：文件中的机房名 → 站点名 → “导入的机房”，重名自动加后缀。
func (m *Manager) defaultImportName(exp *RoomExport) string {
	name := strings.TrimSpace(exp.Source.ProfileName)
	if name == "" {
		name = strings.TrimSpace(exp.Topology.Site.Name)
	}
	if name == "" {
		name = "导入的机房"
	}
	return m.uniqueName(name)
}

// newProfilePreview 说明导入为新机房的结果（不修改数据）。
func newProfilePreview(name string, exp *RoomExport) (*ImportPreview, config.RoomSettings, error) {
	rs, notes := config.ResolveImportedSecrets(config.RoomSettings{}, exp.Settings, exp.Secrets == SecretsMasked)
	rs.Normalize()
	alertOff := disableAlertIfChannelsCleared(&rs, notes)
	if err := rs.Validate(); err != nil {
		return nil, rs, &ImportError{Msg: "机房设置校验失败，未做任何修改", Problems: []string{err.Error()}}
	}
	p := &ImportPreview{
		TargetName: name, NewProfile: true, SourceName: exp.Source.ProfileName, SourceID: exp.Source.ProfileID,
		ExportedAt: exp.ExportedAt, FileSecrets: exp.Secrets, Mode: "create",
		Incoming: countsOf(exp.Topology, rs), ChangedSection: []string{}, Secrets: notes, Warnings: []string{},
		Unaffected: []string{"所有现有机房（包括当前激活机房）", "全局配置（监听地址、登录密码、MCP、AI 设置、数据保留天数）"},
	}
	if p.Secrets == nil {
		p.Secrets = []config.SecretResolution{}
	}
	for _, n := range notes {
		if n.Action == "cleared" {
			p.Warnings = append(p.Warnings, "文件中的凭据已脱敏，导入后需要重新填写："+n.Field)
		}
	}
	if alertOff != "" {
		p.Warnings = append(p.Warnings, alertOff)
	}
	return p, rs, nil
}

// PreviewImportAsNew 校验导入文件并说明将创建的新机房（不修改任何数据）。
func (m *Manager) PreviewImportAsNew(name string, raw []byte) (*ImportPreview, error) {
	exp, err := ParseImport(raw)
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = m.defaultImportName(exp)
	} else {
		m.mu.RLock()
		_, err := validateProfileName(name, m.reg.Profiles, "")
		m.mu.RUnlock()
		if err != nil {
			return nil, err
		}
	}
	p, _, err := newProfilePreview(name, exp)
	return p, err
}

// disableAlertIfChannelsCleared 脱敏凭据被清空后如果告警推送已开启却没有可用渠道，则关闭推送（否则设置校验无法通过），并返回提示。
func disableAlertIfChannelsCleared(rs *config.RoomSettings, notes []config.SecretResolution) string {
	cleared := false
	for _, n := range notes {
		if n.Action == "cleared" {
			cleared = true
		}
	}
	if !cleared || !rs.Alert.Enabled || rs.Alert.ValidateChannels() == nil {
		return ""
	}
	rs.Alert.Enabled = false
	return "导入文件中的通知渠道地址已脱敏且目标机房没有原值，告警推送将先关闭；重新填写通知渠道后再开启"
}
