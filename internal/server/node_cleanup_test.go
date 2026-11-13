package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"netping/internal/topology"
)

func nodeRequest(t *testing.T, ts *httptest.Server, method, path, body string) (int, map[string]interface{}) {
	t.Helper()
	r, _ := http.NewRequest(method, ts.URL+"/api/nodes/"+path, strings.NewReader(body))
	r.SetBasicAuth("", testPassword)
	r.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}

func TestNodeCleanupProtectsAndRechecks(t *testing.T) {
	s, ts := newTestServer(t)
	off := false
	if err := s.Topo.Update(func(topo *topology.Topology) error {
		topo.Nodes = []*topology.Node{
			{ID: "noip"},
			{ID: "disabled", IP: "10.222.0.1", Monitor: topology.MonitorSpec{Enabled: &off}},
			{ID: "locked-noip", Locked: true},
			{ID: "locked-disabled", IP: "10.222.0.2", Locked: true, Monitor: topology.MonitorSpec{Enabled: &off}},
			{ID: "active", IP: "10.222.0.3"}, // 无采样/暂停也不能清理
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	original, rev := s.Topo.Snapshot()
	code, preview := nodeRequest(t, ts, "GET", "cleanup", "")
	if code != 200 || len(preview["items"].([]interface{})) != 2 || preview["protected"].(float64) != 2 || s.Topo.Rev() != rev {
		t.Fatalf("preview changed state or incorrect candidates: %d %v", code, preview)
	}
	// 用户预览后，另一个页面上锁一台、恢复一台、新增另一台无效节点。
	if code, out := nodeRequest(t, ts, "PUT", "noip/lock", `{"locked":true}`); code != 200 {
		t.Fatalf("lock: %d %v", code, out)
	}
	if err := s.Topo.Update(func(topo *topology.Topology) error {
		topo.FindNode("disabled").Monitor.Enabled = nil
		topo.Nodes = append(topo.Nodes, &topology.Node{ID: "new-invalid"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	code, out := nodeRequest(t, ts, "POST", "cleanup", `{"ids":["noip","disabled","locked-noip","locked-disabled","active","missing","`+original.Switches[0].ID+`"]}`)
	if code != 200 || out["changed"].(float64) != 0 || out["skipped"].(float64) != 7 {
		t.Fatalf("recheck: %d %v", code, out)
	}
	after, _ := s.Topo.Snapshot()
	if len(after.Nodes) != 6 || len(after.Switches) != len(original.Switches) || len(after.Links) != len(original.Links) {
		t.Fatal("protected, valid, new or infrastructure records changed")
	}
	// 解锁后清理，仅删除提交的候选（重复 ID 不重复计数）。
	nodeRequest(t, ts, "PUT", "noip/lock", `{"locked":false}`)
	code, out = nodeRequest(t, ts, "POST", "cleanup", `{"ids":["noip","noip","new-invalid"]}`)
	if code != 200 || out["changed"].(float64) != 2 || out["skipped"].(float64) != 0 {
		t.Fatalf("cleanup: %d %v", code, out)
	}
	reopened, err := topology.Open(s.Topo.Path())
	if err != nil {
		t.Fatal(err)
	}
	persisted, _ := reopened.Snapshot()
	if len(persisted.Nodes) != 4 || persisted.FindNode("noip") != nil || !persisted.FindNode("locked-noip").Locked {
		t.Fatal("cleanup or locks not persisted")
	}
}

func TestNodeLockSurvivesEditing(t *testing.T) {
	s, ts := newTestServer(t)
	code, out := nodeRequest(t, ts, "POST", "", `{"id":"lock-test","locked":true}`)
	if code != 200 {
		t.Fatalf("create: %d %v", code, out)
	}
	code, out = nodeRequest(t, ts, "PUT", "lock-test", `{"name":"renamed"}`)
	topo, _ := s.Topo.Snapshot()
	if code != 200 || !topo.FindNode("lock-test").Locked {
		t.Fatalf("old client erased lock: %d %v", code, out)
	}
	code, out = nodeRequest(t, ts, "PUT", "lock-test", `{"name":"renamed","locked":false}`)
	topo, _ = s.Topo.Snapshot()
	if code != 200 || topo.FindNode("lock-test").Locked {
		t.Fatalf("explicit unlock failed: %d %v", code, out)
	}
	for _, body := range []string{`{}`, `{"locked":null}`, `{"locked":"true"}`} {
		if code, out := nodeRequest(t, ts, "PUT", "lock-test/lock", body); code != 400 {
			t.Fatalf("invalid lock: %d %v", code, out)
		}
	}
	if code, out := nodeRequest(t, ts, "PUT", "missing/lock", `{"locked":true}`); code != 404 {
		t.Fatalf("missing node: %d %v", code, out)
	}
	if code, out := nodeRequest(t, ts, "POST", "cleanup", `{}`); code != 400 {
		t.Fatalf("empty cleanup: %d %v", code, out)
	}
}

func TestNodeCleanupRequiresLogin(t *testing.T) {
	_, ts := newTestServer(t)
	for _, action := range []struct{ method, path, body string }{
		{"GET", "cleanup", ""}, {"POST", "cleanup", `{"ids":["any"]}`}, {"PUT", "any/lock", `{"locked":true}`},
	} {
		r, _ := http.NewRequest(action.method, ts.URL+"/api/nodes/"+action.path, strings.NewReader(action.body))
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("unauthenticated %s %s: %d", action.method, action.path, resp.StatusCode)
		}
	}
}
