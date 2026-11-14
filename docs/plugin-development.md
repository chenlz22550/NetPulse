# netPing 插件开发指南

插件让你在不改动、不重新编译 netPing 的情况下扩展它。目前插件可以做三件事：

| 能力 | 怎么声明 | 典型用途 |
|---|---|---|
| **通知渠道** | `hooks: [notify]` | 把告警发到邮件、短信、电话、自建 IM、工单系统 |
| **事件处理** | `hooks: [alert, discovery, locate]` | 告警时自动重启设备、写入 CMDB / 数据库、联动其他系统 |
| **AI 工具** | `tools: [...]` | 让 MCP 客户端和网页「AI 诊断」能查询资产库、机柜图、值班表等外部数据 |

本文适用于插件协议 **api_version 1**（netPing 1.5.0 起）。运行中的 netPing 也提供本文：
网页「插件」页的「开发指南」，或 MCP 资源 `netping://docs/plugin-development.md`（可以直接让 AI 助手按它帮你写插件）。

## 1. 工作方式

插件是一个**独立的小程序**，用什么语言写都行。netPing 需要它时：

1. 启动插件程序（一次调用启动一个进程，处理完就退出，不常驻）；
2. 把**一个 JSON 请求**写到它的标准输入（stdin），然后关闭标准输入；
3. 从它的标准输出（stdout）读回**一个 JSON 响应**；
4. 插件退出。退出码为 0 表示成功。

没有需要链接的 SDK，也没有网络端口：能读标准输入、写标准输出的程序都能当插件。

```
netPing ──stdin: {"hook":"notify", "settings":{…}, "payload":{…}}──▶ 插件进程
netPing ◀──stdout: {"ok":true, "message":"已发送"}──────────────── 插件进程（随后退出）
```

## 2. 五分钟写一个插件

下面用 Python 写一个最小的通知插件：每收到一条通知就追加到文本文件。

**第 1 步：建目录。** 在 netPing 数据目录（`config.yaml` 所在目录；macOS 为 `~/Library/Application Support/netPing`）下建
`plugins/notify-file/`。目录名就是插件 id。

**第 2 步：写清单** `plugins/notify-file/plugin.yaml`：

```yaml
id: notify-file              # 必须与目录名相同
name: 通知存档
version: 1.0.0
description: 把每条通知追加保存到文本文件
command: ["python", "main.py"]   # macOS / Linux 上通常是 python3
hooks: [notify]
settings:
  - key: filename
    label: 文件名
    default: notify.log
```

**第 3 步：写程序** `plugins/notify-file/main.py`：

```python
import json, os, sys

req = json.loads(sys.stdin.buffer.read().decode("utf-8"))      # 读请求
path = os.path.join(req["plugin"]["data_dir"], req["settings"]["filename"])
with open(path, "a", encoding="utf-8") as f:                   # 干活
    f.write(req["time"] + " " + req["payload"]["title"] + "\n" + req["payload"]["text"] + "\n\n")
print(json.dumps({"ok": True, "message": "saved to " + path}))  # 写响应
```

**第 4 步：启用并测试。** 打开管理页面「插件」页 → 点「重新扫描」→ 在「通知存档」里点「测试运行」，
应显示“✓ 成功”；确认无误后点「启用」。之后每条告警通知都会交给它。

> 只改 `main.py` 立即生效（每次调用都重新启动进程）；改了 `plugin.yaml` 需要点「重新扫描」。

## 3. 目录约定

```
<数据目录>/
  plugins/
    <插件 id>/              ← 插件目录：只放程序和清单，升级插件时整个覆盖
      plugin.yaml           ← 清单（必需）
      main.py / xxx.exe …   ← 插件程序
  data/
    plugins.json            ← 各插件的启用状态与设置（netPing 维护，含密钥，不要提交到版本库）
    plugins/<插件 id>/      ← 插件私有数据目录：可写，升级插件不受影响
```

- 以 `.` 或 `_` 开头的目录会被忽略，可用来临时停放插件（如 `_backup`）。
- 插件要保存的文件请写到请求里给出的 `plugin.data_dir`，不要写到插件目录。
- 插件进程的当前目录是插件目录，所以 `command` 和程序里都可以用相对路径引用自带文件。

