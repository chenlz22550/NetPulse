package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"netping/internal/config"
	"netping/internal/plugin"
	"netping/internal/plugin/plugintest"
)

func TestMain(m *testing.M) {
	plugintest.RunIfHelper()
	os.Exit(m.Run())
}

// notifySender：内置渠道与通知插件并行发送，互不影响；两边都没有时报“未配置通知渠道”。
func TestNotifySender(t *testing.T) {
	base := t.TempDir()
	dir, data := filepath.Join(base, "plugins"), filepath.Join(base, "data")
	plugintest.Install(t, dir, "sms", "name: 短信\nhooks: [notify]\nsettings:\n  - {key: mode, default: echo}\n")
	plugins := plugin.New(dir, data)
	defer plugins.Close()
	send := notifySender(plugins, "discovery")
	none := config.AlertConfig{}

	// 没有内置渠道，插件未启用。
	if err := send(none, "### 标题\n内容"); err == nil || !strings.Contains(err.Error(), "未配置通知渠道") {
		t.Fatalf("no channels: %v", err)
	}
	if err := notifySender(nil, "alert")(none, "x"); err == nil {
		t.Fatal("nil manager without channels succeeded")
	}
	// 只有插件。
	if err := plugins.SetEnabled("sms", true); err != nil {
		t.Fatal(err)
	}
	if err := send(none, "### 标题\n内容"); err != nil {
		t.Fatalf("plugin only: %v", err)
	}
	calls := plugintest.Calls(t, data, "sms")
	if len(calls) != 1 || calls[0]["payload"].(map[string]interface{})["source"] != "discovery" || calls[0]["payload"].(map[string]interface{})["title"] != "标题" {
		t.Fatalf("plugin calls: %v", calls)
	}
	// 内置渠道失败（地址不可达）不妨碍插件收到消息，错误里只有内置渠道。
	bad := config.AlertConfig{GenericWebhookURL: "http://127.0.0.1:1/hook"}
	err := send(bad, "### 标题\n内容")
	if err == nil || !strings.Contains(err.Error(), "Webhook") || strings.Contains(err.Error(), "短信") {
		t.Fatalf("builtin failure: %v", err)
	}
	if len(plugintest.Calls(t, data, "sms")) != 2 {
		t.Fatal("plugin skipped when builtin channel failed")
	}
	// 插件失败：错误里带插件名。
	if err := plugins.UpdateSettings("sms", map[string]interface{}{"mode": "fail"}); err != nil {
		t.Fatal(err)
	}
	if err := send(none, "x"); err == nil || !strings.Contains(err.Error(), "插件 短信") {
		t.Fatalf("plugin failure: %v", err)
	}
}
