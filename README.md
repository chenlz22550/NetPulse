# netPing — 机房网络 Ping 监控

常驻 Windows 系统托盘的小工具：持续 ping 机房里的交换机和终端，统计延迟、抖动和丢包，
用可拖动的拓扑图管理节点和接线位置，异常时推送到企业微信群机器人。

- 支持 **Windows Server 2008 R2 / Windows 7** 及以上（用 Go 1.20.14 编译，单个 exe，无需安装运行库）
- 使用系统自带的 `IcmpSendEcho` 发包，**不需要管理员权限**
- 节点数据保存在带字段说明的 `topology.yaml`，方便迁移机房时交给 AI 批量生成
- 历史数据按分钟聚合保存 30 天（可配置），不依赖数据库

## 使用

1. 把 `netPing.exe` 放到一个固定目录（如 `D:\netPing\`）并双击运行。首次运行会在同目录生成：
   - `config.yaml` 程序配置（监听端口、默认 ping 参数、告警）
   - `topology.yaml` 节点拓扑（内置当前机房 VLAN 1 的全部节点）
   - `data/` 历史数据与告警记录，`logs/` 日志
2. 右下角托盘出现图标（绿=正常，橙=丢包/延迟高，红=有离线，灰=暂停），浏览器会自动打开管理页面。
   之后可从托盘菜单「打开管理页面」进入，默认地址 `http://127.0.0.1:8090/`。
3. 托盘菜单还可以暂停/恢复监控、设置「登录后自动启动」、打开数据目录、退出。
4. 管理页面需要密码登录，登录后浏览器 30 天内免登录。第一次打开时在**运行 netPing 的电脑上**设置密码
   （其它电脑上打开会提示先到这台电脑设置）；之后可在「设置 → 登录密码」修改或退出登录。
   忘记密码：删除 `config.yaml` 中 `auth` 下的 `password_hash` / `password` 两行并重启，再在本机打开网页重新设置。

> 管理页面需要现代浏览器（Chrome / Edge）。服务器上可安装 Chrome 109（Server 2008 R2 能装的最后版本），
> 或在其它电脑上访问 `http://服务器IP:8090/`（默认监听所有网卡）。

## 管理页面

| 页面 | 功能 |
|---|---|
| 拓扑 | 交换机、端口、终端卡片实时显示延迟和丢包；拖动卡片或交换机标题调整位置；点击编辑节点/交换机/链路；新增交换机、节点、链路；检索、按厂商和类别筛选 |
| 监控列表 | 所有地址的当前延迟、1 分钟平均、抖动、1m/5m/1h 丢包率、最近 60 次迷你曲线，可排序筛选；逐个或批量开关告警；设备上锁保护；预览确认后一键清理无 IP 或关闭监控的终端，保留已上锁设备、交换机和历史数据 |
| 节点详情 | 延迟曲线（平均值 + 最小~最大区间 + 阈值线）和丢包柱状图，范围 15 分钟 ~ 30 天 |
| 扫描发现 | 立即/定时扫描网段，自动添加新设备（未定位）、按 MAC 跟踪 IP 变化、待确认建议、未登记设备、在线地址表、变更记录、扫描设置 |
| 端口定位 | SNMP 只读读取交换机 MAC 地址表，自动确定设备接在哪个端口；交换机读取设置与测试、端口变化待确认、MAC 表、内置 Telnet / SSH 终端与 SFTP 文件上传 |
| 告警 | 告警历史及发送结果 |
| 设置 | 企业微信机器人配置与测试发送、监控时段、默认 ping 参数、机房信息、在线编辑/导入/下载 topology.yaml、从磁盘重新加载 |
| AI 诊断 | 用大模型（默认 DeepSeek deepseek-flash）结合实时数据与历史曲线诊断网络问题：一键诊断、快捷问题、追问，设备详情可直接诊断单台设备 |
| MCP | AI 客户端接入：开关与只读模式、访问令牌、各客户端配置片段、连接测试、工具清单和示例问法 |
| 教程 | 使用指南：快速上手、看懂拓扑图、告警、扫描发现、端口定位、MCP、登录与安全、常见问题 |

