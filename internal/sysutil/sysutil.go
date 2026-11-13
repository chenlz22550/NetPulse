// Package sysutil 封装与操作系统相关的小功能：打开浏览器、单实例、开机自启。
package sysutil

import (
	"os"
	"path/filepath"
	"strings"
)

// BaseDir 返回程序数据目录：默认是 exe 所在目录；go run 时使用当前目录。
func BaseDir() string {
	exe, err := os.Executable()
	if err == nil {
		exe, _ = filepath.EvalSymlinks(exe)
		// macOS .app 包内不可写（且可能被系统随机挂载），数据放在 ~/Library/Application Support/netPing
		if appBundle(exe) != "" {
			if home, err := os.UserHomeDir(); err == nil {
				return filepath.Join(home, "Library", "Application Support", "netPing")
			}
		}
		dir := filepath.Dir(exe)
		tmp := os.TempDir()
		if !strings.Contains(dir, "go-build") && !strings.HasPrefix(dir, tmp) {
			return dir
		}
	}
	wd, _ := os.Getwd()
	return wd
}

// appBundle 若 exe 位于 xxx.app/Contents/MacOS/ 中，返回 .app 路径，否则返回空字符串。
func appBundle(exe string) string {
	dir := filepath.Dir(exe)
	if filepath.Base(dir) != "MacOS" || filepath.Base(filepath.Dir(dir)) != "Contents" {
		return ""
	}
	app := filepath.Dir(filepath.Dir(dir))
	if !strings.HasSuffix(app, ".app") {
		return ""
	}
	return app
}
