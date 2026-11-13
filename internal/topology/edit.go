package topology

// 供网页 API 与 MCP 共用的拓扑编辑操作（在 Store.Update 的副本上调用）。

// RenameSwitchRefs 交换机改 id 后同步更新节点和链路中的引用。
func (t *Topology) RenameSwitchRefs(oldID, newID string) {
	if oldID == newID {
		return
	}
	for _, n := range t.Nodes {
		if n.Attach.Switch == oldID {
			n.Attach.Switch = newID
		}
	}
	for _, l := range t.Links {
		if l.From.Switch == oldID {
			l.From.Switch = newID
		}
		if l.To.Switch == oldID {
			l.To.Switch = newID
		}
	}
}

// DeleteNode 删除节点，返回是否存在。
func (t *Topology) DeleteNode(id string) bool {
	for i, x := range t.Nodes {
		if x.ID == id {
			t.Nodes = append(t.Nodes[:i], t.Nodes[i+1:]...)
			return true
		}
	}
	return false
}

// DeleteLink 删除链路，返回是否存在。
func (t *Topology) DeleteLink(id string) bool {
	for i, x := range t.Links {
		if x.ID == id {
			t.Links = append(t.Links[:i], t.Links[i+1:]...)
			return true
		}
	}
	return false
}

// DeleteSwitch 删除交换机：下挂节点变为未定位，相关链路一并删除。返回被解除接线的节点数。
func (t *Topology) DeleteSwitch(id string) (detached int, ok bool) {
	idx := -1
	for i, x := range t.Switches {
		if x.ID == id {
			idx = i
		}
	}
	if idx < 0 {
		return 0, false
	}
	t.Switches = append(t.Switches[:idx], t.Switches[idx+1:]...)
	for _, n := range t.Nodes {
		if n.Attach.Switch == id {
			n.Attach = Endpoint{}
			if !n.HasFlag("unlocated") {
				n.Flags = append(n.Flags, "unlocated")
			}
			detached++
		}
	}
	links := t.Links[:0]
	for _, l := range t.Links {
		if l.From.Switch != id && l.To.Switch != id {
			links = append(links, l)
		}
	}
	t.Links = links
	return detached, true
}

// SetAlert 批量开关设备（节点或交换机）告警，返回实际变更数与未找到的 id。
func (t *Topology) SetAlert(ids []string, enabled bool) (changed int, missing []string) {
	for _, id := range ids {
		if n := t.FindNode(id); n != nil {
			if n.Monitor.Alert != enabled {
				n.Monitor.Alert = enabled
				changed++
			}
		} else if sw := t.FindSwitch(id); sw != nil {
			if sw.Monitor.Alert != enabled {
				sw.Monitor.Alert = enabled
				changed++
			}
		} else {
			missing = append(missing, id)
		}
	}
	return changed, missing
}

// FindByIP 按 IP 查找节点或交换机，返回 kind（node/switch）与 id。
func (t *Topology) FindByIP(ip string) (kind, id string) {
	for _, n := range t.Nodes {
		if n.IP == ip {
			return "node", n.ID
		}
	}
	for _, s := range t.Switches {
		if s.IP == ip {
			return "switch", s.ID
		}
	}
	return "", ""
}
