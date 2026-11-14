// Package plugin 实现 netPing 的插件系统。
//
// 插件是数据目录 plugins/<id>/ 下的一个文件夹：plugin.yaml 清单 + 任意语言写的可执行程序或脚本。
// netPing 在需要时启动插件进程，把一个 JSON 请求写入它的标准输入，从标准输出读回一个 JSON 响应
// （一次调用一个进程，不常驻）。这样插件与主程序完全隔离，单个 exe 的发布方式不变，
// 也不依赖 Go 的 plugin 包（它不支持 Windows）。协议与开发方法见 docs/plugin-development.md。
package plugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// APIVersion 是当前插件协议版本，写在请求的 api_version 中；清单的 api_version 不能高于它。
const APIVersion = 1

// ManifestFile 是插件目录中的清单文件名。
const ManifestFile = "plugin.yaml"

// 钩子类型。
const (
	HookNotify    = "notify"    // 通知渠道：收到 netPing 准备推送的消息
	HookAlert     = "alert"     // 结构化告警事件（离线、恢复、丢包、延迟高…）
	HookDiscovery = "discovery" // 网段扫描发现的变化（新设备、IP 变化…）
	HookLocate    = "locate"    // 端口定位的变化（设备挂到端口、端口变化待确认）
	HookTool      = "tool"      // 工具调用（清单 tools 中声明，不写在 hooks 里）
)

// Hooks 是可以在清单 hooks 中声明的钩子。
var Hooks = []string{HookNotify, HookAlert, HookDiscovery, HookLocate}

// 设置项类型。
const (
	SettingString = "string"
	SettingSecret = "secret" // 网页上不回显
	SettingText   = "text"   // 多行文本
	SettingBool   = "bool"
	SettingNumber = "number"
	SettingSelect = "select"
)

const (
	defaultTimeoutSec = 15
	maxTimeoutSec     = 120
	maxTools          = 32
	maxSettings       = 32
)

var (
	idRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,23}$`)
	toolRe    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,23}$`)
	settingRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

// Manifest 对应 plugin.yaml。
type Manifest struct {
	ID          string       `yaml:"id" json:"id"`
	Name        string       `yaml:"name" json:"name"`
	Version     string       `yaml:"version" json:"version"`
	Description string       `yaml:"description" json:"description"`
	Author      string       `yaml:"author" json:"author"`
	Homepage    string       `yaml:"homepage" json:"homepage"`
	APIVersion  int          `yaml:"api_version" json:"api_version"`
	Command     []string     `yaml:"command" json:"command"`
	TimeoutSec  int          `yaml:"timeout_sec" json:"timeout_sec"`
	Hooks       []string     `yaml:"hooks" json:"hooks"`
	Tools       []ToolDef    `yaml:"tools" json:"tools"`
	Settings    []SettingDef `yaml:"settings" json:"settings"`
}

// ToolDef 是插件提供给 MCP 客户端 / AI 诊断的一个工具。
type ToolDef struct {
	Name        string                 `yaml:"name" json:"name"`
	Title       string                 `yaml:"title" json:"title"`
	Description string                 `yaml:"description" json:"description"`
	Write       bool                   `yaml:"write" json:"write"` // 会修改外部系统或数据：MCP 只读模式下隐藏，AI 诊断永不调用
	AI          bool                   `yaml:"ai" json:"ai"`       // 允许网页「AI 诊断」调用（仅只读工具有效）
	InputSchema map[string]interface{} `yaml:"input_schema" json:"input_schema"`
}

// SettingDef 是插件的一个设置项，在网页「插件」页填写，调用时随请求传给插件。
type SettingDef struct {
	Key      string      `yaml:"key" json:"key"`
	Label    string      `yaml:"label" json:"label"`
	Type     string      `yaml:"type" json:"type"`
	Help     string      `yaml:"help" json:"help,omitempty"`
	Default  interface{} `yaml:"default" json:"default,omitempty"`
	Required bool        `yaml:"required" json:"required,omitempty"`
	Options  []string    `yaml:"options" json:"options,omitempty"`
}

// HasHook 清单是否声明了某个钩子。
func (m *Manifest) HasHook(h string) bool {
	for _, x := range m.Hooks {
		if x == h {
			return true
		}
	}
	return false
}

// FullToolName 是工具在 MCP / AI 中的名字：plugin_<插件 id>_<工具名>。
// 插件 id 不含下划线，因此不会与其他插件的工具重名。
func FullToolName(pluginID, tool string) string { return "plugin_" + pluginID + "_" + tool }

// LoadManifest 读取并校验插件目录中的 plugin.yaml。目录名必须与清单 id 相同。
func LoadManifest(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("缺少 " + ManifestFile)
		}
		return nil, err
	}
	if len(data) > 256<<10 {
		return nil, errors.New(ManifestFile + " 超过 256 KB")
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s 不是有效的 YAML: %v", ManifestFile, err)
	}
	if err := m.normalize(); err != nil {
		return nil, err
	}
	if base := filepath.Base(dir); m.ID != base {
		return nil, fmt.Errorf("清单 id %q 必须与目录名 %q 相同", m.ID, base)
	}
	return &m, nil
}

