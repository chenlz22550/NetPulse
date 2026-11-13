package server

import (
	"log"
	"net/http"

	"netping/internal/topology"
)

// cleanupReason 只检查登记信息；离线、监控暂停和尚无采样均不作为删除依据。
func cleanupReason(n *topology.Node) string {
	if n.IP == "" {
		return "无 IP"
	}
	if !n.Monitor.IsEnabled() {
		return "未启用监控"
	}
	return ""
}

func (s *Server) handleNodeCleanupPreview(w http.ResponseWriter) {
	t, _ := s.Topo.Snapshot()
	items := []map[string]string{}
	protected := 0
	for _, n := range t.Nodes {
		if n.Locked {
			protected++
			continue
		}
		if reason := cleanupReason(n); reason != "" {
			items = append(items, map[string]string{"id": n.ID, "ip": n.IP, "name": n.DisplayName(), "reason": reason})
		}
	}
	writeJSON(w, map[string]interface{}{"items": items, "protected": protected})
}

func (s *Server) handleNodeCleanup(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	if len(req.IDs) == 0 {
		return badRequest("请先预览并选择要清理的设备")
	}
	selected := make(map[string]bool, len(req.IDs))
	for _, id := range req.IDs {
		selected[id] = true
	}
	deleted := []string{}
	err := s.Topo.Update(func(t *topology.Topology) error {
		kept := make([]*topology.Node, 0, len(t.Nodes))
		for _, n := range t.Nodes {
			// 与上锁操作共用 Store 的写锁：预览后上锁或恢复有效的设备也会保留。
			if selected[n.ID] && !n.Locked && cleanupReason(n) != "" {
				deleted = append(deleted, n.ID)
			} else {
				kept = append(kept, n)
			}
		}
		t.Nodes = kept
		return nil
	})
	if err != nil {
		return err
	}
	log.Printf("设备清理（来源 %s）：删除 %d 台，跳过 %d 台，节点 %v", clientAddr(r), len(deleted), len(selected)-len(deleted), deleted)
	s.okRev(w, map[string]interface{}{"deleted": deleted, "changed": len(deleted), "skipped": len(selected) - len(deleted)})
	return nil
}

func (s *Server) handleNodeLock(w http.ResponseWriter, r *http.Request, id string) error {
	var req struct {
		Locked *bool `json:"locked"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	if req.Locked == nil {
		return badRequest("需要提供 locked（true/false）")
	}
	if err := s.Topo.Update(func(t *topology.Topology) error {
		n := t.FindNode(id)
		if n == nil {
			return notFound("节点 " + id)
		}
		n.Locked = *req.Locked
		return nil
	}); err != nil {
		return err
	}
	s.okRev(w, map[string]interface{}{"locked": *req.Locked})
	return nil
}
