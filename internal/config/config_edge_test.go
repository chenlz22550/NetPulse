package config

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/quick"
)

func TestConfigValidationAndHelpers(t *testing.T) {
	c := Default()
	c.Normalize()
	if !c.AI.SelfCheckOn() {
		t.Fatal("SelfCheckOn default should be true")
	}
	f := false
	c.AI.SelfCheck = &f
	if c.AI.SelfCheckOn() {
		t.Fatal("SelfCheckOn should be false when explicitly disabled")
	}
	if (Auth{}).HasPassword() || !(Auth{Password: "x"}).HasPassword() || !(Auth{PasswordHash: "h"}).HasPassword() {
		t.Fatal("Auth.HasPassword mismatch")
	}

	// Normalize repairs out-of-range defaults
	bad := Config{
		AI:            AIConfig{TimeoutSec: 5},
		RetentionDays: 0,
		Ping:          PingDefaults{IntervalMs: 10, TimeoutMs: 10, PacketSize: 70000, LatencyWarnMs: -1, LossWarnPct: 120},
		Alert:         AlertConfig{DownAfterFailures: -1, UpAfterSuccesses: 0, LossDurationMin: 0, LatencyDurationMin: 0, RepeatIntervalMin: -3, MergeWindowSec: 0},
		Discovery:     Discovery{TimeoutMs: 10, Concurrency: 999, Jobs: []ScanJob{{}}},
		Locate:        Locate{IntervalMin: 0, TimeoutMs: 10, Switches: map[string]LocateSwitch{"empty": {}, "ok": {Host: " 1.1.1.1 "}}},
	}
	bad.Normalize()
	if bad.Listen == "" || bad.TopologyFile == "" || bad.AI.TimeoutSec != 120 || bad.RetentionDays != 30 {
		t.Fatalf("Normalize core defaults failed: %+v", bad)
	}
	if bad.Ping.IntervalMs != 1000 || bad.Ping.PacketSize != 32 || bad.Ping.LossWarnPct != 20 {
		t.Fatalf("Normalize ping defaults failed: %+v", bad.Ping)
	}
	if bad.Alert.RepeatIntervalMin != 0 || bad.Alert.MergeWindowSec != 10 || bad.Alert.SiteTitle == "" {
		t.Fatalf("Normalize alert defaults failed: %+v", bad.Alert)
	}
	if bad.Discovery.Jobs[0].ID != "job-1" || bad.Discovery.Concurrency != 64 {
		t.Fatalf("Normalize discovery failed: %+v", bad.Discovery)
	}
	if _, exists := bad.Locate.Switches["empty"]; exists || bad.Locate.Switches["ok"].Host != "1.1.1.1" {
		t.Fatalf("Normalize locate switches failed: %+v", bad.Locate.Switches)
	}

	// Validate error branches
	checkInvalid := func(name string, mut func(*Config), want string) {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			cfg.Normalize()
			mut(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("got err=%v, want substring %q", err, want)
			}
		})
	}
	checkInvalid("bad webhook", func(c *Config) { c.Alert.WebhookURL = "ftp://x" }, "webhook_url")
	checkInvalid("enabled no webhook", func(c *Config) { c.Alert.Enabled = true }, "webhook")
	checkInvalid("locate interval", func(c *Config) { c.Locate.IntervalMin = 1 }, "端口定位间隔")
	checkInvalid("bad alert proxy", func(c *Config) { c.Alert.ProxyURL = "http://" }, "proxy_url")
	checkInvalid("bad ai proxy", func(c *Config) { c.AI.ProxyURL = "http://" }, "ai.proxy_url")
	checkInvalid("bad ai base url", func(c *Config) { c.AI.BaseURL = "ftp://x" }, "ai.base_url")
	checkInvalid("bad ip tracking", func(c *Config) { c.Discovery.IPTracking = "bad" }, "ip_tracking")
	checkInvalid("bad job id", func(c *Config) { c.Discovery.Jobs = []ScanJob{{ID: "bad id", IntervalMin: 10}} }, "扫描任务 id")
	checkInvalid("bad job interval", func(c *Config) { c.Discovery.Jobs = []ScanJob{{ID: "j1", IntervalMin: 2}} }, "不能小于 5 分钟")
	checkInvalid("bad exclude", func(c *Config) { c.Discovery.Exclude = []string{"not-an-ip"} }, "排除项")
	checkInvalid("bad oui", func(c *Config) { c.Discovery.OUI = map[string]string{"zz": "x"} }, "oui")

	// FindJob, InSubnets, Excluded
	d := Discovery{
		Jobs:    []ScanJob{{ID: "j1", Subnets: []string{"192.168.10.0/24"}}, {ID: "j2", Subnets: []string{"10.0.0.0/25"}}},
		Exclude: []string{"192.168.10.5", "10.0.0.0/28"},
	}
	if j, ok := d.FindJob("j2"); !ok || j.ID != "j2" {
		t.Fatalf("FindJob j2 = %+v %v", j, ok)
	}
	if _, ok := d.FindJob("missing"); ok {
		t.Fatal("FindJob missing should be false")
	}
	if !InSubnets("192.168.10.99", []string{"192.168.10.0/24"}) || InSubnets("192.168.11.1", []string{"192.168.10.0/24"}) || InSubnets("bad", []string{"192.168.10.0/24"}) {
		t.Fatal("InSubnets mismatch")
	}
	if !Excluded("192.168.10.5", d.Exclude) || !Excluded("10.0.0.3", d.Exclude) || Excluded("10.0.0.50", d.Exclude) || Excluded("bad", d.Exclude) {
		t.Fatal("Excluded mismatch")
	}

	// Load invalid yaml
	badPath := filepath.Join(t.TempDir(), "bad.yaml")
	_ = os.WriteFile(badPath, []byte("listen: ["), 0o644)
	if _, err := Load(badPath); err == nil {
		t.Fatal("Load should fail on bad yaml")
	}
}

// 性质测试：ExpandSubnets 返回的每个 IP 均属于给定网段、不在排除列表中、互不重复，且非网络/广播地址。
func TestExpandSubnetsFormalInvariant(t *testing.T) {
	if err := quick.Check(func(oct3, exOct uint8, maskBits uint8) bool {
		bits := 24 + int(maskBits%7) // /24 .. /30
		cidr := netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 20, oct3, 0}), bits).Masked()
		exIP := netip.AddrFrom4([4]byte{10, 20, oct3, exOct}).String()
		ips, err := ExpandSubnets([]string{cidr.String(), cidr.String()}, []string{exIP})
		if err != nil {
			return false
		}
		seen := map[string]bool{}
		for _, ip := range ips {
			if seen[ip] || ip == exIP {
				return false
			}
			seen[ip] = true
			a, err := netip.ParseAddr(ip)
			if err != nil || !cidr.Contains(a) {
				return false
			}
		}
		// 期望数量校验
		hostTotal := 1 << (32 - bits)
		usable := hostTotal - 2
		exAddr, _ := netip.ParseAddr(exIP)
		bcast := cidr.Addr()
		for i := 0; i < hostTotal-1; i++ {
			bcast = bcast.Next()
		}
		if cidr.Contains(exAddr) && exAddr != cidr.Addr() && exAddr != bcast {
			usable--
		}
		return len(ips) == usable
	}, &quick.Config{MaxCount: 100}); err != nil {
		t.Fatal(err)
	}
}
