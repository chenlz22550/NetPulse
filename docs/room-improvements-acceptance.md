# 机房管理七项改进验收记录

本文档记录共享目录 `/path/to/NetPulse` 下前端与多机房七项能力的入口、实测结果、受控 Mock 边界验证与限制说明。

## 一、七项清单实现入口

| 序号 | 需求项 | 前端入口 | 后端接口 |
|---|---|---|---|
| 1 | 多机房管理（新建/复制/改名/删除/单一激活） | `设置` (`#/settings`) → 「机房配置」，含跨浏览器激活同步与旧视图重置（`X-NetPing-Profile` HTTP 409 / `active_profile`） | `GET/POST /api/profiles`、`PUT/POST/DELETE /api/profiles/:id` |
| 2 | 机房独立配置与历史隔离 | `设置` / `扫描发现` / `端口定位` 页面区分全局设置（登录密码、历史保留天数、MCP、AI）与当前机房设置，未激活机房可独立编辑保存 | `GET/PUT /api/profiles/:id/settings` |
| 3 | 激活前预览（设备数、扫描范围、脱敏通知去向、连通性） | `设置` (`#/settings`) → 「机房配置」 → 「激活预览」面板（不可达仅警告，不阻塞切换） | `GET/POST /api/profiles/:id/preview` |
| 4 | 配置版本列表与恢复 | `设置` (`#/settings`) → 「机房配置」 → 「配置版本」（保存快照、查看详情、确认恢复并返回自动备份版本） | `GET/POST /api/profiles/:id/versions`、`GET /api/profiles/:id/versions/:verID`、`POST .../rollback` |
| 5 | 维护窗口（整机房或指定设备、起止/取消/到期恢复、损坏保护） | `设置` (`#/settings`) → 「维护窗口 · 当前机房」（范围切换动态显隐设备选择、创建时间段、取消维护、损坏文件锁定新建并提示保护性静默） | `GET/POST /api/maintenance`、`DELETE /api/maintenance/:id` |
| 6 | 可解释上游故障关联提示与维护静默原因 | `告警` (`#/alerts`) 列表（点击上游设备跳转 `#/node/:id`、显示拓扑关联说明、`suppressed: "maintenance"` 标识） | `GET /api/alerts` |
| 7 | 无需 AI 机房健康报告与无数据真实表达 | `运维报告` (`#/report`) → 「生成统计报告（无需 AI）」（无观测样本时显示「无数据」与提示横幅，不渲染 `100%`；复用 `GET/DELETE /api/ai/reports/:id(.md)`） | `POST /api/health-report` |

## 二、验证结果（严格区分真实服务与受控 Mock）

### 1. 静态语法与孤立运行包构建
- **JS 语法检查**：`node --check web/static/app.js` → **PASS**（日志：`/tmp/netping-verify/js-syntax.log`）
- **本地验证二进制构建**：`go build -o /tmp/netping-verify/netping .` → **PASS**（日志：`/tmp/netping-verify/go-build.log`）

