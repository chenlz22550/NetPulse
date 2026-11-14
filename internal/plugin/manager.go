package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"netping/internal/config"
	"regexp"
)

const (
	runsKeep      = 100 // 保留最近多少条调用记录（内存）
	eventQueueLen = 256 // 待分发事件队列长度，满了丢弃最新事件
	maxConcurrent = 4   // 同时运行的插件进程数
)

// HostInfo 是随每次请求告诉插件的 netPing 信息。
type HostInfo struct {
	Version string `json:"version"`
	Site    string `json:"site"` // 机房名称
	URL     string `json:"url"`  // 管理页面地址
}

// ToolInfo 是一个已加载的插件工具。
type ToolInfo struct {
	ToolDef
	FullName   string `json:"full_name"` // plugin_<id>_<name>
	PluginID   string `json:"plugin_id"`
	PluginName string `json:"plugin_name"`
}

// SettingView 是设置项及其当前值；secret 类型不返回值，只返回是否已填写。
type SettingView struct {
	SettingDef
	Value interface{} `json:"value"`
	Set   bool        `json:"set"`
}

// Info 是网页上展示的插件信息。
type Info struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Version     string        `json:"version"`
	Description string        `json:"description"`
	Author      string        `json:"author"`
	Homepage    string        `json:"homepage"`
	Dir         string        `json:"dir"`
	Command     []string      `json:"command"`
	TimeoutSec  int           `json:"timeout_sec"`
	Enabled     bool          `json:"enabled"`
	Error       string        `json:"error,omitempty"` // 清单无效时的原因（此时无法启用）
	Hooks       []string      `json:"hooks"`
	Tools       []ToolInfo    `json:"tools"`
	Settings    []SettingView `json:"settings"`
	LastRun     *RunRecord    `json:"last_run,omitempty"`
}

// RunRecord 是一次插件调用的结果记录。
type RunRecord struct {
	Time    time.Time `json:"time"`
	Plugin  string    `json:"plugin"`
	Hook    string    `json:"hook"`
	Tool    string    `json:"tool,omitempty"`
	Test    bool      `json:"test,omitempty"`
	OK      bool      `json:"ok"`
	Ms      int64     `json:"ms"`
	Message string    `json:"message,omitempty"`
	Error   string    `json:"error,omitempty"`
	Stderr  string    `json:"stderr,omitempty"`
}

type pluginState struct {
	Enabled  bool                   `json:"enabled"`
	Settings map[string]interface{} `json:"settings,omitempty"`
}

type loaded struct {
	id       string
	dir      string
	manifest *Manifest // 清单无效时为 nil
	err      string
}

type queued struct {
	hook    string
	payload interface{}
}

// Manager 管理插件的加载、启停、设置与调用。所有方法可并发调用；nil *Manager 的查询方法返回空结果。
type Manager struct {
	// Host 返回随请求传给插件的 netPing 信息（可为 nil）。
	Host func() HostInfo

	dir       string // plugins 目录
	dataDir   string // 插件私有数据根目录 data/plugins
	statePath string // 启停状态与设置 data/plugins.json

	mu      sync.Mutex
	plugins map[string]*loaded
	state   map[string]*pluginState
	runs    []RunRecord

	sem       chan struct{}
	queue     chan queued
	startOnce sync.Once
	closeOnce sync.Once
	done      chan struct{}
}

// New 创建插件管理器并扫描 dir 下的插件。dataDir 是 netPing 的 data 目录。
func New(dir, dataDir string) *Manager {
	m := &Manager{
		dir:       dir,
		dataDir:   filepath.Join(dataDir, "plugins"),
		statePath: filepath.Join(dataDir, "plugins.json"),
		plugins:   map[string]*loaded{},
		state:     map[string]*pluginState{},
		sem:       make(chan struct{}, maxConcurrent),
		queue:     make(chan queued, eventQueueLen),
		done:      make(chan struct{}),
	}
	m.loadState()
	m.Reload()
	return m
}

// Dir 返回插件目录。
func (m *Manager) Dir() string {
	if m == nil {
		return ""
	}
	return m.dir
}

// Close 停止事件分发（已启动的插件进程会在超时内自行结束）。
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.closeOnce.Do(func() { close(m.done) })
}

