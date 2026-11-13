package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAppendQueryCleanup(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Minute)
	for i := 0; i < 5; i++ {
		m := Minute{T: now.Add(time.Duration(i-4) * time.Minute).Unix(), Sent: 60, Recv: 58, Min: 0.5, Avg: 1.25, Max: 9}
		if err := s.Append("n1", m); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Query("n1", now.Add(-3*time.Minute).Unix(), now.Add(time.Minute).Unix())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0].Recv != 58 || got[0].Avg != 1.25 {
		t.Fatalf("query = %+v", got)
	}
	old := filepath.Join(dir, "n1", time.Now().AddDate(0, 0, -40).Format("20060102")+".bin")
	if err := os.WriteFile(old, make([]byte, recordSize), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := s.Cleanup(30)
	if err != nil || n != 1 {
		t.Fatalf("cleanup removed %d, err %v", n, err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old file still exists")
	}
}
