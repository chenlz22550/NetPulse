// genicon 生成 exe 图标用的 PNG（winres/icon.png、winres/icon16.png）。
// 用法：go run ./tools/genicon
package main

import (
	"log"
	"os"
	"path/filepath"

	"netping/internal/tray"
)

func main() {
	dir := "winres"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Fatal(err)
	}
	for name, size := range map[string]int{"icon.png": 256, "icon16.png": 16} {
		if err := os.WriteFile(filepath.Join(dir, name), tray.PNG(size, tray.LevelOK), 0o644); err != nil {
			log.Fatal(err)
		}
	}
}
