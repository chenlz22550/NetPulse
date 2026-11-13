# topology.yaml 字段说明

`topology.yaml` 是 netPing 唯一的节点数据源，位于程序目录（可在 `config.yaml` 的 `topology_file` 修改）。
网页上的新增、编辑、删除、拖动位置都会写回这个文件；也可以直接编辑它（或交给 AI 修改），
然后在网页「设置 → 从磁盘重新加载」生效。保存和加载都会做校验，校验失败不会生效，并逐条列出问题。

机器可读的校验规则见 [`schema/topology.schema.json`](../schema/topology.schema.json)。

## 整体结构

```yaml
version: 1          # 固定为 1
site: {...}         # 机房信息
switches: [...]     # 交换机
links: [...]        # 交换机之间的连线
nodes: [...]        # 终端节点
```

## 通用约定

| 规则 | 说明 |
|---|---|
| id 唯一 | 交换机、链路、节点三者的 id 全局不能重复；只能包含字母、数字、`_ . -`，以字母或数字开头，最长 64 字符 |
| id 可留空 | 新增时 id 留空，程序按 IP 生成（节点 `n-192-168-1-10`，交换机 `sw-192-168-1-2`），没有 IP 时按 MAC 生成 |
| 不要随意改 id | 节点 id 是历史数据目录名，改 id 等于换了一个节点，原历史曲线不再关联 |
| IP | 只支持 IPv4；同一 IP 在所有交换机和节点中只能出现一次；为空则只登记不 ping |
| 端口名 | 纯数字端口请加引号，如 `port: "12"`，避免被当成数字 |
| 未知字段 | 拼错的字段名会直接报错（例如 `adress`），防止静默丢数据 |
| position | 为空表示按所接端口自动排布；拖动后写入坐标；交换机坐标是左上角 |

## site

| 字段 | 类型 | 说明 |
|---|---|---|
| name | 字符串 | 机房名称，显示在网页标题与告警消息 |
| subnet | 字符串 | 网段，如 `192.168.50.0/24` |
| gateway | IPv4 | 默认网关；顶栏「网关」显示它的延迟 |
| description | 字符串 | 备注 |

## switches[]

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| id | id | 自动 | 被 `links`、`nodes[].attach.switch` 引用 |
| name | 字符串 | | 显示名（必填） |
| role | 枚举 | access | `core` 核心 / `aggregation` 汇聚 / `access` 接入 / `other`；核心与汇聚画成黑色实心 |
| layer | 字符串 | | `L2`、`L3` 等，仅展示 |
| vendor / model | 字符串 | | 厂商 / 型号 |
| ip | IPv4 | | 管理地址；填写后交换机本身也会被 ping |
| mac | 字符串 | | |
| location | 字符串 | | 物理位置，如 `A01 机柜 U20` |
| note | 字符串 | | 备注 |
| port_locations | 映射 | | 可选；端口 → 物理位置，如 `{"12": "A03 机柜 配线架 2-12"}`。端口定位把设备挂到该端口时自动填入设备的 `location` |
| ports | 字符串数组 | | 可选；用于网页端口下拉、端口排序和**绘制交换机前面板**（网页里支持 `Gi0/1-24` 范围写法，保存时展开）。`Te`/`XGE`/`SFP`/`TF` 等开头的端口画成光口；纯数字编号时，超出 8 的整数倍的尾部端口画成光口（如 26 口 = 24 电口 + 2 光口）。不填时只画已接设备的端口 |
| node_side | left/right | right | 终端卡片画在交换机左侧还是右侧 |
| node_columns | 1~6 | 2 | 每个端口下终端卡片的列数 |
| monitor | 对象 | | 同 `nodes[].monitor` |
| position | {x, y} | | 拓扑图坐标 |

## links[]

| 字段 | 类型 | 说明 |
|---|---|---|
| id | id | 可空，自动生成 |
| from | {switch, port} | **上游**一端（靠近核心） |
| to | {switch, port} | **下游**一端 |
| kind | 枚举 | `uplink`（默认）/ `stack` / `other` |
| note | 字符串 | 备注，显示在连线上 |

告警时（仅对开启了 `monitor.alert` 的设备），如果下游交换机本身不可达、或其下挂节点全部同时离线，会合并为一条「交换机故障」消息。

