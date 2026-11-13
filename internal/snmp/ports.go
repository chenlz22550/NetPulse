package snmp

import (
	"sort"
	"time"
)

// 端口状态与计数（IF-MIB / EtherLike-MIB），用于核实“端口 down、速率/双工异常、CRC 错误、拥塞、交换机重启”等判断。

var (
	oidIfEntry       = ParseOID("1.3.6.1.2.1.2.2.1")
	oidSysUpTime     = ParseOID("1.3.6.1.2.1.1.3.0")
	oidIfSpeed       = ParseOID("1.3.6.1.2.1.2.2.1.5")
	oidIfAdmin       = ParseOID("1.3.6.1.2.1.2.2.1.7")
	oidIfOper        = ParseOID("1.3.6.1.2.1.2.2.1.8")
	oidIfLastChange  = ParseOID("1.3.6.1.2.1.2.2.1.9")
	oidIfInOctets    = ParseOID("1.3.6.1.2.1.2.2.1.10")
	oidIfInDiscards  = ParseOID("1.3.6.1.2.1.2.2.1.13")
	oidIfInErrors    = ParseOID("1.3.6.1.2.1.2.2.1.14")
	oidIfOutOctets   = ParseOID("1.3.6.1.2.1.2.2.1.16")
	oidIfOutDiscards = ParseOID("1.3.6.1.2.1.2.2.1.19")
	oidIfOutErrors   = ParseOID("1.3.6.1.2.1.2.2.1.20")
	oidIfHCInOctets  = ParseOID("1.3.6.1.2.1.31.1.1.1.6")
	oidIfHCOutOctets = ParseOID("1.3.6.1.2.1.31.1.1.1.10")
	oidIfHighSpeed   = ParseOID("1.3.6.1.2.1.31.1.1.1.15")
	oidIfAlias       = ParseOID("1.3.6.1.2.1.31.1.1.1.18")
	oidDot3FCSErrors = ParseOID("1.3.6.1.2.1.10.7.2.1.3")
	oidDot3Duplex    = ParseOID("1.3.6.1.2.1.10.7.2.1.19")
)

// PortStat 是一个端口某一时刻的状态与累计计数。
type PortStat struct {
	IfIndex    int
	Name       string
	Alias      string
	Admin      int   // 1 up、2 down、3 testing
	Oper       int   // 1 up、2 down、3 testing、5 dormant、6 notPresent、7 lowerLayerDown
	SpeedMbps  int64 // 0 表示未知
	Duplex     int   // 0 设备未提供、1 unknown、2 半双工、3 全双工
	LastChange int64 // 进入当前状态时的 sysUpTime（1/100 秒）；0 表示开机以来未变化
	InOctets   uint64
	OutOctets  uint64
	HCOctets   bool // 字节计数来自 64 位计数器
	InErrors   int64
	OutErrors  int64
	InDiscards int64
	OutDiscard int64
	FCSErrors  int64
	HasFCS     bool
}

// PortSnapshot 是一次读取的结果。
type PortSnapshot struct {
	At     time.Time
	UpTime int64 // sysUpTime（1/100 秒）
	Ports  map[int]*PortStat
	Order  []int // ifIndex 升序
}

// portColumns 每个端口要读取的列。
var portColumns = []OID{
	oidIfSpeed, oidIfAdmin, oidIfOper, oidIfLastChange, oidIfInOctets, oidIfInDiscards, oidIfInErrors,
	oidIfOutOctets, oidIfOutDiscards, oidIfOutErrors, oidIfHCInOctets, oidIfHCOutOctets, oidIfHighSpeed,
	oidIfAlias, oidDot3FCSErrors, oidDot3Duplex,
}

func present(v Var) bool {
	return v.Type != TypeNull && v.Type != TypeNoSuchObj && v.Type != TypeNoSuchInst && v.Type != TypeEndOfMib
}

