// netPing：机房节点 Ping 监控（系统托盘 + 管理网页 + 企业微信告警）。
package main

import (
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"netping/internal/alert"
	"netping/internal/config"
	"netping/internal/discovery"
	"netping/internal/locate"
	"netping/internal/monitor"
	"netping/internal/plugin"
	"netping/internal/profiles"
	"netping/internal/server"
	"netping/internal/store"
	"netping/internal/sysutil"
	"netping/internal/topology"
	"netping/internal/tray"
)

var version = "1.5.0"

//go:embed docs/topology-fields.md
var docsMD []byte

//go:embed docs/plugin-development.md
var pluginDocMD []byte

//go:embed schema/topology.schema.json
var schemaJSON []byte

func main() {
	dirFlag := flag.String("dir", "", "数据目录（默认为程序所在目录）")
	noTray := flag.Bool("notray", false, "不显示托盘图标，仅运行监控和网页（命令行/调试用）")
	noBrowser := flag.Bool("nobrowser", false, "首次启动时不自动打开浏览器")
	flag.Parse()

	baseDir := *dirFlag
	if baseDir == "" {
		baseDir = sysutil.BaseDir()
	}
	baseDir, _ = filepath.Abs(baseDir)
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		fatal(err)
	}
	logFile := setupLog(baseDir)
	if logFile != nil {
		defer logFile.Close()
	}
	log.Printf("netPing %s 启动，数据目录 %s", version, baseDir)

	cfgMgr, err := config.Load(filepath.Join(baseDir, "config.yaml"))
	if err != nil {
		fatal(err)
	}
	cfg := cfgMgr.Get()
	uiURL := localURL(cfg.Listen)

	if ok, err := sysutil.AcquireSingleInstance(); err == nil && !ok {
		log.Printf("已有实例在运行，打开管理页面后退出")
		_ = sysutil.OpenURL(uiURL)
		return
	}

	topoPath := cfg.TopologyFile
	if !filepath.IsAbs(topoPath) {
		topoPath = filepath.Join(baseDir, topoPath)
	}
	_, statErr := os.Stat(topoPath)
	firstRun := os.IsNotExist(statErr)
	topo, err := topology.Open(topoPath)
	if err != nil {
		fatal(err)
	}

	dataDir := filepath.Join(baseDir, "data")
	st, err := store.New(filepath.Join(dataDir, "history"))
	if err != nil {
		fatal(err)
	}
	mon := monitor.New(st, func() int { return cfgMgr.Get().Alert.DownAfterFailures })
	mon.SetSchedule(func() bool { return cfgMgr.Get().Schedule.Active(time.Now()) })
	alerts := alert.NewEngine(cfgMgr.Get, mon, filepath.Join(dataDir, "alerts.jsonl"))

	// 插件：数据目录下 plugins/<id>/，默认全部未启用，在网页「插件」页启用。
	plugins := plugin.New(filepath.Join(baseDir, "plugins"), dataDir)
	plugins.Host = func() plugin.HostInfo {
		site := ""
		if t, _ := topo.Snapshot(); t != nil {
			site = t.Site.Name
		}
		return plugin.HostInfo{Version: version, Site: site, URL: uiURL}
	}
	config.SetExtraChannels(plugins.HasNotifiers)
	alerts.SetSender(notifySender(plugins, "alert"))
	alerts.OnEvents = func(evs []alert.Event, notified bool) {
		plugins.Emit(plugin.HookAlert, map[string]interface{}{"events": evs, "notified": notified})
	}

	disc := discovery.New(cfgMgr, topo, mon, dataDir)
	notify := func(page, source string) func(title string, lines []string) {
		send := notifySender(plugins, source)
		return func(title string, lines []string) {
			c := cfgMgr.Get().Alert
			if !c.Enabled || !(c.HasChannels() || plugins.HasNotifiers()) {
				return
			}
			content := "### " + c.SiteTitle + " · " + title + "\n"
			for i, l := range lines {
				if len(content)+len(l) > 3800 {
					content += fmt.Sprintf("…另有 %d 条，请在管理页面「%s」查看\n", len(lines)-i, page)
					break
				}
				content += l + "\n"
			}
			if err := send(c, content); err != nil {
				log.Printf("发送%s通知失败: %v", page, err)
			}
		}
	}
	disc.Notify = notify("扫描发现", "discovery")
	disc.OnChanges = func(r *discovery.ScanResult) {
		plugins.Emit(plugin.HookDiscovery, map[string]interface{}{"job": r.JobName, "changes": r.Changes, "suggestions": r.Suggestions})
	}
	alerts.HoldDown = disc.HoldDown
	loc := locate.New(cfgMgr, topo, dataDir)
	loc.Notify = notify("端口定位", "locate")
	loc.OnChanges = func(r *locate.RunResult) {
		plugins.Emit(plugin.HookLocate, map[string]interface{}{"changes": r.Changes, "suggestions": r.Suggestions})
	}
	disc.OnAdded = loc.Trigger

	profMgr, err := profiles.Open(profiles.Options{
		BaseDir: baseDir,
		Cfg:     cfgMgr,
		Topo:    topo,
		Mon:     mon,
		Store:   st,
		Alerts:  alerts,
		Disc:    disc,
		Loc:     loc,
	})
	if err != nil {
		fatal(err)
	}

	apply := func(t *topology.Topology) {
		mon.Sync(monitor.BuildTargets(t, cfgMgr.Get().Ping))
		alerts.SetTopology(t)
	}
	topo.OnChange(apply)
	t0, _ := topo.Snapshot()
	apply(t0)

	stop := make(chan struct{})
	go alerts.Run(stop)
	go retentionLoop(st, cfgMgr, stop)
	go disc.Run(stop)
	go loc.Run(stop)
	go func() { // 记录进入/离开监控时段
		last := mon.InSchedule()
		tk := time.NewTicker(10 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				if now := mon.InSchedule(); now != last {
					last = now
					if now {
						log.Printf("进入监控时段，恢复 ping 与告警")
					} else {
						log.Printf("离开监控时段，暂停 ping 与告警")
					}
				}
			}
		}
	}()
	// 网页里修改了默认 ping 参数后需要重新下发目标。
	go func() {
		last := cfgMgr.Get().Ping
		tk := time.NewTicker(3 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				if p := cfgMgr.Get().Ping; p != last {
					last = p
					t, _ := topo.Snapshot()
					mon.Sync(monitor.BuildTargets(t, p))
				}
			}
		}
	}()

	server.Version = version
	srv := &server.Server{Cfg: cfgMgr, Topo: topo, Mon: mon, Alerts: alerts, Store: st, Profiles: profMgr, BaseDir: baseDir,
		DocsMD: docsMD, SchemaJSON: schemaJSON, Disc: disc, Loc: loc, Plugins: plugins, PluginDocMD: pluginDocMD}
	addr, err := srv.Start()
	if err != nil {
		fatal(err)
	}
	log.Printf("管理页面: %s （监听 %s）", uiURL, addr)

	shutdown := func() {
		close(stop)
		plugins.Close()
		srv.Close()
		mon.Close()
		profMgr.Shutdown()
		log.Printf("netPing 已退出")
	}

	if firstRun && !*noBrowser {
		go func() {
			time.Sleep(800 * time.Millisecond)
			_ = sysutil.OpenURL(uiURL)
		}()
	}

	if *noTray {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		shutdown()
		return
	}

	go func() { // 控制台 Ctrl+C 时也能正常退出托盘
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		tray.Quit()
	}()
	tray.Run(tray.Options{
		URL:       uiURL,
		DataDir:   baseDir,
		Status:    func() (string, int, bool) { return trayStatus(mon) },
		SetPaused: mon.SetPaused,
		OnExit: func() {
			shutdown()
			// 托盘消息循环在个别环境下（如 macOS 无图形会话）不会自行返回，兜底退出。
			go func() {
				time.Sleep(3 * time.Second)
				os.Exit(0)
			}()
		},
	})
}