### 2. 真实本地服务联调（Chromium Desktop 1440×900 + Mobile 390×844）
- **执行命令**：`/tmp/netping-verify/run_pw_real.sh`（独立临时数据目录 `/tmp/netping-verify/rundata`，独立 `TMPDIR=/tmp/netping-verify/tmp`，端口 `127.0.0.1:18098`，关闭自动扫描/定位/真实告警推送） → **PASS**（日志：`/tmp/netping-verify/real-e2e.log`）
- **覆盖流程（12 步全部通过，零 `pageerror` / `console.error`）**：
  1. 全局与当前机房设置分区文案核对（`全局 · 登录密码`、`历史保留天数（全局）`、`告警通知渠道 · 当前机房`、`维护窗口 · 当前机房`）。
  2. 复制当前机房新建未激活机房 `分部机房B`。
  3. 重命名机房 `分部机房B` → `分部机房B-已改名`。
  4. 未激活机房独立设置 `GET/PUT /api/profiles/:id/settings`（修改 `interval_ms=4500`、保存提示仅在激活后生效、重开回显一致）。
  5. 激活预览渲染（`scan_ranges: ["127.0.0.0/24"]`、`通知去向（已脱敏）`、`127.0.0.1` 可达延迟、确认激活可用）。
  6. 版本列表与恢复（手动快照带备注 `联调手动快照`、详情核对站点与 `4500ms` 参数、执行恢复）。
  7. 双浏览器上下文跨端激活同步（Browser B 停留在 `#/node/n-test-1` 且带过期 `sessionStorage`；Browser A 激活新机房后，Browser B 自动检测 `active_profile` 变更、清空旧会话缓存并重置离开旧设备详情页）。
  8. 新建并删除临时机房 `临时待删机房`。
  9. 维护窗口 `site` / `devices` 切换显隐控制、创建维护窗口并取消恢复 `已取消` 状态。
  10. 无需 AI 确定性统计日报生成（`程序确定性统计（无需 AI）`、无观测数据时渲染「无数据」卡片与提示横幅）、Markdown 下载（`.md`）。
  11. `#/alerts` 页面加载正常。
  12. `390×844` 手机视口遍历 `#/topo`、`#/list`、`#/alerts`、`#/settings`、`#/report`、`#/discovery`、`#/locate`、`#/mcp`、`#/ai`、`#/guide` 全 10 个路由，均满足 `scrollWidth <= innerWidth`（无横向溢出）。
- **视觉截图核验**：
  - Desktop 激活预览：`/tmp/netping-verify/shot-desktop-preview.png`
  - Desktop 确定性健康报告：`/tmp/netping-verify/shot-desktop-report.png`
  - Mobile 设置页（`390×844`）：`/tmp/netping-verify/shot-mobile-settings.png`
  - Mobile 健康报告页（`390×844`）：`/tmp/netping-verify/shot-mobile-report.png`

### 3. 受控 Mock 边界验证（仅针对真实本地环回难以现场构造的状态）
- **执行命令**：`/tmp/netping-verify/run_pw_mock.sh`（基于 `/tmp/netping-verify/mock_server.py` 端口 `18099` + `/tmp/netping-verify/mock_edge_e2e.mjs`） → **PASS**（日志：`/tmp/netping-verify/mock-edge-e2e.log`）
- **覆盖边界（4 项全部通过）**：
  1. `#/alerts` 含 `suppressed: "maintenance"` 与 `root_cause_id: "sw-core"`，可渲染维护静默标签与上游关联说明并点击跳转打开 `#/node/sw-core`。
  2. `POST /api/health-report` 在 `has_data: false` 时显示「无数据」且不伪造 `100%`（使用中性 `muted` 样式）；切换有样本数据后正确显示 `99.17%`。
  3. 维护文件损坏状态（`status: "error"`）下显示保护性静默提示并禁用新建维护提交按钮。
  4. 预览返回 `connectivity: unreachable` 与警告提示时，激活确认按钮保持可点击，仍可切换到该机房。

## 三、限制说明

1. **网络与通知隔离**：真实本地联调使用临时目录 `/tmp/netping-verify/rundata` 与测试拓扑（`127.0.0.1` 与 RFC 5737 测试网段 `192.0.2.10`），未对真实交换机发起 SNMP/网段扫描，也未发送真实 Webhook 告警；因此未实测真实物理网络持续采样 1 分钟以上的延迟曲线。
2. **Mock 边界范围**：`invalid-maintenance-file` 损坏态、有观测数据的 `99.17%` 确定性报告对比、`root_cause_id` 告警联动、以及网关 `unreachable` 预览告警提示均在受控 Mock 中验证渲染与交互逻辑，未在真实运行环境直接篡改维护文件触发。
3. **仅改动前端与文档范围**：所有验证均在共享目录完成且未提交；未修改任何 `.go` 后端代码，也未触碰宿主既有运行实例 `./dist/devdata`。
