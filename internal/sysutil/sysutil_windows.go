//go:build windows

package sysutil

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const appName = "netPing"

// OpenURL 用默认浏览器打开地址（兼容 Windows Server 2008 R2）。
func OpenURL(url string) error {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}

// OpenPath 用资源管理器打开目录。
func OpenPath(path string) error {
	return exec.Command("explorer", path).Start()
}

var mutexHandle windows.Handle

// AcquireSingleInstance 通过命名互斥体确保只运行一个实例，已有实例时返回 false。
func AcquireSingleInstance() (bool, error) {
	name, _ := windows.UTF16PtrFromString(`Local\netPing-single-instance`)
	h, err := windows.CreateMutex(nil, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if h != 0 {
			windows.CloseHandle(h)
		}
		return false, nil
	}
	if err != nil {
		return true, err
	}
	mutexHandle = h
	return true, nil
}

// AutoStartSupported 当前平台是否支持开机自启设置。
func AutoStartSupported() bool { return true }

// AutoStartEnabled 是否已设置登录后自动启动。
func AutoStartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(appName)
	if err != nil {
		return false
	}
	exe, _ := os.Executable()
	return v == `"`+exe+`"`
}

// SetAutoStart 设置/取消登录后自动启动（当前用户）。
func SetAutoStart(on bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		err := k.DeleteValue(appName)
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return k.SetStringValue(appName, `"`+exe+`"`)
}
