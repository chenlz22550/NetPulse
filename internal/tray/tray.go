// Package tray 实现系统托盘图标和菜单。
package tray

import (
	"fmt"
	"log"
	"runtime"
	"time"

	"fyne.io/systray"

	"netping/internal/sysutil"
)

// 图标级别。
const (
	LevelOK = iota
	LevelWarn
	LevelDown
	LevelPaused
)

// Options 托盘依赖的回调。
type Options struct {
	URL       string
	DataDir   string
	Status    func() (tooltip string, level int, paused bool)
	SetPaused func(bool)
	OnExit    func()
}

func iconBytes(level int) []byte {
	if runtime.GOOS == "windows" {
		return ICO(level)
	}
	return PNG(64, level)
}

// Run 运行托盘消息循环（阻塞，必须在主协程调用）。
func Run(o Options) {
	var icons [4][]byte
	for i := range icons {
		icons[i] = iconBytes(i)
	}
	systray.Run(func() { onReady(o, icons) }, func() {
		if o.OnExit != nil {
			o.OnExit()
		}
	})
}

// Quit 退出托盘循环。
func Quit() { systray.Quit() }

func onReady(o Options, icons [4][]byte) {
	systray.SetIcon(icons[LevelOK])
	systray.SetTooltip("网脉 NetPulse")
	if runtime.GOOS == "darwin" {
		systray.SetTitle("")
	}

	mStatus := systray.AddMenuItem("网脉 NetPulse 启动中…", "")
	mStatus.Disable()
	systray.AddSeparator()
	mOpen := systray.AddMenuItem("打开管理页面", o.URL)
	mPause := systray.AddMenuItem("暂停监控", "暂停/恢复所有 ping")
	var mAuto *systray.MenuItem
	if sysutil.AutoStartSupported() {
		mAuto = systray.AddMenuItemCheckbox("登录后自动启动", "写入当前用户的启动项", sysutil.AutoStartEnabled())
	}
	mDir := systray.AddMenuItem("打开数据目录", o.DataDir)
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("退出", "停止监控并退出")

	autoCh := make(chan struct{})
	if mAuto != nil {
		autoCh = mAuto.ClickedCh
	}

	go func() {
		tk := time.NewTicker(2 * time.Second)
		defer tk.Stop()
		lastLevel, lastTip := -1, ""
		refresh := func() {
			tip, level, paused := o.Status()
			if level != lastLevel {
				systray.SetIcon(icons[level])
				lastLevel = level
			}
			if tip != lastTip {
				systray.SetTooltip(truncate(tip, 120))
				mStatus.SetTitle(tip)
				lastTip = tip
			}
			if paused {
				mPause.SetTitle("恢复监控")
			} else {
				mPause.SetTitle("暂停监控")
			}
		}
		refresh()
		for {
			select {
			case <-tk.C:
				refresh()
			case <-mOpen.ClickedCh:
				if err := sysutil.OpenURL(o.URL); err != nil {
					log.Printf("打开浏览器失败: %v", err)
				}
			case <-mPause.ClickedCh:
				_, _, paused := o.Status()
				o.SetPaused(!paused)
				refresh()
			case <-autoCh:
				on := !mAuto.Checked()
				if err := sysutil.SetAutoStart(on); err != nil {
					log.Printf("设置开机自启失败: %v", err)
				} else if on {
					mAuto.Check()
				} else {
					mAuto.Uncheck()
				}
			case <-mDir.ClickedCh:
				_ = sysutil.OpenPath(o.DataDir)
			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return fmt.Sprint(string(r[:n-1]), "…")
}
