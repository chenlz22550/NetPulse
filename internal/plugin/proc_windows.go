//go:build windows

package plugin

import (
	"os/exec"
	"syscall"
)

// hideWindow 让插件进程不弹出控制台窗口（netPing 是无控制台的托盘程序）。
func hideWindow(cmd *exec.Cmd) {
	const createNoWindow = 0x08000000
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
