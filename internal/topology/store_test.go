package topology

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestConcurrentNotificationsKeepLatestTopology(t *testing.T) {
	for _, reload := range []bool{false, true} {
		name := "update"
		if reload {
			name = "reload"
		}
		t.Run(name, func(t *testing.T) {
			s, err := Open(filepath.Join(t.TempDir(), "topology.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			newerApplied := make(chan struct{}, 2)
			var mu sync.Mutex
			applied := ""
			s.OnChange(func(top *Topology) {
				// 回调应能读取 Store，不能持有 Store 的数据写锁。
				s.Snapshot()
				if top.Site.Name == "older" {
					close(entered)
					<-release
				} else {
					newerApplied <- struct{}{}
				}
				mu.Lock()
				applied = top.Site.Name
				mu.Unlock()
			})
			first := make(chan error, 1)
			go func() {
				first <- s.Update(func(top *Topology) error { top.Site.Name = "older"; return nil })
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("first callback did not start")
			}
			second := make(chan error, 1)
			go func() {
				if !reload {
					second <- s.Update(func(top *Topology) error { top.Site.Name = "newer"; return nil })
					return
				}
				top, _ := s.Snapshot()
				top.Site.Name = "newer"
				data, err := Marshal(top)
				if err == nil {
					err = os.WriteFile(s.Path(), data, 0o644)
				}
				if err == nil {
					err = s.Reload()
				}
				second <- err
			}()
			select {
			case <-newerApplied:
				t.Error("new callback overtook the blocked old callback")
			case <-time.After(100 * time.Millisecond):
			}
			unblock()
			for _, done := range []<-chan error{first, second} {
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("topology update did not finish")
				}
			}
			top, _ := s.Snapshot()
			mu.Lock()
			defer mu.Unlock()
			if applied != "newer" || applied != top.Site.Name {
				t.Fatalf("last applied = %q, current = %q", applied, top.Site.Name)
			}
		})
	}
}