## nodes[]

| 字段 | 类型 | 说明 |
|---|---|---|
| id | id | 可空，自动生成 |
| locked | 布尔 | 默认 false；true 表示上锁保护，一键清理始终保留。与 `ip_mode: static` 的 IP 锁定无关，不影响监控、扫描和手动编辑 |
| name | 字符串 | 显示名，可空 |
| hostname | 字符串 | 主机名，网段扫描自动获得（NetBIOS / mDNS / 反向 DNS），未填 name 时显示它；手工生成拓扑时可不填 |
| ip | IPv4 | 为空则不监控 |
| mac | 字符串 | 大写、冒号分隔；网段扫描按 MAC 识别设备、跟踪 IP 变化，建议尽量填写 |
| ip_mode | 枚举 | IP 分配方式：留空=自动跟踪；`dhcp`=自动获取（同样自动跟踪）；`static`=固定 IP，锁定不自动修改 |
| vendor | 字符串 | 厂商（一般按 MAC 前缀 OUI 判断），拓扑图按厂商着色 |
| **attach.switch** | 交换机 id | **接线位置**：接在哪台交换机；为空表示未定位 |
| **attach.port** | 字符串 | **接线位置**：交换机端口名；同一端口多台设备表示口后还有交换机/集线器 |
| location | 字符串 | 物理位置（机柜 / U 位 / 房间） |
| flags | 数组 | 标记，见下表 |
| tags | 字符串数组 | 自定义标签，可被检索 |
| note | 字符串 | 备注 |
| monitor | 对象 | 见下表；省略的字段继承 `config.yaml` 的 `ping` 默认值 |
| position | {x, y} | 为空则自动排布 |

### flags 取值

| 值 | 含义 | 拓扑图表现 |
|---|---|---|
| gateway | 默认网关 | 橙色卡片；告警标注「关键」 |
| critical | 关键设备 | 告警标注「关键」 |
| suspect | 存疑：复扫未见 / ping 不通 / 信息待核实 | 斜纹 |
| unlocated | 未定位：只在 ARP 见到，不知道接在哪个端口 | 放在「未定位」区域 |
| arp_only | 仅 ARP 可见，交换机 MAC 表没有记录 | |
| jump_host | 跳板机 | 黑色标签 |
| no_ipv4 | 没有 IPv4 地址 | |
| discovered | 由网段扫描自动添加 | 「自动发现」标签 |

### monitor

| 字段 | 说明 |
|---|---|
| enabled | 是否 ping，默认 `true` |
| alert | 是否推送企业微信告警，**默认 `false`**；只有设为 `true` 的设备才会告警 |
| interval_ms | ping 间隔（毫秒，≥200） |
| timeout_ms | 单次超时（毫秒） |
| latency_warn_ms | 1 分钟平均延迟超过此值视为「延迟高」 |
| loss_warn_pct | 1 分钟丢包率超过此值（%）视为「丢包」 |

## 示例

```yaml
version: 1
site:
  name: 二号机房
  subnet: 10.20.0.0/24
  gateway: 10.20.0.1
switches:
  - id: core
    name: H3C 核心
    role: core
    layer: L3
    ip: 10.20.0.2
    ports: [GE1/0/1, GE1/0/2, GE1/0/3, GE1/0/48]
    node_side: left
    node_columns: 1
  - id: acc-a01
    name: A01 机柜接入
    role: access
    ip: 10.20.0.3
    location: A01 机柜 U42
links:
  - from: {switch: core, port: GE1/0/48}
    to: {switch: acc-a01, port: "48"}
nodes:
  - ip: 10.20.0.1
    vendor: 华为
    attach: {switch: core, port: GE1/0/1}
    flags: [gateway]
    monitor: {alert: true}
  - ip: 10.20.0.31
    name: 存储控制器 A
    mac: 00:11:22:33:44:55
    attach: {switch: acc-a01, port: "7"}
    location: A01 机柜 U10
    flags: [critical]
    monitor: {alert: true, interval_ms: 500, latency_warn_ms: 5}
```

## 告警开关与监控时段

- 设备默认**只监控、不告警**。需要推送企业微信告警的设备设置 `monitor.alert: true`，
  也可以在网页的节点/交换机表单里勾选「推送企业微信告警」，或在「监控列表」里逐个勾选、对筛选结果批量开启/关闭。
