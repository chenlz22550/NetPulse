// Package config 负责 config.yaml（程序运行参数）的读写。
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"gopkg.in/yaml.v3"
	"netping/internal/llm"
)

// PingDefaults 是节点未单独设置时使用的默认 ping 参数。
type PingDefaults struct {
	IntervalMs    int     `yaml:"interval_ms" json:"interval_ms"`
	TimeoutMs     int     `yaml:"timeout_ms" json:"timeout_ms"`
	PacketSize    int     `yaml:"packet_size" json:"packet_size"`
	LatencyWarnMs int     `yaml:"latency_warn_ms" json:"latency_warn_ms"`
	LossWarnPct   float64 `yaml:"loss_warn_pct" json:"loss_warn_pct"`
}

// AlertConfig 是多渠道告警配置；旧 webhook_url 仍为企业微信。
type AlertConfig struct {
	DingTalkURL       string `yaml:"dingtalk_url" json:"dingtalk_url"`
	DingTalkSecret    string `yaml:"dingtalk_secret" json:"dingtalk_secret"`
	FeishuURL         string `yaml:"feishu_url" json:"feishu_url"`
	FeishuSecret      string `yaml:"feishu_secret" json:"feishu_secret"`
	SlackURL          string `yaml:"slack_url" json:"slack_url"`
	GenericWebhookURL string `yaml:"generic_webhook_url" json:"generic_webhook_url"`

	Enabled            bool   `yaml:"enabled" json:"enabled"`
	WebhookURL         string `yaml:"webhook_url" json:"webhook_url"`
	DownAfterFailures  int    `yaml:"down_after_failures" json:"down_after_failures"`
	UpAfterSuccesses   int    `yaml:"up_after_successes" json:"up_after_successes"`
	LossDurationMin    int    `yaml:"loss_duration_min" json:"loss_duration_min"`
	LatencyDurationMin int    `yaml:"latency_duration_min" json:"latency_duration_min"`
	RepeatIntervalMin  int    `yaml:"repeat_interval_min" json:"repeat_interval_min"`
	MergeWindowSec     int    `yaml:"merge_window_sec" json:"merge_window_sec"`
	SendRecovery       bool   `yaml:"send_recovery" json:"send_recovery"`
	SiteTitle          string `yaml:"site_title" json:"site_title"`
	ProxyURL           string `yaml:"proxy_url" json:"proxy_url"`
	TLSSkipVerify      bool   `yaml:"tls_skip_verify" json:"tls_skip_verify"`
}

// Auth 为管理网页的登录密码。网页上设置的密码以 PBKDF2 摘要保存在 password_hash；
// 也可以手工在 password 填明文（下次在网页上修改密码时会改存为摘要）。
// 两者都为空表示尚未设置密码，此时只能在运行 netPing 的电脑上打开网页设置。
// username 仅用于脚本/MCP 客户端的 HTTP Basic 认证，为空时 Basic 认证不校验用户名。
type Auth struct {
	Username     string `yaml:"username" json:"username"`
	Password     string `yaml:"password,omitempty" json:"password,omitempty"`
	PasswordHash string `yaml:"password_hash,omitempty" json:"password_hash,omitempty"`
}

// HasPassword 是否已设置登录密码。
func (a Auth) HasPassword() bool { return a.Password != "" || a.PasswordHash != "" }

// ScanJob 是一个自动扫描任务：按间隔扫描一个或多个网段，发现的新设备自动添加。
type ScanJob struct {
	ID          string   `yaml:"id" json:"id"`
	Name        string   `yaml:"name" json:"name"`
	Enabled     bool     `yaml:"enabled" json:"enabled"`
	Subnets     []string `yaml:"subnets" json:"subnets"` // 为空表示 topology.yaml 的 site.subnet
	IntervalMin int      `yaml:"interval_min" json:"interval_min"`
	AutoAdd     bool     `yaml:"auto_add" json:"auto_add"`
	Alert       bool     `yaml:"alert,omitempty" json:"alert"` // 自动添加的设备同时开启告警
	Tags        []string `yaml:"tags,omitempty" json:"tags"`   // 自动添加的设备附加的标签
}