## 4. 清单 plugin.yaml

```yaml
id: asset-lookup             # 必填。小写字母、数字、连字符，2~24 个字符，以字母或数字开头；必须与目录名相同
name: 资产查询               # 显示名，默认同 id
version: 1.0.0               # 版本号，默认 0.0.0
description: 从资产库查询设备的负责人和保修信息
author: 张三
homepage: https://example.com/asset-lookup
api_version: 1               # 插件使用的协议版本，默认 1；高于 netPing 支持的版本时拒绝加载
command: ["./asset-lookup"]  # 必填。启动命令，见下
timeout_sec: 15              # 单次调用最长时间（秒），默认 15，最大 120；超时进程会被终止并记为失败
hooks: [alert]               # 要接收的钩子：notify / alert / discovery / locate（hooks 与 tools 至少有一项）
tools:                       # 提供给 AI 的工具（最多 32 个）
  - name: lookup             # 小写字母、数字、下划线，最多 24 个字符，以字母开头
    title: 查资产            # 显示名
    description: 按 IP 或 MAC 查询设备的负责人、部门、购买日期和保修状态。   # 必填：AI 靠它判断何时调用，写清楚“能查什么、需要什么参数”
    write: false             # true = 会修改外部系统或数据（见 §8）
    ai: true                 # true = 允许网页「AI 诊断」调用（只对 write: false 的工具有效），默认 false
    input_schema:            # 参数的 JSON Schema，type 必须是 object；省略表示无参数
      type: object
      properties:
        ip: {type: string, description: 设备 IP}
        mac: {type: string, description: 设备 MAC}
settings:                    # 设置项（最多 32 个），在网页「插件」页填写，调用时随请求传给插件
  - key: api_url             # 小写字母、数字、下划线，以字母开头
    label: 资产库接口地址    # 显示名，默认同 key
    type: string             # string（默认）/ secret / text / bool / number / select
    help: 如 http://10.0.0.8/api   # 输入框下方的说明
    default: ""              # 默认值，类型须与 type 一致
    required: true           # 必填：未填写时不能启用插件
  - key: api_token
    label: 接口令牌
    type: secret             # 密钥：网页不回显
  - key: region
    type: select
    options: [华东, 华北]    # select 类型必须给出 options
```

### command 怎么写

`command` 是一个数组：第一项是程序，后面是参数。第一项按以下规则查找：

| 写法 | 含义 | 例子 |
|---|---|---|
| 带路径分隔符或以 `.` 开头 | 相对**插件目录** | `["./asset-lookup"]`、`["bin/tool.exe", "--fast"]` |
| 只有名字 | 先找插件目录下的同名文件，再找系统 `PATH` | `["python", "main.py"]`、`["node", "index.js"]` |
| 绝对路径 | 原样使用 | `["C:\\Python39\\python.exe", "main.py"]` |

Windows 上没写扩展名时会依次尝试 `.exe`、`.bat`、`.cmd`，所以 `["./asset-lookup"]` 在 Windows 和 macOS / Linux 上通用。
命令不经过 shell 解析：不能写管道、重定向或 `&&`；需要时自己包一层脚本（如 `["cmd", "/c", "run.bat"]`、`["sh", "run.sh"]`）。

### 设置项类型

| type | 网页控件 | 传给插件的值 |
|---|---|---|
| `string` | 单行文本 | 字符串（已去掉首尾空格） |
| `secret` | 密码框，不回显；留空保存表示不修改 | 字符串 |
| `text` | 多行文本 | 字符串 |
| `bool` | 开关 | `true` / `false` |
| `number` | 数字框 | 数字（JSON number，请按数字而不是整数解析） |
| `select` | 下拉框 | `options` 中的某个字符串，未选为 `""` |

请求里的 `settings` **总是包含清单中声明的全部 key**：用户没填的用 `default`，没有 `default` 的是 `""` / `false` / `0`。

## 5. 请求格式