func (m *Manifest) normalize() error {
	m.ID = strings.TrimSpace(m.ID)
	m.Name = strings.TrimSpace(m.Name)
	m.Version = strings.TrimSpace(m.Version)
	if !idRe.MatchString(m.ID) {
		return fmt.Errorf("id %q 无效：只能用小写字母、数字和连字符，2~24 个字符，以字母或数字开头", m.ID)
	}
	if m.Name == "" {
		m.Name = m.ID
	}
	if m.Version == "" {
		m.Version = "0.0.0"
	}
	if m.APIVersion == 0 {
		m.APIVersion = 1
	}
	if m.APIVersion < 1 || m.APIVersion > APIVersion {
		return fmt.Errorf("api_version %d 不受支持（当前 netPing 支持到 %d，请升级 netPing）", m.APIVersion, APIVersion)
	}
	if len(m.Command) == 0 || strings.TrimSpace(m.Command[0]) == "" {
		return errors.New("command 不能为空，如 [\"python\", \"main.py\"] 或 [\"./myplugin.exe\"]")
	}
	if m.TimeoutSec <= 0 {
		m.TimeoutSec = defaultTimeoutSec
	}
	if m.TimeoutSec > maxTimeoutSec {
		return fmt.Errorf("timeout_sec 最大 %d", maxTimeoutSec)
	}
	seen := map[string]bool{}
	hooks := []string{}
	for _, h := range m.Hooks {
		h = strings.TrimSpace(h)
		ok := false
		for _, k := range Hooks {
			ok = ok || h == k
		}
		if !ok {
			return fmt.Errorf("未知钩子 %q（可用：%s）", h, strings.Join(Hooks, "、"))
		}
		if !seen[h] {
			seen[h] = true
			hooks = append(hooks, h)
		}
	}
	m.Hooks = hooks
	if len(m.Tools) > maxTools {
		return fmt.Errorf("工具最多 %d 个", maxTools)
	}
	if m.Tools == nil {
		m.Tools = []ToolDef{}
	}
	seen = map[string]bool{}
	for i := range m.Tools {
		t := &m.Tools[i]
		t.Name = strings.TrimSpace(t.Name)
		if !toolRe.MatchString(t.Name) {
			return fmt.Errorf("工具名 %q 无效：只能用小写字母、数字和下划线，最多 24 个字符，以字母开头", t.Name)
		}
		if seen[t.Name] {
			return fmt.Errorf("工具名 %q 重复", t.Name)
		}
		seen[t.Name] = true
		if strings.TrimSpace(t.Description) == "" {
			return fmt.Errorf("工具 %s 缺少 description（AI 靠它判断何时调用）", t.Name)
		}
		if t.Title == "" {
			t.Title = t.Name
		}
		if t.InputSchema == nil {
			t.InputSchema = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		if typ, _ := t.InputSchema["type"].(string); typ != "object" {
			return fmt.Errorf("工具 %s 的 input_schema.type 必须是 object", t.Name)
		}
		if t.Write {
			t.AI = false
		}
	}
	if len(m.Hooks) == 0 && len(m.Tools) == 0 {
		return errors.New("清单没有声明任何 hooks 或 tools")
	}
	if len(m.Settings) > maxSettings {
		return fmt.Errorf("设置项最多 %d 个", maxSettings)
	}
	if m.Settings == nil {
		m.Settings = []SettingDef{}
	}
	seen = map[string]bool{}
	for i := range m.Settings {
		d := &m.Settings[i]
		d.Key = strings.TrimSpace(d.Key)
		if !settingRe.MatchString(d.Key) {
			return fmt.Errorf("设置项 key %q 无效：只能用小写字母、数字和下划线，以字母开头", d.Key)
		}
		if seen[d.Key] {
			return fmt.Errorf("设置项 key %q 重复", d.Key)
		}
		seen[d.Key] = true
		if d.Label == "" {
			d.Label = d.Key
		}
		if d.Type == "" {
			d.Type = SettingString
		}
		switch d.Type {
		case SettingString, SettingSecret, SettingText, SettingBool, SettingNumber:
		case SettingSelect:
			if len(d.Options) == 0 {
				return fmt.Errorf("设置项 %s 为 select 类型，需要 options", d.Key)
			}
		default:
			return fmt.Errorf("设置项 %s 的类型 %q 无效（可用：string、secret、text、bool、number、select）", d.Key, d.Type)
		}
		if d.Default != nil {
			v, err := coerceSetting(*d, d.Default)
			if err != nil {
				return fmt.Errorf("设置项 %s 的 default 无效：%v", d.Key, err)
			}
			d.Default = v
		}
	}
	return nil
}

// coerceSetting 把用户提交（JSON）或清单（YAML）中的值转换成设置项类型对应的 Go 值。
func coerceSetting(d SettingDef, v interface{}) (interface{}, error) {
	switch d.Type {
	case SettingBool:
		if b, ok := v.(bool); ok {
			return b, nil
		}
		return nil, errors.New("需要 true 或 false")
	case SettingNumber:
		switch n := v.(type) {
		case float64:
			return n, nil
		case int:
			return float64(n), nil
		case int64:
			return float64(n), nil
		}
		return nil, errors.New("需要数字")
	}
	s, ok := v.(string)
	if !ok {
		return nil, errors.New("需要文本")
	}
	if d.Type != SettingText {
		s = strings.TrimSpace(s)
	}
	if len(s) > 16<<10 {
		return nil, errors.New("内容过长（最多 16 KB）")
	}
	if d.Type == SettingSelect && s != "" {
		for _, o := range d.Options {
			if o == s {
				return s, nil
			}
		}
		return nil, fmt.Errorf("只能是 %s 之一", strings.Join(d.Options, " / "))
	}
	return s, nil
}

func emptySetting(v interface{}) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}