// Discovery 是网段扫描与 IP 跟踪配置。
type Discovery struct {
	Enabled     bool              `yaml:"enabled" json:"enabled"` // 自动扫描总开关
	Jobs        []ScanJob         `yaml:"jobs" json:"jobs"`
	IPTracking  string            `yaml:"ip_tracking" json:"ip_tracking"`
	FastResolve bool              `yaml:"fast_resolve" json:"fast_resolve"`
	TimeoutMs   int               `yaml:"timeout_ms" json:"timeout_ms"`
	Concurrency int               `yaml:"concurrency" json:"concurrency"`
	Exclude     []string          `yaml:"exclude" json:"exclude"`
	Notify      bool              `yaml:"notify" json:"notify"`
	OUI         map[string]string `yaml:"oui" json:"oui"`

	// 1.2.0 的单网段写法，读取时自动迁移为一个扫描任务，不再写出。
	LegacySubnets  []string `yaml:"subnets,omitempty" json:"-"`
	LegacyInterval int      `yaml:"interval_min,omitempty" json:"-"`
	LegacyAutoAdd  *bool    `yaml:"auto_add,omitempty" json:"-"`
}

// DefaultJob 是首次运行时的扫描任务：扫描机房网段（site.subnet），每 30 分钟，自动添加。
func DefaultJob() ScanJob {
	return ScanJob{ID: "default", Name: "机房网段", Enabled: true, Subnets: []string{}, IntervalMin: 30, AutoAdd: true, Tags: []string{}}
}

// LocateSwitch 是单台交换机的 SNMP 读取设置（键为 topology.yaml 中的交换机 id）。
type LocateSwitch struct {
	Host      string `yaml:"host,omitempty" json:"host"`           // 留空使用交换机管理 IP
	Community string `yaml:"community,omitempty" json:"community"` // 留空使用默认团体名
	Disabled  bool   `yaml:"disabled,omitempty" json:"disabled"`
}

// Locate 是端口定位配置：通过 SNMP 只读读取交换机 MAC 地址表，自动确定设备接在哪个端口。
type Locate struct {
	Enabled     bool                    `yaml:"enabled" json:"enabled"`
	IntervalMin int                     `yaml:"interval_min" json:"interval_min"`
	Community   string                  `yaml:"community" json:"community"`
	TimeoutMs   int                     `yaml:"timeout_ms" json:"timeout_ms"`
	Notify      bool                    `yaml:"notify" json:"notify"`
	Switches    map[string]LocateSwitch `yaml:"switches" json:"switches"`
}

// MCPConfig 是 MCP（Model Context Protocol）接口配置，供 AI 客户端查询监控数据、管理节点。
type MCPConfig struct {
	Enabled  bool   `yaml:"enabled" json:"enabled"`
	ReadOnly bool   `yaml:"read_only" json:"read_only"`
	Token    string `yaml:"token" json:"token"`
}

// AIConfig 是 AI 诊断（大模型）配置，支持 OpenAI Chat Completions、OpenAI Responses、Anthropic Messages、Google Gemini 协议，默认 DeepSeek。
type AIConfig struct {
	Enabled    bool   `yaml:"enabled" json:"enabled"`
	Protocol   string `yaml:"protocol" json:"protocol"`
	BaseURL    string `yaml:"base_url" json:"base_url"`
	Model      string `yaml:"model" json:"model"`
	APIKey     string `yaml:"api_key" json:"api_key,omitempty"`
	Thinking   bool   `yaml:"thinking" json:"thinking"`
	ProxyURL   string `yaml:"proxy_url" json:"proxy_url"`
	TimeoutSec int    `yaml:"timeout_sec" json:"timeout_sec"`
	SelfCheck  *bool  `yaml:"self_check,omitempty" json:"self_check,omitempty"` // 回答前自检，未设置时默认开启
}

// SelfCheckOn 是否在给出诊断结论前自检（默认开启）。
func (a AIConfig) SelfCheckOn() bool { return a.SelfCheck == nil || *a.SelfCheck }

