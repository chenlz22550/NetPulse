package snmp

import (
	"testing"
	"testing/quick"
	"time"
)

func TestVarStringAndCodecEdgeCases(t *testing.T) {
	cases := []struct {
		v    Var
		want string
	}{
		{Var{Type: TypeOctetString, Bytes: []byte("abc")}, "abc"},
		{Var{Type: TypeIPAddress, Bytes: []byte{192, 168, 1, 10}}, "192.168.1.10"},
		{Var{Type: TypeOID, Obj: OID{1, 3, 6, 1}}, "1.3.6.1"},
		{Var{Type: TypeNull}, ""},
		{Var{Type: TypeNoSuchObj}, ""},
		{Var{Type: TypeNoSuchInst}, ""},
		{Var{Type: TypeEndOfMib}, ""},
		{Var{Type: TypeInteger, Int: -42}, "-42"},
		{Var{Type: TypeGauge32, Int: 1000}, "1000"},
	}
	for _, tc := range cases {
		if got := tc.v.String(); got != tc.want {
			t.Fatalf("Var(%#x).String() = %q, want %q", tc.v.Type, got, tc.want)
		}
	}
	if ParseOID("1.3.bad") != nil {
		t.Fatal("invalid OID should return nil")
	}
	if !(OID{1, 3, 6}).HasPrefix(OID{1, 3}) || (OID{1, 3}).HasPrefix(OID{1, 3}) || (OID{1, 4}).HasPrefix(OID{1, 3}) {
		t.Fatal("HasPrefix mismatch")
	}
	if (OID{1, 3}).cmp(OID{1, 3, 1}) >= 0 || (OID{1, 3, 2}).cmp(OID{1, 3, 1}) <= 0 {
		t.Fatal("OID.cmp mismatch")
	}

	// Long BER length encoding (>127 bytes) & error cases
	longPayload := make([]byte, 300)
	longMsg := tlv(TypeOctetString, longPayload)
	r := &reader{b: longMsg}
	tag, val, err := r.next()
	if err != nil || tag != TypeOctetString || len(val) != 300 {
		t.Fatalf("long BER decode failed: tag=%x len=%d err=%v", tag, len(val), err)
	}
	for _, bad := range [][]byte{
		{},
		{0x30, 0x80},             // indefinite length
		{0x30, 0x85, 1, 2, 3},    // >4 length bytes
		{0x30, 0x10, 1, 2},       // truncated payload
		{0x04, 0x00},             // not 0x30 sequence
		tlv(0x30, []byte{0x02}),  // truncated inner
		{0x30, 0x06, 2, 1, 1, 4}, // missing pdu
	} {
		if _, _, _, _, err := decodeResponse(bad); err == nil {
			t.Fatalf("corrupt response %x accepted", bad)
		}
	}

	// Unconnected client fails cleanly
	c := &Client{}
	if _, err := c.Get(OID{1, 3, 6}); err == nil {
		t.Fatal("unconnected Get should fail")
	}
	_ = c.Close()
}

