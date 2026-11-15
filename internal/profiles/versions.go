package profiles

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	snap, err := m.GetVersion(profileID, verID)
	if err != nil {
		return VersionMeta{}, VersionMeta{}, err
	}

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

	backup, err := m.CreateVersion(profileID, "pre_rollback", "回滚至 "+verID+" 前自动备份")
	if err != nil {
		return VersionMeta{}, VersionMeta{}, fmt.Errorf("创建回滚前备份失败: %w", err)
	}

	if err := m.applyRoomState(profileID, active, snap.Topology, snap.Settings); err != nil {
		return VersionMeta{}, VersionMeta{}, err
	}

	m.touchProfile(profileID)
	return snap.Meta, backup, nil
}

// applyRoomState 把拓扑与机房设置写入 profileID 的独立目录；如果它是当前激活机房，同时切换运行时状态。
// 任一步失败都会恢复原拓扑与设置。调用方须持有 switchMu，激活机房还须持有拓扑锁。
func (m *Manager) applyRoomState(profileID string, active bool, t *topology.Topology, rs config.RoomSettings) error {
	prevTopo, err := m.loadRoomTopology(profileID)
	if err != nil {
		return err
	}
	prevSettings, err := m.loadRoomSettings(profileID)
	if err != nil {
		return err
	}

	if err := m.saveRoomTopology(profileID, t); err != nil {
		return err
	}
	if err := config.SaveRoomSettings(m.roomSettingsPath(profileID), rs); err != nil {
		_ = m.saveRoomTopology(profileID, prevTopo)
		return err
	}

	if active {
		if err := m.topo.SwapActive(t); err != nil {
			_ = m.saveRoomTopology(profileID, prevTopo)
			_ = config.SaveRoomSettings(m.roomSettingsPath(profileID), prevSettings)
			return err
		}
		if err := m.cfg.ApplyRoom(rs); err != nil {
			_ = m.topo.SwapActive(prevTopo)
			_ = m.saveRoomTopology(profileID, prevTopo)
			_ = config.SaveRoomSettings(m.roomSettingsPath(profileID), prevSettings)
			return err
		}
		if m.alerts != nil {
			m.alerts.SetTopology(t)
		}
		if m.mon != nil {
			m.mon.Sync(monitor.BuildTargets(t, rs.Ping))
		}
		m.topo.NotifyCurrent()
	}
	return nil
}