// Config 对应 config.yaml。
type Config struct {
	Listen        string       `yaml:"listen" json:"listen"`
	Auth          Auth         `yaml:"auth" json:"auth"`
	TopologyFile  string       `yaml:"topology_file" json:"topology_file"`
	RetentionDays int          `yaml:"retention_days" json:"retention_days"`
	Schedule      Schedule     `yaml:"schedule" json:"schedule"`
	MCP           MCPConfig    `yaml:"mcp" json:"mcp"`
	Discovery     Discovery    `yaml:"discovery" json:"discovery"`
	Locate        Locate       `yaml:"locate" json:"locate"`
	Ping          PingDefaults `yaml:"ping" json:"ping"`
	Alert         AlertConfig  `yaml:"alert" json:"alert"`
	AI            AIConfig     `yaml:"ai" json:"ai"`
}

// Default 返回默认配置。
func Default() Config {
	return Config{
		Listen:        "0.0.0.0:8090",
		AI:            AIConfig{Enabled: true, Protocol: llm.ProtocolOpenAI, BaseURL: "https://api.deepseek.com", Model: "deepseek-flash", TimeoutSec: 120},
		TopologyFile:  "topology.yaml",
		RetentionDays: 30,
		MCP:           MCPConfig{Enabled: true},
		Locate:        Locate{Enabled: true, IntervalMin: 15, TimeoutMs: 2000, Switches: map[string]LocateSwitch{}},
		Discovery: Discovery{
			Enabled: true, IPTracking: "all", FastResolve: true,
			TimeoutMs: 1000, Concurrency: 64, Exclude: []string{}, OUI: map[string]string{},
		},
		Ping: PingDefaults{
			IntervalMs:    1000,
			TimeoutMs:     1000,
			PacketSize:    32,
			LatencyWarnMs: 100,
			LossWarnPct:   20,
		},
		Alert: AlertConfig{
			Enabled:            false,
			DownAfterFailures:  5,
			UpAfterSuccesses:   3,
			LossDurationMin:    2,
			LatencyDurationMin: 3,
			RepeatIntervalMin:  30,
			MergeWindowSec:     10,
			SendRecovery:       true,
			SiteTitle:          "机房网络监控",
		},
	}
}