## 企业微信告警

在企业微信群中「添加群机器人」，复制 Webhook 地址填到「设置 → 企业微信机器人告警」，勾选启用，点「发送测试消息」确认。

**设备默认只监控、不告警。** 需要告警的设备可以：在「监控列表」的「告警」列逐个勾选，或筛选后点「筛选结果开启告警」批量开启；
也可以在拓扑页的节点/交换机表单里勾选「推送企业微信告警」，或在 `topology.yaml` 中设置 `monitor.alert: true`。
已开启告警的设备在拓扑图上带橙色「告警」标签。

| 规则 | 默认 |
|---|---|
| 离线 | 连续 5 次无响应；恢复需连续 3 次成功，恢复时附离线时长 |
| 丢包 | 1 分钟丢包率 ≥ 节点阈值（默认 20%）持续 2 分钟 |
| 延迟高 | 1 分钟平均延迟 ≥ 节点阈值（默认 100ms）持续 3 分钟 |
| 重复提醒 | 故障持续时每 30 分钟提醒一次 |
| 合并 | 10 秒内的事件合并为一条消息；同一交换机下多台同时离线合并为「交换机故障」 |

服务器不能直接访问外网时可填写 HTTP 代理；老系统根证书过旧导致 HTTPS 失败时，可临时勾选「跳过证书校验」。

## AI 诊断

「AI 诊断」页（`config.yaml` 的 `ai` 段）支持 OpenAI Chat Completions、OpenAI Responses、Anthropic Claude Messages、Google Gemini 四种常见大模型协议（`ai.protocol`），默认 DeepSeek `deepseek-flash`（`https://api.deepseek.com`）：

1. 在页面右侧填写 API Key，点「测试连接」。服务器不能直接上网时填写 HTTP 代理（留空使用告警设置里的代理）。
2. 点「一键诊断」等快捷按钮或直接描述现象；在设备详情点「AI 诊断」可诊断单台设备。
3. 模型先看当前概况，再按需调用只读工具（get_overview、list_devices、get_device、get_history、list_alerts、get_settings、
   get_discovery、get_locate_status、get_switch_mac_table、locate_device）补充数据，流式给出结论、依据和排查步骤。**不能修改任何配置。**
   另有一组分析工具，让模型不用逐台翻数据就能做关联分析：

   | 工具 | 作用 |
   |---|---|
   | analyze_impact | 把当前异常设备按交换机（含下游交换机）和端口归类，找出最靠近核心的公共故障点，并把同一时刻开始异常的设备分组 |
   | get_uplink_path | 设备到核心交换机的逐跳路径：每跳交换机状态、下挂异常数、上联端口，以及同端口下的其他设备 |
   | find_outages | 历史中每段离线/丢包的起止时间、时长、丢包率，可用率与累计中断时长，按小时分布（发现固定时段掉线） |
   | correlate_events | 按分钟对齐多台设备的异常，找出多台同时出问题的时段（按交换机归类）及每台设备与他人同时异常的比例 |
   | rank_devices | 按 1h/6h/24h/7d 统计丢包率、延迟、中断分钟数、抖动排行，附全网中位数 |
   | compare_baseline | 与昨天同时段、7 天前同时段、过去 7 天整体对比延迟和丢包的变化幅度 |
   | ping_now | 从 netPing 主机立即 ping 设备或任意 IPv4 地址（最多 10 次），确认当前是否真的不通或测试外网出口 |

   还有一组现场核实工具，从 netPing 主机实时探测，让模型用证据验证判断而不是只凭历史数据推测：

   | 工具 | 作用 |
   |---|---|
   | get_switch_ports | 通过 SNMP（使用端口定位的团体名）读交换机端口的 up/down、协商速率、双工、错误/丢弃/CRC 计数、最近状态变化和交换机运行时长；间隔几秒采样两次，看错误是否仍在增长和带宽占用。可直接给设备，自动定位到它接的端口 |
   | tcp_check | 探测设备的常用 TCP 端口（开放/拒绝/超时），ping 不通时区分“真离线”和“禁 ping”，也可确认打印机、摄像头、远程桌面等服务 |
   | arp_lookup | 查 netPing 主机 ARP 表中该 IP 的 MAC，与登记的 MAC 对比，发现 IP 冲突、设备被更换或二层在线但禁 ping |
   | trace_route | 逐跳路由追踪（Windows 用系统 ICMP 接口，无需管理员权限），看外网或跨网段不通断在哪一跳 |
   | dns_lookup | 解析域名或反向解析 IP，可指定 DNS 服务器对比，判断“上不了网”是否是 DNS 问题 |
