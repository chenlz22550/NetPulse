package snmp

import (
	"fmt"
	"testing"
	"time"
)

// FakePorts 在模拟代理上设置 n 个端口的 IF-MIB 数据（端口 3 半双工、端口 5 down）。
func fakePorts(a *Agent, n int) {
	a.Set(Var{OID: ParseOID("1.3.6.1.2.1.1.3.0"), Type: TypeTimeTicks, Int: 360000}) // 1 小时
	for i := 1; i <= n; i++ {
		a.SetString(fmt.Sprintf("1.3.6.1.2.1.31.1.1.1.1.%d", i), fmt.Sprintf("GigabitEthernet 0/%d", i))
		a.SetInt(fmt.Sprintf("1.3.6.1.2.1.2.2.1.7.%d", i), 1)
		oper := int64(1)
		if i == 5 {
			oper = 2
		}
		a.SetInt(fmt.Sprintf("1.3.6.1.2.1.2.2.1.8.%d", i), oper)
		a.Set(Var{OID: ParseOID(fmt.Sprintf("1.3.6.1.2.1.2.2.1.5.%d", i)), Type: TypeGauge32, Int: 1000000000})
		a.Set(Var{OID: ParseOID(fmt.Sprintf("1.3.6.1.2.1.31.1.1.1.15.%d", i)), Type: TypeGauge32, Int: 1000})
		a.Set(Var{OID: ParseOID(fmt.Sprintf("1.3.6.1.2.1.2.2.1.9.%d", i)), Type: TypeTimeTicks, Int: 350000})
		a.Set(Var{OID: ParseOID(fmt.Sprintf("1.3.6.1.2.1.2.2.1.14.%d", i)), Type: TypeCounter32, Int: int64(i)})
		a.Set(Var{OID: ParseOID(fmt.Sprintf("1.3.6.1.2.1.31.1.1.1.6.%d", i)), Type: TypeCounter64, Int: 1 << 40})
		duplex := int64(3)
		if i == 3 {
			duplex = 2
		}
		a.SetInt(fmt.Sprintf("1.3.6.1.2.1.10.7.2.1.19.%d", i), duplex)
	}
}

func TestReadPorts(t *testing.T) {
	a := &Agent{Community: "ro"}
	fakePorts(a, 24)
	addr, err := a.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	c := &Client{Host: addr, Community: "ro", Timeout: time.Second}
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// 全部端口：按列遍历
	snap, err := c.ReadPorts(nil)
	if err != nil {
		t.Fatal(err)
	}
	if snap.UpTime != 360000 || len(snap.Order) != 24 {
		t.Fatalf("uptime=%d ports=%d", snap.UpTime, len(snap.Order))
	}
	p3, p5 := snap.Ports[3], snap.Ports[5]
	if p3.Duplex != 2 || p3.SpeedMbps != 1000 || p3.InErrors != 3 || p3.InOctets != 1<<40 || !p3.HCOctets || p3.LastChange != 350000 {
		t.Fatalf("port 3: %+v", p3)
	}
	if p5.Oper != 2 || p5.Admin != 1 || p5.Name != "GigabitEthernet 0/5" {
		t.Fatalf("port 5: %+v", p5)
	}
	// 少量端口：逐个 Get
	snap, err = c.ReadPorts(func(idx int, name string) bool { return idx == 3 || idx == 7 })
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Order) != 2 || snap.Ports[3].Duplex != 2 || snap.Ports[7].InErrors != 7 || snap.Ports[7].SpeedMbps != 1000 {
		t.Fatalf("filtered: %v %+v %+v", snap.Order, snap.Ports[3], snap.Ports[7])
	}
	// 不支持扩展 MIB 的设备：只有基本列也能读
	b := &Agent{Community: "ro"}
	b.Set(Var{OID: ParseOID("1.3.6.1.2.1.1.3.0"), Type: TypeTimeTicks, Int: 100})
	b.SetString("1.3.6.1.2.1.2.2.1.2.1", "Port 1")
	b.SetInt("1.3.6.1.2.1.2.2.1.8.1", 1)
	addr2, _ := b.Listen("127.0.0.1:0")
	defer b.Close()
	c2 := &Client{Host: addr2, Community: "ro", Timeout: time.Second}
	_ = c2.Connect()
	defer c2.Close()
	snap, err = c2.ReadPorts(nil)
	if err != nil || len(snap.Order) != 1 || snap.Ports[1].Oper != 1 || snap.Ports[1].Duplex != 0 {
		t.Fatalf("basic only: %v %+v", err, snap)
	}
}