// Normalize 把非法或缺省值修正为默认值。
func (c *Config) Normalize() {
	d := Default()
	if c.Listen == "" {
		c.Listen = d.Listen
	}
	if c.TopologyFile == "" {
		c.TopologyFile = d.TopologyFile
	}
	c.AI.Protocol = llm.NormalizeProtocol(c.AI.Protocol)
	c.AI.BaseURL = strings.TrimRight(strings.TrimSpace(c.AI.BaseURL), "/")
	if c.AI.BaseURL == "" {
		c.AI.BaseURL = d.AI.BaseURL
	}
	c.AI.Model = strings.TrimSpace(c.AI.Model)
	if c.AI.Model == "" {
		c.AI.Model = d.AI.Model
	}
	c.AI.APIKey = strings.TrimSpace(c.AI.APIKey)
	if c.AI.TimeoutSec < 10 || c.AI.TimeoutSec > 600 {
		c.AI.TimeoutSec = d.AI.TimeoutSec
	}
	if c.RetentionDays <= 0 {
		c.RetentionDays = d.RetentionDays
	}
	if c.Ping.IntervalMs < 200 {
		c.Ping.IntervalMs = d.Ping.IntervalMs
	}
	if c.Ping.TimeoutMs < 100 {
		c.Ping.TimeoutMs = d.Ping.TimeoutMs
	}
	if c.Ping.PacketSize <= 0 || c.Ping.PacketSize > 65500 {
		c.Ping.PacketSize = d.Ping.PacketSize
	}
	if c.Ping.LatencyWarnMs <= 0 {
		c.Ping.LatencyWarnMs = d.Ping.LatencyWarnMs
	}
	if c.Ping.LossWarnPct <= 0 || c.Ping.LossWarnPct > 100 {
		c.Ping.LossWarnPct = d.Ping.LossWarnPct
	}
	if c.Schedule.Windows == nil {
		c.Schedule.Windows = []Window{}
	}
	for i := range c.Schedule.Windows {
		w := &c.Schedule.Windows[i]
		w.Start, w.End = strings.TrimSpace(w.Start), strings.TrimSpace(w.End)
	}
	a := &c.Alert
	if a.DownAfterFailures <= 0 {
		a.DownAfterFailures = d.Alert.DownAfterFailures
	}
	if a.UpAfterSuccesses <= 0 {
		a.UpAfterSuccesses = d.Alert.UpAfterSuccesses
	}
	if a.LossDurationMin <= 0 {
		a.LossDurationMin = d.Alert.LossDurationMin
	}
	if a.LatencyDurationMin <= 0 {
		a.LatencyDurationMin = d.Alert.LatencyDurationMin
	}
	if a.RepeatIntervalMin < 0 {
		a.RepeatIntervalMin = 0
	}
	if a.MergeWindowSec <= 0 {
		a.MergeWindowSec = d.Alert.MergeWindowSec
	}
	if a.SiteTitle == "" {
		a.SiteTitle = d.Alert.SiteTitle
	}
	a.WebhookURL = strings.TrimSpace(a.WebhookURL)
	a.DingTalkURL = strings.TrimSpace(a.DingTalkURL)
	a.DingTalkSecret = strings.TrimSpace(a.DingTalkSecret)
	a.FeishuURL = strings.TrimSpace(a.FeishuURL)
	a.FeishuSecret = strings.TrimSpace(a.FeishuSecret)
	a.SlackURL = strings.TrimSpace(a.SlackURL)
	a.GenericWebhookURL = strings.TrimSpace(a.GenericWebhookURL)

	c.MCP.Token = strings.TrimSpace(c.MCP.Token)
	dd := &c.Discovery
	if dd.Jobs == nil { // 旧版配置或首次运行
		j := DefaultJob()
		if len(dd.LegacySubnets) > 0 {
			j.Subnets = dd.LegacySubnets
		}
		if dd.LegacyInterval > 0 {
			j.IntervalMin = dd.LegacyInterval
		}
		if dd.LegacyAutoAdd != nil {
			j.AutoAdd = *dd.LegacyAutoAdd
		}
		dd.Jobs = []ScanJob{j}
	}
	dd.LegacySubnets, dd.LegacyInterval, dd.LegacyAutoAdd = nil, 0, nil
	usedID := map[string]bool{}
	for i := range dd.Jobs {
		j := &dd.Jobs[i]
		j.ID = strings.TrimSpace(j.ID)
		j.Name = strings.TrimSpace(j.Name)
		j.Subnets = cleanList(j.Subnets)
		j.Tags = cleanList(j.Tags)
		if j.IntervalMin <= 0 {
			j.IntervalMin = 30
		}
		if j.ID == "" {
			for n := i + 1; ; n++ {
				if id := fmt.Sprintf("job-%d", n); !usedID[id] {
					j.ID = id
					break
				}
			}
		}
		usedID[j.ID] = true
		if j.Name == "" {
			if len(j.Subnets) > 0 {
				j.Name = strings.Join(j.Subnets, "、")
			} else {
				j.Name = "机房网段"
			}
		}
	}
	if dd.TimeoutMs < 100 {
		dd.TimeoutMs = d.Discovery.TimeoutMs
	}
	if dd.Concurrency <= 0 || dd.Concurrency > 256 {
		dd.Concurrency = d.Discovery.Concurrency
	}
	if dd.IPTracking == "" {
		dd.IPTracking = "all"
	}
	dd.Exclude = cleanList(dd.Exclude)
	lc := &c.Locate
	if lc.IntervalMin <= 0 {
		lc.IntervalMin = d.Locate.IntervalMin
	}
	if lc.TimeoutMs < 200 {
		lc.TimeoutMs = d.Locate.TimeoutMs
	}
	lc.Community = strings.TrimSpace(lc.Community)
	if lc.Switches == nil {
		lc.Switches = map[string]LocateSwitch{}
	}
	for k, v := range lc.Switches {
		v.Host, v.Community = strings.TrimSpace(v.Host), strings.TrimSpace(v.Community)
		if v == (LocateSwitch{}) {
			delete(lc.Switches, k)
			continue
		}
		lc.Switches[k] = v
	}
	if dd.OUI == nil {
		dd.OUI = map[string]string{}
	}
	a.ProxyURL = strings.TrimSpace(a.ProxyURL)
}

