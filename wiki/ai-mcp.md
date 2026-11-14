# AI 诊断与 MCP

## AI 诊断

在页面配置模型协议、接口地址、模型与 API Key，测试连接后再提问。支持 OpenAI Chat Completions、OpenAI Responses、Claude Messages 和 Gemini 协议（默认 DeepSeek `deepseek-flash`）。默认开启回答前自检，核对证据后再输出结论。

例如：“哪些设备近一小时丢包最严重？”、“这几台设备同时离线，有共同上游吗？”、“检查这台设备是禁 Ping 还是真离线。” 模型可以结合历史、上联路径和现场探测给出依据与建议。

AI 诊断使用只读工具，不能修改配置。不要将凭据放入问题中；实时设备信息和工具结果会发给你配置的模型服务，应按现场要求选择供应商与代理。

## MCP 接入

进入「MCP」复制客户端配置。端点为 `POST http://主机:8090/mcp`（Streamable HTTP，返回 JSON，不使用 SSE 流），建议配置专用令牌（`Authorization: Bearer <token>`）并优先开启只读。没有令牌时需通过 HTTP Basic 提供管理密码。远程访问应使用受控网络或带 TLS 的反向代理。

只读模式允许概况、设备、历史、告警、插件日志、维护窗口、运维报告以及多机房档案/版本预检等只读查询；关闭只读后，除拓扑、告警、扫描和监控设置外，还支持：

- **多机房与版本**：`list_profiles`、`create_profile`、`rename_profile`、`preview_profile`（默认不主动探测）、`activate_profile` / `delete_profile`（均需 `confirm=true`）、`get_profile_settings` / `set_profile_settings`（只修改机房级设置并脱敏敏感字段），以及版本快照与回滚（`list_profile_versions`、`create_profile_version`、`get_profile_version`、`preview_profile_version_rollback`、`rollback_profile_version`）。
- **维护窗口**：`list_maintenance`、`create_maintenance`、`cancel_maintenance`（仅作用于当前激活机房）。
- **运维报告**：`generate_health_report`（纯程序统计，不调 AI）、`generate_ai_report`（显式调用 AI）、`list_reports`、`get_report`（支持 JSON / Markdown / HTML 内容）、`delete_report`。
- **设备锁定、清理与 AI 优化**：`set_device_locked`（也可通过 `upsert_device` 传 `locked`）、`preview_device_cleanup`（返回绑定 `profile_id` 与 `epoch`）、`cleanup_devices`（须传入预览匹配的 `profile_id`、`epoch` 与 `confirm=true`）；`generate_ai_optimize`（需非只读模式，调用已配置模型并产生费用，不会自动修改拓扑）、`apply_ai_optimize`（默认 `dry_run=true`，应用需 `dry_run=false` 与 `confirm=true` 且绑定 `profile_id` 与 `epoch`）、`undo_ai_optimize`（需 `confirm=true`）。
- **插件只读查看**：`get_plugins`（在只读模式下可用，不返回本地路径并脱敏密钥与 URL 凭据/查询参数；运行记录仅返回结构化状态摘要，不返回自由文本 `message`/`error`/`stderr`，运行详情请查看网页「插件」页）。

先确认客户端信任范围再放开写权限。

## 运维报告

程序统计报告无需模型，可导出 Markdown、HTML 或打印 PDF；可选 AI 日报、周报、月报。通过 MCP 亦可直接生成确定性报告（`generate_health_report`）或显式触发 AI 报告（`generate_ai_report`）。审阅时间范围和采样完整性，再将报告发送给他人。
