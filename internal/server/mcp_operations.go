package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"netping/internal/alert"
	"netping/internal/config"
)

var forbiddenRoomSettingKeys = map[string]bool{
	"auth":           true,
	"mcp":            true,
	"ai":             true,
	"listen":         true,
	"retention_days": true,
	"topology_file":  true,
	"data_dir":       true,
	"plugins":        true,
	"dir":            true,
	"path":           true,
	"file":           true,
}

func operationsTools() []*mcpTool {
	return []*mcpTool{
		// ---- 多机房档案与机房设置 ----
		{
			Name: "list_profiles", Title: "列出机房档案", Write: false,
			Description: "列出全部机房档案（名称、ID、设备数、是否当前激活）以及当前激活机房 ID 与 epoch 版本号（用于绑定 AI 优化操作）。",
			Schema:      obj(nil),
			Run:         toolListProfiles,
		},
		{
			Name: "create_profile", Title: "创建机房档案", Write: true,
			Description: "创建一个新的机房档案（默认隔离的拓扑和机房设置）；可选 copy_current=true 复制当前激活机房，或传入 source_id 克隆指定机房。",
			Schema: obj(map[string]interface{}{
				"name":         str("机房名称（必填）"),
				"copy_current": boolean("是否复制当前激活机房的拓扑与机房设置"),
				"source_id":    str("从指定机房 ID 克隆；填写时优先于 copy_current"),
			}, "name"),
			Run: toolCreateProfile,
		},
		{
			Name: "rename_profile", Title: "重命名机房档案", Write: true,
			Description: "修改指定机房档案的显示名称。",
			Schema: obj(map[string]interface{}{
				"id":   str("机房档案 ID"),
				"name": str("新机房名称"),
			}, "id", "name"),
			Run: toolRenameProfile,
		},
		{
			Name: "delete_profile", Title: "删除机房档案", Write: true, Destructive: true,
			Description: "删除一个非当前激活、非唯一的机房档案（不可恢复，历史和配置版本一并清理）。必须传入 confirm=true 才会执行。",
			Schema: obj(map[string]interface{}{
				"id":      str("要删除的机房档案 ID"),
				"confirm": boolean("必须为 true 才会真正删除"),
			}, "id", "confirm"),
			Run: toolDeleteProfile,
		},
		{
			Name: "preview_profile", Title: "预检机房档案", Write: false, OpenWorld: true,
			Description: "切换前只读预检指定机房档案：返回设备统计、定时任务摘要、扫描网段、脱敏后的通知渠道，以及当前是否忙碌（can_switch）；仅在明确传入 probe=true 时才会从本机发起网络连通性探测（默认 false 不发包）。",
			Schema: obj(map[string]interface{}{
				"id":    str("机房档案 ID"),
				"probe": boolean("是否执行主动网络探测（ping/SNMP），默认 false"),
			}, "id"),
			Run: toolPreviewProfile,
		},
		{
			Name: "activate_profile", Title: "切换激活机房", Write: true, Destructive: true,
			Description: "将运行中的主服务切换到指定机房（会重载监控、告警、扫描与定位，并自动递增 epoch）。必须传入 confirm=true 才会执行。",
			Schema: obj(map[string]interface{}{
				"id":      str("目标机房档案 ID"),
				"confirm": boolean("必须为 true 才会切换机房"),
			}, "id", "confirm"),
			Run: toolActivateProfile,
		},
		{
			Name: "get_profile_settings", Title: "查看机房配置", Write: false,
			Description: "读取指定机房（可激活或未激活）的机房级设置（schedule、discovery、locate、ping、alert），Webhook 与 SNMP 团体字等敏感字段已脱敏。",
			Schema: obj(map[string]interface{}{
				"id": str("机房档案 ID；省略则读取当前激活机房"),
			}),
			Run: toolGetProfileSettings,
		},
		{
			Name: "set_profile_settings", Title: "修改机房配置", Write: true,
			Description: "按补丁修改指定机房的机房级设置（仅允许 schedule、discovery、locate、ping、alert；省略或传入脱敏占位符的密钥会保留原值）。禁止修改全局字段（auth、mcp、ai、listen、retention_days 等）。",
			Schema: obj(map[string]interface{}{
				"id": str("机房档案 ID"),
				"settings": map[string]interface{}{
					"type":        "object",
					"description": "机房设置补丁对象，仅允许顶层键：schedule、discovery、locate、ping、alert",
				},
			}, "id", "settings"),
			Run: toolSetProfileSettings,
		},

		// ---- 机房配置版本 ----
		{
			Name: "list_profile_versions", Title: "列出机房配置版本", Write: false,
			Description: "列出指定机房已保存的配置快照版本列表（包含手动备份与切换/回滚前自动备份，按时间倒序）。",
			Schema: obj(map[string]interface{}{
				"id": str("机房档案 ID；省略则为当前激活机房"),
			}),
			Run: toolListProfileVersions,
		},
		{
			Name: "create_profile_version", Title: "创建配置版本快照", Write: true,
			Description: "为指定机房手动创建一份当前拓扑与机房设置的快照版本。",
			Schema: obj(map[string]interface{}{
				"id":   str("机房档案 ID；省略则为当前激活机房"),
				"note": str("版本备注（最多 200 字，可选）"),
			}),
			Run: toolCreateProfileVersion,
		},
		{
			Name: "get_profile_version", Title: "查看配置版本详情", Write: false,
			Description: "读取指定机房配置版本快照的元数据、拓扑与脱敏后的机房设置（不会返回明文密钥）。",
			Schema: obj(map[string]interface{}{
				"id":         str("机房档案 ID；省略则为当前激活机房"),
				"version_id": str("配置版本 ID（如 v-20261114-101500-001）"),
			}, "version_id"),
			Run: toolGetProfileVersion,
		},
		{
			Name: "preview_profile_version_rollback", Title: "预检版本回滚差异", Write: false,
			Description: "只读对比目标配置版本与该机房当前配置/拓扑的差异（含机房、交换机/节点增删改列表、受影响的设置段及是否可立即回滚），不修改任何状态且不暴露敏感字段。",
			Schema: obj(map[string]interface{}{
				"id":         str("机房档案 ID；省略则为当前激活机房"),
				"version_id": str("目标配置版本 ID"),
			}, "version_id"),
			Run: toolPreviewProfileVersionRollback,
		},
		{
			Name: "rollback_profile_version", Title: "回滚机房配置版本", Write: true, Destructive: true,
			Description: "将指定机房的拓扑与机房设置回滚到目标历史版本（回滚前会自动创建 pre_rollback 备份）。若为当前激活机房且正在切换/扫描/定位会返回冲突错误。必须传入 confirm=true。",
			Schema: obj(map[string]interface{}{
				"id":         str("机房档案 ID；省略则为当前激活机房"),
				"version_id": str("目标配置版本 ID"),
				"confirm":    boolean("必须为 true 才会执行回滚"),
			}, "version_id", "confirm"),
			Run: toolRollbackProfileVersion,
		},

		// ---- 维护窗口（仅作用于当前激活机房） ----
		{
			Name: "list_maintenance", Title: "查看维护窗口", Write: false,
			Description: "列出当前激活机房的全部告警维护窗口（含 active、scheduled、expired、cancelled 状态）。仅作用于当前激活机房。",
			Schema: obj(map[string]interface{}{
				"profile_id": str("可选，用于校验仅查询当前激活机房（若与当前激活机房不符会拒绝）"),
				"epoch":      integer("可选，用于校验机房版本 epoch 未切换"),
			}),
			Run: toolListMaintenance,
		},
		{
			Name: "create_maintenance", Title: "创建维护窗口", Write: true,
			Description: "为当前激活机房创建告警维护窗口（支持整站 site 或指定设备 devices 静默告警）。时间支持 RFC3339 或服务器本地时间（YYYY-MM-DD HH:MM[:SS] / YYYY-MM-DDTHH:MM[:SS]），原因必填且不超过 500 字。",
			Schema: obj(map[string]interface{}{
				"profile_id": str("可选，必须与当前激活机房一致"),
				"epoch":      integer("可选，必须与当前机房 epoch 一致"),
				"scope":      enum("维护范围：site=整站静默，devices=指定设备", "site", "devices"),
				"target_ids": strArr("scope=devices 时的设备 ID 列表（节点或交换机）"),
				"start":      str("开始时间（RFC3339 或本地时间如 2026-11-14T22:00）"),
				"end":        str("结束时间（需晚于 start 且晚于当前时间）"),
				"reason":     str("维护原因（必填，不超过 500 字）"),
			}, "scope", "start", "end", "reason"),
			Run: toolCreateMaintenance,
		},
		{
			Name: "cancel_maintenance", Title: "取消维护窗口", Write: true, Destructive: true,
			Description: "取消当前激活机房中的一个维护窗口。",
			Schema: obj(map[string]interface{}{
				"profile_id": str("可选，必须与当前激活机房一致"),
				"epoch":      integer("可选，必须与当前机房 epoch 一致"),
				"id":         str("维护窗口 ID"),
			}, "id"),
			Run: toolCancelMaintenance,
		},

		// ---- 运维报告（确定性统计 + 显式 AI 生成） ----
		{
			Name: "generate_health_report", Title: "生成确定性健康报告", Write: true,
			Description: "不调用 AI，直接由程序对当前激活机房近 24h / 7d / 30d 监控历史做确定性汇总并保存报告（无样本时 KPI 保持 null，保留历史不足/设备截断提示）。",
			Schema: obj(map[string]interface{}{
				"profile_id": str("可选，必须与当前激活机房一致"),
				"epoch":      integer("可选，必须与当前机房 epoch 一致"),
				"range":      enum("统计周期，默认 24h", "24h", "7d", "30d"),
			}),
			Run: toolGenerateHealthReport,
		},
		{
			Name: "generate_ai_report", Title: "生成 AI 运维报告", Write: true, OpenWorld: true,
			Description: "显式触发 AI 撰写当前激活机房的周期运维报告并保存（仅在主动调用本工具时运行 AI，不会在其他只读工具中隐式调用）。需已启用 AI 诊断与 API Key。",
			Schema: obj(map[string]interface{}{
				"profile_id": str("可选，必须与当前激活机房一致"),
				"epoch":      integer("可选，必须与当前机房 epoch 一致"),
				"range":      enum("统计周期，默认 24h", "24h", "7d", "30d"),
				"note":       str("给 AI 的额外报告要求（可选，最多 500 字）"),
			}),
			Run: toolGenerateAIReport,
		},
		{
			Name: "list_reports", Title: "列出运维报告", Write: false,
			Description: "列出当前激活机房已保存的运维报告摘要（最新在前，不含本地文件路径）。",
			Schema: obj(map[string]interface{}{
				"profile_id": str("可选，必须与当前激活机房一致"),
				"epoch":      integer("可选，必须与当前机房 epoch 一致"),
				"limit":      integer("最多返回多少条，默认 20，最大 100"),
			}),
			Run: toolListReports,
		},
		{
			Name: "get_report", Title: "查看/导出运维报告", Write: false,
			Description: "按报告 ID 获取当前激活机房的一份已保存运维报告，支持导出为 json、markdown 或 html 内容（不返回服务器本地文件路径）。",
			Schema: obj(map[string]interface{}{
				"profile_id": str("可选，必须与当前激活机房一致"),
				"epoch":      integer("可选，必须与当前机房 epoch 一致"),
				"id":         str("报告 ID（如 20261114-103000）"),
				"format":     enum("输出格式：json（默认）、markdown 或 html", "json", "markdown", "html"),
			}, "id"),
			Run: toolGetReport,
		},
		{
			Name: "delete_report", Title: "删除运维报告", Write: true, Destructive: true,
			Description: "按报告 ID 删除当前激活机房的一份已保存运维报告。",
			Schema: obj(map[string]interface{}{
				"profile_id": str("可选，必须与当前激活机房一致"),
				"epoch":      integer("可选，必须与当前机房 epoch 一致"),
				"id":         str("报告 ID"),
			}, "id"),
			Run: toolDeleteReport,
		},

		// ---- 设备锁定与清理 ----
		{
			Name: "set_device_locked", Title: "锁定/解锁终端设备", Write: true,
			Description: "设置终端节点的锁定状态（locked=true 时受保护，不会被设备清理误删）。仅支持终端节点，不能对交换机使用。",
			Schema: obj(map[string]interface{}{
				"profile_id": str("可选，必须与当前激活机房一致"),
				"epoch":      integer("可选，必须与当前机房 epoch 一致"),
				"id":         str("终端节点 ID"),
				"locked":     boolean("true 锁定，false 解锁"),
			}, "id", "locked"),
			Run: toolSetDeviceLocked,
		},
		{
			Name: "preview_device_cleanup", Title: "预检可清理设备", Write: false,
			Description: "只读列出当前激活机房中满足清理条件的未锁定终端节点（无 IP 或未启用监控）、受锁定保护数量，以及绑定的 profile_id、epoch 和 topology rev（供 cleanup_devices 校验）。",
			Schema:      obj(nil),
			Run:         toolPreviewDeviceCleanup,
		},
		{
			Name: "cleanup_devices", Title: "清理无效终端设备", Write: true, Destructive: true,
			Description: "按 preview_device_cleanup 返回的 profile_id、epoch 和设备 ID 列表清理无效终端节点。写入锁内会再次校验“当前激活机房+epoch匹配”及“未锁定且满足清理条件”，不会删除交换机或历史数据。必须传入 confirm=true。",
			Schema: obj(map[string]interface{}{
				"profile_id": str("预览返回的机房档案 ID（必填，防止跨机房误删）"),
				"epoch":      integer("预览返回的机房 epoch 版本号（必填）"),
				"ids":        strArr("要清理的终端节点 ID 列表（必填，请先调用 preview_device_cleanup 获取）"),
				"confirm":    boolean("必须为 true 才会执行清理"),
			}, "profile_id", "epoch", "ids", "confirm"),
			Run: toolCleanupDevices,
		},

		// ---- 插件只读查询 ----
		{
			Name: "get_plugins", Title: "查看插件状态与日志", Write: false,
			Description: "只读查看已安装插件、声明的 hooks/tools、脱敏后的设置摘要与结构化运行状态摘要（不返回插件本地目录、命令路径、任何密钥/敏感 URL，也不返回自由文本 message/error/stderr，运行详情请在网页插件页查看）。",
			Schema: obj(map[string]interface{}{
				"id":         str("可选，只查看指定插件 ID"),
				"runs_limit": integer("最多返回多少条近期运行记录，默认 20，最大 100"),
			}),
			Run: toolGetPlugins,
		},

		// ---- AI 一键优化与撤销 ----
		{
			Name: "generate_ai_optimize", Title: "生成 AI 一键优化建议", Write: true, OpenWorld: true,
			Description: "显式调用 AI 根据当前激活机房拓扑与近 7 天监控统计生成设备分类与配置优化建议（会发送设备摘要至已配置模型并产生调用费用，但不会自动修改拓扑），返回绑定 profile_id 与 epoch 的建议。",
			Schema: obj(map[string]interface{}{
				"profile_id": str("可选，必须与当前激活机房一致"),
				"epoch":      integer("可选，必须与当前机房 epoch 一致"),
			}),
			Run: toolGenerateAIOptimize,
		},
		{
			Name: "apply_ai_optimize", Title: "预检或应用 AI 优化建议", Write: true, Destructive: true,
			Description: "预检或应用已生成的 AI 设备分类与配置建议。dry_run 默认 true（仅返回变更计划）；真正修改拓扑必须传入 dry_run=false 且 confirm=true，且要求 profile_id 和 epoch 与当前激活机房严格匹配，返回用于撤销的 undo 快照。",
			Schema: obj(map[string]interface{}{
				"profile_id": str("生成建议时返回的机房档案 ID"),
				"epoch":      integer("生成建议时返回的机房 epoch 版本号"),
				"classify": map[string]interface{}{
					"type":        "array",
					"description": "要应用的分类项 [{id, category}]",
					"items": obj(map[string]interface{}{
						"id":       str("节点 ID"),
						"category": str("目标设备类别"),
					}, "id", "category"),
				},
				"config": map[string]interface{}{
					"type":        "array",
					"description": "要应用的配置建议项（原样传入 generate_ai_optimize 返回的 config 项）",
					"items":       map[string]interface{}{"type": "object"},
				},
				"dry_run": boolean("默认 true（仅预检变更计划，不写入）；传入 false 才真正修改"),
				"confirm": boolean("dry_run=false 时必须同时传 confirm=true 才会应用"),
			}, "profile_id", "epoch"),
			Run: toolApplyAIOptimize,
		},
		{
			Name: "undo_ai_optimize", Title: "撤销 AI 优化建议", Write: true, Destructive: true,
			Description: "使用 apply_ai_optimize 返回的 undo 快照恢复设备名称、标签、标记与监控参数。必须校验 profile_id、epoch 与当前激活机房匹配，且必须传入 confirm=true。",
			Schema: obj(map[string]interface{}{
				"profile_id": str("应用建议时的机房档案 ID"),
				"epoch":      integer("应用建议时的机房 epoch 版本号"),
				"restore": map[string]interface{}{
					"type":        "array",
					"description": "apply_ai_optimize 返回的 undo 快照数组",
					"items":       map[string]interface{}{"type": "object"},
				},
				"confirm": boolean("必须为 true 才会执行撤销"),
			}, "profile_id", "epoch", "restore", "confirm"),
			Run: toolUndoAIOptimize,
		},
	}
}