// Validate 检查用户提交的配置。
func (c *Config) Validate() error {
	if err := c.Alert.ValidateChannels(); err != nil {
		return err
	}
	if err := c.Schedule.Validate(); err != nil {
		return err
	}
	if err := c.Discovery.Validate(); err != nil {
		return err
	}
	if c.Locate.IntervalMin < 2 {
		return errors.New("端口定位间隔不能小于 2 分钟")
	}
	if c.Alert.ProxyURL != "" {
		u, err := url.Parse(c.Alert.ProxyURL)
		if err != nil || u.Host == "" {
			return errors.New("proxy_url 不是有效的代理地址")
		}
	}
	if c.AI.ProxyURL != "" {
		u, err := url.Parse(c.AI.ProxyURL)
		if err != nil || u.Host == "" {
			return errors.New("ai.proxy_url 不是有效的代理地址")
		}
	}
	if u, err := url.Parse(c.AI.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("ai.base_url 不是有效的接口地址")
	}
	return nil
}

const header = `# netPing 程序配置（修改 listen / topology_file 后需重启程序生效）
#
# listen                 管理网页监听地址，0.0.0.0:8090 表示允许局域网访问，127.0.0.1:8090 仅本机
# auth                   管理网页登录密码（登录后浏览器 30 天内免登录）：
#   password_hash        网页上设置的密码摘要；忘记密码时删掉 password_hash 和 password 两行并重启，
#                        再在运行 netPing 的电脑上打开网页重新设置
#   password             也可以直接填明文密码（下次在网页上修改密码时改存为摘要）
#   username             仅供脚本 / MCP 客户端用 HTTP Basic 认证时校验，留空则不校验用户名
# topology_file          节点/交换机拓扑文件（相对程序目录）
# retention_days         历史数据（分钟聚合）保留天数
# schedule               全局监控时段：
#   enabled              false=全天候监控；true=只在下列任一时段内 ping 和告警，其余时间暂停
#   windows[].days       星期几生效，1=周一 … 7=周日，省略表示每天
#   windows[].start/end  "HH:MM"，end 小于等于 start 表示跨过午夜，如 start "22:00" end "06:00"
# discovery.*            网段扫描与 IP 跟踪（网页「扫描发现」页）：
#   enabled              自动扫描总开关（关闭后各任务不再定时运行，仍可手动扫描）
#   jobs[]               自动扫描任务，每个任务按自己的间隔扫描一个或多个网段：
#     id / name          任务标识 / 显示名
#     enabled            是否定时运行
#     subnets            网段列表（CIDR，每个最多 /20），为空时使用 topology.yaml 中 site.subnet
#     interval_min       扫描间隔（分钟，最小 5）；处于暂停/非监控时段时不扫描
#     auto_add           扫描到拓扑中没有的设备时自动添加为“未定位”
#     alert              自动添加的设备同时开启企业微信告警
#     tags               自动添加的设备附加的标签（如网段、楼层）
#   ip_tracking          按 MAC 跟踪 IP 变化：all=除 ip_mode: static 外的节点 / dhcp=仅 ip_mode: dhcp 的节点 / off=不跟踪
#   fast_resolve         可跟踪的设备连续 ping 失败时立即重新扫描解析 IP，并暂缓其离线告警（避免 DHCP 换地址误报）
#   timeout_ms           扫描时单次 ping 超时；concurrency 并发数
#   exclude              不扫描、不自动添加的 IP 或网段
#   notify               新增设备、IP 变化时推送企业微信（需告警通道已启用）
#   oui                  额外的 MAC 前缀 → 厂商对照，如 "60:D7:E3": 深大智能（拓扑中已有设备的前缀会自动学习）
# locate.*               端口定位（网页「端口定位」页）：用 SNMP v2c 只读读取交换机 MAC 地址表，自动确定设备接在哪个端口
#   enabled              是否定时定位；“未定位”的设备自动挂到查到的端口，已定位但端口变化的只提示，确认后修改
#   interval_min         定位间隔（分钟，最小 2）
#   community            默认只读团体名（交换机上：snmp-server community <名称> ro）；为空则不读取
#   timeout_ms           SNMP 超时
#   notify               设备端口变化时推送企业微信（需告警通道已启用）
#   switches.<交换机id>  单台交换机设置：host 读取地址（留空用交换机管理 IP）、community 团体名（留空用默认）、disabled 不读取
# mcp.*                  MCP 接口（AI 客户端通过 http://<地址>:<端口>/mcp 连接，网页「MCP」页有接入说明）：
#   enabled              是否开启 MCP 接口
#   read_only            只读模式：只能查询，不能修改拓扑、告警开关、监控时段
#   token                访问令牌，非空时客户端需带请求头 Authorization: Bearer <token>；
#                        为空时客户端需用 HTTP Basic 认证提供网页登录密码
# ai.*                   AI 诊断（网页「AI 诊断」页），默认 DeepSeek：
#   enabled              是否启用
#   protocol             协议格式：openai（OpenAI Chat Completions，默认）/ openai-responses（OpenAI Responses）/
#                        anthropic（Anthropic Claude Messages）/ gemini（Google Gemini）
#   base_url / model     接口地址与模型，默认 https://api.deepseek.com、deepseek-flash
#   api_key              API Key（网页和 MCP 都不会返回它）
#   thinking             深度思考模式（更慢、更贵，复杂问题时可开启）
#   proxy_url            HTTP 代理，留空时使用 alert.proxy_url
#   timeout_sec          单次诊断最长时间（秒）
#   self_check           回答前自检（默认 true）：模型写完初稿后逐条核对依据，必要时调用工具实测，再给出通过或修正后的回答
# ping.*                 节点未单独设置时的默认值：
#   interval_ms          ping 间隔（毫秒，最小 200）
#   timeout_ms           单次超时（毫秒）
#   packet_size          ICMP 数据长度（字节）
#   latency_warn_ms      1 分钟平均延迟超过该值视为“延迟高”
#   loss_warn_pct        1 分钟丢包率超过该值（%）视为“丢包”
# alert.*                多渠道告警（填写地址即参与推送，清空停用）：
#   dingtalk_url / dingtalk_secret   钉钉 Webhook / 可选加签密钥
#   feishu_url / feishu_secret       飞书 Webhook / 可选签名密钥
#   slack_url                       Slack Incoming Webhook
#   generic_webhook_url             通用 Webhook，POST JSON {title,text}，2xx 为成功
#   enabled              是否启用
#   webhook_url          机器人 webhook 地址 https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=...
#   down_after_failures  连续失败多少次判定离线
#   up_after_successes   离线后连续成功多少次判定恢复
#   loss_duration_min    丢包超阈值持续多少分钟才告警
#   latency_duration_min 延迟超阈值持续多少分钟才告警
#   repeat_interval_min  故障持续时重复提醒间隔（分钟，0 表示不重复）
#   merge_window_sec     多少秒内的告警合并成一条消息
#   send_recovery        是否发送恢复通知
#   site_title           消息标题前缀（区分不同机房）
#                        注意：每台设备默认不告警，需在拓扑文件中设置 monitor.alert: true 或在网页上开启
#   proxy_url            访问企业微信使用的 HTTP 代理（可空），如 http://10.0.0.1:3128
#   tls_skip_verify      跳过 HTTPS 证书校验（仅在老系统根证书过旧导致发送失败时临时使用）

`

