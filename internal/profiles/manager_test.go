package profiles

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"netping/internal/alert"
	"netping/internal/config"
	"netping/internal/discovery"
	"netping/internal/locate"
	"netping/internal/monitor"
	"netping/internal/store"
	"netping/internal/topology"
)

func setupTestEnv(t *testing.T, baseDir string, seedLegacy bool) (*config.Manager, *topology.Store, *monitor.Monitor, *store.Store, *alert.Engine, *discovery.Service, *locate.Service, *Manager) {
	t.Helper()
	cfgPath := filepath.Join(baseDir, "config.yaml")
	cfgMgr, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	c := cfgMgr.Get()
	c.Listen = "127.0.0.1:18090"
	c.Auth = config.Auth{Username: "admin", Password: "secret-password"}
	c.MCP.Token = "mcp-secret-token"
	c.AI.APIKey = "sk-global-ai-key"
	c.Alert.Enabled = true
	c.Alert.WebhookURL = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=room-a-secret"
	c.Locate.Community = "snmp-room-a"
	c.Ping.IntervalMs = 500
	c.Discovery.Jobs = []config.ScanJob{{
		ID:          "job-a",
		Name:        "A机房网段",
		Enabled:     true,
		Subnets:     []string{"10.1.0.0/24"},
		IntervalMin: 15,
	}}
	if err := cfgMgr.Update(c); err != nil {
		t.Fatal(err)
	}

	topoPath := filepath.Join(baseDir, "topology.yaml")
	topo, err := topology.Open(topoPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := topo.Replace(&topology.Topology{
		Version: 1,
		Site:    topology.Site{Name: "A机房", Subnet: "10.1.0.0/24", Gateway: "10.1.0.1"},
		Switches: []*topology.Switch{
			{ID: "sw-a", Name: "A核心", Role: "core", IP: "10.1.0.254", Ports: []string{"1", "2"}},
		},
		Nodes: []*topology.Node{
			{ID: "n1", Name: "A节点1", IP: "10.1.0.10", Attach: topology.Endpoint{Switch: "sw-a", Port: "1"}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	dataDir := filepath.Join(baseDir, "data")
	if seedLegacy {
		_ = os.MkdirAll(filepath.Join(dataDir, "history", "n1"), 0o755)
		_ = os.MkdirAll(filepath.Join(dataDir, "reports"), 0o755)
		legacyStore, err := store.New(filepath.Join(dataDir, "history"))
		if err != nil {
			t.Fatal(err)
		}
		_ = legacyStore.Append("n1", store.Minute{T: time.Now().Add(-time.Hour).Unix(), Sent: 10, Recv: 10, Avg: 1.5, Min: 1.0, Max: 2.0})
		_ = os.WriteFile(filepath.Join(dataDir, "alerts.jsonl"), []byte(`{"time":"2026-11-14T00:00:00Z","type":"down","target_id":"n1","detail":"legacy"}`+"\n"), 0o644)
		_ = os.WriteFile(filepath.Join(dataDir, "maintenance.json"), []byte(`[]`), 0o600)
		_ = os.WriteFile(filepath.Join(dataDir, "reports", "20261114-000000.json"), []byte(`{"id":"20261114-000000"}`), 0o644)
		_ = os.WriteFile(filepath.Join(dataDir, "discovery_results.json"), []byte(`{"job-a":{"job":"job-a","job_name":"legacy"}}`), 0o644)
		_ = os.WriteFile(filepath.Join(dataDir, "discovery_events.jsonl"), []byte(`{"type":"added","node_id":"n1"}`+"\n"), 0o644)
		_ = os.WriteFile(filepath.Join(dataDir, "locate_last.json"), []byte(`{"reason":"legacy"}`), 0o644)
	}

	st, err := store.New(filepath.Join(dataDir, "history"))
	if err != nil {
		t.Fatal(err)
	}
	mon := monitor.New(st, func() int { return cfgMgr.Get().Alert.DownAfterFailures })
	mon.SetPaused(true)
	al := alert.NewEngine(cfgMgr.Get, mon, filepath.Join(dataDir, "alerts.jsonl"))
	disc := discovery.New(cfgMgr, topo, mon, dataDir)
	loc := locate.New(cfgMgr, topo, dataDir)

	mgr, err := Open(Options{
		BaseDir: baseDir,
		Cfg:     cfgMgr,
		Topo:    topo,
		Mon:     mon,
		Store:   st,
		Alerts:  al,
		Disc:    disc,
		Loc:     loc,
		ProbeFunc: func(ip string, timeout time.Duration) (string, string) {
			if ip == "10.2.0.1" {
				return "reachable", "延迟 1.20 ms"
			}
			return "unreachable", "超时（>800ms）"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfgMgr, topo, mon, st, al, disc, loc, mgr
}

func TestLegacyMigrationIdempotentAndRestartRecovery(t *testing.T) {
	baseDir := t.TempDir()
	cfgMgr, topo, mon, st, al, disc, loc, mgr := setupTestEnv(t, baseDir, true)

	if mgr.ActiveID() != DefaultProfileID {
		t.Fatalf("want active=default, got %s", mgr.ActiveID())
	}
	defDir := mgr.RoomDir(DefaultProfileID)
	for _, rel := range []string{
		"alerts.jsonl",
		"maintenance.json",
		"reports/20261114-000000.json",
		"discovery_results.json",
		"discovery_events.jsonl",
		"locate_last.json",
		"settings.yaml",
		"topology.yaml",
	} {
		if _, err := os.Stat(filepath.Join(defDir, rel)); err != nil {
			t.Fatalf("expected migrated file %s: %v", rel, err)
		}
	}
	recs, err := st.Query("n1", time.Now().Add(-2*time.Hour).Unix(), time.Now().Unix()+60)
	if err != nil || len(recs) != 1 {
		t.Fatalf("expected 1 migrated history record, got %d (err=%v)", len(recs), err)
	}

	// Simulate newer data written to default, plus leftover stale file in old root data/
	_ = os.WriteFile(filepath.Join(defDir, "alerts.jsonl"), []byte(`{"time":"2026-11-14T01:00:00Z","type":"recover","target_id":"n1","detail":"new"}`+"\n"), 0o644)
	_ = os.WriteFile(filepath.Join(baseDir, "data", "alerts.jsonl"), []byte(`{"detail":"stale-should-not-overwrite"}`+"\n"), 0o644)

	mgr.Shutdown()
	mon.Close()
	_ = disc
	_ = loc
	_ = al
	_ = topo
	_ = cfgMgr

	// Reopen: migration must NOT repeat or overwrite default's new data.
	cfgMgr2, err := config.Load(filepath.Join(baseDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	topo2, err := topology.Open(filepath.Join(baseDir, "topology.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	st2, _ := store.New(filepath.Join(baseDir, "data", "history"))
	mon2 := monitor.New(st2, func() int { return 5 })
	mon2.SetPaused(true)
	defer mon2.Close()
	al2 := alert.NewEngine(cfgMgr2.Get, mon2, filepath.Join(baseDir, "data", "alerts.jsonl"))
	disc2 := discovery.New(cfgMgr2, topo2, mon2, filepath.Join(baseDir, "data"))
	loc2 := locate.New(cfgMgr2, topo2, filepath.Join(baseDir, "data"))

	mgr2, err := Open(Options{
		BaseDir: baseDir, Cfg: cfgMgr2, Topo: topo2, Mon: mon2, Store: st2, Alerts: al2, Disc: disc2, Loc: loc2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr2.Shutdown()

	gotAlerts, _ := os.ReadFile(filepath.Join(defDir, "alerts.jsonl"))
	if strings.Contains(string(gotAlerts), "stale-should-not-overwrite") || !strings.Contains(string(gotAlerts), "new") {
		t.Fatalf("legacy migration repeated on restart: %s", gotAlerts)
	}
}

func TestProfileIsolationActivationPreviewAndRollbackOnFailure(t *testing.T) {
	baseDir := t.TempDir()
	cfgMgr, topo, mon, st, al, _, _, mgr := setupTestEnv(t, baseDir, false)
	defer func() {
		mgr.Shutdown()
		mon.Close()
	}()

	// Create room B (blank) and configure distinct room settings + topology.
	bInfo, err := mgr.Create("机房B", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if bInfo.Active {
		t.Fatal("newly created profile should not be active")
	}

	// Put topology into room B directly (simulating preparing room B before activation).
	bTopo := &topology.Topology{
		Version: 1,
		Site:    topology.Site{Name: "机房B", Subnet: "10.2.0.0/24", Gateway: "10.2.0.1"},
		Switches: []*topology.Switch{
			{ID: "sw-b", Name: "B核心", Role: "core", IP: "10.2.0.254", Ports: []string{"1"}},
		},
		Nodes: []*topology.Node{
			{ID: "nb1", Name: "B节点1", IP: "10.2.0.10", Attach: topology.Endpoint{Switch: "sw-b", Port: "1"}},
		},
	}
	bTopoBytes, err := topology.Marshal(bTopo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mgr.RoomDir(bInfo.ID), "topology.yaml"), bTopoBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	patch := []byte(`{
		"ping": {"interval_ms": 2000, "timeout_ms": 1500, "packet_size": 64, "latency_warn_ms": 200, "loss_warn_pct": 10},
		"alert": {"enabled": true, "webhook_url": "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=room-b-secret", "site_title": "B标题"},
		"locate": {"enabled": true, "community": "snmp-room-b"},
		"schedule": {"enabled": true, "windows": [{"start": "08:00", "end": "20:00"}]},
		"discovery": {"enabled": true, "jobs": [{"id": "job-b", "name": "B扫描", "enabled": true, "subnets": ["10.2.0.0/24"], "interval_min": 20}]}
	}`)
	if _, err := mgr.UpdateSettings(bInfo.ID, patch); err != nil {
		t.Fatal(err)
	}

	// Preview room B before activation: scan_ranges, masked destinations, connectivity.
	prev, err := mgr.Preview(bInfo.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !prev.CanActivate || prev.AlreadyActive {
		t.Fatalf("unexpected preview flags: %+v", prev)
	}
	if len(prev.ScanRanges) != 1 || prev.ScanRanges[0] != "10.2.0.0/24" {
		t.Fatalf("unexpected scan_ranges: %+v", prev.ScanRanges)
	}
	if len(prev.NotificationDestinations) != 1 || prev.NotificationDestinations[0].Channel != "企业微信" || strings.Contains(prev.NotificationDestinations[0].Destination, "room-b-secret") {
		t.Fatalf("notification destination missing or leaked secret: %+v", prev.NotificationDestinations)
	}
	if len(prev.Connectivity) != 2 || prev.Connectivity[0].Status != "reachable" || prev.Connectivity[1].Status != "unreachable" {
		t.Fatalf("unexpected connectivity checks: %+v", prev.Connectivity)
	}
	if len(prev.Warnings) == 0 {
		t.Fatal("expected unreachable switch to produce a non-blocking warning")
	}

	// Busy lock must block activation.
	release := topo.ProfileWork()
	if err := mgr.Activate(bInfo.ID); err != ErrBusy {
		release()
		t.Fatalf("expected ErrBusy while ProfileWork held, got %v", err)
	}
	release()

	// Write same-minute history to room A before switch.
	minTS := time.Now().Truncate(time.Minute).Unix()
	_ = st.Append("n1", store.Minute{T: minTS, Sent: 5, Recv: 5, Avg: 2.0, Min: 1.0, Max: 3.0})

	// Activate room B.
	if err := mgr.Activate(bInfo.ID); err != nil {
		t.Fatalf("Activate failed: %v", err)
	}
	if mgr.ActiveID() != bInfo.ID {
		t.Fatalf("active profile = %s, want %s", mgr.ActiveID(), bInfo.ID)
	}

	// Verify global credentials stayed intact and room B settings took effect.
	gotCfg := cfgMgr.Get()
	if gotCfg.Listen != "127.0.0.1:18090" || gotCfg.Auth.Password != "secret-password" || gotCfg.MCP.Token != "mcp-secret-token" || gotCfg.AI.APIKey != "sk-global-ai-key" {
		t.Fatalf("global settings corrupted after switch: %+v", gotCfg)
	}
	if gotCfg.Ping.IntervalMs != 2000 || gotCfg.Locate.Community != "snmp-room-b" || !gotCfg.Schedule.Enabled || gotCfg.Alert.SiteTitle != "B标题" {
		t.Fatalf("room B settings not active: %+v", gotCfg)
	}

	// Verify topology YAML export contains zero secrets.
	yamlBytes, err := topo.YAML()
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"room-b-secret", "snmp-room-b", "sk-global-ai-key", "secret-password", "mcp-secret-token"} {
		if strings.Contains(string(yamlBytes), secret) {
			t.Fatalf("topology export leaked secret %q", secret)
		}
	}

	// Verify history and alert DataDir are isolated from room A.
	recsB, err := st.Query("n1", minTS-60, minTS+120)
	if err != nil || len(recsB) != 0 {
		t.Fatalf("room A history leaked into room B: %+v", recsB)
	}
	if al.DataDir() != mgr.RoomDir(bInfo.ID) {
		t.Fatalf("alert DataDir = %s, want %s", al.DataDir(), mgr.RoomDir(bInfo.ID))
	}

	// Switch back to room A within the same minute and write more samples -> Query merges cleanly!
	if err := mgr.Activate(DefaultProfileID); err != nil {
		t.Fatal(err)
	}
	_ = st.Append("n1", store.Minute{T: minTS, Sent: 4, Recv: 2, Avg: 4.0, Min: 2.0, Max: 6.0})
	recsA, err := st.Query("n1", minTS-60, minTS+120)
	if err != nil || len(recsA) != 1 || recsA[0].Sent != 9 || recsA[0].Recv != 7 {
		t.Fatalf("expected merged same-minute record (sent=9, recv=7), got %+v (err=%v)", recsA, err)
	}

	// Simulate disk failure during activation of room B (replace room B's history dir with a regular file).
	_ = os.RemoveAll(filepath.Join(mgr.RoomDir(bInfo.ID), "history"))
	if err := os.WriteFile(filepath.Join(mgr.RoomDir(bInfo.ID), "history"), []byte("blocker"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Activate(bInfo.ID); err == nil {
		t.Fatal("expected activation error when history path is blocked by file")
	}
	if mgr.ActiveID() != DefaultProfileID {
		t.Fatalf("expected rollback to default after failed switch, got %s", mgr.ActiveID())
	}
	if cfgMgr.Get().Ping.IntervalMs != 500 || al.DataDir() != mgr.RoomDir(DefaultProfileID) {
		t.Fatalf("rollback did not restore default settings/dataDir")
	}
	curTopo, _ := topo.Snapshot()
	if curTopo.Site.Name != "A机房" {
		t.Fatalf("rollback did not restore default topology: %s", curTopo.Site.Name)
	}

	// Simulate mid-switch failure AFTER BeginSwitch & store.SwitchDir (corrupt maintenance.json in room B),
	// with live monitor workers running to exercise probe drain + full subsystem rollback.
	_ = os.Remove(filepath.Join(mgr.RoomDir(bInfo.ID), "history"))
	_ = os.MkdirAll(filepath.Join(mgr.RoomDir(bInfo.ID), "history"), 0o755)
	if err := os.WriteFile(filepath.Join(mgr.RoomDir(bInfo.ID), "maintenance.json"), []byte("{not-valid-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	mon.SetPaused(false)
	mon.Sync(monitor.BuildTargets(curTopo, cfgMgr.Get().Ping))
	if err := mgr.Activate(bInfo.ID); err == nil {
		t.Fatal("expected mid-switch activation error when maintenance.json is corrupt")
	}
	if mgr.ActiveID() != DefaultProfileID {
		t.Fatalf("expected rollback to default after mid-switch failure, got %s", mgr.ActiveID())
	}
	if st.Dir() != filepath.Join(mgr.RoomDir(DefaultProfileID), "history") || al.DataDir() != mgr.RoomDir(DefaultProfileID) {
		t.Fatalf("mid-switch rollback left stale dirs: store=%s alert=%s", st.Dir(), al.DataDir())
	}
	if cfgMgr.Get().Ping.IntervalMs != 500 || len(mon.Statuses(false)) == 0 {
		t.Fatalf("mid-switch rollback did not resume monitor/config: interval=%d targets=%d", cfgMgr.Get().Ping.IntervalMs, len(mon.Statuses(false)))
	}
}

func TestConfigVersionsAndRollback(t *testing.T) {
	baseDir := t.TempDir()
	cfgMgr, topo, mon, _, _, _, _, mgr := setupTestEnv(t, baseDir, false)
	defer func() {
		mgr.Shutdown()
		mon.Close()
	}()

	v1, err := mgr.CreateVersion(DefaultProfileID, "manual", "基线版本")
	if err != nil {
		t.Fatal(err)
	}

	// Mutate both topology and room settings.
	if err := topo.Update(func(tp *topology.Topology) error {
		tp.Site.Name = "A机房-已变更"
		tp.Nodes = append(tp.Nodes, &topology.Node{ID: "n2", Name: "新增节点", IP: "10.1.0.20"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.UpdateSettings(DefaultProfileID, []byte(`{"ping":{"interval_ms":3000},"locate":{"community":"changed-comm"}}`)); err != nil {
		t.Fatal(err)
	}

	restored, backup, err := mgr.RollbackVersion(DefaultProfileID, v1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.ID != v1.ID || backup.Reason != "pre_rollback" {
		t.Fatalf("unexpected rollback result: restored=%+v backup=%+v", restored, backup)
	}

	curTopo, _ := topo.Snapshot()
	if curTopo.Site.Name != "A机房" || len(curTopo.Nodes) != 1 {
		t.Fatalf("topology did not roll back: %+v", curTopo)
	}
	curCfg := cfgMgr.Get()
	if curCfg.Ping.IntervalMs != 500 || curCfg.Locate.Community != "snmp-room-a" {
		t.Fatalf("room settings did not roll back: %+v", curCfg)
	}

	// Verify version retention cap (30 max per room).
	for i := 0; i < 35; i++ {
		if _, err := mgr.CreateVersion(DefaultProfileID, "manual", "cap"); err != nil {
			t.Fatal(err)
		}
	}
	list, err := mgr.ListVersions(DefaultProfileID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != maxVersionsPerRoom {
		t.Fatalf("expected %d versions after pruning, got %d", maxVersionsPerRoom, len(list))
	}
}

func TestCreateVersionConcurrency(t *testing.T) {
	baseDir := t.TempDir()
	_, _, mon, _, _, _, _, mgr := setupTestEnv(t, baseDir, false)
	defer func() {
		mgr.Shutdown()
		mon.Close()
	}()

	const workers = 16
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	ids := make(chan string, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			meta, err := mgr.CreateVersion(DefaultProfileID, "manual", fmt.Sprintf("concurrent-%d", idx))
			if err != nil {
				errs <- err
				return
			}
			ids <- meta.ID
			if _, err := mgr.ListVersions(DefaultProfileID); err != nil {
				errs <- err
				return
			}
			if _, err := mgr.PreviewRollback(DefaultProfileID, meta.ID); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	close(ids)

	for err := range errs {
		t.Fatalf("concurrent version op error: %v", err)
	}

	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate version ID generated: %s", id)
		}
		seen[id] = true
	}
	if len(seen) != workers {
		t.Fatalf("expected %d unique version IDs, got %d", workers, len(seen))
	}

	list, err := mgr.ListVersions(DefaultProfileID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != workers {
		t.Fatalf("expected %d versions in index, got %d", workers, len(list))
	}
	for _, meta := range list {
		snap, err := mgr.GetVersion(DefaultProfileID, meta.ID)
		if err != nil {
			t.Fatalf("snapshot %s unreadable: %v", meta.ID, err)
		}
		if snap.Topology == nil {
			t.Fatalf("snapshot %s missing topology", meta.ID)
		}
	}
}