每次调用，标准输入里是一个 UTF-8 编码的 JSON 对象（单行，以换行结束）：

```json
{
  "api_version": 1,
  "hook": "notify",
  "id": "20261114-135200-9f3a1c2b4d5e",
  "time": "2026-11-14T13:52:00+08:00",
  "test": false,
  "netping": {"version": "1.5.0", "site": "四号机房", "url": "http://127.0.0.1:8090/"},
  "plugin": {
    "id": "notify-file", "version": "1.0.0",
    "dir": "D:\\netPing\\plugins\\notify-file",
    "data_dir": "D:\\netPing\\data\\plugins\\notify-file"
  },
  "settings": {"filename": "notify.log"},
  "payload": { }
}
```

| 字段 | 说明 |
|---|---|
| `api_version` | 协议版本。以后的版本只会**增加**字段，请忽略不认识的字段 |
| `hook` | 本次调用的类型：`notify` / `alert` / `discovery` / `locate` / `tool`。一个插件声明了多个钩子时靠它区分 |
| `id` | 本次调用的唯一编号，可用于日志或去重 |
| `time` | 调用时间（RFC 3339，服务器本地时区） |
| `test` | `true` 表示这是网页「测试运行」发来的**示例数据**。有副作用的插件（发短信、重启设备、写数据库）应在测试时标明或跳过真实动作 |
| `netping` | netPing 版本、机房名称、管理页面地址 |
| `plugin` | 插件 id、版本、插件目录 `dir`、私有数据目录 `data_dir`（已创建好，可写） |
| `settings` | 设置项的当前值 |
| `payload` | 随 `hook` 而不同，见 §7 |

另外会设置这些环境变量（不含任何设置项，密钥只走标准输入）：`NETPING_PLUGIN_ID`、`NETPING_PLUGIN_DIR`、
`NETPING_PLUGIN_DATA_DIR`、`NETPING_HOOK`，以及 `PYTHONIOENCODING=utf-8`、`PYTHONUTF8=1`。

## 6. 响应格式

把一个 JSON 对象写到标准输出，然后退出：

```json
{"ok": true, "message": "已发送给 3 人", "result": {"任意": "JSON"}}
```

```json
{"ok": false, "error": "短信网关返回 403：余额不足"}
```

| 字段 | 说明 |
|---|---|
| `ok` | 必填。是否成功 |
| `message` | 可选。一句话结果，显示在「插件」页的「最近调用」里 |
| `error` | 失败原因。通知类插件的失败原因会出现在「告警」页该条告警的发送结果里 |
| `result` | 可选，任意 JSON。**工具调用的返回值**，会原样交给 AI |

判定规则（按顺序）：

1. **超时** → 失败，进程被终止。
2. **标准输出超过 1 MB** → 失败。
3. **退出码非 0** → 失败。原因取响应里的 `error`；没有则取标准错误的最后一行。
4. 退出码为 0 且输出了带 `ok` 字段的 JSON 对象 → 按 `ok` 判定。
5. 退出码为 0 但输出的不是上述格式 → **成功**，全部输出作为 `result`（是 JSON 就按 JSON，否则按文本）。
   所以简单脚本什么都不输出、只靠退出码表示成败也可以。

**标准错误（stderr）是你的调试通道**：随便写，netPing 保留最后 8 KB，失败时最后一行记入 `logs/netping.log`，
「测试运行」时直接显示在网页上。不要把调试信息写到标准输出，那会破坏响应。

## 7. 各钩子的 payload

### 7.1 notify — 通知渠道

netPing 要推送一条消息时调用（与企业微信、钉钉等内置渠道并列，收到的是同一条已合并的消息）。

```json
{
  "source": "alert",
  "title": "机房网络监控 · 四号机房",
  "markdown": "### 机房网络监控 · 四号机房\n<font color=\"warning\">● 离线</font> **192.168.50.29** 3楼打印机 **[关键]**\n>位置：接入交换机 Port 12 / A03 机柜\n>连续 5 次无响应\n<font color=\"comment\">2026-11-14 13:52:00</font>",
  "text": "### 机房网络监控 · 四号机房\n● 离线 **192.168.50.29** 3楼打印机 **[关键]**\n>位置：接入交换机 Port 12 / A03 机柜\n>连续 5 次无响应\n2026-11-14 13:52:00"
}
```