// Manager 提供线程安全的配置访问和持久化。
type Manager struct {
	mu   sync.RWMutex
	path string
	cfg  Config
}

// Load 读取配置文件；文件不存在时写出默认配置。
func Load(path string) (*Manager, error) {
	m := &Manager{path: path, cfg: Default()}
	m.cfg.Normalize()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return m, m.save(m.cfg)
	}
	if err != nil {
		return nil, err
	}
	cfg := Default()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	cfg.Normalize()
	m.cfg = cfg
	return m, nil
}

// Get 返回当前配置副本。
func (m *Manager) Get() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg.Clone()
}

// Clone 深拷贝切片与映射，调用方修改副本（如 JSON 解码进去）不会影响当前配置。
func (c Config) Clone() Config {
	c.Schedule.Windows = append([]Window(nil), c.Schedule.Windows...)
	for i := range c.Schedule.Windows {
		c.Schedule.Windows[i].Days = append([]int(nil), c.Schedule.Windows[i].Days...)
	}
	d := &c.Discovery
	if d.Jobs != nil {
		jobs := make([]ScanJob, len(d.Jobs))
		for i, j := range d.Jobs {
			j.Subnets = append([]string{}, j.Subnets...)
			j.Tags = append([]string{}, j.Tags...)
			jobs[i] = j
		}
		d.Jobs = jobs
	}
	d.Exclude = append([]string{}, d.Exclude...)
	oui := make(map[string]string, len(d.OUI))
	for k, v := range d.OUI {
		oui[k] = v
	}
	d.OUI = oui
	sw := make(map[string]LocateSwitch, len(c.Locate.Switches))
	for k, v := range c.Locate.Switches {
		sw[k] = v
	}
	c.Locate.Switches = sw
	return c
}