func TestBridgeFallbackAndErrorBranches(t *testing.T) {
	// Device uses ifDescr + BRIDGE-MIB instead of ifName + Q-BRIDGE-MIB, and includes multicast/invalid FDB entries
	a := &Agent{Community: "pub"}
	a.SetString("1.3.6.1.2.1.1.1.0", "Old Switch")
	a.SetString("1.3.6.1.2.1.1.5.0", "old-sw")
	a.SetString("1.3.6.1.2.1.2.2.1.2.1", "Eth0/1")
	a.SetInt("1.3.6.1.2.1.17.1.4.1.2.1", 1)
	// Valid learned MAC
	a.SetInt("1.3.6.1.2.1.17.4.3.1.2.96.215.227.0.0.1", 1)
	a.SetInt("1.3.6.1.2.1.17.4.3.1.3.96.215.227.0.0.1", 3)
	// Multicast MAC (should be skipped)
	a.SetInt("1.3.6.1.2.1.17.4.3.1.2.1.0.94.0.0.1", 1)
	// Invalid status (2)
	a.SetInt("1.3.6.1.2.1.17.4.3.1.2.96.215.227.0.0.2", 1)
	a.SetInt("1.3.6.1.2.1.17.4.3.1.3.96.215.227.0.0.2", 2)
	addr, err := a.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	c := &Client{Host: addr, Community: "pub", Timeout: time.Second, MaxRep: 5}
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tb, err := c.ReadTable()
	if err != nil {
		t.Fatal(err)
	}
	if tb.Source != "bridge" || len(tb.Entries) != 1 || tb.Entries[0].IfName != "Eth0/1" || tb.Entries[0].MAC != "60:D7:E3:00:00:01" {
		t.Fatalf("bridge table = %+v", tb)
	}

	// Empty FDB generates warning
	b := &Agent{Community: "pub"}
	b.SetString("1.3.6.1.2.1.1.5.0", "empty-sw")
	b.SetString("1.3.6.1.2.1.31.1.1.1.1.1", "Gi0/1")
	addr2, _ := b.Listen("127.0.0.1:0")
	defer b.Close()
	c2 := &Client{Host: addr2, Community: "pub", Timeout: time.Second}
	_ = c2.Connect()
	defer c2.Close()
	tb2, err := c2.ReadTable()
	if err != nil || len(tb2.Warnings) == 0 {
		t.Fatalf("empty FDB should warn: %v %+v", err, tb2)
	}
	// Walk early stop via fn returning false
	count := 0
	if err := c2.Walk(OidIfName, func(Var) bool { count++; return false }); err != nil || count != 1 {
		t.Fatalf("Walk early stop count=%d err=%v", count, err)
	}
	// GetNext via agent directly
	resp := a.handle(encodeMessage("pub", pduGetNext, 42, 0, 0, []OID{ParseOID("1.3.6.1.2.1.1.1.0")}))
	if id, _, _, vars, err := decodeResponse(resp); err != nil || id != 42 || len(vars) != 1 {
		t.Fatalf("GetNext response err=%v id=%d vars=%v", err, id, vars)
	}
	if errStatusText(2) != "noSuchName" || errStatusText(99) != "99" {
		t.Fatal("errStatusText mismatch")
	}
	if macFromOID(OID{1, 2, 3}) != "" || macFromOID(OID{1, 0, 0, 0, 0, 300}) != "" {
		t.Fatal("macFromOID edge mismatch")
	}
}

func TestPortStatAllColumns(t *testing.T) {
	p := &PortStat{}
	p.set(oidIfSpeed, Var{Type: TypeGauge32, Int: 100000000})
	p.set(oidIfInOctets, Var{Type: TypeCounter32, Int: 1234})
	p.set(oidIfOutOctets, Var{Type: TypeCounter32, Int: 5678})
	p.set(oidIfOutErrors, Var{Type: TypeCounter32, Int: 3})
	p.set(oidIfInDiscards, Var{Type: TypeCounter32, Int: 4})
	p.set(oidIfOutDiscards, Var{Type: TypeCounter32, Int: 5})
	p.set(oidIfAlias, Var{Type: TypeOctetString, Bytes: []byte("uplink")})
	p.set(oidDot3FCSErrors, Var{Type: TypeCounter32, Int: 7})
	if p.SpeedMbps != 100 || p.InOctets != 1234 || p.OutOctets != 5678 || p.OutErrors != 3 || p.InDiscards != 4 || p.OutDiscard != 5 || p.Alias != "uplink" || !p.HasFCS || p.FCSErrors != 7 {
		t.Fatalf("PortStat fields: %+v", p)
	}
	// 64-bit counters override 32-bit
	p.set(oidIfHCInOctets, Var{Type: TypeCounter64, Int: 999999})
	p.set(oidIfHCOutOctets, Var{Type: TypeCounter64, Int: 888888})
	p.set(oidIfInOctets, Var{Type: TypeCounter32, Int: 1})
	if !p.HCOctets || p.InOctets != 999999 || p.OutOctets != 888888 {
		t.Fatalf("HC octets override failed: %+v", p)
	}
}

// 性质测试：任意随机 SNMP OID 与整型/字符串 Var 经过 BER 编解码后保持完全一致。
func TestBERCodecRoundTripProperty(t *testing.T) {
	if err := quick.Check(func(a, b, c uint16, v int32, s string) bool {
		oid := OID{1, 3, 6, 1, uint32(a), uint32(b), uint32(c)}
		r := &reader{b: encOID(oid)}
		tag, val, err := r.next()
		if err != nil || tag != TypeOID || decOID(val).cmp(oid) != 0 {
			return false
		}
		vars := []Var{
			{OID: oid, Type: TypeInteger, Int: int64(v)},
			{OID: append(OID{}, oid...), Type: TypeOctetString, Bytes: []byte(s)},
		}
		vars[1].OID[len(vars[1].OID)-1]++
		raw := encodeResponse("comm", 123, vars)
		id, stat, idx, dec, err := decodeResponse(raw)
		if err != nil || id != 123 || stat != 0 || idx != 0 || len(dec) != 2 {
			return false
		}
		return dec[0].Int == int64(v) && string(dec[1].Bytes) == s
	}, &quick.Config{MaxCount: 150}); err != nil {
		t.Fatal(err)
	}
}