| 字段 | 说明 |
|---|---|
| `source` | 来源：`alert` 告警（含「设置」页的测试消息）、`discovery` 扫描发现、`locate` 端口定位、`test` 插件页「测试运行」 |
| `title` | 标题（消息第一行去掉 `#`） |
| `markdown` | 企业微信风格的 markdown 原文，颜色用 `<font color="warning">`、`info`、`comment` 标签，最长约 4000 字节 |
| `text` | 去掉 `<font>` 标签后的文本（仍保留 `**加粗**`、`>引用` 等 markdown 符号） |

什么时候会被调用——规则与内置渠道完全相同：

- 「设置」里的**告警总开关**开启，且设备开启了告警（设备默认只监控、不告警）；
- 消息已经过合并（默认 10 秒内的事件合成一条）、上游交换机故障归并和重复提醒间隔控制，插件不需要自己去重；
- 扫描发现、端口定位的通知还需要在各自页面开启“推送通知”。

只启用了通知插件、没有填写任何内置渠道地址时，也可以开启告警总开关。
插件返回失败不影响其他渠道，但这批告警在「告警」页会显示为发送失败并带上你的 `error`。
调用是**同步**的：告警引擎会等插件结束（最多 `timeout_sec`），请尽快返回，网络请求务必设置超时。

### 7.2 alert — 结构化告警事件

每批告警事件写入告警历史后调用。与 notify 的区别：拿到的是**逐条的结构化数据**而不是拼好的文字；
**不受告警总开关影响**（只要设备开启了告警就会产生事件）；也不做交换机故障归并。适合做联动而不是发消息。

```json
{
  "notified": true,
  "events": [
    {
      "time": "2026-11-14T13:52:00.123+08:00",
      "type": "down",
      "target_id": "n-192-168-50-29",
      "kind": "node",
      "name": "3楼打印机",
      "ip": "192.168.50.29",
      "location": "接入交换机 Port 12 / A03 机柜",
      "detail": "连续 5 次无响应",
      "sent": true
    }
  ]
}
```

| 字段 | 说明 |
|---|---|
| `notified` | 这批事件是否已通过通知渠道成功发出 |
| `events[].type` | `down` 离线、`still_down` 仍离线（重复提醒）、`recover` 恢复、`loss` 丢包、`loss_recover` 丢包恢复、`slow` 延迟高、`slow_recover` 延迟恢复 |
| `events[].target_id` | 设备 id。TCP 端口监控的目标为 `_tcp_<端口>_<设备 id>`，此时 `name` 以 ` TCP:<端口>` 结尾 |
| `events[].kind` | `node` 终端设备 / `switch` 交换机 |
| `events[].name` / `ip` / `location` | 名称、IP、接线位置与物理位置 |
| `events[].detail` | 文字说明，如“离线时长 5分12秒，当前延迟 1.20ms” |
| `events[].sent` / `error` | 该事件的通知发送结果；失败时 `error` 为原因 |

### 7.3 discovery — 扫描发现

一次网段扫描**产生了变更**时调用（没有变化的扫描不调用）。

```json
{
  "job": "机房网段",
  "changes": [
    {"time": "2026-11-14T13:52:00+08:00", "type": "added", "node_id": "n-aabbccddeeff",
     "ip": "192.168.50.19", "mac": "AA:BB:CC:DD:EE:FF", "detail": "…", "job": "机房网段"},
    {"time": "2026-11-14T13:52:00+08:00", "type": "ip_changed", "node_id": "n-112233445566", "name": "前台电脑",
     "old_ip": "192.168.50.50", "ip": "192.168.50.73", "mac": "11:22:33:44:55:66", "detail": "…"}
  ],
  "suggestions": [
    {"node_id": "srv-1", "name": "备份存储", "current_ip": "192.168.50.60", "seen_ip": "192.168.50.61",
     "mac": "00:11:22:33:44:55", "reason": "…"}
  ]
}
```