// Update 校验并保存新配置。
func (m *Manager) Update(c Config) error {
	c = c.Clone()
	c.Normalize()
	if err := c.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// 保存与发布配置属于同一个事务；失败时保持原有内存配置。
	if err := m.save(c); err != nil {
		return err
	}
	m.cfg = c
	return nil
}

// save 的调用方须持有写锁，或尚未将 Manager 发布给其它协程。
func (m *Manager) save(cfg Config) error {
	var buf bytes.Buffer
	buf.WriteString(header)
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return err
	}
	_ = enc.Close()
	return WriteFileAtomic(m.path, buf.Bytes())
}

// WriteFileAtomic 先写临时文件再改名，避免写一半时断电导致文件损坏。
func WriteFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	// Windows 上 os.Rename 会覆盖已存在的目标文件（MoveFileEx + REPLACE_EXISTING）。
	return os.Rename(tmp, path)
}

func cleanList(in []string) []string {
	out := []string{}
	for _, x := range in {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// extraChannels 由插件系统注册：是否有已启用的通知类插件。
var extraChannels atomic.Value // func() bool

// SetExtraChannels 注册“是否存在内置渠道以外的通知渠道”（通知类插件）的判断函数。
// 注册后，只启用了通知插件、没有填写任何内置渠道地址时也允许开启告警。
func SetExtraChannels(fn func() bool) { extraChannels.Store(fn) }

// HasExtraChannels 是否有内置渠道以外的通知渠道（通知类插件）。
func HasExtraChannels() bool {
	fn, _ := extraChannels.Load().(func() bool)
	return fn != nil && fn()
}

// HasChannels indicates whether at least one notification destination is configured.
func (a AlertConfig) HasChannels() bool {
	return a.WebhookURL != "" || a.DingTalkURL != "" || a.FeishuURL != "" || a.SlackURL != "" || a.GenericWebhookURL != ""
}
func (a AlertConfig) ValidateChannels() error {
	for name, raw := range map[string]string{"webhook_url": a.WebhookURL, "dingtalk_url": a.DingTalkURL, "feishu_url": a.FeishuURL, "slack_url": a.SlackURL, "generic_webhook_url": a.GenericWebhookURL} {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return fmt.Errorf("%s 不是有效的 http(s) 地址", name)
		}
	}
	if a.Enabled && !a.HasChannels() && !HasExtraChannels() {
		return errors.New("启用告警前需要填写至少一个通知渠道的 webhook 地址，或启用一个通知类插件")
	}
	return nil
}
