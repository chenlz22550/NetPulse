package store

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"testing/quick"
	"time"
)

func TestEncodeDecodePropertiesAndBounds(t *testing.T) {
	// 边界裁剪与编解码不变量：Sent/Recv 裁剪至 [0, 65535]，时间与浮点精度保留（float32 误差范围内）。
	cases := []Minute{
		{T: 0, Sent: -10, Recv: 70000, Min: 0, Avg: 0, Max: 0},
		{T: 1700000000, Sent: 65535, Recv: 65535, Min: 0.125, Avg: 1.5, Max: 999.25},
	}
	for _, m := range cases {
		dec := decode(encode(m))
		if dec.T != m.T {
			t.Fatalf("T mismatch: %d vs %d", dec.T, m.T)
		}
		if dec.Sent < 0 || dec.Sent > 65535 || dec.Recv < 0 || dec.Recv > 65535 {
			t.Fatalf("clamp failed: %+v", dec)
		}
	}
	if err := quick.Check(func(ts int64, sent, recv uint16, minV, avgV, maxV float32) bool {
		if math.IsNaN(float64(minV)) || math.IsNaN(float64(avgV)) || math.IsNaN(float64(maxV)) {
			return true
		}
		m := Minute{T: ts, Sent: int(sent), Recv: int(recv), Min: float64(minV), Avg: float64(avgV), Max: float64(maxV)}
		dec := decode(encode(m))
		return dec.T == m.T && dec.Sent == m.Sent && dec.Recv == m.Recv &&
			float32(dec.Min) == minV && float32(dec.Avg) == avgV && float32(dec.Max) == maxV
	}, &quick.Config{MaxCount: 200}); err != nil {
		t.Fatal(err)
	}
}

func TestStoreEdgeCasesAndCleanupEmptyDir(t *testing.T) {
	// New fails on file path
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(f); err == nil {
		t.Fatal("expected New to fail when path is a file")
	}

	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Query empty / cross-day / out-of-order append returns sorted [from, to)
	base := time.Now().Truncate(24 * time.Hour)
	day1 := base.Add(-25 * time.Hour)
	day2 := base.Add(-1 * time.Hour)
	for _, m := range []Minute{
		{T: day2.Unix(), Sent: 10, Recv: 10, Avg: 2},
		{T: day1.Unix(), Sent: 10, Recv: 10, Avg: 1},
	} {
		if err := s.Append("n1", m); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Query("n1", day1.Unix(), day2.Unix()) // to is exclusive
	if err != nil || len(got) != 1 || got[0].T != day1.Unix() {
		t.Fatalf("Query half-open [from, to): %+v err=%v", got, err)
	}
	got, err = s.Query("n1", day1.Unix(), day2.Unix()+1)
	if err != nil || len(got) != 2 || got[0].T >= got[1].T {
		t.Fatalf("Query sorted cross-day: %+v err=%v", got, err)
	}
	// Truncated trailing bytes are ignored safely
	p := filepath.Join(dir, "n1", dayName(day2))
	raw, _ := os.ReadFile(p)
	_ = os.WriteFile(p, append(raw, 1, 2, 3), 0o644)
	got, err = s.Query("n1", day2.Unix(), day2.Unix()+60)
	if err != nil || len(got) != 1 {
		t.Fatalf("Query with trailing bytes: %+v err=%v", got, err)
	}

	// Non-directory entry inside store root is skipped; node dir with only old files is removed
	_ = os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("x"), 0o644)
	oldNode := filepath.Join(dir, "oldnode")
	_ = os.MkdirAll(oldNode, 0o755)
	oldFile := filepath.Join(oldNode, time.Now().AddDate(0, 0, -40).Format("20060102")+".bin")
	_ = os.WriteFile(oldFile, make([]byte, recordSize), 0o644)
	removed, err := s.Cleanup(30)
	if err != nil || removed != 1 {
		t.Fatalf("Cleanup removed=%d err=%v", removed, err)
	}
	if _, err := os.Stat(oldNode); !os.IsNotExist(err) {
		t.Fatal("empty node dir should be removed")
	}
}
