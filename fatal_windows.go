//go:build windows

package main

import (
	"golang.org/x/sys/windows"
)

// showError 在没有控制台的 GUI 程序中用消息框提示启动错误。
func showError(title, msg string) {
	t, _ := windows.UTF16PtrFromString(title)
	m, _ := windows.UTF16PtrFromString(msg)
	windows.MessageBox(0, m, t, windows.MB_OK|windows.MB_ICONERROR)
}