- 全局监控时段不在本文件中，而在 `config.yaml` 的 `schedule`（网页「设置 → 监控时段」）。时段外暂停 ping 与告警：

```yaml
schedule:
  enabled: true
  windows:
    - {days: [1, 2, 3, 4, 5], start: "08:00", end: "20:00"}   # 工作日白天
    - {days: [6], start: "22:00", end: "06:00"}               # 周六晚到周日早（跨午夜）
```

## 网段扫描与 IP 跟踪

`config.yaml` 的 `discovery.jobs`（网页「扫描发现」页，或 MCP 工具 `upsert_scan_job`）定义自动扫描任务：每个任务按间隔扫描一个或多个网段，可为自动添加的设备附加标签、开启告警：

- **自动添加**：扫描到、但拓扑里没有的设备自动加入 `nodes`，`attach.switch` 为空、带 `flags: [unlocated, discovered]`，
  有 MAC 时 id 按 MAC 生成（如 `n-020000000015`），之后 IP 变化 id 也不变。
- **IP 跟踪**：设备以 MAC 为身份。扫描时 ARP 显示某个 MAC 换了 IP，就把该节点的 `ip` 改成新地址（id、历史曲线、告警设置都保留）。
  `ip_mode: static` 的节点不会被自动修改，只在扫描结果里给出提示。原 IP 被别的 MAC 占用、而本设备不在线时，会清空其 `ip`（等再次出现时自动恢复）。
- **补全 MAC**：只登记了 IP 的节点，扫描时会从 ARP 补上 MAC 和厂商。
- **仅 ARP 的设备**：不响应 ping、只在 ARP 中出现的设备，自动添加时带 `arp_only` 标记并设 `monitor.enabled: false`。
- **共享 MAC**：一个 MAC 对应多个 IP（无线中继 MAC-NAT、桥接）时，这些地址按 IP 分别登记，不写入该 MAC，`note` 中注明共享关系。
- ARP 只能看到与运行 netPing 的服务器**同一二层网段**的设备；跨网段只能确认在线，无法取得 MAC。

## 端口定位

`config.yaml` 的 `locate` 段（网页「端口定位」页）用 SNMP v2c 只读读取交换机 MAC 地址表：
`attach` 为空（未定位）的设备自动填上 `attach.switch` / `attach.port` 并去掉 `unlocated` 标记；
已定位设备的端口变化只提示，确认后修改。交换机的 `port_locations` 会在定位时填入设备的 `location`。
`links` 中登记的交换机间链路端口被视为级联口，不作为设备位置，所以请把交换机之间的连线登记完整。

## 迁移到新机房：交给 AI 生成

把下面的提示词连同 **交换机 MAC 地址表**（如 `show mac-address-table`）和 **ARP 表**（如 `show arp`）一起发给 AI：

> 请按照 netPing 的 topology.yaml 格式（字段说明见附带的 topology-fields.md 和 topology.schema.json）生成新机房的拓扑文件：
> 1. 每台交换机一个 `switches` 条目，核心交换机 `role: core`、`node_side: left`；接入交换机 `role: access`。
> 2. 交换机之间的上联口写入 `links`，`from` 为上游。
> 3. 用 MAC 表确定每个 MAC 的接入交换机和端口（忽略上联口学到的 MAC），用 ARP 表把 MAC 对应到 IP，写成 `nodes`，`attach` 填交换机 id 和端口。
> 4. ARP 中有、但 MAC 表里没有的 IP 写成 `attach.switch: ""`，并加 `flags: [unlocated, arp_only]`。
> 5. 按 MAC 前缀判断厂商填 `vendor`；本地管理位 MAC（第一个字节第 2 位为 1）写“本地私有地址”。
> 6. 默认网关加 `flags: [gateway]`。id 和 position 留空。纯数字端口加引号。
> 7. 所有设备默认不告警；只给我指定的关键设备（如网关、交换机管理地址、服务器）加 `monitor: {alert: true}`。
> 只输出 YAML。

把生成的内容粘贴到网页「设置 → 拓扑文件」编辑框，点「校验并保存」；或直接覆盖程序目录下的 `topology.yaml` 后点「从磁盘重新加载」。若校验失败，把错误列表再发给 AI 修正即可。
