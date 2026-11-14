package profiles

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"netping/internal/config"
	"netping/internal/monitor"
	"netping/internal/topology"
)

const (
	maxVersionsPerRoom      = 30
	maxVersionsBytesPerRoom = 32 << 20 // 32 MiB cap per room
)

var (
	ErrVersionNotFound = errors.New("配置版本不存在")
	versionIDRe        = regexp.MustCompile(`^v-[0-9]{8}-[0-9]{6}-[0-9]{3}$`)
)

// VersionMeta describes a saved configuration snapshot (topology + per-room settings).
type VersionMeta struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Reason    string    `json:"reason"` // manual | pre_activate | pre_rollback | topo_edit | settings_edit
	Note      string    `json:"note,omitempty"`
	Switches  int       `json:"switches"`
	Nodes     int       `json:"nodes"`
	SiteName  string    `json:"site_name"`
	SizeBytes int64     `json:"size_bytes,omitempty"`
}

// VersionSnapshot stores both Topology and RoomSettings for full rollback.
type VersionSnapshot struct {
	Meta     VersionMeta         `json:"meta"`
	Topology *topology.Topology  `json:"topology"`
	Settings config.RoomSettings `json:"settings"`
}

func (m *Manager) versionsDir(profileID string) string {
	return filepath.Join(m.RoomDir(profileID), "versions")
}

func (m *Manager) versionsIndexPath(profileID string) string {
	return filepath.Join(m.versionsDir(profileID), "index.json")
}

func (m *Manager) loadVersionIndex(profileID string) ([]VersionMeta, error) {
	data, err := os.ReadFile(m.versionsIndexPath(profileID))
	if os.IsNotExist(err) {
		return []VersionMeta{}, nil
	}
	if err != nil {
		return nil, err
	}
	var list []VersionMeta
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (m *Manager) saveVersionIndex(profileID string, list []VersionMeta) error {
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomicMode(m.versionsIndexPath(profileID), data, 0o600)
}

// ListVersions returns all saved versions for profileID, newest first.
func (m *Manager) ListVersions(profileID string) ([]VersionMeta, error) {
	m.verMu.Lock()
	defer m.verMu.Unlock()
	return m.listVersionsLocked(profileID)
}

func (m *Manager) listVersionsLocked(profileID string) ([]VersionMeta, error) {
	m.mu.RLock()
	exists := m.findIdxLocked(profileID) >= 0
	m.mu.RUnlock()
	if !exists {
		return nil, ErrNotFound
	}
	list, err := m.loadVersionIndex(profileID)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(list, func(i, j int) bool {
		if !list[i].CreatedAt.Equal(list[j].CreatedAt) {
			return list[i].CreatedAt.After(list[j].CreatedAt)
		}
		return list[i].ID > list[j].ID
	})
	return list, nil
}

// CreateVersion captures a snapshot of both topology and per-room settings for profileID,
// enforcing per-room count and total byte retention limits.
func (m *Manager) CreateVersion(profileID, reason, note string) (VersionMeta, error) {
	if m.topo != nil {
		defer m.topo.ProfileWork()()
	}
	m.verMu.Lock()
	defer m.verMu.Unlock()
	return m.createVersionLocked(profileID, reason, note)
}

func (m *Manager) createVersionLocked(profileID, reason, note string) (VersionMeta, error) {
	m.mu.RLock()
	exists := m.findIdxLocked(profileID) >= 0
	m.mu.RUnlock()
	if !exists {
		return VersionMeta{}, ErrNotFound
	}

	t, err := m.loadRoomTopology(profileID)
	if err != nil {
		return VersionMeta{}, err
	}
	rs, err := m.loadRoomSettings(profileID)
	if err != nil {
		return VersionMeta{}, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "manual"
	}
	note = strings.TrimSpace(note)
	if len([]rune(note)) > 200 {
		note = string([]rune(note)[:200])
	}

	vdir := m.versionsDir(profileID)
	if err := os.MkdirAll(vdir, 0o755); err != nil {
		return VersionMeta{}, err
	}

	list, err := m.loadVersionIndex(profileID)
	if err != nil {
		return VersionMeta{}, err
	}
	used := map[string]bool{}
	for _, it := range list {
		used[it.ID] = true
	}

	now := time.Now()
	prefix := "v-" + now.Format("20060102-150405")
	id := ""
	for seq := 1; seq <= 999; seq++ {
		cand := fmt.Sprintf("%s-%03d", prefix, seq)
		if !used[cand] {
			id = cand
			break
		}
	}
	if id == "" {
		return VersionMeta{}, errors.New("同一秒内创建的版本过多，请稍后重试")
	}

	meta := VersionMeta{
		ID:        id,
		CreatedAt: now,
		Reason:    reason,
		Note:      note,
		Switches:  len(t.Switches),
		Nodes:     len(t.Nodes),
		SiteName:  t.Site.Name,
	}
	snap := VersionSnapshot{
		Meta:     meta,
		Topology: t.Clone(),
		Settings: rs.Clone(),
	}
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return VersionMeta{}, err
	}
	meta.SizeBytes = int64(len(raw))
	snap.Meta.SizeBytes = meta.SizeBytes
	raw, err = json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return VersionMeta{}, err
	}
	meta.SizeBytes = int64(len(raw))

	snapPath := filepath.Join(vdir, id+".json")
	if err := config.WriteFileAtomicMode(snapPath, raw, 0o600); err != nil {
		return VersionMeta{}, err
	}

	list = append([]VersionMeta{meta}, list...)
	list, evicted := pruneVersions(list, maxVersionsPerRoom, maxVersionsBytesPerRoom)
	if err := m.saveVersionIndex(profileID, list); err != nil {
		_ = os.Remove(snapPath)
		return VersionMeta{}, err
	}
	for _, oldID := range evicted {
		_ = os.Remove(filepath.Join(vdir, oldID+".json"))
	}
	return meta, nil
}