4. **回答前自检**（默认开启，可在页面右侧或 `ai.self_check` 关闭）：模型写完回答后，程序先把回答里提到的每台设备按拓扑和此刻的监控状态核对一遍
   （不在拓扑中、所有数据里也没出现过的地址会被标出，并列出哪些设备还没做现场核实），再让模型以审核人身份逐条核对事实和证据，
   缺证据时调用上面的工具实测（最多 8 次）。核对通过时回答下方显示“✓ 已自检”；发现问题时直接替换为修正后的回答，可展开查看修正前的初稿。
   自检出错或超时会保留原回答。自检会多一次模型调用（需要实测时更多）。
5. 默认关闭深度思考以加快速度；复杂问题可勾选「深度思考」。每次诊断记录在 `logs/netping.log`（问题摘要、查询次数、token 用量）。

诊断时会把相关设备的 IP、MAC、名称和延迟丢包数据发送给模型服务商。API Key 保存在 `config.yaml`，网页和 MCP 都不会返回它。

### 拓扑页「AI 一键优化」

拓扑页左上角的「✦ AI 一键优化」会把全部设备的厂商、主机名、接线、告警设置和近 7 天的丢包/延迟/中断统计交给模型，生成三类结果：

1. **设备分类**：服务器、台式机、笔记本、手机平板、打印机、摄像头、录像机、无线AP、网络存储、IP电话、门禁考勤等，写成同名标签（每台设备只保留一个类别标签），之后可在拓扑页按类别筛选。
2. **配置建议**：开/关告警、标记关键设备、补充名称、放宽告警阈值、调整 ping 间隔、停止监控，以及需要人工处理的建议。
3. **建议检查的设备**：按优先级列出，附依据数据与检查步骤，可一键在图中定位、看曲线或进入 AI 诊断。

所有修改都需要勾选后点「应用所选」才会生效，应用后可在提示中撤销；服务器会再次校验每条建议（设备不存在、参数越界、与现状相同的会被跳过）。

## MCP 接口（AI 接入）

netPing 内置 MCP（Model Context Protocol）服务端，地址为 `http://<服务器IP>:8090/mcp`（Streamable HTTP），
可接入 Claude Code、Cursor、WorkBuddy、VS Code、Claude Desktop（经 mcp-remote）、Cherry Studio 等支持 MCP 的 AI 客户端，
用自然语言查询延迟丢包、分析历史、维护节点与告警。管理页面「MCP」页有各客户端的配置片段、连接测试、工具清单和示例问法。
AI 客户端连接需要访问令牌：在「MCP」页点「生成」并保存，页面上的配置片段会自动带上 `Authorization: Bearer <令牌>`。

