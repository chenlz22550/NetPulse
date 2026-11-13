package snmp

import (
	"fmt"
	"testing"
	"time"
)

// FakeSwitch 构造一台模拟的示例接入交换机：26 口，ifIndex = 端口号。
func fakeSwitch(t *testing.T, macs map[string]int) (*Agent, string) {
	a := &Agent{Community: "public"}
	a.SetString("1.3.6.1.2.1.1.1.0", "Ruijie Full Layer 2 Switch(S2910-24GT4XS-E)")
	a.SetString("1.3.6.1.2.1.1.5.0", "access-sw")
	for i := 1; i <= 26; i++ {
		a.SetString(fmt.Sprintf("1.3.6.1.2.1.31.1.1.1.1.%d", i), fmt.Sprintf("GigabitEthernet 0/%d", i))
		a.SetInt(fmt.Sprintf("1.3.6.1.2.1.17.1.4.1.2.%d", i), int64(i))
	}
	for mac, port := range macs {
		var b [6]int
		fmt.Sscanf(mac, "%x:%x:%x:%x:%x:%x", &b[0], &b[1], &b[2], &b[3], &b[4], &b[5])
		idx := fmt.Sprintf("1.%d.%d.%d.%d.%d.%d", b[0], b[1], b[2], b[3], b[4], b[5])
		a.SetInt("1.3.6.1.2.1.17.7.1.2.2.1.2."+idx, int64(port))
		a.SetInt("1.3.6.1.2.1.17.7.1.2.2.1.3."+idx, 3)
	}
	addr, err := a.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a, addr
}

func TestReadTable(t *testing.T) {
	macs := map[string]int{}
	for i := 0; i < 120; i++ { // 足够多，跨越多次 GetBulk
		macs[fmt.Sprintf("60:D7:E3:00:%02X:%02X", i/256, i%256)] = i%24 + 1
	}
	macs["02:00:00:00:00:17"] = 26
	a, addr := fakeSwitch(t, macs)
	a.SetInt("1.3.6.1.2.1.17.7.1.2.2.1.3.1.2.0.0.0.0.23", 4) // self：应被排除
	c := &Client{Host: addr, Community: "public", Timeout: time.Second}
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tb, err := c.ReadTable()
	if err != nil {
		t.Fatal(err)
	}
	if tb.SysName != "access-sw" || len(tb.IfNames) != 26 || tb.Source != "q-bridge" {
		t.Fatalf("table: %+v", tb)
	}
	if len(tb.Entries) != 120 {
		t.Fatalf("entries = %d", len(tb.Entries))
	}
	e := tb.Entries[0]
	if e.VLAN != 1 || e.IfName != "GigabitEthernet 0/1" || e.MAC == "" {
		t.Fatalf("entry: %+v", e)
	}

	// 团体名错误 → 超时
	c2 := &Client{Host: addr, Community: "wrong", Timeout: 200 * time.Millisecond}
	_ = c2.Connect()
	defer c2.Close()
	if _, _, err := c2.Probe(); err != ErrTimeout {
		t.Fatalf("want timeout, got %v", err)
	}
}

func TestOIDCodec(t *testing.T) {
	for _, s := range []string{"1.3.6.1.2.1.17.7.1.2.2.1.2.1.2.0.0.0.0.18", "1.3.6.1.4.1.4881.1.1.10.2.1.1.1.0", "1.3.268435455"} {
		b := encOID(ParseOID(s))
		r := &reader{b: b}
		_, v, err := r.next()
		if err != nil || decOID(v).String() != s {
			t.Fatalf("%s -> %v %v", s, decOID(v), err)
		}
	}
	for _, n := range []int64{0, 1, 127, 128, 255, 256, -1, -129, 1 << 31, 1<<31 - 1} {
		b := encInt(n)
		if got := decInt(b[2:]); got != n {
			t.Fatalf("int %d -> %d", n, got)
		}
	}
}
