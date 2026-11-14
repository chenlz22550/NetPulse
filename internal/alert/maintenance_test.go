package alert

import (
	"netping/internal/config"
	"netping/internal/monitor"
	"netping/internal/topology"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMaintenancePersistenceExpiryIsolation(t *testing.T) {
	e, sent := newTestEngine(t)
	dir := t.TempDir()
	if err := e.SwitchDataDir(dir); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	id := "n-192-168-50-1"
	window, err := e.AddMaintenance(MaintenanceWindow{Scope: "devices", TargetIDs: []string{id}, Start: now.Add(-time.Minute), End: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	e.Evaluate(now, []monitor.Status{st(id, "node", 5, 0)})
	e.flushIfDue(now.Add(11 * time.Second))
	if len(*sent) != 0 || len(e.pending) != 0 || len(e.History(0)) != 1 || e.History(0)[0].MaintenanceID != window.ID {
		t.Fatal("maintenance did not record and drain")
	}
	reloaded := NewEngine(func() config.Config { return config.Default() }, nil, filepath.Join(dir, "alerts.jsonl"))
	if reloaded.MaintenanceWindows(now)[0].Status != "active" || reloaded.MaintenanceWindows(now.Add(time.Minute))[0].Status != "expired" {
		t.Fatal("restart/expiry")
	}
	e.Evaluate(now.Add(2*time.Minute), []monitor.Status{st(id, "node", 0, 3)})
	e.flushIfDue(now.Add(3 * time.Minute))
	if len(*sent) != 1 {
		t.Fatal("expiry must restore delivery")
	}
	if err := e.CancelMaintenance(window.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.SwitchDataDir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if len(e.History(0)) != 0 || len(e.MaintenanceWindows(now)) != 0 || len(e.ActiveDown()) != 0 {
		t.Fatal("profile data leaked")
	}
	if err := e.SwitchDataDir(dir); err != nil {
		t.Fatal(err)
	}
	if e.MaintenanceWindows(now)[0].Status != "cancelled" {
		t.Fatal("cancel not saved")
	}
}
func TestCascadeCauseAndCycle(t *testing.T) {
	e, sent := newTestEngine(t)
	topo := &topology.Topology{Switches: []*topology.Switch{{ID: "root", Name: "root", Monitor: topology.MonitorSpec{Alert: true}}, {ID: "child", Name: "child", Monitor: topology.MonitorSpec{Alert: true}}}, Links: []*topology.Link{{From: topology.Endpoint{Switch: "root"}, To: topology.Endpoint{Switch: "child"}}}, Nodes: []*topology.Node{{ID: "leaf", Attach: topology.Endpoint{Switch: "child"}, Monitor: topology.MonitorSpec{Alert: true}}}}
	e.SetTopology(topo)
	now := time.Now()
	e.Evaluate(now, []monitor.Status{st("leaf", "node", 5, 0), st("child", "switch", 5, 0), st("root", "switch", 5, 0)})
	e.flushIfDue(now.Add(time.Minute))
	if len(*sent) != 1 || strings.Count((*sent)[0], "● 离线") != 1 {
		t.Fatalf("cascade not merged: %v", *sent)
	}
	for _, ev := range e.History(0) {
		if ev.TargetID != "root" && ev.RootCauseID != "root" {
			t.Fatalf("missing original cause %+v", ev)
		}
	}
	topo.Links = append(topo.Links, &topology.Link{From: topology.Endpoint{Switch: "child"}, To: topology.Endpoint{Switch: "root"}})
	e.SetTopology(topo)
	e.mu.Lock()
	root := e.offlineAncestorLocked(Event{Type: EvDown, TargetID: "leaf"})
	e.mu.Unlock()
	if root != "" {
		t.Fatal("cycle must not invent a root")
	}
}

func TestMaintenanceExitRenotifiesCurrentFault(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		e, sent := newTestEngine(t)
		now := time.Now()
		id := "n-192-168-50-1"
		w, err := e.AddMaintenance(MaintenanceWindow{Scope: "site", Start: now.Add(-time.Minute), End: now.Add(time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		e.Evaluate(now, []monitor.Status{st(id, "node", 5, 0)})
		next := now.Add(2 * time.Minute)
		if cancel {
			if err := e.CancelMaintenance(w.ID); err != nil {
				t.Fatal(err)
			}
			next = now.Add(time.Second)
		}
		e.Evaluate(next, []monitor.Status{st(id, "node", 9, 0)})
		e.flushIfDue(next.Add(11 * time.Second))
		if len(*sent) != 1 || len(e.History(0)) != 2 || e.History(0)[0].Type != EvDown {
			t.Fatalf("exit did not reevaluate current fault: %+v", e.History(0))
		}
	}
}

func TestCorruptMaintenanceFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "maintenance.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	e := NewEngine(func() config.Config { return cfg }, nil, filepath.Join(dir, "alerts.jsonl"))
	topo := topology.Seed()
	for _, n := range topo.Nodes {
		n.Monitor.Alert = true
	}
	e.SetTopology(topo)
	e.Evaluate(time.Now(), []monitor.Status{st("n-192-168-50-1", "node", 5, 0)})
	if len(e.pending) != 0 || e.History(0)[0].Suppressed != "maintenance" || e.MaintenanceWindows(time.Now())[0].Status != "error" {
		t.Fatal("corrupt maintenance must not enable notifications")
	}
}

func TestMaintenanceRecoveryBoundariesNoDuplicates(t *testing.T) {
	e, sent := newTestEngine(t)
	now := time.Now()
	id := "n-192-168-50-1"
	e.Evaluate(now, []monitor.Status{st(id, "node", 5, 0)})
	// An active window drains the queued outage; recovery inside it is historical only.
	w, err := e.AddMaintenance(MaintenanceWindow{Scope: "site", Start: now.Add(-time.Second), End: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if len(e.pending) != 0 {
		t.Fatal("queued outage retained")
	}
	e.Evaluate(now.Add(time.Second), []monitor.Status{st(id, "node", 0, 3)})
	e.flushIfDue(now.Add(20 * time.Second))
	if len(*sent) != 0 || len(e.History(0)) != 2 || e.History(0)[0].Type != EvRecover || e.History(0)[0].MaintenanceID != w.ID {
		t.Fatal("recovery not silently recorded")
	}
	e.Evaluate(now.Add(2*time.Minute), []monitor.Status{st(id, "node", 0, 5)})
	e.Evaluate(now.Add(3*time.Minute), []monitor.Status{st(id, "node", 0, 6)})
	e.flushIfDue(now.Add(4 * time.Minute))
	if len(*sent) != 0 || len(e.History(0)) != 2 {
		t.Fatal("maintenance recovery replayed")
	}
}

func TestBriefMaintenanceWithoutEvaluationResumes(t *testing.T) {
	e, sent := newTestEngine(t)
	now := time.Now()
	id := "n-192-168-50-1"
	e.Evaluate(now, []monitor.Status{st(id, "node", 5, 0)})
	e.flushIfDue(now.Add(11 * time.Second))
	w, err := e.AddMaintenance(MaintenanceWindow{Scope: "site", Start: now.Add(-time.Second), End: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.CancelMaintenance(w.ID); err != nil {
		t.Fatal(err)
	}
	// No tick occurred inside this window; adding it must still mark existing state.
	e.Evaluate(now.Add(12*time.Second), []monitor.Status{st(id, "node", 6, 0)})
	e.flushIfDue(now.Add(23 * time.Second))
	e.Evaluate(now.Add(24*time.Second), []monitor.Status{st(id, "node", 7, 0)})
	e.flushIfDue(now.Add(35 * time.Second))
	if len(*sent) != 2 {
		t.Fatalf("want original + current fault once, got %d", len(*sent))
	}
}