func argInt64(a map[string]json.RawMessage, k string) (int64, bool, error) {
	raw, ok := a[k]
	if !ok {
		return 0, false, nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return 0, false, nil
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, true, fmt.Errorf("参数 %s 必须为整数", k)
	}
	return v, true, nil
}

func (s *Server) requireProfilesReady() error {
	if s.Profiles == nil {
		return errors.New("机房配置管理器未初始化")
	}
	return nil
}

func (s *Server) resolveProfileID(a map[string]json.RawMessage) string {
	if id := argStr(a, "id"); id != "" {
		return id
	}
	return s.activeProfileID()
}

func parseOptionalBinding(a map[string]json.RawMessage) (string, *int64, error) {
	reqPID := argStr(a, "profile_id")
	ep, hasEp, err := argInt64(a, "epoch")
	if err != nil {
		return "", nil, err
	}
	if hasEp {
		return reqPID, &ep, nil
	}
	return reqPID, nil, nil
}

func (s *Server) checkActiveRoomBindingLocked(a map[string]json.RawMessage) (string, int64, error) {
	reqPID, reqEp, err := parseOptionalBinding(a)
	if err != nil {
		activeID, curEp := s.activeProfileState()
		return activeID, curEp, err
	}
	return s.checkActiveBindingLocked(reqPID, reqEp)
}