`changes[].type`：`added` 自动添加了新设备、`ip_changed` 按 MAC 跟踪到新 IP、`ip_released` IP 被其他设备占用后清空、
`mac_learned` 补全了 MAC、`conflict` 无法自动处理的冲突、`hostname` 识别或更新了主机名。
值为空的字段（如没有名称的新设备的 `name`）会被省略，读取时请给默认值。
`suggestions` 是需要人工确认的 IP 变化（当前全部待确认项，不只是新增的）。

### 7.4 locate — 端口定位

一次端口定位**产生了变更**时调用。

```json
{
  "changes": [
    {"time": "2026-11-14T13:52:00+08:00", "type": "located", "node_id": "n-aabbccddeeff", "name": "新打印机",
     "ip": "192.168.50.19", "mac": "AA:BB:CC:DD:EE:FF", "from": "未定位", "to": "接入交换机 Port 12",
     "location": "A03 机柜 配线架 2-12", "detail": "…"}
  ],
  "suggestions": [
    {"node_id": "pc-7", "name": "财务电脑", "ip": "192.168.50.41", "mac": "66:77:88:99:AA:BB",
     "from": "接入交换机 Port 5", "to": "接入交换机 Port 9", "switch": "access", "port": "9"}
  ]
}
```

`changes[].type`：`located` 未定位的设备被自动挂到端口、`moved` 端口变化已采纳、`location` 补全了物理位置。
`suggestions` 是端口变化待确认的设备（当前全部待确认项）。

### 7.5 tool — 工具调用

MCP 客户端或网页「AI 诊断」调用你在 `tools` 里声明的工具时触发，`hook` 为 `"tool"`：

```json
{"tool": "lookup", "arguments": {"ip": "192.168.50.29"}, "remote": "192.168.50.5"}
```

| 字段 | 说明 |
|---|---|
| `tool` | 工具名（清单里的 `name`，不带前缀）。一个插件有多个工具时靠它区分 |
| `arguments` | AI 给出的参数。**netPing 不按 `input_schema` 校验**，请自己检查必填项和类型 |
| `remote` | 调用来源：MCP 客户端的 IP，或 AI 诊断的标识 |

把结果放在响应的 `result` 里（任意 JSON，建议是对象）：

```json
{"ok": true, "result": {"ip": "192.168.50.29", "owner": "行政部 李四", "warranty_until": "2027-03-01"}}
```

查不到、参数不对时返回 `{"ok": false, "error": "没有 IP 为 … 的资产"}`，AI 会看到这句话并据此调整。

工具对 AI 的名字是 **`plugin_<插件 id>_<工具名>`**（如 `plugin_asset-lookup_lookup`），描述前会自动加上“[插件 名称]”。
结果过大时会被截断：AI 诊断单个工具结果最多约 16000 字符，请只返回需要的字段，必要时提供筛选和 `limit` 参数。

## 8. 权限与安全

**对插件作者：**

- `write: true` 的工具表示会修改外部系统或数据。它在 MCP **只读模式**下不可见，网页「AI 诊断」**永远不会**调用。
  有副作用的工具必须如实标记。
- `ai: true` 才会开放给网页「AI 诊断」（MCP 客户端不受此限制）。AI 诊断会把工具结果发送给模型服务商，
  返回敏感数据（手机号、密码、内部地址）的工具不要开启。
- 设置（含密钥）只通过标准输入传递。不要把密钥打印到标准输出或标准错误——它们会进日志。
- 所有来自请求的内容（设备名、主机名、AI 给的参数）都是**不可信输入**：拼接 shell 命令、SQL、文件路径前必须校验或转义。
  设备的主机名来自网络，可以被局域网里的任何设备伪造。
- 插件没有访问 netPing 管理接口的凭据。需要 netPing 的数据时，用请求里已有的字段，或让用户在设置里另填 MCP 令牌。

**对使用者（也请写进你的插件说明）：**