// notifySender 在内置通知渠道之外，把消息同时发给已启用的通知类插件；两边互不影响，失败原因合并返回。
func notifySender(plugins *plugin.Manager, source string) alert.Sender {
	return func(cfg config.AlertConfig, content string) error {
		builtin := cfg.HasChannels()
		var builtinErr error
		done := make(chan struct{})
		go func() {
			defer close(done)
			if builtin {
				builtinErr = alert.SendNotifications(cfg, content)
			}
		}()
		n, pluginErr := plugins.Notify(source, content)
		<-done
		if !builtin && n == 0 {
			return errors.New("未配置通知渠道")
		}
		return errors.Join(builtinErr, pluginErr)
	}
}

func trayStatus(mon *monitor.Monitor) (string, int, bool) {
	sts := mon.Statuses(false)
	switch mon.PauseReason() {
	case "manual":
		return fmt.Sprintf("netPing 已手动暂停（%d 个目标）", len(sts)), tray.LevelPaused, true
	case "schedule":
		return "netPing 非监控时段，暂停中", tray.LevelPaused, false
	}
	var down, warn, ok int
	for _, s := range sts {
		switch s.State {
		case monitor.StateDown:
			down++
		case monitor.StateLoss, monitor.StateSlow:
			warn++
		case monitor.StateOK:
			ok++
		}
	}
	tip := fmt.Sprintf("netPing 正常 %d / 告警 %d / 离线 %d", ok, warn, down)
	level := tray.LevelOK
	if warn > 0 {
		level = tray.LevelWarn
	}
	if down > 0 {
		level = tray.LevelDown
	}
	return tip, level, false
}

func retentionLoop(st *store.Store, cfg *config.Manager, stop <-chan struct{}) {
	run := func() {
		n, err := st.Cleanup(cfg.Get().RetentionDays)
		if err != nil {
			log.Printf("清理历史数据失败: %v", err)
		} else if n > 0 {
			log.Printf("已清理 %d 个过期历史文件", n)
		}
	}
	run()
	tk := time.NewTicker(6 * time.Hour)
	defer tk.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tk.C:
			run()
		}
	}
}

// localURL 把监听地址转换成本机浏览器可访问的地址。
func localURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://127.0.0.1:8090/"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

func setupLog(baseDir string) *os.File {
	dir := filepath.Join(baseDir, "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil
	}
	path := filepath.Join(dir, "netping.log")
	if fi, err := os.Stat(path); err == nil && fi.Size() > 10<<20 {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil
	}
	// windowsgui 程序没有控制台，写 stderr 会失败，但 MultiWriter 遇错会中断，因此只在有控制台时附加。
	var w io.Writer = f
	if fi, err := os.Stderr.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		w = io.MultiWriter(f, os.Stderr)
	}
	log.SetOutput(w)
	log.SetFlags(log.LstdFlags)
	return f
}

func fatal(err error) {
	msg := strings.TrimSpace(err.Error())
	log.Printf("启动失败: %s", msg)
	fmt.Fprintln(os.Stderr, "启动失败:", msg)
	showError("netPing 启动失败", msg)
	os.Exit(1)
}