// ---- Profiles & Room Settings ----

func toolListProfiles(s *Server, _ map[string]json.RawMessage, _ string) (interface{}, error) {
	if err := s.requireProfilesReady(); err != nil {
		return nil, err
	}
	items := s.Profiles.List()
	activeID, epoch := s.activeProfileState()
	return map[string]interface{}{
		"active_profile_id": activeID,
		"epoch":             epoch,
		"profiles":          items,
	}, nil
}

func toolCreateProfile(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	if err := s.requireProfilesReady(); err != nil {
		return nil, err
	}
	name := argStr(a, "name")
	if name == "" {
		return nil, errors.New("需要提供机房名称 name")
	}
	copyCurrent, _ := argBool(a, "copy_current")
	sourceID := argStr(a, "source_id")
	info, err := s.Profiles.Create(name, copyCurrent, sourceID)
	if err != nil {
		return nil, mapProfileErr(err)
	}
	activeID, epoch := s.activeProfileState()
	log.Printf("MCP(%s) 创建机房档案 %s (%s)", remote, info.ID, info.Name)
	return map[string]interface{}{
		"profile":           info,
		"active_profile_id": activeID,
		"epoch":             epoch,
	}, nil
}

func toolRenameProfile(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	if err := s.requireProfilesReady(); err != nil {
		return nil, err
	}
	id := argStr(a, "id")
	name := argStr(a, "name")
	if id == "" || name == "" {
		return nil, errors.New("需要提供 id 和 name")
	}
	if err := s.Profiles.Rename(id, name); err != nil {
		return nil, mapProfileErr(err)
	}
	log.Printf("MCP(%s) 重命名机房档案 %s -> %s", remote, id, name)
	return map[string]interface{}{"ok": true, "id": id, "name": name}, nil
}