- 插件以 netPing 的系统账号运行，能做这个账号能做的任何事。只安装来源可信的插件。
- 插件只能通过把文件放进 `plugins/` 目录安装；网页和 MCP 都不能上传插件。新插件默认未启用。
- AI 助手（MCP）不能启用、停用或配置插件。

## 9. 运行时行为

| 项目 | 行为 |
|---|---|
| 进程模型 | 每次调用启动一个新进程，结束即退出。不要常驻，不要等待更多输入 |
| 并发 | 全局最多同时运行 4 个插件进程。**同一个插件可能被并发调用**（例如 AI 同时调用两次工具），写共享文件时请追加写或加锁 |
| 事件顺序 | `alert` / `discovery` / `locate` 事件异步排队分发，每个插件按产生顺序逐个收到；队列（256 个）满时丢弃新事件并记日志 |
| 超时 | 超过 `timeout_sec` 进程被终止，本次记为失败，**不会重试** |
| 失败 | 任何失败都只记录，不重试、不自动停用插件。需要可靠投递请在插件内自己重试或落盘 |
| 暂停 | 监控暂停或处于非监控时段时不产生告警，因此也不会调用 notify / alert |
| 输出上限 | 标准输出 1 MB；标准错误保留最后 8 KB |
| 窗口 | Windows 上插件进程不显示控制台窗口 |
| 记录 | 每次调用在 `logs/netping.log` 记一行（插件、类型、成败、耗时）；「插件」页显示最近 50 次 |

## 10. 调试

**在网页上测试。** 「插件」页每个插件都有「测试运行」：选择钩子（发送 §7 中的示例数据，`test: true`）或工具（可填 JSON 参数），
结果区会显示成败、耗时、`result`、标准错误，并可展开查看**发给插件的完整请求**。未启用的插件也可以测试。

**在命令行里测试。** 把请求存成文件，直接喂给插件程序，最方便反复调试：

```bash
# 在「测试运行」里展开“查看发给插件的请求”，复制保存为 req.json（其中的密钥显示为 ******，需要的话手工改回）
python main.py < req.json            # Windows PowerShell:  Get-Content req.json | python main.py
echo "退出码 $?"
```

**常见问题：**

| 现象 | 原因与处理 |
|---|---|
| 插件页显示“清单无效” | 按红字提示修改 `plugin.yaml` 后点「重新扫描」。最常见的是 id 与目录名不一致、YAML 缩进错误 |
| “找不到程序 python” | 运行 netPing 的电脑没装 Python，或它不在系统 PATH 里。把 `command` 第一项改成绝对路径，或改用编译型语言 |
| 中文乱码、JSON 解析失败 | 按 UTF-8 读写。Python 用 `sys.stdin.buffer.read().decode("utf-8")`；PowerShell 脚本开头设置 `[Console]::OutputEncoding = [Text.Encoding]::UTF8` |
| 命令行里正常，netPing 调用失败 | netPing 以托盘程序的身份运行，PATH、代理、当前用户可能和你的命令行不同；看「测试运行」里的标准错误 |
| “超过 N 秒未结束” | 给网络请求加超时，或调大 `timeout_sec`（最大 120）。插件不应等待用户输入 |
| 启用后没有收到 notify | 检查「设置」里的告警总开关、设备是否开启了告警；点「设置」里的「发送测试消息」会同时发给通知插件 |
| 改了 plugin.yaml 没生效 | 点「重新扫描」 |

## 11. 语言选择与示例

| 语言 | 优点 | 注意 |
|---|---|---|
| **Go** | 编译成单个 exe，目标机器不需要装任何东西；与 netPing 一样能在 Windows 7 / Server 2008 R2 上运行（须用 Go 1.20 编译） | 改代码要重新编译 |
| **Python** | 写起来最快，标准库够用 | 目标机器要装 Python；Server 2008 R2 最高只能装 Python 3.8 |
| **PowerShell** | Windows 自带 | `ConvertFrom-Json` 需要 PowerShell 3.0+（Server 2008 R2 自带的是 2.0，需升级 WMF） |
| **Node.js 等** | 生态丰富 | 目标机器要装运行环境 |

仓库里有两个可以直接使用、也可以照着改的完整示例：

