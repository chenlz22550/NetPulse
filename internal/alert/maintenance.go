package alert

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type MaintenanceWindow struct {
	ID        string    `json:"id"`
	Scope     string    `json:"scope"`
	TargetIDs []string  `json:"target_ids"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	Reason    string    `json:"reason"`
	Cancelled bool      `json:"cancelled"`
	Status    string    `json:"status"`
}

func (e *Engine) DataDir() string { e.mu.Lock(); defer e.mu.Unlock(); return e.dataDirLocked() }
func (e *Engine) dataDirLocked() string {
	if e.histPath == "" {
		return ""
	}
	return filepath.Dir(e.histPath)
}

// SwitchDataDir must be called while the owner has paused profile requests and monitoring.
// In-flight notifications finish in the old profile before switching.
func (e *Engine) SwitchDataDir(dir string) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	old := e.histPath
	e.histPath = filepath.Join(dir, "alerts.jsonl")
	if err := e.loadMaintenanceLocked(); err != nil {
		e.histPath = old
		return err
	}
	e.maintenanceErr = nil
	e.history = nil
	e.pending = nil
	e.firstPend = time.Time{}
	e.states = map[string]*tstate{}
	e.observedDown = map[string]bool{}
	e.loadHistory()
	return nil
}
func (e *Engine) loadMaintenanceLocked() error {
	var windows []MaintenanceWindow
	if dir := e.dataDirLocked(); dir != "" {
		b, err := os.ReadFile(filepath.Join(dir, "maintenance.json"))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil {
			if err = json.Unmarshal(b, &windows); err != nil {
				return err
			}
		}
	}
	e.maintenance = windows
	return nil
}
func (e *Engine) saveMaintenanceLocked(windows []MaintenanceWindow) error {
	if dir := e.dataDirLocked(); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		b, err := json.MarshalIndent(windows, "", "  ")
		if err != nil {
			return err
		}
		path := filepath.Join(dir, "maintenance.json")
		if err = os.WriteFile(path+".tmp", b, 0600); err != nil {
			return err
		}
		if err = os.Rename(path+".tmp", path); err != nil {
			return err
		}
	}
	e.maintenance = windows
	return nil
}
func (e *Engine) AddMaintenance(w MaintenanceWindow) (MaintenanceWindow, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.maintenanceErr != nil {
		return w, e.maintenanceErr
	}
	if w.Scope != "site" && w.Scope != "devices" {
		return w, errors.New("维护范围只能是 site 或 devices")
	}
	if w.Start.IsZero() || !w.End.After(w.Start) || !w.End.After(time.Now()) {
		return w, errors.New("维护起止时间无效或已过期")
	}
	if len(w.Reason) > 2000 {
		return w, errors.New("维护原因过长")
	}
	if w.Scope == "devices" {
		if len(w.TargetIDs) == 0 {
			return w, errors.New("请选择维护设备")
		}
		for _, id := range w.TargetIDs {
			if _, ok := e.info[id]; !ok {
				return w, errors.New("维护设备不存在")
			}
		}
	} else {
		w.TargetIDs = nil
	}
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return w, err
	}
	w.ID = hex.EncodeToString(b)
	w.Cancelled = false
	w.Status = ""
	w.TargetIDs = append([]string(nil), w.TargetIDs...)
	windows := append(append([]MaintenanceWindow(nil), e.maintenance...), w)
	if err := e.saveMaintenanceLocked(windows); err != nil {
		return w, err
	}
	e.drainMaintenanceLocked(time.Now())
	return w, nil
}
func (e *Engine) CancelMaintenance(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	windows := append([]MaintenanceWindow(nil), e.maintenance...)
	for i := range windows {
		if windows[i].ID == id {
			windows[i].Cancelled = true
			return e.saveMaintenanceLocked(windows)
		}
	}
	return os.ErrNotExist
}
func (e *Engine) MaintenanceWindows(now time.Time) []MaintenanceWindow {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]MaintenanceWindow, 0, len(e.maintenance))
	if e.maintenanceErr != nil {
		return []MaintenanceWindow{{ID: "invalid-maintenance-file", Scope: "site", Status: "error", Reason: "维护窗口文件损坏，告警静默保护中，请修复文件并重新加载"}}
	}
	for _, w := range e.maintenance {
		w.TargetIDs = append([]string(nil), w.TargetIDs...)
		switch {
		case w.Cancelled:
			w.Status = "cancelled"
		case !now.Before(w.End):
			w.Status = "expired"
		case now.Before(w.Start):
			w.Status = "scheduled"
		default:
			w.Status = "active"
		}
		out = append(out, w)
	}
	return out
}
func (e *Engine) maintenanceIDLocked(id, parent string, now time.Time) string {
	if e.maintenanceErr != nil {
		return "invalid-maintenance-file"
	}
	if parent == "" && strings.HasPrefix(id, "_tcp_") {
		parts := strings.SplitN(strings.TrimPrefix(id, "_tcp_"), "_", 2)
		if len(parts) == 2 {
			parent = parts[1]
		}
	}
	for _, w := range e.maintenance {
		if w.Cancelled || now.Before(w.Start) || !now.Before(w.End) {
			continue
		}
		if w.Scope == "site" {
			return w.ID
		}
		for _, target := range w.TargetIDs {
			if target == id || target == parent {
				return w.ID
			}
		}
	}
	return ""
}

// Only observed offline switches can be a cause; descendants alone never establish one.
func (e *Engine) offlineAncestorLocked(ev Event) string {
	if ev.Type != EvDown && ev.Type != EvStillDown {
		return ""
	}
	seen := map[string]bool{ev.TargetID: true}
	root := ""
	for p := e.info[ev.TargetID].Parent; p != ""; p = e.info[p].Parent {
		if seen[p] {
			return ""
		}
		seen[p] = true
		if _, ok := e.switchName[p]; ok && e.observedDown[p] {
			root = p
		}
	}
	return root
}

// Drop queued notifications as soon as a window starts, retaining the raw events.
func (e *Engine) drainMaintenanceLocked(now time.Time) {
	for id, state := range e.states {
		if e.maintenanceIDLocked(id, "", now) != "" {
			state.maintenance = true
		}
	}
	keep := e.pending[:0]
	var muted []Event
	for _, ev := range e.pending {
		if id := e.maintenanceIDLocked(ev.TargetID, "", now); id != "" {
			ev.Suppressed = "maintenance"
			ev.MaintenanceID = id
			muted = append(muted, ev)
		} else {
			keep = append(keep, ev)
		}
	}
	e.pending = keep
	if len(keep) > 0 {
		e.firstPend = keep[0].Time
	}
	if len(muted) > 0 {
		e.appendHistoryLocked(muted)
	}
}