| 类别 | 工具 |
|---|---|
| 查询 | get_overview、list_devices、get_device、get_history、list_alerts、get_topology、get_settings、get_discovery |
| 分析 | analyze_impact、get_uplink_path、find_outages、correlate_events、rank_devices、compare_baseline、ping_now |
| 现场核实 | get_switch_ports、tcp_check、arp_lookup、trace_route、dns_lookup |
| 端口定位 | get_locate_status、get_switch_mac_table、locate_device、auto_locate（可只预览）、apply_location、test_switch_snmp、set_locate_settings |
| 扫描 | upsert_scan_job、delete_scan_job、scan_network（可临时扫描指定网段）、set_discovery_settings、add_discovered_devices、apply_ip_suggestion |
| 设置 | set_alert_config、send_test_alert、set_ping_defaults、set_site、set_schedule、set_monitoring_paused、set_ai_settings（AI 启用、协议格式、深度思考、回答前自检、模型、超时；不能改 API Key / 接口地址 / 代理） |
| 修改 | reset_layout、upsert_device、delete_device、upsert_switch、delete_switch、upsert_link、delete_link、set_alerting、replace_topology_yaml（默认只校验）、reload_topology_from_disk |

另提供资源（当前拓扑、字段说明、JSON Schema、实时状态）和提示词模板 `generate_topology`（按 MAC/ARP 表生成拓扑）、`health_report`（网络健康报告）。

`config.yaml` 中的 `mcp` 段：`enabled` 开关、`read_only` 只读模式、`token` 访问令牌（设置后客户端需带 `Authorization: Bearer <token>`）。
所有写操作记录在 `logs/netping.log`。

## 网段扫描与 IP 跟踪

「扫描发现」页（`config.yaml` 的 `discovery` 段）提供**自动扫描模式**：建立一个或多个扫描任务，每个任务指定一个或多个网段和扫描间隔，
按间隔自动扫描并自动添加新设备（可为新设备附加标签、同时开启告警）。默认有一个“机房网段”任务（扫描 `site.subnet`，每 30 分钟）。
也可以「临时扫描」任意网段而不保存为任务。总开关「自动扫描」关闭后各任务停止定时运行，仍可手动扫描。

扫描时并发 ping 每个地址，再读取系统 ARP 表（Windows `GetIpNetTable`、macOS sysctl，无需管理员权限）得到 IP ↔ MAC。

- **自动添加**：拓扑里没有的设备加为「未定位」节点（拓扑图左下虚线区，带「自动发现」标签），有 MAC 时 id 按 MAC 生成；
  关闭自动添加时列在「未登记设备」中，可逐个或一键添加。只出现在 ARP、不响应 ping 的设备添加时关闭 ping 监控（标记「仅 ARP」）。
- **按 MAC 跟踪 IP**：设备以 MAC 为身份，DHCP 换了地址时直接改该节点的 `ip`，id、历史曲线、告警开关、接线位置都保留。
  节点「IP 分配方式」：自动跟踪（默认）/ DHCP / 静态锁定；静态锁定的节点只在「待确认」中提示，点一下即可采纳。
- **快速重解析**：原本能 ping 通的可跟踪设备连续失败 2 次时立即扫描一次，并暂缓其离线告警最多 90 秒；找到新 IP 就更新，不误报离线。
- **主机名**：每次扫描对在线地址并发查询 NetBIOS（UDP 137，Windows 计算机名）、mDNS（UDP 5353，苹果 / Linux / 打印机的 .local 名）
  和反向 DNS，结果记录在节点的 `hostname` 字段（不改动手工填写的 `name`），变化记入变更记录；未填 `name` 时拓扑图显示主机名，可按主机名检索。
- **补全 MAC**：只登记了 IP 的节点，扫描时从 ARP 补上 MAC 和厂商（厂商按拓扑中已有设备的 MAC 前缀自动学习，也可在设置中补充）。
- **保护**：一个 MAC 对应大量 IP（代理 ARP）时忽略；一个 MAC 对应少量 IP（无线中继 MAC-NAT、桥接）时这些地址按 IP 分别登记；
  两台设备抢同一 IP、与交换机管理 IP 相同等情况记为「冲突」，不自动处理。可开启企业微信推送新增设备和 IP 变化。
- ARP 只能看到与服务器**同一二层网段（VLAN）**的设备；跨网段只能发现在线，无法取得 MAC、跟踪 IP。
- 处于暂停或非监控时段时不做定时扫描。