func toolDeleteProfile(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	if err := s.requireProfilesReady(); err != nil {
		return nil, err
	}
	id := argStr(a, "id")
	if id == "" {
		return nil, errors.New("需要提供机房档案 id")
	}
	confirm, _ := argBool(a, "confirm")
	if !confirm {
		return nil, errors.New("删除机房档案不可恢复，请传入 confirm=true 确认删除")
	}
	if err := s.Profiles.Delete(id); err != nil {
		return nil, mapProfileErr(err)
	}
	activeID, epoch := s.activeProfileState()
	log.Printf("MCP(%s) 删除机房档案 %s", remote, id)
	return map[string]interface{}{"ok": true, "deleted": id, "active_profile_id": activeID, "epoch": epoch}, nil
}

func toolPreviewProfile(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	if err := s.requireProfilesReady(); err != nil {
		return nil, err
	}
	id := argStr(a, "id")
	if id == "" {
		return nil, errors.New("需要提供机房档案 id")
	}
	probe, ok := argBool(a, "probe")
	if !ok {
		probe = false // 默认不主动发包
	}
	res, err := s.Profiles.Preview(id, probe)
	if err != nil {
		return nil, mapProfileErr(err)
	}
	return map[string]interface{}{
		"probe":   probe,
		"preview": res,
	}, nil
}

