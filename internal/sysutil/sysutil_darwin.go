//go:build darwin

package sysutil

import (
	"encoding/xml"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const agentLabel = "com.netping.agent"

// OpenURL 用默认浏览器打开地址。
func OpenURL(url string) error { return exec.Command("open", url).Start() }

// OpenPath 在访达中打开目录。
func OpenPath(path string) error { return exec.Command("open", path).Start() }

var lockFile *os.File

// AcquireSingleInstance 用文件锁保证当前用户只运行一个实例。
func AcquireSingleInstance() (bool, error) {
	f, err := os.OpenFile(filepath.Join(os.TempDir(), "netping-single-instance.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return true, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if err == unix.EWOULDBLOCK {
			return false, nil
		}
		return true, err
	}
	lockFile = f
	return true, nil
}

func agentPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", agentLabel+".plist")
}

// AutoStartSupported macOS 通过 LaunchAgent 实现登录后自动启动。
func AutoStartSupported() bool { return true }

// AutoStartEnabled 是否已写入 LaunchAgent。
func AutoStartEnabled() bool {
	_, err := os.Stat(agentPath())
	return err == nil
}

// SetAutoStart 写入或删除 ~/Library/LaunchAgents/com.netping.agent.plist。
func SetAutoStart(on bool) error {
	p := agentPath()
	if !on {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	args := []string{exe}
	if app := appBundle(exe); app != "" {
		args = []string{"/usr/bin/open", "-a", app}
	}
	esc := func(s string) string {
		var b strings.Builder
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>` + agentLabel + `</string>
	<key>ProgramArguments</key>
	<array>
`)
	for _, a := range args {
		sb.WriteString("\t\t<string>" + esc(a) + "</string>\n")
	}
	sb.WriteString(`	</array>
	<key>RunAtLoad</key><true/>
	<key>ProcessType</key><string>Interactive</string>
</dict>
</plist>
`)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(sb.String()), 0o644)
}