| 示例 | 语言 | 演示内容 |
|---|---|---|
| [`examples/plugins/email-notify`](../examples/plugins/email-notify) | Python | notify 钩子：SMTP 邮件通知；各种类型的设置项；错误处理；用假 SMTP 服务器写自测 |
| [`examples/plugins/event-log`](../examples/plugins/event-log) | Go | alert / discovery / locate 三个钩子；使用 `data_dir` 保存数据；提供 AI 工具 `recent_events`；处理 `test` 标志；单元测试 |

安装示例：

```bash
# Python 示例：直接复制目录
cp -r examples/plugins/email-notify  <数据目录>/plugins/

# Go 示例：先编译，再复制清单和可执行文件
cd examples/plugins/event-log
GOOS=windows GOARCH=amd64 go1.20.14 build -o event-log.exe .     # 目标是 Windows 时
mkdir -p <数据目录>/plugins/event-log
cp plugin.yaml event-log.exe <数据目录>/plugins/event-log/
```

### Go 插件骨架

```go
package main

import (
	"encoding/json"
	"os"
)

type request struct {
	Hook   string `json:"hook"`
	Test   bool   `json:"test"`
	Plugin struct {
		DataDir string `json:"data_dir"`
	} `json:"plugin"`
	Settings map[string]interface{} `json:"settings"`
	Payload  json.RawMessage        `json:"payload"`
}

type response struct {
	OK      bool        `json:"ok"`
	Message string      `json:"message,omitempty"`
	Error   string      `json:"error,omitempty"`
	Result  interface{} `json:"result,omitempty"`
}

func main() {
	var req request
	resp := response{OK: true}
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		resp = response{Error: "请求无效: " + err.Error()}
	} else {
		switch req.Hook {
		case "alert":
			// TODO: 解析 req.Payload，做你的事
			resp.Message = "已处理"
		default:
			resp = response{Error: "不支持的钩子 " + req.Hook}
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(resp)
	if !resp.OK {
		os.Exit(1)
	}
}
```

### PowerShell 插件骨架（PowerShell 3.0+）

```powershell
# plugin.yaml:  command: ["powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", "main.ps1"]
[Console]::OutputEncoding = [Text.Encoding]::UTF8
$reader = New-Object IO.StreamReader([Console]::OpenStandardInput(), [Text.Encoding]::UTF8)
$req = $reader.ReadToEnd() | ConvertFrom-Json
try {
    # TODO: 用 $req.settings / $req.payload 做你的事
    @{ ok = $true; message = "已处理 $($req.hook)" } | ConvertTo-Json -Compress
} catch {
    @{ ok = $false; error = "$_" } | ConvertTo-Json -Compress
    exit 1
}
```

## 12. 发布你的插件

1. 目录里放一个 `README`：插件做什么、需要什么运行环境、每个设置项怎么填、有哪些副作用。
2. 编译型插件同时提供源码和各平台的可执行文件；`command` 写成不带扩展名的 `["./名字"]` 可跨平台。
3. 升级时递增 `version`。新增设置项要给 `default`，这样老用户升级后不用重新配置；
   删除或改名的设置项，旧值会在用户下次保存设置时自动清理。
4. 不要依赖本文没有写明的行为；协议有不兼容变化时 `api_version` 会递增，netPing 会继续支持旧版本的插件。

## 附：管理接口

网页「插件」页使用以下接口（需网页登录）：

| 接口 | 作用 |
|---|---|
| `GET /api/plugins` | 插件列表（含清单无效的）、设置项当前值（密钥不返回）、最近调用记录 |
| `POST /api/plugins/reload` | 重新扫描插件目录 |
| `PUT /api/plugins/<id>` | `{"enabled": true}` 启停；`{"settings": {"key": 值}}` 保存设置（未出现的 key 不变，`null` 恢复默认，密钥传 `""` 表示不修改） |
| `POST /api/plugins/<id>/test` | `{"hook": "notify"}` 或 `{"tool": "lookup", "arguments": {…}}`，真实运行一次并返回结果 |
| `GET /api/plugins/guide` | 本文（Markdown） |