func (m *Manager) loadState() {
	data, err := os.ReadFile(m.statePath)
	if err != nil {
		return
	}
	var f struct {
		Plugins map[string]*pluginState `json:"plugins"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		log.Printf("插件：读取 %s 失败（所有插件按未启用处理）: %v", m.statePath, err)
		return
	}
	for id, st := range f.Plugins {
		if st != nil {
			m.state[id] = st
		}
	}
}

// saveLocked 调用方须持有 m.mu。
func (m *Manager) saveLocked() error {
	data, err := json.MarshalIndent(map[string]interface{}{"plugins": m.state}, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(m.statePath, data)
}

// Reload 重新扫描插件目录（新增、删除插件或修改 plugin.yaml 后调用）。返回有效插件数量。
func (m *Manager) Reload() int {
	if m == nil {
		return 0
	}
	found := map[string]*loaded{}
	entries, err := os.ReadDir(m.dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("插件：读取目录 %s 失败: %v", m.dir, err)
	}
	valid := 0
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		dir := filepath.Join(m.dir, name)
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		p := &loaded{id: name, dir: dir}
		mf, err := LoadManifest(dir)
		if err != nil {
			p.err = err.Error()
		} else {
			p.manifest = mf
			valid++
		}
		found[name] = p
	}
	m.mu.Lock()
	for id, p := range found {
		old := m.plugins[id]
		switch {
		case p.err != "" && (old == nil || old.err != p.err):
			log.Printf("插件 %s 无法加载: %s", id, p.err)
		case p.err == "" && (old == nil || old.manifest == nil || old.manifest.Version != p.manifest.Version):
			st := "未启用"
			if s := m.state[id]; s != nil && s.Enabled {
				st = "已启用"
			}
			log.Printf("插件 %s（%s %s）已加载，%s", id, p.manifest.Name, p.manifest.Version, st)
		}
	}
	m.plugins = found
	m.mu.Unlock()
	return valid
}

// effectiveLocked 返回插件的生效设置（默认值叠加已保存的值）。调用方须持有 m.mu。
func (m *Manager) effectiveLocked(p *loaded) map[string]interface{} {
	out := map[string]interface{}{}
	var saved map[string]interface{}
	if st := m.state[p.id]; st != nil {
		saved = st.Settings
	}
	for _, d := range p.manifest.Settings {
		var v interface{}
		if sv, ok := saved[d.Key]; ok {
			// 清单升级后类型可能变化，已保存的值不再合法时退回默认值。
			if cv, err := coerceSetting(d, sv); err == nil {
				v = cv
			}
		}
		if v == nil {
			v = d.Default
		}
		if v == nil {
			switch d.Type {
			case SettingBool:
				v = false
			case SettingNumber:
				v = float64(0)
			default:
				v = ""
			}
		}
		out[d.Key] = v
	}
	return out
}

func (m *Manager) missingLocked(p *loaded) []string {
	eff := m.effectiveLocked(p)
	var out []string
	for _, d := range p.manifest.Settings {
		if d.Required && emptySetting(eff[d.Key]) {
			out = append(out, d.Label)
		}
	}
	return out
}

func toolInfos(mf *Manifest) []ToolInfo {
	out := make([]ToolInfo, 0, len(mf.Tools))
	for _, t := range mf.Tools {
		out = append(out, ToolInfo{ToolDef: t, FullName: FullToolName(mf.ID, t.Name), PluginID: mf.ID, PluginName: mf.Name})
	}
	return out
}

// List 返回全部插件（含清单无效的），按 id 排序。
func (m *Manager) List() []Info {
	out := []Info{}
	if m == nil {
		return out
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	last := map[string]*RunRecord{}
	for i := range m.runs {
		r := m.runs[i]
		last[r.Plugin] = &r
	}
	for _, p := range m.plugins {
		in := Info{ID: p.id, Name: p.id, Dir: p.dir, Error: p.err, Command: []string{}, Hooks: []string{}, Tools: []ToolInfo{}, Settings: []SettingView{}, LastRun: last[p.id]}
		if st := m.state[p.id]; st != nil {
			in.Enabled = st.Enabled && p.manifest != nil
		}
		if mf := p.manifest; mf != nil {
			in.Name, in.Version, in.Description, in.Author, in.Homepage = mf.Name, mf.Version, mf.Description, mf.Author, mf.Homepage
			in.Command, in.TimeoutSec, in.Hooks, in.Tools = mf.Command, mf.TimeoutSec, mf.Hooks, toolInfos(mf)
			eff := m.effectiveLocked(p)
			for _, d := range mf.Settings {
				v := SettingView{SettingDef: d, Value: eff[d.Key], Set: !emptySetting(eff[d.Key])}
				if d.Type == SettingSecret {
					v.Value = ""
				}
				in.Settings = append(in.Settings, v)
			}
		}
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (m *Manager) getLocked(id string) (*loaded, error) {
	p := m.plugins[id]
	if p == nil {
		return nil, fmt.Errorf("插件 %s 不存在", id)
	}
	if p.manifest == nil {
		return nil, fmt.Errorf("插件 %s 的清单无效：%s", id, p.err)
	}
	return p, nil
}

// SetEnabled 启用或停用插件。必填设置项未填写时不能启用。
func (m *Manager) SetEnabled(id string, on bool) error {
	if m == nil {
		return errors.New("插件系统未启用")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p, err := m.getLocked(id)
	if err != nil {
		return err
	}
	if on {
		if miss := m.missingLocked(p); len(miss) > 0 {
			return fmt.Errorf("启用前需要先填写：%s", strings.Join(miss, "、"))
		}
	}
	st := m.state[id]
	if st == nil {
		st = &pluginState{}
		m.state[id] = st
	}
	if st.Enabled == on {
		return nil
	}
	st.Enabled = on
	if err := m.saveLocked(); err != nil {
		st.Enabled = !on
		return err
	}
	if on {
		log.Printf("插件 %s 已启用", id)
	} else {
		log.Printf("插件 %s 已停用", id)
	}
	return nil
}

// UpdateSettings 合并保存设置：未出现的键保持不变；值为 null 恢复默认值；
// secret 类型提交空字符串表示保持原值（网页不回显密钥）。
func (m *Manager) UpdateSettings(id string, patch map[string]interface{}) error {
	if m == nil {
		return errors.New("插件系统未启用")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p, err := m.getLocked(id)
	if err != nil {
		return err
	}
	defs := map[string]SettingDef{}
	for _, d := range p.manifest.Settings {
		defs[d.Key] = d
	}
	st := m.state[id]
	if st == nil {
		st = &pluginState{}
		m.state[id] = st
	}
	next := map[string]interface{}{}
	for k, v := range st.Settings {
		if _, ok := defs[k]; ok { // 清单中已删除的设置项顺便清理
			next[k] = v
		}
	}
	for k, v := range patch {
		d, ok := defs[k]
		if !ok {
			return fmt.Errorf("插件 %s 没有设置项 %s", id, k)
		}
		if v == nil {
			delete(next, k)
			continue
		}
		cv, err := coerceSetting(d, v)
		if err != nil {
			return fmt.Errorf("%s：%v", d.Label, err)
		}
		if d.Type == SettingSecret && cv == "" {
			continue
		}
		next[k] = cv
	}
	old := st.Settings
	st.Settings = next
	if st.Enabled {
		if miss := m.missingLocked(p); len(miss) > 0 {
			st.Settings = old
			return fmt.Errorf("插件已启用，不能清空必填项：%s", strings.Join(miss, "、"))
		}
	}
	if err := m.saveLocked(); err != nil {
		st.Settings = old
		return err
	}
	return nil
}

// enabledWith 返回已启用且满足条件的插件及其生效设置。
func (m *Manager) enabledWith(match func(*Manifest) bool) []target {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []target
	for _, p := range m.plugins {
		st := m.state[p.id]
		if p.manifest == nil || st == nil || !st.Enabled || !match(p.manifest) {
			continue
		}
		out = append(out, target{id: p.id, dir: p.dir, manifest: p.manifest, settings: m.effectiveLocked(p)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// HasNotifiers 是否有已启用的通知类插件（声明了 notify 钩子）。
func (m *Manager) HasNotifiers() bool {
	return len(m.enabledWith(func(mf *Manifest) bool { return mf.HasHook(HookNotify) })) > 0
}

// Tools 返回已启用插件提供的全部工具。
func (m *Manager) Tools() []ToolInfo {
	var out []ToolInfo
	for _, t := range m.enabledWith(func(mf *Manifest) bool { return len(mf.Tools) > 0 }) {
		out = append(out, toolInfos(t.manifest)...)
	}
	return out
}

// Runs 返回最近的调用记录（新的在前）。
func (m *Manager) Runs(limit int) []RunRecord {
	out := []RunRecord{}
	if m == nil {
		return out
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.runs) - 1; i >= 0 && (limit <= 0 || len(out) < limit); i-- {
		out = append(out, m.runs[i])
	}
	return out
}

func (m *Manager) record(r RunRecord) {
	m.mu.Lock()
	m.runs = append(m.runs, r)
	if len(m.runs) > runsKeep {
		m.runs = append([]RunRecord(nil), m.runs[len(m.runs)-runsKeep:]...)
	}
	m.mu.Unlock()
	what := r.Hook
	if r.Tool != "" {
		what += " " + r.Tool
	}
	if r.Test {
		what += "（测试）"
	}
	if r.OK {
		log.Printf("插件 %s：%s 成功，%d ms", r.Plugin, what, r.Ms)
		return
	}
	msg := r.Error
	if r.Stderr != "" {
		msg += "；stderr: " + lastLine(r.Stderr)
	}
	log.Printf("插件 %s：%s 失败，%d ms：%s", r.Plugin, what, r.Ms, msg)
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return clip(strings.TrimSpace(s), 300)
}

// MCPToolSummary describes a plugin tool without exposing internal command paths.
type MCPToolSummary struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	FullName    string `json:"full_name"`
	Description string `json:"description,omitempty"`
}

// MCPSettingSummary describes a plugin setting with secrets and sensitive URLs masked.
type MCPSettingSummary struct {
	Key      string      `json:"key"`
	Label    string      `json:"label"`
	Type     string      `json:"type"`
	Required bool        `json:"required,omitempty"`
	Set      bool        `json:"set"`
	Masked   bool        `json:"masked,omitempty"`
	Value    interface{} `json:"value,omitempty"`
}

// MCPRunRecord is a structured run summary for MCP clients; free-text message/error/stderr
// are omitted so arbitrary plugin output or bare tokens cannot leak (inspect details on the web UI).
type MCPRunRecord struct {
	Time     time.Time `json:"time"`
	Plugin   string    `json:"plugin"`
	Hook     string    `json:"hook"`
	Tool     string    `json:"tool,omitempty"`
	Test     bool      `json:"test,omitempty"`
	OK       bool      `json:"ok"`
	Ms       int64     `json:"ms"`
	HasError bool      `json:"has_error"`
	Summary  string    `json:"summary"`
}

// MCPPluginInfo is the sanitized plugin view returned to MCP clients (no dir/command or raw secrets).
type MCPPluginInfo struct {
	ID          string              `json:"id"`
	Name        string              `json:"name"`
	Version     string              `json:"version,omitempty"`
	Description string              `json:"description,omitempty"`
	Author      string              `json:"author,omitempty"`
	Homepage    string              `json:"homepage,omitempty"`
	TimeoutSec  int                 `json:"timeout_sec,omitempty"`
	Enabled     bool                `json:"enabled"`
	Error       string              `json:"error,omitempty"`
	Hooks       []string            `json:"hooks"`
	Tools       []MCPToolSummary    `json:"tools"`
	Settings    []MCPSettingSummary `json:"settings"`
	LastRun     *MCPRunRecord       `json:"last_run,omitempty"`
}

// MCPViewPayload is the sanitized read-only plugins + runs payload returned to MCP.
type MCPViewPayload struct {
	Plugins   []MCPPluginInfo `json:"plugins"`
	Runs      []MCPRunRecord  `json:"runs"`
	RunsTotal int             `json:"runs_total"`
}

var (
	mcpURLUserinfoRe = regexp.MustCompile(`(https?://)([^/\s:@]+(?::[^/\s@]*)?)@([^\s"'?#]*)`)
	mcpURLQueryRe    = regexp.MustCompile(`(https?://[^\s"'?#]+)\?[^\s"']*`)
	mcpAuthTokenRe   = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]+`)
	mcpKVSecretRe    = regexp.MustCompile(`(?i)\b(token|secret|password|passwd|api[-_]?key|access[-_]?key|auth[-_]?key|signing[-_]?key)\s*[:=]\s*([^\s"',;]+)`)
)

func isSensitiveSettingKey(key string) bool {
	lk := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(key)), "-", "_")
	for _, kw := range []string{"secret", "token", "password", "passwd", "apikey", "api_key", "access_key", "auth_key", "signing_key", "private", "credential", "bearer", "authorization", "webhook"} {
		if strings.Contains(lk, kw) {
			return true
		}
	}
	return false
}

func isSensitiveSettingValue(key, typ string, val interface{}) bool {
	if typ == SettingSecret || isSensitiveSettingKey(key) {
		return true
	}
	if s, ok := val.(string); ok && strings.Contains(s, "://") {
		if u, err := url.Parse(strings.TrimSpace(s)); err == nil {
			if u.User != nil || u.RawQuery != "" {
				return true
			}
		}
		if strings.Contains(s, "@") || strings.Contains(s, "?") || strings.Contains(strings.ToLower(s), "webhook") {
			return true
		}
	}
	return false
}

// MCPView returns a sanitized read-only snapshot of installed plugins and recent structured run summaries.
func (m *Manager) MCPView(pluginID string, runsLimit int) (MCPViewPayload, bool) {
	out := MCPViewPayload{
		Plugins: []MCPPluginInfo{},
		Runs:    []MCPRunRecord{},
	}
	if m == nil {
		return out, true
	}
	if runsLimit <= 0 {
		runsLimit = 20
	}
	if runsLimit > runsKeep {
		runsLimit = runsKeep
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	secrets := m.collectSensitiveStringsLocked()
	paths := m.collectPathReplacementsLocked()

	filterID := strings.TrimSpace(pluginID)
	found := filterID == ""

	last := map[string]*MCPRunRecord{}
	for i := len(m.runs) - 1; i >= 0; i-- {
		r := summarizeMCPRun(m.runs[i])
		if _, ok := last[r.Plugin]; !ok {
			rc := r
			last[r.Plugin] = &rc
		}
		if filterID != "" && r.Plugin != filterID {
			continue
		}
		out.RunsTotal++
		if len(out.Runs) < runsLimit {
			out.Runs = append(out.Runs, r)
		}
	}

	ids := make([]string, 0, len(m.plugins))
	for id := range m.plugins {
		if filterID != "" && id != filterID {
			continue
		}
		found = true
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		p := m.plugins[id]
		info := MCPPluginInfo{
			ID:       p.id,
			Name:     p.id,
			Error:    sanitizeMCPText(p.err, secrets, paths),
			Hooks:    []string{},
			Tools:    []MCPToolSummary{},
			Settings: []MCPSettingSummary{},
			LastRun:  last[p.id],
		}
		if st := m.state[p.id]; st != nil {
			info.Enabled = st.Enabled && p.manifest != nil
		}
		if mf := p.manifest; mf != nil {
			info.Name = sanitizeMCPText(mf.Name, secrets, paths)
			info.Version = sanitizeMCPText(mf.Version, secrets, paths)
			info.Description = sanitizeMCPText(mf.Description, secrets, paths)
			info.Author = sanitizeMCPText(mf.Author, secrets, paths)
			info.Homepage = sanitizeMCPText(mf.Homepage, secrets, paths)
			info.TimeoutSec = mf.TimeoutSec
			if len(mf.Hooks) > 0 {
				info.Hooks = append([]string(nil), mf.Hooks...)
			}
			for _, t := range mf.Tools {
				info.Tools = append(info.Tools, MCPToolSummary{
					Name:        sanitizeMCPText(t.Name, secrets, paths),
					Title:       sanitizeMCPText(t.Title, secrets, paths),
					FullName:    FullToolName(mf.ID, t.Name),
					Description: sanitizeMCPText(t.Description, secrets, paths),
				})
			}
			eff := m.effectiveLocked(p)
			for _, d := range mf.Settings {
				val := eff[d.Key]
				isSet := !emptySetting(val)
				masked := isSensitiveSettingValue(d.Key, d.Type, val)
				item := MCPSettingSummary{
					Key:      d.Key,
					Label:    sanitizeMCPText(d.Label, secrets, paths),
					Type:     d.Type,
					Required: d.Required,
					Set:      isSet,
					Masked:   masked,
				}
				if !masked && isSet {
					if s, ok := val.(string); ok {
						item.Value = sanitizeMCPText(s, secrets, paths)
					} else {
						item.Value = val
					}
				}
				info.Settings = append(info.Settings, item)
			}
		}
		out.Plugins = append(out.Plugins, info)
	}
	return out, found
}

func (m *Manager) collectSensitiveStringsLocked() []string {
	seen := map[string]bool{}
	var out []string
	addRaw := func(s string) {
		s = strings.TrimSpace(s)
		if len(s) < 2 || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	add := func(v interface{}) {
		s, ok := v.(string)
		if !ok {
			return
		}
		addRaw(s)
		if strings.Contains(s, "://") {
			if u, err := url.Parse(strings.TrimSpace(s)); err == nil {
				if u.User != nil {
					if pw, ok := u.User.Password(); ok {
						addRaw(pw)
					}
					if un := u.User.Username(); un != "" {
						addRaw(un)
					}
				}
				for _, vals := range u.Query() {
					for _, qv := range vals {
						addRaw(qv)
					}
				}
			}
		}
	}
	for _, p := range m.plugins {
		if p.manifest == nil {
			continue
		}
		eff := m.effectiveLocked(p)
		for _, d := range p.manifest.Settings {
			if isSensitiveSettingValue(d.Key, d.Type, eff[d.Key]) {
				add(eff[d.Key])
			}
		}
	}
	for _, st := range m.state {
		if st == nil {
			continue
		}
		for k, v := range st.Settings {
			if isSensitiveSettingValue(k, "", v) {
				add(v)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

type pathReplacement struct {
	from string
	to   string
}

func (m *Manager) collectPathReplacementsLocked() []pathReplacement {
	var out []pathReplacement
	add := func(p, label string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		out = append(out, pathReplacement{from: p, to: label})
		if cleaned := filepath.Clean(p); cleaned != "" && cleaned != p {
			out = append(out, pathReplacement{from: cleaned, to: label})
		}
	}
	for _, p := range m.plugins {
		add(p.dir, "[PLUGIN_DIR]")
		if p.manifest != nil && len(p.manifest.Command) > 0 && (filepath.IsAbs(p.manifest.Command[0]) || strings.ContainsRune(p.manifest.Command[0], filepath.Separator)) {
			add(p.manifest.Command[0], "[PLUGIN_CMD]")
		}
	}
	add(m.dir, "[PLUGINS_DIR]")
	add(m.dataDir, "[PLUGIN_DATA_DIR]")
	add(m.statePath, "[PLUGIN_STATE]")
	sort.Slice(out, func(i, j int) bool { return len(out[i].from) > len(out[j].from) })
	return out
}

func summarizeMCPRun(r RunRecord) MCPRunRecord {
	summary := "执行成功"
	if !r.OK {
		summary = "执行失败（请查看网页插件日志获取详情）"
	}
	return MCPRunRecord{
		Time:     r.Time,
		Plugin:   r.Plugin,
		Hook:     r.Hook,
		Tool:     r.Tool,
		Test:     r.Test,
		OK:       r.OK,
		Ms:       r.Ms,
		HasError: !r.OK || strings.TrimSpace(r.Error) != "" || strings.TrimSpace(r.Stderr) != "",
		Summary:  summary,
	}
}

func sanitizeMCPText(s string, secrets []string, paths []pathReplacement) string {
	if s == "" {
		return ""
	}
	for _, sec := range secrets {
		s = strings.ReplaceAll(s, sec, "[REDACTED]")
	}
	for _, pr := range paths {
		s = strings.ReplaceAll(s, pr.from, pr.to)
	}
	s = mcpURLUserinfoRe.ReplaceAllString(s, "${1}[REDACTED]@${3}")
	s = mcpURLQueryRe.ReplaceAllString(s, "${1}?[REDACTED]")
	s = mcpAuthTokenRe.ReplaceAllString(s, "${1} [REDACTED]")
	s = mcpKVSecretRe.ReplaceAllString(s, "${1}=[REDACTED]")
	return s
}