func toolActivateProfile(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	if err := s.requireProfilesReady(); err != nil {
		return nil, err
	}
	id := argStr(a, "id")
	if id == "" {
		return nil, errors.New("需要提供目标机房档案 id")
	}
	confirm, _ := argBool(a, "confirm")
	if !confirm {
		return nil, errors.New("切换机房会重载监控与告警上下文，请先预览并传入 confirm=true 确认切换")
	}
	if err := s.Profiles.Activate(id); err != nil {
		return nil, mapProfileErr(err)
	}
	activeID, epoch := s.activeProfileState()
	log.Printf("MCP(%s) 切换激活机房为 %s (epoch=%d)", remote, activeID, epoch)
	return map[string]interface{}{
		"ok":                true,
		"active_profile_id": activeID,
		"epoch":             epoch,
	}, nil
}

func toolGetProfileSettings(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	if err := s.requireProfilesReady(); err != nil {
		return nil, err
	}
	id := s.resolveProfileID(a)
	rs, meta, active, err := s.Profiles.GetSettings(id)
	if err != nil {
		return nil, mapProfileErr(err)
	}
	_, epoch := s.activeProfileState()
	return map[string]interface{}{
		"id":       meta.ID,
		"name":     meta.Name,
		"active":   active,
		"epoch":    epoch,
		"settings": config.MaskRoomSettings(rs),
	}, nil
}

func toolSetProfileSettings(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	if err := s.requireProfilesReady(); err != nil {
		return nil, err
	}
	defer s.Topo.ProfileWork()()

	id := argStr(a, "id")
	if id == "" {
		return nil, errors.New("需要提供机房档案 id")
	}
	raw, ok := a["settings"]
	if !ok || strings.TrimSpace(string(raw)) == "" || strings.TrimSpace(string(raw)) == "null" {
		return nil, errors.New("需要提供 settings 补丁对象")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil || top == nil {
		return nil, errors.New("settings 必须是 JSON 对象")
	}
	for k := range top {
		lk := strings.ToLower(strings.TrimSpace(k))
		if forbiddenRoomSettingKeys[lk] {
			return nil, fmt.Errorf("禁止通过机房配置修改全局字段 %q（仅允许 schedule、discovery、locate、ping、alert）", k)
		}
		switch lk {
		case "schedule", "discovery", "locate", "ping", "alert":
		default:
			return nil, fmt.Errorf("未知或不允许的机房设置段 %q（仅允许 schedule、discovery、locate、ping、alert）", k)
		}
	}
	rs, err := s.Profiles.UpdateSettings(id, raw)
	if err != nil {
		return nil, mapProfileErr(err)
	}
	log.Printf("MCP(%s) 修改机房 %s 的设置", remote, id)
	return map[string]interface{}{
		"ok":       true,
		"id":       id,
		"settings": config.MaskRoomSettings(rs),
	}, nil
}

// ---- Profile Config Versions ----

func toolListProfileVersions(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	if err := s.requireProfilesReady(); err != nil {
		return nil, err
	}
	id := s.resolveProfileID(a)
	list, err := s.Profiles.ListVersions(id)
	if err != nil {
		return nil, mapProfileErr(err)
	}
	return map[string]interface{}{
		"profile_id": id,
		"versions":   list,
	}, nil
}

func toolCreateProfileVersion(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	if err := s.requireProfilesReady(); err != nil {
		return nil, err
	}
	id := s.resolveProfileID(a)
	note := argStr(a, "note")
	meta, err := s.Profiles.CreateVersion(id, "manual", note)
	if err != nil {
		return nil, mapProfileErr(err)
	}
	log.Printf("MCP(%s) 为机房 %s 创建配置版本 %s", remote, id, meta.ID)
	return map[string]interface{}{
		"ok":         true,
		"profile_id": id,
		"version":    meta,
	}, nil
}

func toolGetProfileVersion(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	if err := s.requireProfilesReady(); err != nil {
		return nil, err
	}
	id := s.resolveProfileID(a)
	verID := argStr(a, "version_id")
	if verID == "" {
		return nil, errors.New("需要提供 version_id")
	}
	snap, err := s.Profiles.GetVersion(id, verID)
	if err != nil {
		return nil, mapProfileErr(err)
	}
	return map[string]interface{}{
		"profile_id": id,
		"meta":       snap.Meta,
		"topology":   snap.Topology,
		"settings":   config.MaskRoomSettings(snap.Settings),
	}, nil
}

func toolPreviewProfileVersionRollback(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	if err := s.requireProfilesReady(); err != nil {
		return nil, err
	}
	id := s.resolveProfileID(a)
	verID := argStr(a, "version_id")
	if verID == "" {
		return nil, errors.New("需要提供 version_id")
	}
	preview, err := s.Profiles.PreviewRollback(id, verID)
	if err != nil {
		return nil, mapProfileErr(err)
	}
	return preview, nil
}

func toolRollbackProfileVersion(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	if err := s.requireProfilesReady(); err != nil {
		return nil, err
	}
	id := s.resolveProfileID(a)
	verID := argStr(a, "version_id")
	if verID == "" {
		return nil, errors.New("需要提供 version_id")
	}
	confirm, _ := argBool(a, "confirm")
	if !confirm {
		return nil, errors.New("回滚配置版本会覆盖该机房当前拓扑与设置，请先预检差异并传入 confirm=true 确认回滚")
	}
	restored, backup, err := s.Profiles.RollbackVersion(id, verID)
	if err != nil {
		return nil, mapProfileErr(err)
	}
	activeID, epoch := s.activeProfileState()
	log.Printf("MCP(%s) 回滚机房 %s 至版本 %s（自动备份 %s）", remote, id, restored.ID, backup.ID)
	return map[string]interface{}{
		"ok":                true,
		"profile_id":        id,
		"restored_version":  restored.ID,
		"backup_version":    backup.ID,
		"active_profile_id": activeID,
		"epoch":             epoch,
	}, nil
}

// ---- Maintenance Windows ----

func parseMaintenanceTime(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, errors.New("时间不能为空")
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, nil
		}
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("时间格式 %q 无效（请使用 RFC3339 或 YYYY-MM-DD HH:MM）", raw)
}