## 端口定位（SNMP 读取交换机 MAC 地址表）

「端口定位」页（`config.yaml` 的 `locate` 段）通过 **SNMP v2c 只读**读取交换机的 MAC 地址表（标准 BRIDGE-MIB / Q-BRIDGE-MIB / IF-MIB），
按设备 MAC 找到它实际插在哪台交换机的哪个端口：

- **“未定位”的设备自动挂到查到的端口**；已定位但端口变化的设备只列为「待确认」，点「采纳」（或 MCP `apply_location`）后才修改。
- **级联口判定**：`topology.yaml` 中交换机间链路的端口、聚合口、学到其他交换机 MAC 的端口不算设备位置；
  同一 MAC 出现在多台交换机的普通端口时，取学到 MAC 最少的端口（离设备最近）。端口后接傻瓜交换机/AP 时多台设备会挂在同一口。
- **物理位置**：交换机可配置「端口位置对照」（`port_locations`，如 `12: A03 机柜 配线架 2-12`），定位时自动填入设备的物理位置。
- 端口名自动对应：交换机上报的 `GigabitEthernet 0/12` 会对应到拓扑中的 `Gi0/12` 或纯数字端口 `12`。
- 默认每 15 分钟定位一次；网段扫描添加了新设备后也会立即定位一次。端口变化可推送企业微信。
- 交换机上只需开启只读团体名，如锐捷 `snmp-server community <团体名> ro`；团体名保存在 `config.yaml`，网页和 MCP 都不会把它返回给 AI。
- 没有真实交换机时可用 `go run ./tools/fakeswitch` 启动一台模拟交换机演示。

在「交换机读取设置」中点击某台交换机的 **Telnet** 可打开内置终端，直接登录并手动修改交换机配置：

- 使用已保存的读取地址，留空则使用管理 IP；Telnet 端口默认 **23**，可在终端中修改后重新连接。读取地址若带 SNMP 端口，会忽略该端口。
- 连接由 **netPing 服务器**发起，因此服务器必须能访问交换机的 Telnet 端口。需在交换机上启用 Telnet 和登录认证。
- 终端支持实时回显、方向键、Tab 补全、分页及 Ctrl+C / Ctrl+Z。按交换机提示输入账号密码，netPing 不保存登录凭据或命令记录。Telnet 在网络上明文传输。
- 手动输入的配置命令会直接作用于交换机，按设备要求执行保存命令；SNMP 自动定位仍为只读。终端不开放给 MCP 或 AI 诊断。
- 断开、关闭终端、离开页面或关闭程序都会结束连接；Telnet 15 分钟没有交换机输出时自动断开。Telnet 和 SSH 合计最多连接 8 个终端。
- 终端脚本、样式随程序打包，使用时无需访问外部 CDN。

点击 **SSH** 可使用加密终端，默认端口 **22**，支持用户名与密码认证（包括设备的密码交互认证）。连接地址与 Telnet 相同。账号密码仅用于本次连接，不写入配置或日志。首次连接会显示设备的 SHA256 主机指纹，核对后点击「信任此主机并继续」；已信任指纹保存在 `data/ssh_known_hosts.json`，后续指纹变化会拒绝连接。设备更换或密钥重置后，应先核实身份，再清理对应旧指纹记录。

SSH 登录后，在终端下方的 **SFTP 文件上传** 选择文件、填写远程目录（默认 `.`，也可填写设备支持的 `/tmp`、`flash:/` 等路径），点击「上传文件」。单个文件最大 **1 GiB**，可查看传送进度及设备确认结果，或取消上传。上传期间仍可使用终端。

- 交换机需支持并启用 **SFTP 子系统**；仅启用 SSH 命令行不一定能传文件，不支持时页面会提示。
- 默认不覆盖远端同名文件；需要覆盖时勾选「覆盖远端同名文件」。取消或失败后远端可能保留部分文件，重试前请检查。
- 文件从浏览器经 netPing 流式传到设备，不在 netPing 服务器落盘，不会自动安装、执行或应用配置。断开 SSH 时同时终止上传。
- SFTP 子系统启动超过 15 秒会停止等待；如果设备连文件通道也未响应，当前 SSH 连接会一起结束，需重新连接。