func pruneVersions(list []VersionMeta, maxCount int, maxBytes int64) (kept []VersionMeta, evicted []string) {
	sort.SliceStable(list, func(i, j int) bool {
		if !list[i].CreatedAt.Equal(list[j].CreatedAt) {
			return list[i].CreatedAt.After(list[j].CreatedAt)
		}
		return list[i].ID > list[j].ID
	})
	var total int64
	for _, v := range list {
		sz := v.SizeBytes
		if sz <= 0 {
			sz = 1
		}
		if len(kept) < maxCount && (len(kept) == 0 || total+sz <= maxBytes) {
			kept = append(kept, v)
			total += sz
			continue
		}
		evicted = append(evicted, v.ID)
	}
	return kept, evicted
}

// GetVersion loads the full snapshot for verID. Callers returning HTTP JSON must mask Settings.
func (m *Manager) GetVersion(profileID, verID string) (*VersionSnapshot, error) {
	m.verMu.Lock()
	defer m.verMu.Unlock()
	return m.getVersionLocked(profileID, verID)
}

func (m *Manager) getVersionLocked(profileID, verID string) (*VersionSnapshot, error) {
	m.mu.RLock()
	exists := m.findIdxLocked(profileID) >= 0
	m.mu.RUnlock()
	if !exists {
		return nil, ErrNotFound
	}
	if !versionIDRe.MatchString(verID) {
		return nil, ErrVersionNotFound
	}
	data, err := os.ReadFile(filepath.Join(m.versionsDir(profileID), verID+".json"))
	if os.IsNotExist(err) {
		return nil, ErrVersionNotFound
	}
	if err != nil {
		return nil, err
	}
	var snap VersionSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, err
	}
	if snap.Topology == nil {
		return nil, errors.New("版本快照缺少拓扑数据")
	}
	snap.Topology.Normalize()
	if probs := snap.Topology.Validate(); len(probs) > 0 {
		return nil, &topology.ValidationError{Problems: probs}
	}
	snap.Settings.Normalize()
	if err := snap.Settings.Validate(); err != nil {
		return nil, err
	}
	return &snap, nil
}