func toolListMaintenance(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	defer s.Topo.ProfileWork()()

	activeID, epoch, err := s.checkActiveRoomBindingLocked(a)
	if err != nil {
		return nil, err
	}
	windows := []alert.MaintenanceWindow{}
	if s.Alerts != nil {
		windows = s.Alerts.MaintenanceWindows(time.Now())
	}
	return map[string]interface{}{
		"profile_id": activeID,
		"epoch":      epoch,
		"windows":    windows,
	}, nil
}

func toolCreateMaintenance(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	defer s.Topo.ProfileWork()()

	activeID, epoch, err := s.checkActiveRoomBindingLocked(a)
	if err != nil {
		return nil, err
	}
	if s.Alerts == nil {
		return nil, errors.New("告警引擎未初始化")
	}

	scope := argStr(a, "scope")
	if scope != "site" && scope != "devices" {
		return nil, errors.New("维护范围 scope 只能是 site 或 devices")
	}
	reason := argStr(a, "reason")
	if reason == "" {
		return nil, errors.New("维护原因 reason 不能为空")
	}
	if utf8.RuneCountInString(reason) > 500 {
		return nil, errors.New("维护原因 reason 不能超过 500 字")
	}
	start, err := parseMaintenanceTime(argStr(a, "start"))
	if err != nil {
		return nil, fmt.Errorf("开始时间 start 无效: %w", err)
	}
	end, err := parseMaintenanceTime(argStr(a, "end"))
	if err != nil {
		return nil, fmt.Errorf("结束时间 end 无效: %w", err)
	}
	if !end.After(start) || !end.After(time.Now()) {
		return nil, errors.New("维护起止时间无效或已过期（end 必须晚于 start 和当前时间）")
	}
	var targetIDs []string
	if scope == "devices" {
		targetIDs = argStrings(a, "target_ids")
		if len(targetIDs) == 0 {
			return nil, errors.New("scope=devices 时必须提供至少一个 target_ids")
		}
	}
	created, err := s.Alerts.AddMaintenance(alert.MaintenanceWindow{
		Scope:     scope,
		TargetIDs: targetIDs,
		Start:     start,
		End:       end,
		Reason:    reason,
	})
	if err != nil {
		return nil, err
	}
	log.Printf("MCP(%s) 创建维护窗口 %s (profile=%s, epoch=%d, scope=%s)", remote, created.ID, activeID, epoch, created.Scope)
	return map[string]interface{}{
		"ok":         true,
		"profile_id": activeID,
		"epoch":      epoch,
		"window":     created,
	}, nil
}

func toolCancelMaintenance(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	defer s.Topo.ProfileWork()()

	activeID, epoch, err := s.checkActiveRoomBindingLocked(a)
	if err != nil {
		return nil, err
	}
	if s.Alerts == nil {
		return nil, errors.New("告警引擎未初始化")
	}

	id := argStr(a, "id")
	if id == "" {
		return nil, errors.New("需要提供维护窗口 id")
	}
	if err := s.Alerts.CancelMaintenance(id); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("维护窗口 %q 不存在（或不属于当前激活机房）", id)
		}
		return nil, err
	}
	log.Printf("MCP(%s) 取消维护窗口 %s (profile=%s, epoch=%d)", remote, id, activeID, epoch)
	return map[string]interface{}{
		"ok":         true,
		"profile_id": activeID,
		"epoch":      epoch,
		"cancelled":  id,
	}, nil
}

// ---- Operations Reports ----