## 监控时段

「设置 → 监控时段」可以设置全局监控时间段（保存在 `config.yaml` 的 `schedule`）：

- 未启用时全天候监控；启用后只在任一时段内 ping 和告警，时段外自动暂停（顶栏显示「非监控时段」，托盘图标变灰）
- 每个时段可选星期几和开始/结束时间；结束早于开始表示跨过午夜，如周六 22:00 至 06:00
- 托盘或顶栏的「手动暂停」与监控时段相互独立

## 迁移到其他机房

参见 [docs/topology-fields.md](docs/topology-fields.md)：其中有全部字段说明和一段可直接交给 AI 的提示词，
提供交换机 MAC 表和 ARP 表即可生成新的 `topology.yaml`。JSON Schema 见 [schema/topology.schema.json](schema/topology.schema.json)。

## macOS 版本

`dist/netPing-macOS-<版本>.zip` 解压得到 `netPing.app`（通用二进制，Apple Silicon 与 Intel 均可，macOS 10.13+）：

1. 拖到「应用程序」后双击运行。首次打开如提示“无法验证开发者”，在访达中右键 →「打开」，或在「系统设置 → 隐私与安全性」中点「仍要打开」。
2. 程序只在菜单栏显示图标（不占程序坞），菜单与 Windows 版相同，「登录后自动启动」写入 `~/Library/LaunchAgents/com.netping.agent.plist`。
3. 数据目录为 `~/Library/Application Support/netPing`（config.yaml、topology.yaml、data、logs），可从菜单「打开数据目录」进入。
4. 系统询问“允许 netPing 查找本地网络上的设备”时请选择**允许**；否则 macOS 会隐藏 ARP 中的 MAC，网段扫描无法按 MAC 跟踪 IP
   （可在「系统设置 → 隐私与安全性 → 本地网络」中重新打开）。
5. 命令行运行：`netPing.app/Contents/MacOS/netPing -dir <数据目录> -notray`。

macOS 下 ping 调用系统 `ping` 命令（无需管理员权限），ARP 通过 sysctl 读取。编译：`./build_mac.sh`（需要 Xcode 命令行工具）。

## 编译

必须使用 **Go 1.20.x**（Go 1.21 起不再支持 Windows 7 / Server 2008 R2）：

```bash
go install golang.org/dl/go1.20.14@latest && go1.20.14 download
./build.sh          # macOS / Linux 交叉编译，输出 dist/netPing.exe
# 或在 Windows 上： powershell -File build.ps1
```

开发调试：`go1.20.14 run . -dir ./devdata`（`-notray` 只运行网页和监控，`-nobrowser` 不自动打开浏览器）。

修改图标后重新生成 exe 资源：`go1.20.14 run ./tools/genicon && go-winres make --arch amd64`。

## 目录结构

```
main.go                    启动、单实例、托盘与各模块组装
internal/config            config.yaml
internal/topology          拓扑模型、校验、YAML 读写（含字段说明头注释）、内置默认节点
internal/pinger            Windows: IcmpSendEcho；其他平台调用系统 ping（开发用）
internal/monitor           每目标调度、1 小时秒级环形缓冲、实时指标
internal/store             分钟聚合历史文件 data/history/<id>/<日期>.bin
internal/alert             告警规则与企业微信机器人
internal/discovery         网段扫描、ARP 读取、按 MAC 跟踪 IP、自动添加
internal/snmp              精简 SNMP v2c 只读客户端（及测试用模拟代理）
internal/locate            读取交换机 MAC 表、判定设备所在端口、自动定位
internal/server            REST API + SSE 实时推送
internal/tray              托盘图标与菜单
web/static                 管理页面（原生 JS + SVG，无构建步骤，打包进 exe）
```