// RollbackVersion restores both topology and per-room settings of profileID to verID,
// saving an automatic pre_rollback snapshot beforehand.
func (m *Manager) RollbackVersion(profileID, verID string) (VersionMeta, VersionMeta, error) {
	m.switchMu.Lock()
	defer m.switchMu.Unlock()

	active := profileID == m.ActiveID()
	if active {
		unlockTopo, ok := m.topo.TryLockProfile()
		if !ok {
			return VersionMeta{}, VersionMeta{}, ErrBusy
		}
		defer unlockTopo()
		if (m.disc != nil && m.disc.Running()) || (m.loc != nil && m.loc.Running()) {
			return VersionMeta{}, VersionMeta{}, ErrBusy
		}
	}

	m.verMu.Lock()
	snap, err := m.getVersionLocked(profileID, verID)
	if err != nil {
		m.verMu.Unlock()
		return VersionMeta{}, VersionMeta{}, err
	}
	backup, err := m.createVersionLocked(profileID, "pre_rollback", "回滚至 "+verID+" 前自动备份")
	m.verMu.Unlock()
	if err != nil {
		return VersionMeta{}, VersionMeta{}, fmt.Errorf("创建回滚前备份失败: %w", err)
	}

	prevTopo, err := m.loadRoomTopology(profileID)
	if err != nil {
		return VersionMeta{}, VersionMeta{}, err
	}
	prevSettings, err := m.loadRoomSettings(profileID)
	if err != nil {
		return VersionMeta{}, VersionMeta{}, err
	}

	if err := m.saveRoomTopology(profileID, snap.Topology); err != nil {
		return VersionMeta{}, VersionMeta{}, err
	}
	if err := config.SaveRoomSettings(m.roomSettingsPath(profileID), snap.Settings); err != nil {
		_ = m.saveRoomTopology(profileID, prevTopo)
		return VersionMeta{}, VersionMeta{}, err
	}

	if active {
		if err := m.topo.SwapActive(snap.Topology); err != nil {
			_ = m.saveRoomTopology(profileID, prevTopo)
			_ = config.SaveRoomSettings(m.roomSettingsPath(profileID), prevSettings)
			return VersionMeta{}, VersionMeta{}, err
		}
		if err := m.cfg.ApplyRoom(snap.Settings); err != nil {
			_ = m.topo.SwapActive(prevTopo)
			_ = m.saveRoomTopology(profileID, prevTopo)
			_ = config.SaveRoomSettings(m.roomSettingsPath(profileID), prevSettings)
			return VersionMeta{}, VersionMeta{}, err
		}
		if m.alerts != nil {
			m.alerts.SetTopology(snap.Topology)
		}
		if m.mon != nil {
			m.mon.Sync(monitor.BuildTargets(snap.Topology, snap.Settings.Ping))
		}
		// An active rollback changes the context used by in-flight AI work and
		// previously previewed operations, just like a profile activation.
		m.epoch.Add(1)
		m.topo.NotifyCurrent()
	}

	m.touchProfile(profileID)
	return snap.Meta, backup, nil
}

// RollbackPreview summarizes what rolling back profileID to verID would change without mutating state.
type RollbackPreview struct {
	ProfileID   string              `json:"profile_id"`
	Active      bool                `json:"active"`
	Target      VersionMeta         `json:"target"`
	Current     RollbackRoomSummary `json:"current"`
	Diff        RollbackDiff        `json:"diff"`
	CanRollback bool                `json:"can_rollback"`
	BusyReason  string              `json:"busy_reason,omitempty"`
}

// RollbackRoomSummary describes the current state of a room before rollback.
type RollbackRoomSummary struct {
	Switches int    `json:"switches"`
	Nodes    int    `json:"nodes"`
	SiteName string `json:"site_name"`
}

// RollbackDiff describes high-level topology and settings changes for a version rollback (without exposing secrets).
type RollbackDiff struct {
	SiteChanged             bool     `json:"site_changed"`
	SwitchesAdded           []string `json:"switches_added,omitempty"`
	SwitchesRemoved         []string `json:"switches_removed,omitempty"`
	SwitchesModified        []string `json:"switches_modified,omitempty"`
	NodesAdded              []string `json:"nodes_added,omitempty"`
	NodesRemoved            []string `json:"nodes_removed,omitempty"`
	NodesModified           []string `json:"nodes_modified,omitempty"`
	SettingsSectionsChanged []string `json:"settings_sections_changed,omitempty"`
}