func toolGenerateHealthReport(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	reqPID, reqEpoch, err := parseOptionalBinding(a)
	if err != nil {
		return nil, err
	}
	rg := argStr(a, "range")
	if rg == "" {
		rg = "24h"
	}
	rp, d, activeID, epoch, err := s.generateHealthReportWithBinding(rg, reqPID, reqEpoch)
	if err != nil {
		return nil, err
	}
	log.Printf("MCP(%s) 生成确定性健康报告 %s (range=%s, profile=%s, epoch=%d)", remote, rp.ID, rp.Range, activeID, epoch)
	return map[string]interface{}{
		"ok":         true,
		"profile_id": activeID,
		"epoch":      epoch,
		"notices": map[string]interface{}{
			"no_data":         d.Minutes == 0,
			"retention_short": d.RetentionShort,
			"truncated":       d.Truncated,
		},
		"report": rp,
	}, nil
}

func toolGenerateAIReport(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	reqPID, reqEpoch, err := parseOptionalBinding(a)
	if err != nil {
		return nil, err
	}
	rg := argStr(a, "range")
	if rg == "" {
		rg = "24h"
	}
	note := argStr(a, "note")
	rp, _, _, _, _, boundID, boundEpoch, err := s.generateAIReportWithBinding(context.Background(), reqPID, reqEpoch, rg, note, "MCP/"+remote, nil)
	if err != nil {
		if rp != nil {
			return nil, fmt.Errorf("报告已生成但保存失败：%w", err)
		}
		return nil, err
	}
	log.Printf("MCP(%s) 生成 AI 运维报告 %s (range=%s, profile=%s, epoch=%d)", remote, rp.ID, rp.Range, boundID, boundEpoch)
	return map[string]interface{}{
		"ok":         true,
		"profile_id": boundID,
		"epoch":      boundEpoch,
		"report":     rp,
	}, nil
}

func toolListReports(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	defer s.Topo.ProfileWork()()

	activeID, epoch, err := s.checkActiveRoomBindingLocked(a)
	if err != nil {
		return nil, err
	}
	limit := argInt(a, "limit", 20)
	if limit < 1 {
		limit = 1
	}
	if limit > 100 {
		limit = 100
	}
	return map[string]interface{}{
		"profile_id": activeID,
		"epoch":      epoch,
		"reports":    s.listReportSummaries(limit),
	}, nil
}

func toolGetReport(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	defer s.Topo.ProfileWork()()

	activeID, epoch, err := s.checkActiveRoomBindingLocked(a)
	if err != nil {
		return nil, err
	}
	id := argStr(a, "id")
	if id == "" {
		return nil, errors.New("需要提供报告 id")
	}
	format := argStr(a, "format")
	if format == "" {
		format = "json"
	}
	rp, err := s.loadReport(id)
	if err != nil {
		return nil, err
	}
	switch format {
	case "json":
		return map[string]interface{}{
			"profile_id": activeID,
			"epoch":      epoch,
			"format":     "json",
			"report":     rp,
		}, nil
	case "markdown":
		return map[string]interface{}{
			"profile_id": activeID,
			"epoch":      epoch,
			"id":         rp.ID,
			"title":      rp.Title,
			"format":     "markdown",
			"content":    exportReportMarkdown(rp),
		}, nil
	case "html":
		return map[string]interface{}{
			"profile_id": activeID,
			"epoch":      epoch,
			"id":         rp.ID,
			"title":      rp.Title,
			"format":     "html",
			"content":    exportReportHTML(rp),
		}, nil
	default:
		return nil, fmt.Errorf("不支持的报告格式 %q（只能是 json、markdown 或 html）", format)
	}
}

func toolDeleteReport(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	defer s.Topo.ProfileWork()()

	activeID, epoch, err := s.checkActiveRoomBindingLocked(a)
	if err != nil {
		return nil, err
	}
	id := argStr(a, "id")
	if id == "" {
		return nil, errors.New("需要提供报告 id")
	}
	if err := s.deleteReport(id); err != nil {
		return nil, err
	}
	log.Printf("MCP(%s) 删除运维报告 %s (profile=%s, epoch=%d)", remote, id, activeID, epoch)
	return map[string]interface{}{
		"ok":         true,
		"profile_id": activeID,
		"epoch":      epoch,
		"deleted":    id,
	}, nil
}

// ---- Device Lock & Cleanup ----

func toolSetDeviceLocked(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	defer s.Topo.ProfileWork()()

	activeID, epoch, err := s.checkActiveRoomBindingLocked(a)
	if err != nil {
		return nil, err
	}
	id := argStr(a, "id")
	if id == "" {
		return nil, errors.New("需要提供节点 id")
	}
	locked, ok := argBool(a, "locked")
	if !ok {
		return nil, errors.New("需要提供 locked（true/false）")
	}
	if err := s.setNodeLocked(id, locked); err != nil {
		return nil, err
	}
	log.Printf("MCP(%s) 设置节点 %s locked=%v (profile=%s, epoch=%d)", remote, id, locked, activeID, epoch)
	return map[string]interface{}{
		"ok":         true,
		"profile_id": activeID,
		"epoch":      epoch,
		"id":         id,
		"locked":     locked,
	}, nil
}

