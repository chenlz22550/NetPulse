//go:build !windows && !darwin

package sysutil

import (
	"errors"
	"os/exec"
	"runtime"
)

// OpenURL 用默认浏览器打开地址。
func OpenURL(url string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", url).Start()
	}
	return exec.Command("xdg-open", url).Start()
}

// OpenPath 打开目录。
func OpenPath(path string) error { return OpenURL(path) }

// AcquireSingleInstance 非 Windows 平台不做限制。
func AcquireSingleInstance() (bool, error) { return true, nil }

// AutoStartSupported 当前平台是否支持开机自启设置。
func AutoStartSupported() bool { return false }

// AutoStartEnabled 非 Windows 平台不支持。
func AutoStartEnabled() bool { return false }

// SetAutoStart 非 Windows 平台不支持。
func SetAutoStart(bool) error { return errors.New("仅 Windows 支持开机自启") }
