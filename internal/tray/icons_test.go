package tray

import (
	"bytes"
	"image/png"
	"testing"
)

func TestIconGenerationAndTruncate(t *testing.T) {
	for _, level := range []int{-1, LevelOK, LevelWarn, LevelDown, LevelPaused, 99} {
		img := Draw(16, level)
		if img.Bounds().Dx() != 16 || img.Bounds().Dy() != 16 {
			t.Fatalf("Draw(16, %d) bounds = %v", level, img.Bounds())
		}
		pngBytes := PNG(32, level)
		decoded, err := png.Decode(bytes.NewReader(pngBytes))
		if err != nil || decoded.Bounds().Dx() != 32 {
			t.Fatalf("PNG(32, %d) invalid: err=%v", level, err)
		}
		icoBytes := ICO(level)
		// ICO header: 0x0000, type=1, count=3
		if len(icoBytes) < 22 || icoBytes[0] != 0 || icoBytes[1] != 0 || icoBytes[2] != 1 || icoBytes[4] != 3 {
			t.Fatalf("ICO(%d) header invalid: len=%d", level, len(icoBytes))
		}
	}
	if len(iconBytes(LevelOK)) == 0 {
		t.Fatal("iconBytes returned empty")
	}
	if got := truncate("hello", 10); got != "hello" {
		t.Fatalf("truncate short = %q", got)
	}
	if got := truncate("机房网络监控状态很长", 5); got != "机房网络…" {
		t.Fatalf("truncate long = %q", got)
	}
}
