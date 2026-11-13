package config

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestConcurrentUpdatesPersistCurrentConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 32)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		c := m.Get()
		c.RetentionDays = 31 + i
		wg.Add(1)
		go func(c Config) {
			defer wg.Done()
			<-start
			errs <- m.Update(c)
		}(c)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent save: %v", err)
		}
	}
	disk, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := disk.Get().RetentionDays, m.Get().RetentionDays; got != want {
		t.Fatalf("disk retention = %d, memory = %d", got, want)
	}
}

func TestFailedUpdateKeepsPreviousConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old := m.Get()
	next := old.Clone()
	next.RetentionDays++
	// 模拟无法写入临时文件，无需依赖当前用户的文件权限。
	if err := os.Mkdir(path+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.Update(next); err == nil {
		t.Fatal("expected save failure")
	}
	if got := m.Get().RetentionDays; got != old.RetentionDays {
		t.Fatalf("failed update changed memory to %d", got)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("failed update changed persisted config")
	}
}

func TestUpdateOwnsConfigMaps(t *testing.T) {
	m, err := Load(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	c := m.Get()
	c.Discovery.OUI["AA:BB:CC"] = "original"
	if err := m.Update(c); err != nil {
		t.Fatal(err)
	}
	c.Discovery.OUI["AA:BB:CC"] = "changed"
	if got := m.Get().Discovery.OUI["AA:BB:CC"]; got != "original" {
		t.Fatalf("caller changed published config: %q", got)
	}
}
