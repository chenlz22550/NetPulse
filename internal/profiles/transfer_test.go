package profiles

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"netping/internal/config"
	"netping/internal/topology"
)

// roomFiles 读取一个机房目录中会被导入影响的文件，用于“未修改”断言。
func roomFiles(t *testing.T, m *Manager, id string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, name := range []string{"topology.yaml", "settings.yaml", "versions/index.json"} {
		b, err := os.ReadFile(filepath.Join(m.RoomDir(id), name))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		out[name] = b
	}
	return out
}

func globalFiles(t *testing.T, baseDir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, name := range []string{"config.yaml", "data/profiles.json"} {
		b, err := os.ReadFile(filepath.Join(baseDir, name))
		if err != nil {
			t.Fatal(err)
		}
		out[name] = b
	}
	return out
}

func sameFiles(t *testing.T, what string, a, b map[string][]byte) {
	t.Helper()
	for k := range a {
		if !bytes.Equal(a[k], b[k]) {
			t.Fatalf("%s: %s changed", what, k)
		}
	}
}

func mustExport(t *testing.T, m *Manager, id string, include bool) []byte {
	t.Helper()
	exp, err := m.Export(id, include, "test")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(exp)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestExportImportRoundTripWithSecrets(t *testing.T) {
	baseDir := t.TempDir()
	cfgMgr, topo, _, _, _, _, _, mgr := setupTestEnv(t, baseDir, false)
	defer mgr.Shutdown()

	raw := mustExport(t, mgr, DefaultProfileID, true)
	if !strings.Contains(string(raw), "room-a-secret") || !strings.Contains(string(raw), "snmp-room-a") {
		t.Fatalf("include-secrets export must contain plaintext secrets")
	}
	for _, global := range []string{"secret-password", "mcp-secret-token", "sk-global-ai-key", "18090"} {
		if strings.Contains(string(raw), global) {
			t.Fatalf("export leaked global config %q", global)
		}
	}

	b, err := mgr.Create("B机房", false, "")
	if err != nil {
		t.Fatal(err)
	}
	defBefore := roomFiles(t, mgr, DefaultProfileID)
	cfgBefore, _ := os.ReadFile(filepath.Join(baseDir, "config.yaml"))
	activeTopoBefore, _ := topo.Snapshot()

	p, err := mgr.Import(b.ID, raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.BackupVersion == "" || p.TargetID != b.ID || p.TargetActive {
		t.Fatalf("unexpected preview: %+v", p)
	}
	vers, _ := mgr.ListVersions(b.ID)
	if len(vers) == 0 || vers[0].Reason != "pre_import" {
		t.Fatalf("expected pre_import backup version, got %+v", vers)
	}

	// 往返一致：B 的导出（含凭据）与 A 的导出拓扑/设置完全相同。
	exA, _ := mgr.Export(DefaultProfileID, true, "")
	exB, _ := mgr.Export(b.ID, true, "")
	ja, _ := json.Marshal(exA.Topology)
	jb, _ := json.Marshal(exB.Topology)
	if !bytes.Equal(ja, jb) {
		t.Fatalf("topology round trip mismatch:\n%s\n%s", ja, jb)
	}
	if !reflect.DeepEqual(exA.Settings, exB.Settings) {
		t.Fatalf("settings round trip mismatch:\n%+v\n%+v", exA.Settings, exB.Settings)
	}

	// 跨机房隔离：导入非激活机房不影响激活机房的文件、运行时拓扑与全局配置。
	sameFiles(t, "default room", defBefore, roomFiles(t, mgr, DefaultProfileID))
	cfgAfter, _ := os.ReadFile(filepath.Join(baseDir, "config.yaml"))
	if !bytes.Equal(cfgBefore, cfgAfter) {
		t.Fatalf("global config.yaml changed by importing into inactive room")
	}
	if cur, _ := topo.Snapshot(); cur != activeTopoBefore {
		if a, b := mustJSON(cur), mustJSON(activeTopoBefore); a != b {
			t.Fatalf("active topology changed")
		}
	}
	if c := cfgMgr.Get(); c.AI.APIKey != "sk-global-ai-key" || c.MCP.Token != "mcp-secret-token" || c.Auth.Password != "secret-password" {
		t.Fatalf("global settings changed: %+v", c.AI)
	}
}

func mustJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestImportMaskedSecretsKeepOrClear(t *testing.T) {
	baseDir := t.TempDir()
	_, _, _, _, _, _, _, mgr := setupTestEnv(t, baseDir, false)
	defer mgr.Shutdown()

	raw := mustExport(t, mgr, DefaultProfileID, false)
	if strings.Contains(string(raw), "room-a-secret") || strings.Contains(string(raw), "snmp-room-a") {
		t.Fatalf("masked export leaked secrets: %s", raw)
	}

	// 导入到自身（已有凭据）：保留原值。
	p, err := mgr.Import(DefaultProfileID, raw)
	if err != nil {
		t.Fatal(err)
	}
	rs, _, _, _ := mgr.GetSettings(DefaultProfileID)
	if rs.Alert.WebhookURL != "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=room-a-secret" || rs.Locate.Community != "snmp-room-a" {
		t.Fatalf("secrets not kept: %+v %+v", rs.Alert.WebhookURL, rs.Locate.Community)
	}
	kept := map[string]string{}
	for _, n := range p.Secrets {
		kept[n.Field] = n.Action
	}
	if kept["alert.webhook_url"] != "kept" || kept["locate.community"] != "kept" {
		t.Fatalf("unexpected secret notes: %+v", p.Secrets)
	}

	// 导入为新机房（没有原值）：清空占位并提示重新填写，不能把占位字符串当作真实地址。
	np, err := mgr.ImportAsNew("", raw)
	if err != nil {
		t.Fatal(err)
	}
	if !np.NewProfile || np.TargetActive || mgr.ActiveID() != DefaultProfileID {
		t.Fatalf("import-as-new must not activate: %+v", np)
	}
	nrs, _, _, _ := mgr.GetSettings(np.TargetID)
	if nrs.Alert.WebhookURL != "" || nrs.Locate.Community != "" {
		t.Fatalf("masked placeholders must be cleared, got %q %q", nrs.Alert.WebhookURL, nrs.Locate.Community)
	}
	if len(np.Warnings) == 0 {
		t.Fatalf("expected warnings about cleared secrets")
	}
	if vers, _ := mgr.ListVersions(np.TargetID); len(vers) != 0 {
		t.Fatalf("import-as-new should not create a backup of the empty room: %+v", vers)
	}
	if nrs.Alert.Enabled {
		t.Fatalf("alerting must be switched off when all channels were cleared")
	}
	if np.TargetName == "A机房" || !strings.HasPrefix(np.TargetName, "A机房") {
		t.Fatalf("expected deduplicated name, got %q", np.TargetName)
	}
}

func TestInvalidImportLeavesEverythingUnchanged(t *testing.T) {
	baseDir := t.TempDir()
	cfgMgr, topo, _, _, _, _, _, mgr := setupTestEnv(t, baseDir, false)
	defer mgr.Shutdown()
	good := mustExport(t, mgr, DefaultProfileID, true)
	b, err := mgr.Create("B机房", true, "")
	if err != nil {
		t.Fatal(err)
	}

	var obj map[string]interface{}
	_ = json.Unmarshal(good, &obj)
	mutate := func(fn func(m map[string]interface{})) []byte {
		cp := map[string]interface{}{}
		bb, _ := json.Marshal(obj)
		_ = json.Unmarshal(bb, &cp)
		fn(cp)
		out, _ := json.Marshal(cp)
		return out
	}
	cases := map[string][]byte{
		"empty":         {},
		"not json":      []byte("listen: 0.0.0.0:80\n"),
		"wrong format":  mutate(func(m map[string]interface{}) { m["format"] = "something-else" }),
		"no version":    mutate(func(m map[string]interface{}) { delete(m, "version") }),
		"future":        mutate(func(m map[string]interface{}) { m["version"] = 99 }),
		"global auth":   mutate(func(m map[string]interface{}) { m["auth"] = map[string]string{"password": "x"} }),
		"global ai":     mutate(func(m map[string]interface{}) { m["ai"] = map[string]string{"api_key": "x"} }),
		"no topology":   mutate(func(m map[string]interface{}) { delete(m, "topology") }),
		"no settings":   mutate(func(m map[string]interface{}) { delete(m, "settings") }),
		"bad secrets":   mutate(func(m map[string]interface{}) { m["secrets"] = "plain" }),
		"unknown field": mutate(func(m map[string]interface{}) { m["settings"].(map[string]interface{})["listen"] = ":80" }),
		"bad topology": mutate(func(m map[string]interface{}) {
			tp := m["topology"].(map[string]interface{})
			tp["nodes"] = []interface{}{map[string]interface{}{"id": "x", "name": "x", "ip": "not-an-ip"}}
		}),
		"bad settings": mutate(func(m map[string]interface{}) {
			m["settings"].(map[string]interface{})["locate"].(map[string]interface{})["interval_min"] = 1
		}),
		"trailing":        append(append([]byte{}, good...), []byte(`{"x":1}`)...),
		"settings null":   mutate(func(m map[string]interface{}) { m["settings"] = nil }),
		"settings array":  mutate(func(m map[string]interface{}) { m["settings"] = []interface{}{} }),
		"settings string": mutate(func(m map[string]interface{}) { m["settings"] = "x" }),
		"settings empty":  mutate(func(m map[string]interface{}) { m["settings"] = map[string]interface{}{} }),
		"alert null":      mutate(func(m map[string]interface{}) { m["settings"].(map[string]interface{})["alert"] = nil }),
		"ping missing":    mutate(func(m map[string]interface{}) { delete(m["settings"].(map[string]interface{}), "ping") }),
		"topology null":   mutate(func(m map[string]interface{}) { m["topology"] = nil }),
		"topology array":  mutate(func(m map[string]interface{}) { m["topology"] = []interface{}{} }),
		"source string":   mutate(func(m map[string]interface{}) { m["source"] = "x" }),
		"too large":       append([]byte(`{"format":"netpulse-room-config","version":1,"pad":"`), append(bytes.Repeat([]byte("a"), MaxImportBytes), '"', '}')...),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			before := []map[string][]byte{roomFiles(t, mgr, DefaultProfileID), roomFiles(t, mgr, b.ID), globalFiles(t, baseDir)}
			runTopoBefore, _ := topo.Snapshot()
			runTopo, runCfg := mustJSON(runTopoBefore), mustJSON(cfgMgr.Get())
			bSettings, _, _, _ := mgr.GetSettings(b.ID)
			for _, target := range []string{DefaultProfileID, b.ID} {
				if _, err := mgr.PreviewImport(target, raw); err == nil {
					t.Fatalf("preview should fail")
				}
				_, err := mgr.Import(target, raw)
				var ie *ImportError
				if err == nil || !errors.As(err, &ie) {
					t.Fatalf("want ImportError, got %v", err)
				}
			}
			if _, err := mgr.ImportAsNew("", raw); err == nil {
				t.Fatalf("import-as-new should fail")
			}
			after := []map[string][]byte{roomFiles(t, mgr, DefaultProfileID), roomFiles(t, mgr, b.ID), globalFiles(t, baseDir)}
			for i := range before {
				sameFiles(t, name, before[i], after[i])
			}
			if n := len(mgr.List()); n != 2 {
				t.Fatalf("profile count changed: %d", n)
			}
			// 运行时（激活机房的拓扑与内存配置）也不变。
			runTopoAfter, _ := topo.Snapshot()
			if mustJSON(runTopoAfter) != runTopo || mustJSON(cfgMgr.Get()) != runCfg {
				t.Fatalf("runtime state changed")
			}
			if bAfter, _, _, _ := mgr.GetSettings(b.ID); !reflect.DeepEqual(bAfter, bSettings) {
				t.Fatalf("room B settings changed")
			}
		})
	}
	if _, err := mgr.Import("room-missing", good); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestPreviewImportDoesNotModifyAndDescribesOverwrite(t *testing.T) {
	baseDir := t.TempDir()
	_, topo, _, _, _, _, _, mgr := setupTestEnv(t, baseDir, false)
	defer mgr.Shutdown()
	b, _ := mgr.Create("B机房", false, "")
	raw := mustExport(t, mgr, DefaultProfileID, false)
	before := roomFiles(t, mgr, b.ID)
	p, err := mgr.PreviewImport(b.ID, raw)
	if err != nil {
		t.Fatal(err)
	}
	sameFiles(t, "preview", before, roomFiles(t, mgr, b.ID))
	if p.Mode != "replace" || p.Current.Nodes != 0 || p.Incoming.Nodes != 1 || p.Incoming.SiteName != "A机房" {
		t.Fatalf("unexpected preview: %+v", p)
	}
	if len(p.ChangedSection) == 0 || !strings.Contains(strings.Join(p.ChangedSection, ","), "拓扑") {
		t.Fatalf("expected topology in changed sections: %v", p.ChangedSection)
	}
	if len(p.Unaffected) == 0 {
		t.Fatalf("preview must list unaffected scope")
	}

	// 导入到激活机房：运行时拓扑立即切换，且有提示。
	other := &RoomExport{Format: ExportFormat, Version: ExportVersion, Secrets: SecretsMasked,
		Source:   ExportSource{ProfileName: "C机房"},
		Topology: &topology.Topology{Version: 1, Site: topology.Site{Name: "C机房"}},
		Settings: config.DefaultRoomSettings()}
	rawC, _ := json.Marshal(other)
	pa, err := mgr.PreviewImport(DefaultProfileID, rawC)
	if err != nil {
		t.Fatal(err)
	}
	if !pa.TargetActive || len(pa.Warnings) == 0 {
		t.Fatalf("expected active-room warning: %+v", pa)
	}
	epochBefore := mgr.Epoch()
	if _, err := mgr.Import(DefaultProfileID, rawC); err != nil {
		t.Fatal(err)
	}
	if mgr.Epoch() <= epochBefore {
		t.Fatalf("importing into the active room must bump the epoch so in-flight AI work is cancelled")
	}
	if cur, _ := topo.Snapshot(); cur.Site.Name != "C机房" || len(cur.Nodes) != 0 {
		t.Fatalf("active topology not replaced: %+v", cur.Site)
	}
	// B 机房不受影响。
	if bt, err := mgr.loadRoomTopology(b.ID); err != nil || bt.Site.Name != "B机房" {
		t.Fatalf("room B changed: %v %+v", err, bt)
	}
}

func TestResolveImportedSecretsFlagsAndProxy(t *testing.T) {
	existing := config.DefaultRoomSettings()
	existing.Alert.WebhookURL = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=old"
	existing.Alert.ProxyURL = "http://example-user:example-old-pass@proxy-a.example:3128"
	in := config.DefaultRoomSettings()
	in.Alert.WebhookURL = "https://example.com/hook/****real"
	in.Alert.ProxyURL = "http://***@proxy-a.example:3128"

	// 明文导出：即使值看起来像占位也原样导入。
	out, notes := config.ResolveImportedSecrets(existing, in, false)
	if out.Alert.WebhookURL != in.Alert.WebhookURL {
		t.Fatalf("included export must import values verbatim, got %q", out.Alert.WebhookURL)
	}
	_ = notes
	// 脱敏导出：同主机代理保留原账号密码。
	out, _ = config.ResolveImportedSecrets(existing, in, true)
	if out.Alert.ProxyURL != existing.Alert.ProxyURL || out.Alert.WebhookURL != existing.Alert.WebhookURL {
		t.Fatalf("masked import should keep: %q %q", out.Alert.ProxyURL, out.Alert.WebhookURL)
	}
	// 不同主机：不能把 A 的账号密码套到 B 上，只保留地址并提示。
	in.Alert.ProxyURL = "http://***@proxy-b.example:3128"
	out, notes = config.ResolveImportedSecrets(existing, in, true)
	if out.Alert.ProxyURL != "http://proxy-b.example:3128" {
		t.Fatalf("different proxy host: %q", out.Alert.ProxyURL)
	}
	cleared := false
	for _, n := range notes {
		if n.Field == "alert.proxy_url" && n.Action == "cleared" {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("expected cleared note for proxy: %+v", notes)
	}
	// 导出时代理账号被隐藏。
	if m := config.MaskRoomSettingsForExport(existing); strings.Contains(m.Alert.ProxyURL, "example-old-pass") {
		t.Fatalf("export leaked proxy password: %q", m.Alert.ProxyURL)
	}
}

func TestParseImportRejectsNullSettingsExplicitly(t *testing.T) {
	for _, raw := range []string{
		`{"format":"netpulse-room-config","version":1,"topology":{"version":1,"site":{"name":"x"}},"settings":null}`,
		`{"format":"netpulse-room-config","version":1,"topology":{"version":1,"site":{"name":"x"}},"settings":[]}`,
	} {
		_, err := ParseImport([]byte(raw))
		var ie *ImportError
		if !errors.As(err, &ie) || !strings.Contains(ie.Msg, "settings") {
			t.Fatalf("want settings rejection, got %v", err)
		}
	}
}
