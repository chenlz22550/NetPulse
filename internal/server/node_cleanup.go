package server

import (
	"log"
	"net/http"
	"strings"

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

type cleanupItem struct {
	ID     string `json:"id"`
	IP     string `json:"ip"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

func (s *Server) previewNodeCleanup() ([]cleanupItem, int, int64) {
	t, rev := s.Topo.Snapshot()
	items := []cleanupItem{}
	protected := 0
	for _, n := range t.Nodes {
		if n.Locked {
			protected++
			continue
		}
		if reason := cleanupReason(n); reason != "" {
			items = append(items, cleanupItem{
				ID:     n.ID,
				IP:     n.IP,
				Name:   n.DisplayName(),
				Reason: reason,
			})
		}
	}
	return items, protected, rev
}

func (s *Server) executeNodeCleanup(ids []string, source string) ([]string, int, error) {
	if len(ids) == 0 {
		return nil, 0, badRequest("请先预览并选择要清理的设备")
	}
	selected := make(map[string]bool, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" {
			selected[id] = true
		}
	}
	if len(selected) == 0 {
		return nil, 0, badRequest("请先预览并选择要清理的设备")
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
		return nil, 0, err
	}
	skipped := len(selected) - len(deleted)
	log.Printf("设备清理（来源 %s）：删除 %d 台，跳过 %d 台，节点 %v", source, len(deleted), skipped, deleted)
	return deleted, skipped, nil
}

func (s *Server) setNodeLocked(id string, locked bool) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return badRequest("需要提供节点 id")
	}
	return s.Topo.Update(func(t *topology.Topology) error {
		if t.FindSwitch(id) != nil {
			return badRequest("仅终端节点支持锁定，不能对交换机使用")
		}
		n := t.FindNode(id)
		if n == nil {
			return notFound("节点 " + id)
		}
		n.Locked = locked
		return nil
	})
}

func (s *Server) handleNodeCleanupPreview(w http.ResponseWriter) {
	items, protected, _ := s.previewNodeCleanup()
	writeJSON(w, map[string]interface{}{"items": items, "protected": protected})
}

func (s *Server) handleNodeCleanup(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	deleted, skipped, err := s.executeNodeCleanup(req.IDs, clientAddr(r))
	if err != nil {
		return err
	}
	s.okRev(w, map[string]interface{}{"deleted": deleted, "changed": len(deleted), "skipped": skipped})
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
	if err := s.setNodeLocked(id, *req.Locked); err != nil {
		return err
	}
	s.okRev(w, map[string]interface{}{"locked": *req.Locked})
	return nil
}