func (p *PortStat) set(col OID, v Var) {
	if !present(v) {
		return
	}
	switch {
	case col.cmp(oidIfSpeed) == 0:
		if p.SpeedMbps == 0 && v.Int > 0 {
			p.SpeedMbps = v.Int / 1000000
		}
	case col.cmp(oidIfHighSpeed) == 0:
		if v.Int > 0 {
			p.SpeedMbps = v.Int // ifHighSpeed 优先（单位 Mbps，万兆口 ifSpeed 会溢出）
		}
	case col.cmp(oidIfAdmin) == 0:
		p.Admin = int(v.Int)
	case col.cmp(oidIfOper) == 0:
		p.Oper = int(v.Int)
	case col.cmp(oidIfLastChange) == 0:
		p.LastChange = v.Int
	case col.cmp(oidIfInOctets) == 0:
		if !p.HCOctets {
			p.InOctets = uint64(v.Int)
		}
	case col.cmp(oidIfOutOctets) == 0:
		if !p.HCOctets {
			p.OutOctets = uint64(v.Int)
		}
	case col.cmp(oidIfHCInOctets) == 0:
		p.InOctets, p.HCOctets = uint64(v.Int), true
	case col.cmp(oidIfHCOutOctets) == 0:
		p.OutOctets, p.HCOctets = uint64(v.Int), true
	case col.cmp(oidIfInErrors) == 0:
		p.InErrors = v.Int
	case col.cmp(oidIfOutErrors) == 0:
		p.OutErrors = v.Int
	case col.cmp(oidIfInDiscards) == 0:
		p.InDiscards = v.Int
	case col.cmp(oidIfOutDiscards) == 0:
		p.OutDiscard = v.Int
	case col.cmp(oidIfAlias) == 0:
		p.Alias = v.String()
	case col.cmp(oidDot3FCSErrors) == 0:
		p.FCSErrors, p.HasFCS = v.Int, true
	case col.cmp(oidDot3Duplex) == 0:
		p.Duplex = int(v.Int)
	}
}

// ReadPorts 读取端口状态与计数。keep 非空时只保留返回 true 的端口（参数为 ifIndex 与端口名）。
// 选中的端口不多时逐个 Get，否则按列遍历。
func (c *Client) ReadPorts(keep func(ifIndex int, name string) bool) (*PortSnapshot, error) {
	snap := &PortSnapshot{At: time.Now(), Ports: map[int]*PortStat{}}
	vars, err := c.Get(oidSysUpTime)
	if err != nil {
		return nil, err
	}
	if len(vars) > 0 && present(vars[0]) {
		snap.UpTime = vars[0].Int
	}
	names := map[int]string{}
	collect := func(root OID) error {
		return c.Walk(root, func(v Var) bool {
			if len(v.OID) == len(root)+1 {
				if n := v.String(); n != "" {
					names[int(v.OID[len(root)])] = n
				}
			}
			return true
		})
	}
	if err := collect(OidIfName); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		if err := collect(OidIfDescr); err != nil {
			return nil, err
		}
	}
	for idx, n := range names {
		if keep == nil || keep(idx, n) {
			snap.Ports[idx] = &PortStat{IfIndex: idx, Name: n}
			snap.Order = append(snap.Order, idx)
		}
	}
	sort.Ints(snap.Order)
	if len(snap.Order) == 0 {
		return snap, nil
	}
	if len(snap.Order) <= 6 {
		for _, idx := range snap.Order {
			oids := make([]OID, len(portColumns))
			for i, col := range portColumns {
				oids[i] = append(append(OID{}, col...), uint32(idx))
			}
			vars, err := c.Get(oids...)
			if err != nil {
				return nil, err
			}
			for i, v := range vars {
				if i < len(portColumns) {
					snap.Ports[idx].set(portColumns[i], v)
				}
			}
		}
		return snap, nil
	}
	for _, col := range portColumns {
		col := col
		err := c.Walk(col, func(v Var) bool {
			if len(v.OID) == len(col)+1 {
				if p := snap.Ports[int(v.OID[len(col)])]; p != nil {
					p.set(col, v)
				}
			}
			return true
		})
		if err != nil && col.HasPrefix(oidIfEntry) {
			return nil, err // 基本列读不到才算失败；扩展 MIB（64 位计数、别名、双工）读取出错时忽略
		}
	}
	return snap, nil
}
