package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func at(day, hm string) time.Time {
	// 2026-11-09 是周一
	t, _ := time.ParseInLocation("2006-01-02 15:04", day+" "+hm, time.Local)
	return t
}

func TestScheduleActive(t *testing.T) {
	s := Schedule{Enabled: true, Windows: []Window{
		{Days: []int{1, 2, 3, 4, 5}, Start: "08:00", End: "18:00"},
		{Days: []int{6}, Start: "22:00", End: "06:00"},
	}}
	cases := []struct {
		t    time.Time
		want bool
	}{
		{at("2026-11-09", "07:59"), false}, // 周一
		{at("2026-11-09", "08:00"), true},
		{at("2026-11-09", "17:59"), true},
		{at("2026-11-09", "18:00"), false},
		{at("2026-11-14", "12:00"), false}, // 周六白天
		{at("2026-11-14", "23:00"), true},  // 周六晚
		{at("2026-11-15", "05:59"), true},  // 跨午夜到周日凌晨
		{at("2026-11-15", "06:00"), false},
		{at("2026-11-15", "23:00"), false}, // 周日晚不在时段
	}
	for _, c := range cases {
		if got := s.Active(c.t); got != c.want {
			t.Errorf("%s: got %v want %v", c.t.Format("Mon 15:04"), got, c.want)
		}
	}
	if !(Schedule{}).Active(time.Now()) {
		t.Fatal("disabled schedule must always be active")
	}
	if d := s.Describe(); d != "周一、二、三、四、五 08:00-18:00；周六 22:00-06:00" {
		t.Fatalf("describe = %q", d)
	}
}

func TestScheduleValidate(t *testing.T) {
	bad := []Schedule{
		{Enabled: true},
		{Enabled: true, Windows: []Window{{Start: "8:00", End: "25:00"}}},
		{Enabled: true, Windows: []Window{{Start: "08:00", End: "08:00"}}},
		{Enabled: true, Windows: []Window{{Days: []int{0}, Start: "08:00", End: "09:00"}}},
	}
	for i, s := range bad {
		if s.Validate() == nil {
			t.Errorf("case %d should fail", i)
		}
	}
	ok := Schedule{Enabled: true, Windows: []Window{{Start: "00:00", End: "24:00"}}}
	if err := ok.Validate(); err != nil || !ok.Active(at("2026-11-09", "23:59")) {
		t.Fatalf("00:00-24:00 should be valid and active: %v", err)
	}
}

func TestExpandSubnets(t *testing.T) {
	ips, err := ExpandSubnets([]string{"192.168.1.0/24"}, []string{"192.168.1.1", "192.168.1.128/25"})
	if err != nil || len(ips) != 126 || ips[0] != "192.168.1.2" || ips[len(ips)-1] != "192.168.1.127" {
		t.Fatalf("ips=%d first=%s err=%v", len(ips), ips[0], err)
	}
	if _, err := ExpandSubnets([]string{"10.0.0.0/16"}, nil); err == nil {
		t.Fatal("/16 should be rejected")
	}
	if NormalizeOUI("60-d7-e3") != "60:D7:E3" || NormalizeOUI("zz") != "" {
		t.Fatal("NormalizeOUI")
	}
}

func TestDiscoveryJobsMigration(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/config.yaml"
	if err := os.WriteFile(p, []byte("discovery:\n  enabled: true\n  subnets: [10.1.0.0/24, 10.2.0.0/24]\n  interval_min: 12\n  auto_add: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	js := m.Get().Discovery.Jobs
	if len(js) != 1 || len(js[0].Subnets) != 2 || js[0].IntervalMin != 12 || js[0].AutoAdd || js[0].ID != "default" {
		t.Fatalf("migrated: %+v", js)
	}
	// 保存后不再写出旧字段，重新读取保持一致
	if err := m.Update(m.Get()); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if strings.Contains(string(data), "\n  subnets:") || !strings.Contains(string(data), "jobs:") {
		t.Fatalf("saved yaml:\n%s", data)
	}
	m2, _ := Load(p)
	if js2 := m2.Get().Discovery.Jobs; len(js2) != 1 || js2[0].IntervalMin != 12 {
		t.Fatalf("reloaded: %+v", js2)
	}
	// 清空任务列表后保持为空，不会被重新迁移出默认任务
	c := m2.Get()
	c.Discovery.Jobs = []ScanJob{}
	if err := m2.Update(c); err != nil {
		t.Fatal(err)
	}
	m3, _ := Load(p)
	if n := len(m3.Get().Discovery.Jobs); n != 0 {
		t.Fatalf("jobs should stay empty, got %d", n)
	}
	// 修改 Get 的副本不影响当前配置
	c = m3.Get()
	c.Discovery.Exclude = append(c.Discovery.Exclude, "1.1.1.1")
	if len(m3.Get().Discovery.Exclude) != 0 {
		t.Fatal("Get must return a copy")
	}
	// 重复 id
	c.Discovery.Jobs = []ScanJob{{ID: "a", IntervalMin: 30}, {ID: "a", IntervalMin: 30}}
	if err := m3.Update(c); err == nil {
		t.Fatal("duplicate job id should fail")
	}
}
