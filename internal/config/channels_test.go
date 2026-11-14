package config

import "testing"

func TestAlertChannelsValidation(t *testing.T) {
	c := Default()
	c.Alert.Enabled = true
	if c.Validate() == nil {
		t.Fatal("accepted empty channels")
	}
	c.Alert.SlackURL = "https://example.com/hook"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Alert.FeishuURL = "file:///tmp/test"
	if c.Validate() == nil {
		t.Fatal("accepted invalid channel URL")
	}
	c.Alert.FeishuURL = ""
	c.Alert.SlackURL = ""
	c.Alert.WebhookURL = "https://example.com/legacy"
	if err := c.Validate(); err != nil {
		t.Fatal("legacy config rejected:", err)
	}
}