func toolPreviewDeviceCleanup(s *Server, _ map[string]json.RawMessage, _ string) (interface{}, error) {
	defer s.Topo.ProfileWork()()

	activeID, epoch := s.activeProfileState()
	items, protected, rev := s.previewNodeCleanup()
	return map[string]interface{}{
		"profile_id": activeID,
		"epoch":      epoch,
		"rev":        rev,
		"items":      items,
		"count":      len(items),
		"protected":  protected,
	}, nil
}

func toolCleanupDevices(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	defer s.Topo.ProfileWork()()

	pid := argStr(a, "profile_id")
	epoch, hasEpoch, err := argInt64(a, "epoch")
	if err != nil {
		return nil, err
	}
	if pid == "" || !hasEpoch {
		return nil, errors.New("必须提供 preview_device_cleanup 返回的 profile_id 和 epoch 以防止跨机房误删")
	}
	confirm, _ := argBool(a, "confirm")
	if !confirm {
		return nil, errors.New("清理设备会永久删除节点记录，请传入 confirm=true 确认执行")
	}
	activeID, curEpoch, err := s.checkActiveBindingLocked(pid, &epoch)
	if err != nil {
		return nil, err
	}
	ids := argStrings(a, "ids")
	deleted, skipped, err := s.executeNodeCleanup(ids, "MCP/"+remote)
	if err != nil {
		return nil, err
	}
	_, rev := s.Topo.Snapshot()
	return map[string]interface{}{
		"ok":         true,
		"profile_id": activeID,
		"epoch":      curEpoch,
		"rev":        rev,
		"deleted":    deleted,
		"changed":    len(deleted),
		"skipped":    skipped,
	}, nil
}

// ---- Plugin Read-Only View ----

func toolGetPlugins(s *Server, a map[string]json.RawMessage, _ string) (interface{}, error) {
	id := argStr(a, "id")
	limit := argInt(a, "runs_limit", 20)
	if limit < 1 {
		limit = 1
	}
	if limit > 100 {
		limit = 100
	}
	view, found := s.Plugins.MCPView(id, limit)
	if id != "" && !found {
		return nil, fmt.Errorf("插件 %q 不存在", id)
	}
	return view, nil
}

// ---- AI One-Click Optimize & Undo ----

func toolGenerateAIOptimize(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	reqPID, reqEpoch, err := parseOptionalBinding(a)
	if err != nil {
		return nil, err
	}
	out, usage, calls, ms, model, err := s.generateOptimizeSuggestionsWithBinding(context.Background(), reqPID, reqEpoch, "MCP/"+remote, nil)
	if err != nil {
		return nil, err
	}
	out["model"] = model
	out["tools"] = calls
	out["ms"] = ms
	out["usage"] = usage
	return out, nil
}

func toolApplyAIOptimize(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	pid := argStr(a, "profile_id")
	epoch, hasEpoch, err := argInt64(a, "epoch")
	if err != nil {
		return nil, err
	}
	if pid == "" || !hasEpoch {
		return nil, errors.New("必须提供 profile_id 和 epoch 以绑定目标机房与版本")
	}
	dryRun, ok := argBool(a, "dry_run")
	if !ok {
		dryRun = true
	}
	confirm, _ := argBool(a, "confirm")
	if !dryRun && !confirm {
		return nil, errors.New("应用 AI 优化建议会直接修改拓扑配置，请传入 dry_run=false 和 confirm=true")
	}

	req := optApplyReq{
		ProfileID: pid,
		Epoch:     &epoch,
	}
	if raw, ok := a["classify"]; ok && strings.TrimSpace(string(raw)) != "" && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &req.Classify); err != nil {
			return nil, fmt.Errorf("classify 格式无效：%w", err)
		}
	}
	if raw, ok := a["config"]; ok && strings.TrimSpace(string(raw)) != "" && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &req.Config); err != nil {
			return nil, fmt.Errorf("config 格式无效：%w", err)
		}
	}
	res, err := s.applyOptimizeSuggestions(req, dryRun, true, "MCP/"+remote)
	if err != nil {
		return nil, err
	}
	return res, nil
}

func toolUndoAIOptimize(s *Server, a map[string]json.RawMessage, remote string) (interface{}, error) {
	pid := argStr(a, "profile_id")
	epoch, hasEpoch, err := argInt64(a, "epoch")
	if err != nil {
		return nil, err
	}
	if pid == "" || !hasEpoch {
		return nil, errors.New("必须提供 profile_id 和 epoch 以校验当前机房与版本")
	}
	confirm, _ := argBool(a, "confirm")
	if !confirm {
		return nil, errors.New("撤销 AI 优化会恢复设备配置，请传入 confirm=true 确认执行")
	}
	raw, ok := a["restore"]
	if !ok || strings.TrimSpace(string(raw)) == "" || strings.TrimSpace(string(raw)) == "null" {
		return nil, errors.New("需要提供 restore 快照列表（来自 apply_ai_optimize 返回的 undo 字段）")
	}
	var restore []devSnap
	if err := json.Unmarshal(raw, &restore); err != nil {
		return nil, fmt.Errorf("restore 格式无效：%w", err)
	}
	if len(restore) == 0 {
		return nil, errors.New("restore 快照列表不能为空")
	}
	req := optApplyReq{
		ProfileID: pid,
		Epoch:     &epoch,
		Restore:   restore,
	}
	res, err := s.applyOptimizeSuggestions(req, false, true, "MCP/"+remote)
	if err != nil {
		return nil, err
	}
	return res, nil
}