// PreviewRollback compares verID against the current room topology and settings without modifying anything.
func (m *Manager) PreviewRollback(profileID, verID string) (*RollbackPreview, error) {
	snap, err := m.GetVersion(profileID, verID)
	if err != nil {
		return nil, err
	}
	curTopo, err := m.loadRoomTopology(profileID)
	if err != nil {
		return nil, err
	}
	curSettings, err := m.loadRoomSettings(profileID)
	if err != nil {
		return nil, err
	}
	curTopo.Normalize()
	curSettings.Normalize()

	active := profileID == m.ActiveID()
	canRollback := true
	busyReason := ""
	if m.switching.Load() {
		canRollback = false
		busyReason = ErrBusy.Error()
	} else if active {
		if (m.disc != nil && m.disc.Running()) || (m.loc != nil && m.loc.Running()) {
			canRollback = false
			busyReason = ErrBusy.Error()
		} else if unlockTopo, ok := m.topo.TryLockProfile(); !ok {
			canRollback = false
			busyReason = ErrBusy.Error()
		} else {
			unlockTopo()
		}
	}

	diff := RollbackDiff{
		SiteChanged: !reflect.DeepEqual(curTopo.Site, snap.Topology.Site),
	}

	curSW := make(map[string]*topology.Switch, len(curTopo.Switches))
	for _, s := range curTopo.Switches {
		if s != nil {
			curSW[s.ID] = s
		}
	}
	tgtSW := make(map[string]*topology.Switch, len(snap.Topology.Switches))
	for _, s := range snap.Topology.Switches {
		if s != nil {
			tgtSW[s.ID] = s
		}
	}
	for id, ts := range tgtSW {
		cs, ok := curSW[id]
		if !ok {
			diff.SwitchesAdded = append(diff.SwitchesAdded, id)
		} else if !reflect.DeepEqual(cs, ts) {
			diff.SwitchesModified = append(diff.SwitchesModified, id)
		}
	}
	for id := range curSW {
		if _, ok := tgtSW[id]; !ok {
			diff.SwitchesRemoved = append(diff.SwitchesRemoved, id)
		}
	}
	sort.Strings(diff.SwitchesAdded)
	sort.Strings(diff.SwitchesRemoved)
	sort.Strings(diff.SwitchesModified)

	curNodes := make(map[string]*topology.Node, len(curTopo.Nodes))
	for _, n := range curTopo.Nodes {
		if n != nil {
			curNodes[n.ID] = n
		}
	}
	tgtNodes := make(map[string]*topology.Node, len(snap.Topology.Nodes))
	for _, n := range snap.Topology.Nodes {
		if n != nil {
			tgtNodes[n.ID] = n
		}
	}
	for id, tn := range tgtNodes {
		cn, ok := curNodes[id]
		if !ok {
			diff.NodesAdded = append(diff.NodesAdded, id)
		} else if !reflect.DeepEqual(cn, tn) {
			diff.NodesModified = append(diff.NodesModified, id)
		}
	}
	for id := range curNodes {
		if _, ok := tgtNodes[id]; !ok {
			diff.NodesRemoved = append(diff.NodesRemoved, id)
		}
	}
	sort.Strings(diff.NodesAdded)
	sort.Strings(diff.NodesRemoved)
	sort.Strings(diff.NodesModified)

	if !reflect.DeepEqual(curSettings.Schedule, snap.Settings.Schedule) {
		diff.SettingsSectionsChanged = append(diff.SettingsSectionsChanged, "schedule")
	}
	if !reflect.DeepEqual(curSettings.Discovery, snap.Settings.Discovery) {
		diff.SettingsSectionsChanged = append(diff.SettingsSectionsChanged, "discovery")
	}
	if !reflect.DeepEqual(curSettings.Locate, snap.Settings.Locate) {
		diff.SettingsSectionsChanged = append(diff.SettingsSectionsChanged, "locate")
	}
	if !reflect.DeepEqual(curSettings.Ping, snap.Settings.Ping) {
		diff.SettingsSectionsChanged = append(diff.SettingsSectionsChanged, "ping")
	}
	if !reflect.DeepEqual(curSettings.Alert, snap.Settings.Alert) {
		diff.SettingsSectionsChanged = append(diff.SettingsSectionsChanged, "alert")
	}

	return &RollbackPreview{
		ProfileID: profileID,
		Active:    active,
		Target:    snap.Meta,
		Current: RollbackRoomSummary{
			Switches: len(curTopo.Switches),
			Nodes:    len(curTopo.Nodes),
			SiteName: curTopo.Site.Name,
		},
		Diff:        diff,
		CanRollback: canRollback,
		BusyReason:  busyReason,
	}, nil
}
