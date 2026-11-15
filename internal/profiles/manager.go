// Package profiles manages multi-datacenter (room) configurations with single-active switching,
// per-room settings and data isolation, legacy migration, pre-activation checks, and config version rollback.
package profiles

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"netping/internal/alert"
	"netping/internal/config"
	"netping/internal/discovery"
	"netping/internal/locate"
	"netping/internal/monitor"
	"netping/internal/store"
	"netping/internal/topology"
)

const DefaultProfileID = "default"

var (
	ErrNotFound = errors.New("机房配置不存在")
	ErrBusy     = errors.New("正在扫描或定位，请等待任务完成后再管理机房配置")
	ErrConflict = errors.New("机房配置状态已变更，请重试")
)

// ObservationSpan records a time window [FromMs, ToMs] during which a profile was active.
// ToMs == 0 indicates the currently open active span.
type ObservationSpan struct {
	FromMs int64 `json:"from_ms"`
	ToMs   int64 `json:"to_ms"`
}

// ProfileMeta stores persistent metadata for a single room profile.
type ProfileMeta struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
	Observations []ObservationSpan `json:"observations,omitempty"`
}

// ProfileInfo is the API summary representation of a profile.
type ProfileInfo struct {
	ID                 string    `json:"id"`
	Name               string    `json:"name"`
	Active             bool      `json:"active"`
	Nodes              int       `json:"nodes"`
	Switches           int       `json:"switches"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
	ObservationMinutes int64     `json:"observation_minutes"`
}

// Registry is persisted to data/profiles.json.
type Registry struct {
	Version        int           `json:"version"`
	ActiveID       string        `json:"active_id"`
	LegacyMigrated bool          `json:"legacy_migrated"`
	Profiles       []ProfileMeta `json:"profiles"`
}

// Options configures a profiles Manager.
type Options struct {
	BaseDir   string
	Cfg       *config.Manager
	Topo      *topology.Store
	Mon       *monitor.Monitor
	Store     *store.Store
	Alerts    *alert.Engine
	Disc      *discovery.Service
	Loc       *locate.Service
	ProbeFunc func(ip string, timeout time.Duration) (string, string) // optional override for testing connectivity preview
}

// Manager coordinates profile CRUD, isolated room directories, safe activation, preview, and version rollback.
type Manager struct {
	mu        sync.RWMutex
	switchMu  sync.Mutex
	switching atomic.Bool
	epoch     atomic.Int64

	baseDir string
	dataDir string
	regPath string
	reg     Registry

	cfg       *config.Manager
	topo      *topology.Store
	mon       *monitor.Monitor
	store     *store.Store
	alerts    *alert.Engine
	disc      *discovery.Service
	loc       *locate.Service
	probeFunc func(ip string, timeout time.Duration) (string, string)
}

// Open initializes the profiles Manager, migrating legacy single-room data once and restoring the active room.
func Open(opt Options) (*Manager, error) {
	if opt.BaseDir == "" || opt.Cfg == nil || opt.Topo == nil {
		return nil, errors.New("profiles.Open: BaseDir, Cfg, and Topo are required")
	}
	dataDir := filepath.Join(opt.BaseDir, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "rooms"), 0o755); err != nil {
		return nil, err
	}
	m := &Manager{
		baseDir:   opt.BaseDir,
		dataDir:   dataDir,
		regPath:   filepath.Join(dataDir, "profiles.json"),
		cfg:       opt.Cfg,
		topo:      opt.Topo,
		mon:       opt.Mon,
		store:     opt.Store,
		alerts:    opt.Alerts,
		disc:      opt.Disc,
		loc:       opt.Loc,
		probeFunc: opt.ProbeFunc,
	}
	if err := m.init(); err != nil {
		return nil, err
	}
	m.topo.OnChange(func(t *topology.Topology) {
		if m.switching.Load() {
			return
		}
		active := m.ActiveID()
		if active == "" {
			return
		}
		_ = m.saveRoomTopology(active, t)
	})
	return m, nil
}

func (m *Manager) init() error {
	now := time.Now()
	nowMs := now.UnixMilli()
	if data, err := os.ReadFile(m.regPath); err == nil {
		if err := json.Unmarshal(data, &m.reg); err != nil {
			return fmt.Errorf("解析 %s 失败: %w", m.regPath, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	if m.reg.Version == 0 {
		m.reg.Version = 1
	}

	if !m.reg.LegacyMigrated {
		curTopo, _ := m.topo.Snapshot()
		if len(m.reg.Profiles) == 0 {
			name := strings.TrimSpace(curTopo.Site.Name)
			if name == "" {
				name = "默认机房"
			}
			m.reg.Profiles = []ProfileMeta{{
				ID:        DefaultProfileID,
				Name:      name,
				CreatedAt: now,
				UpdatedAt: now,
			}}
		}
		if m.reg.ActiveID == "" {
			m.reg.ActiveID = m.reg.Profiles[0].ID
		}
		defDir := m.RoomDir(DefaultProfileID)
		if err := ensureRoomDirs(defDir); err != nil {
			return err
		}
		if err := m.migrateLegacyToDefault(defDir); err != nil {
			return err
		}
		if _, err := os.Stat(m.roomSettingsPath(DefaultProfileID)); os.IsNotExist(err) {
			if err := config.SaveRoomSettings(m.roomSettingsPath(DefaultProfileID), m.cfg.Get().RoomSettings()); err != nil {
				return err
			}
		}
		if _, err := os.Stat(m.roomTopoPath(DefaultProfileID)); os.IsNotExist(err) {
			if err := m.saveRoomTopology(DefaultProfileID, curTopo); err != nil {
				return err
			}
		}
		m.reg.LegacyMigrated = true
		if err := m.saveRegistryLocked(); err != nil {
			return err
		}
	}

	idx := m.findIdxLocked(m.reg.ActiveID)
	if idx < 0 {
		if len(m.reg.Profiles) == 0 {
			return errors.New("profiles.json 中没有任何机房配置")
		}
		idx = 0
		m.reg.ActiveID = m.reg.Profiles[0].ID
	}
	activeID := m.reg.ActiveID
	activeDir := m.RoomDir(activeID)
	if err := ensureRoomDirs(activeDir); err != nil {
		return err
	}

	// Restore active room settings into in-memory config without touching global fields.
	rs, err := config.LoadRoomSettings(m.roomSettingsPath(activeID))
	if err != nil {
		if os.IsNotExist(err) {
			rs = m.cfg.Get().RoomSettings()
			if err := config.SaveRoomSettings(m.roomSettingsPath(activeID), rs); err != nil {
				return err
			}
		} else {
			return err
		}
	}
	if err := m.cfg.ApplyRoom(rs); err != nil {
		return err
	}

	// Reconcile active room topology.yaml vs root topology_file.
	roomTopoPath := m.roomTopoPath(activeID)
	if data, err := os.ReadFile(roomTopoPath); err == nil {
		roomTopo, perr := topology.Parse(data)
		if perr != nil {
			return fmt.Errorf("加载激活机房拓扑失败: %w", perr)
		}
		rootMT := modTime(m.topo.Path())
		roomMT := modTime(roomTopoPath)
		if !rootMT.IsZero() && rootMT.After(roomMT.Add(2*time.Second)) {
			// Root topology file was edited externally while offline; sync it into room dir.
			curTopo, _ := m.topo.Snapshot()
			if err := m.saveRoomTopology(activeID, curTopo); err != nil {
				return err
			}
		} else {
			if err := m.topo.SwapActive(roomTopo); err != nil {
				return err
			}
		}
	} else if os.IsNotExist(err) {
		curTopo, _ := m.topo.Snapshot()
		if err := m.saveRoomTopology(activeID, curTopo); err != nil {
			return err
		}
	} else {
		return err
	}

	// Close any stale open observation span from an earlier run and start a fresh span.
	p := &m.reg.Profiles[idx]
	for i := range p.Observations {
		if p.Observations[i].ToMs == 0 {
			endMs := p.UpdatedAt.UnixMilli()
			if endMs < p.Observations[i].FromMs || endMs > nowMs {
				endMs = nowMs
			}
			p.Observations[i].ToMs = endMs
		}
	}
	p.Observations = append(p.Observations, ObservationSpan{FromMs: nowMs, ToMs: 0})
	p.UpdatedAt = now
	if err := m.saveRegistryLocked(); err != nil {
		return err
	}

	// Point all room-scoped services at the active room directory.
	if err := m.switchSubsystems(activeDir); err != nil {
		return err
	}
	if m.alerts != nil {
		curTopo, _ := m.topo.Snapshot()
		m.alerts.SetTopology(curTopo)
	}
	m.epoch.Store(1)
	return nil
}

func (m *Manager) migrateLegacyToDefault(defDir string) error {
	// Move legacy history directory if destination does not yet exist or is empty.
	oldHist := filepath.Join(m.dataDir, "history")
	newHist := filepath.Join(defDir, "history")
	if err := moveDirIfTargetEmpty(oldHist, newHist); err != nil {
		return err
	}
	oldReports := filepath.Join(m.dataDir, "reports")
	newReports := filepath.Join(defDir, "reports")
	if err := moveDirIfTargetEmpty(oldReports, newReports); err != nil {
		return err
	}
	for _, name := range []string{
		"alerts.jsonl",
		"alerts.jsonl.1",
		"maintenance.json",
		"discovery_results.json",
		"discovery_events.jsonl",
		"discovery_events.jsonl.1",
		"discovery_last.json",
		"locate_last.json",
	} {
		if err := moveFileIfMissing(filepath.Join(m.dataDir, name), filepath.Join(defDir, name)); err != nil {
			return err
		}
	}
	return nil
}

func moveDirIfTargetEmpty(src, dst string) error {
	fi, err := os.Stat(src)
	if os.IsNotExist(err) || (err == nil && !fi.IsDir()) {
		return nil
	}
	if err != nil {
		return err
	}
	ents, err := os.ReadDir(dst)
	if err == nil && len(ents) > 0 {
		return nil // destination already populated; never overwrite or duplicate
	}
	if err == nil {
		_ = os.Remove(dst)
	}
	return os.Rename(src, dst)
}

func moveFileIfMissing(src, dst string) error {
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return nil
	}
	if _, err := os.Stat(dst); err == nil {
		return nil // destination already exists; keep it intact
	}
	return os.Rename(src, dst)
}

func ensureRoomDirs(roomDir string) error {
	for _, sub := range []string{"", "history", "reports", "versions"} {
		if err := os.MkdirAll(filepath.Join(roomDir, sub), 0o755); err != nil {
			return err
		}
	}
	return nil
}

func modTime(path string) time.Time {
	if fi, err := os.Stat(path); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

// RoomDir returns the isolated data directory for profile id.
func (m *Manager) RoomDir(id string) string {
	return filepath.Join(m.dataDir, "rooms", id)
}

func (m *Manager) roomTopoPath(id string) string {
	return filepath.Join(m.RoomDir(id), "topology.yaml")
}

func (m *Manager) roomSettingsPath(id string) string {
	return filepath.Join(m.RoomDir(id), "settings.yaml")
}

// ActiveID returns the currently active profile ID.
func (m *Manager) ActiveID() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.reg.ActiveID == "" {
		return DefaultProfileID
	}
	return m.reg.ActiveID
}

// NameOf returns the display name of profile id, or "" if it does not exist.
func (m *Manager) NameOf(id string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if idx := m.findIdxLocked(id); idx >= 0 {
		return m.reg.Profiles[idx].Name
	}
	return ""
}

// Epoch returns a monotonically increasing counter incremented on every successful profile activation.
func (m *Manager) Epoch() int64 {
	return m.epoch.Load()
}

// ActiveState returns both the active profile ID and current epoch atomically.
func (m *Manager) ActiveState() (string, int64) {
	m.mu.RLock()
	id := m.reg.ActiveID
	if id == "" {
		id = DefaultProfileID
	}
	ep := m.epoch.Load()
	m.mu.RUnlock()
	return id, ep
}

func (m *Manager) findIdxLocked(id string) int {
	for i, p := range m.reg.Profiles {
		if p.ID == id {
			return i
		}
	}
	return -1
}

func (m *Manager) saveRegistryLocked() error {
	data, err := json.MarshalIndent(m.reg, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomicMode(m.regPath, data, 0o600)
}

func (m *Manager) saveRoomTopology(id string, t *topology.Topology) error {
	data, err := topology.Marshal(t)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(m.roomTopoPath(id), data)
}

func (m *Manager) loadRoomTopology(id string) (*topology.Topology, error) {
	if id == m.ActiveID() {
		t, _ := m.topo.Snapshot()
		return t, nil
	}
	data, err := os.ReadFile(m.roomTopoPath(id))
	if err != nil {
		return nil, err
	}
	return topology.Parse(data)
}

func (m *Manager) loadRoomSettings(id string) (config.RoomSettings, error) {
	if id == m.ActiveID() {
		return m.cfg.Get().RoomSettings(), nil
	}
	return config.LoadRoomSettings(m.roomSettingsPath(id))
}

// List returns metadata and counts for all profiles.
func (m *Manager) List() []ProfileInfo {
	m.mu.RLock()
	active := m.reg.ActiveID
	metas := append([]ProfileMeta(nil), m.reg.Profiles...)
	m.mu.RUnlock()

	nowMs := time.Now().UnixMilli()
	out := make([]ProfileInfo, 0, len(metas))
	for _, p := range metas {
		var nodes, switches int
		if t, err := m.loadRoomTopology(p.ID); err == nil && t != nil {
			nodes = len(t.Nodes)
			switches = len(t.Switches)
		}
		out = append(out, ProfileInfo{
			ID:                 p.ID,
			Name:               p.Name,
			Active:             p.ID == active,
			Nodes:              nodes,
			Switches:           switches,
			CreatedAt:          p.CreatedAt,
			UpdatedAt:          p.UpdatedAt,
			ObservationMinutes: totalObservationMinutes(p.Observations, nowMs),
		})
	}
	return out
}

func totalObservationMinutes(spans []ObservationSpan, nowMs int64) int64 {
	var totalMs int64
	for _, sp := range spans {
		end := sp.ToMs
		if end == 0 {
			end = nowMs
		}
		if end > sp.FromMs {
			totalMs += end - sp.FromMs
		}
	}
	return totalMs / 60000
}

// ObservedWindows returns clipped active observation spans for profileID within [from, to].
func (m *Manager) ObservedWindows(profileID string, from, to time.Time) []ObservationSpan {
	if profileID == "" {
		profileID = m.ActiveID()
	}
	fromMs := from.UnixMilli()
	toMs := to.UnixMilli()
	if toMs <= fromMs {
		return nil
	}
	nowMs := time.Now().UnixMilli()
	m.mu.RLock()
	idx := m.findIdxLocked(profileID)
	var spans []ObservationSpan
	if idx >= 0 {
		spans = append([]ObservationSpan(nil), m.reg.Profiles[idx].Observations...)
	}
	m.mu.RUnlock()

	var out []ObservationSpan
	for _, sp := range spans {
		a := sp.FromMs
		b := sp.ToMs
		if b == 0 {
			b = nowMs
		}
		if a < fromMs {
			a = fromMs
		}
		if b > toMs {
			b = toMs
		}
		if b > a {
			if n := len(out); n > 0 && a <= out[n-1].ToMs {
				if b > out[n-1].ToMs {
					out[n-1].ToMs = b
				}
				continue
			}
			out = append(out, ObservationSpan{FromMs: a, ToMs: b})
		}
	}
	return out
}

func validateProfileName(name string, existing []ProfileMeta, skipID string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 80 {
		return "", errors.New("机房名称须为 1–80 个字符")
	}
	for _, p := range existing {
		if p.Name == name && p.ID != skipID {
			return "", errors.New("机房名称已存在")
		}
	}
	return name, nil
}

// Create creates a new inactive profile (blank or cloned from current/sourceID).
func (m *Manager) Create(name string, copyCurrent bool, sourceID string) (ProfileInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cleanName, err := validateProfileName(name, m.reg.Profiles, "")
	if err != nil {
		return ProfileInfo{}, err
	}
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ProfileInfo{}, err
	}
	id := "room-" + hex.EncodeToString(b[:])

	var t *topology.Topology
	var rs config.RoomSettings
	if copyCurrent || sourceID != "" {
		src := sourceID
		if src == "" {
			src = m.reg.ActiveID
		}
		if m.findIdxLocked(src) < 0 {
			return ProfileInfo{}, fmt.Errorf("源机房 %q 不存在", src)
		}
		if src == m.reg.ActiveID {
			cur, _ := m.topo.Snapshot()
			t = cur.Clone()
			rs = m.cfg.Get().RoomSettings()
		} else {
			data, err := os.ReadFile(m.roomTopoPath(src))
			if err != nil {
				return ProfileInfo{}, err
			}
			t, err = topology.Parse(data)
			if err != nil {
				return ProfileInfo{}, err
			}
			rs, err = config.LoadRoomSettings(m.roomSettingsPath(src))
			if err != nil {
				return ProfileInfo{}, err
			}
		}
		t.Site.Name = cleanName
	} else {
		t = &topology.Topology{Version: 1, Site: topology.Site{Name: cleanName}}
		rs = config.DefaultRoomSettings()
		rs.Alert.SiteTitle = cleanName
	}
	t.Normalize()
	if probs := t.Validate(); len(probs) > 0 {
		return ProfileInfo{}, &topology.ValidationError{Problems: probs}
	}
	rs.Normalize()

	roomDir := m.RoomDir(id)
	if err := ensureRoomDirs(roomDir); err != nil {
		return ProfileInfo{}, err
	}
	if err := m.saveRoomTopology(id, t); err != nil {
		_ = os.RemoveAll(roomDir)
		return ProfileInfo{}, err
	}
	if err := config.SaveRoomSettings(m.roomSettingsPath(id), rs); err != nil {
		_ = os.RemoveAll(roomDir)
		return ProfileInfo{}, err
	}

	now := time.Now()
	meta := ProfileMeta{ID: id, Name: cleanName, CreatedAt: now, UpdatedAt: now}
	m.reg.Profiles = append(m.reg.Profiles, meta)
	if err := m.saveRegistryLocked(); err != nil {
		m.reg.Profiles = m.reg.Profiles[:len(m.reg.Profiles)-1]
		_ = os.RemoveAll(roomDir)
		return ProfileInfo{}, err
	}
	return ProfileInfo{
		ID:        id,
		Name:      cleanName,
		Active:    false,
		Nodes:     len(t.Nodes),
		Switches:  len(t.Switches),
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// Rename changes the display name of a profile.
func (m *Manager) Rename(id, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := m.findIdxLocked(id)
	if idx < 0 {
		return ErrNotFound
	}
	cleanName, err := validateProfileName(name, m.reg.Profiles, id)
	if err != nil {
		return err
	}
	old := m.reg.Profiles[idx]
	m.reg.Profiles[idx].Name = cleanName
	m.reg.Profiles[idx].UpdatedAt = time.Now()
	if err := m.saveRegistryLocked(); err != nil {
		m.reg.Profiles[idx] = old
		return err
	}
	return nil
}

// Delete removes an inactive profile and its isolated directory.
func (m *Manager) Delete(id string) error {
	m.switchMu.Lock()
	defer m.switchMu.Unlock()
	m.mu.Lock()
	idx := m.findIdxLocked(id)
	if idx < 0 {
		m.mu.Unlock()
		return ErrNotFound
	}
	if id == m.reg.ActiveID {
		m.mu.Unlock()
		return errors.New("不能删除当前激活的机房，请先激活另一套配置")
	}
	prev := append([]ProfileMeta(nil), m.reg.Profiles...)
	m.reg.Profiles = append(m.reg.Profiles[:idx], m.reg.Profiles[idx+1:]...)
	if err := m.saveRegistryLocked(); err != nil {
		m.reg.Profiles = prev
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()
	_ = os.RemoveAll(m.RoomDir(id))
	return nil
}

// GetSettings returns the profile's unmasked RoomSettings and metadata (callers must mask before HTTP output).
func (m *Manager) GetSettings(id string) (config.RoomSettings, ProfileMeta, bool, error) {
	m.mu.RLock()
	idx := m.findIdxLocked(id)
	if idx < 0 {
		m.mu.RUnlock()
		return config.RoomSettings{}, ProfileMeta{}, false, ErrNotFound
	}
	meta := m.reg.Profiles[idx]
	active := id == m.reg.ActiveID
	m.mu.RUnlock()

	rs, err := m.loadRoomSettings(id)
	if err != nil {
		return config.RoomSettings{}, ProfileMeta{}, false, err
	}
	return rs, meta, active, nil
}

// UpdateSettings patches and persists per-room settings for profile id (active or inactive).
func (m *Manager) UpdateSettings(id string, rawPatch []byte) (config.RoomSettings, error) {
	m.mu.RLock()
	idx := m.findIdxLocked(id)
	active := id == m.reg.ActiveID
	m.mu.RUnlock()
	if idx < 0 {
		return config.RoomSettings{}, ErrNotFound
	}

	cur, err := m.loadRoomSettings(id)
	if err != nil {
		return config.RoomSettings{}, err
	}
	next, err := config.PatchRoomSettings(cur, rawPatch)
	if err != nil {
		return config.RoomSettings{}, err
	}
	if err := config.SaveRoomSettings(m.roomSettingsPath(id), next); err != nil {
		return config.RoomSettings{}, err
	}
	if active {
		if err := m.cfg.ApplyRoom(next); err != nil {
			_ = config.SaveRoomSettings(m.roomSettingsPath(id), cur)
			return config.RoomSettings{}, err
		}
		if m.mon != nil {
			t, _ := m.topo.Snapshot()
			m.mon.Sync(monitor.BuildTargets(t, next.Ping))
		}
	}
	m.touchProfile(id)
	return next, nil
}

// SaveActiveSettings saves updated per-room settings (e.g. from PUT /api/config or MCP) to the active room's settings.yaml.
func (m *Manager) SaveActiveSettings(rs config.RoomSettings) error {
	id := m.ActiveID()
	if err := config.SaveRoomSettings(m.roomSettingsPath(id), rs); err != nil {
		return err
	}
	m.touchProfile(id)
	return nil
}

func (m *Manager) touchProfile(id string) {
	m.mu.Lock()
	if idx := m.findIdxLocked(id); idx >= 0 {
		m.reg.Profiles[idx].UpdatedAt = time.Now()
		_ = m.saveRegistryLocked()
	}
	m.mu.Unlock()
}

// Activate switches the active profile safely: drains Monitor, persists registry & topology,
// switches all room-scoped directories, and rolls back completely if any disk/service step fails.
func (m *Manager) Activate(id string) error {
	m.switchMu.Lock()
	defer m.switchMu.Unlock()

	unlockTopo, ok := m.topo.TryLockProfile()
	if !ok {
		return ErrBusy
	}
	defer unlockTopo()

	if (m.disc != nil && m.disc.Running()) || (m.loc != nil && m.loc.Running()) {
		return ErrBusy
	}

	m.mu.RLock()
	oldID := m.reg.ActiveID
	oldIdx := m.findIdxLocked(oldID)
	newIdx := m.findIdxLocked(id)
	m.mu.RUnlock()

	if newIdx < 0 {
		return ErrNotFound
	}
	if id == oldID {
		return nil
	}

	// Load & validate target profile before touching runtime state.
	oldTopo, _ := m.topo.Snapshot()
	oldSettings := m.cfg.Get().RoomSettings()
	oldDir := m.RoomDir(oldID)

	newDir := m.RoomDir(id)
	if err := ensureRoomDirs(newDir); err != nil {
		return err
	}
	newTopoData, err := os.ReadFile(m.roomTopoPath(id))
	if err != nil {
		return fmt.Errorf("读取目标机房拓扑失败: %w", err)
	}
	newTopo, err := topology.Parse(newTopoData)
	if err != nil {
		return err
	}
	newSettings, err := config.LoadRoomSettings(m.roomSettingsPath(id))
	if err != nil {
		return fmt.Errorf("读取目标机房设置失败: %w", err)
	}
	if err := newSettings.Validate(); err != nil {
		return err
	}

	// Flush active room topology & settings to its room directory before switching.
	if err := m.saveRoomTopology(oldID, oldTopo); err != nil {
		return fmt.Errorf("保存当前机房拓扑失败: %w", err)
	}
	if err := config.SaveRoomSettings(m.roomSettingsPath(oldID), oldSettings); err != nil {
		return fmt.Errorf("保存当前机房设置失败: %w", err)
	}

	// Drain in-flight probes and buffered minute records to old store before swapping directories.
	m.switching.Store(true)
	if m.mon != nil {
		m.mon.BeginSwitch()
	}
	oldTargets := monitor.BuildTargets(oldTopo, oldSettings.Ping)
	newTargets := monitor.BuildTargets(newTopo, newSettings.Ping)

	now := time.Now()
	nowMs := now.UnixMilli()

	// Snapshot registry for rollback.
	m.mu.Lock()
	savedReg := cloneRegistry(m.reg)
	if oldIdx >= 0 {
		closeOpenObservation(&m.reg.Profiles[oldIdx], nowMs)
		m.reg.Profiles[oldIdx].UpdatedAt = now
	}
	m.reg.Profiles[newIdx].Observations = append(m.reg.Profiles[newIdx].Observations, ObservationSpan{FromMs: nowMs, ToMs: 0})
	m.reg.Profiles[newIdx].UpdatedAt = now
	m.reg.ActiveID = id

	rollback := func(cause error) error {
		_ = m.switchSubsystems(oldDir)
		_ = m.cfg.ApplyRoom(oldSettings)
		_ = m.topo.SwapActive(oldTopo)
		if m.alerts != nil {
			m.alerts.SetTopology(oldTopo)
		}
		m.reg = savedReg
		_ = m.saveRegistryLocked()
		m.mu.Unlock()
		m.switching.Store(false)
		if m.mon != nil {
			m.mon.EndSwitch(oldTargets)
		}
		return cause
	}

	if err := m.saveRegistryLocked(); err != nil {
		return rollback(err)
	}
	if err := m.topo.SwapActive(newTopo); err != nil {
		return rollback(err)
	}
	if err := m.switchSubsystems(newDir); err != nil {
		return rollback(err)
	}
	if err := m.cfg.ApplyRoom(newSettings); err != nil {
		return rollback(err)
	}
	if m.alerts != nil {
		m.alerts.SetTopology(newTopo)
	}
	m.epoch.Add(1)
	m.mu.Unlock()

	m.switching.Store(false)
	if m.mon != nil {
		m.mon.EndSwitch(newTargets)
	}
	m.topo.NotifyCurrent()
	return nil
}

func (m *Manager) switchSubsystems(roomDir string) error {
	if m.store != nil {
		if err := m.store.SwitchDir(filepath.Join(roomDir, "history")); err != nil {
			return err
		}
	}
	if m.alerts != nil {
		if err := m.alerts.SwitchDataDir(roomDir); err != nil {
			return err
		}
	}
	if m.disc != nil {
		if err := m.disc.SwitchDataDir(roomDir); err != nil {
			return err
		}
	}
	if m.loc != nil {
		if err := m.loc.SwitchDataDir(roomDir); err != nil {
			return err
		}
	}
	return nil
}

// Shutdown closes the currently open observation span on graceful exit.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if idx := m.findIdxLocked(m.reg.ActiveID); idx >= 0 {
		now := time.Now()
		closeOpenObservation(&m.reg.Profiles[idx], now.UnixMilli())
		m.reg.Profiles[idx].UpdatedAt = now
		_ = m.saveRegistryLocked()
	}
}

func closeOpenObservation(p *ProfileMeta, endMs int64) {
	for i := range p.Observations {
		if p.Observations[i].ToMs == 0 {
			if endMs < p.Observations[i].FromMs {
				endMs = p.Observations[i].FromMs
			}
			p.Observations[i].ToMs = endMs
		}
	}
}

func cloneRegistry(r Registry) Registry {
	out := r
	out.Profiles = make([]ProfileMeta, len(r.Profiles))
	for i, p := range r.Profiles {
		p.Observations = append([]ObservationSpan(nil), p.Observations...)
		out.Profiles[i] = p
	}
	return out
}
