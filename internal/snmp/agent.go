package snmp

import (
	"net"
	"sort"
	"sync"
)

// Agent 是一个最小的 SNMP v2c 只读代理，用于测试和演示（模拟交换机）。
type Agent struct {
	Community string
	mu        sync.Mutex
	vars      []Var
	conn      net.PacketConn
}

// Set 设置一个变量（OCTET STRING / INTEGER 等）。
func (a *Agent) Set(v Var) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.vars {
		if a.vars[i].OID.cmp(v.OID) == 0 {
			a.vars[i] = v
			return
		}
	}
	a.vars = append(a.vars, v)
	sort.Slice(a.vars, func(i, j int) bool { return a.vars[i].OID.cmp(a.vars[j].OID) < 0 })
}

// SetString / SetInt 便捷方法。
func (a *Agent) SetString(oid, s string) {
	a.Set(Var{OID: ParseOID(oid), Type: TypeOctetString, Bytes: []byte(s)})
}

// SetInt 设置 INTEGER 值。
func (a *Agent) SetInt(oid string, n int64) {
	a.Set(Var{OID: ParseOID(oid), Type: TypeInteger, Int: n})
}

// Listen 在 addr（如 127.0.0.1:0）上开始服务，返回实际地址。
func (a *Agent) Listen(addr string) (string, error) {
	c, err := net.ListenPacket("udp", addr)
	if err != nil {
		return "", err
	}
	a.conn = c
	go a.serve()
	return c.LocalAddr().String(), nil
}

// Close 停止服务。
func (a *Agent) Close() error {
	if a.conn != nil {
		return a.conn.Close()
	}
	return nil
}

func (a *Agent) serve() {
	buf := make([]byte, 65536)
	for {
		n, from, err := a.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		if resp := a.handle(buf[:n]); resp != nil {
			_, _ = a.conn.WriteTo(resp, from)
		}
	}
}

func (a *Agent) handle(data []byte) []byte {
	top := &reader{b: data}
	if _, msg, err := top.next(); err == nil {
		r := &reader{b: msg}
		_, ver, e1 := r.next()
		_, comm, e2 := r.next()
		tag, pdu, e3 := r.next()
		if e1 != nil || e2 != nil || e3 != nil || decInt(ver) != 1 || string(comm) != a.Community {
			return nil // 团体名错误：与真实设备一样不回应
		}
		p := &reader{b: pdu}
		var f [3][]byte
		for i := range f {
			if _, f[i], err = p.next(); err != nil {
				return nil
			}
		}
		_, list, err := p.next()
		if err != nil {
			return nil
		}
		var oids []OID
		lr := &reader{b: list}
		for lr.pos < len(list) {
			_, vb, err := lr.next()
			if err != nil {
				return nil
			}
			_, ob, _ := (&reader{b: vb}).next()
			oids = append(oids, decOID(ob))
		}
		a.mu.Lock()
		out := a.answer(tag, oids, int(decInt(f[1])), int(decInt(f[2])))
		a.mu.Unlock()
		return encodeResponse(string(comm), decInt(f[0]), out)
	}
	return nil
}

func (a *Agent) after(o OID) Var {
	i := sort.Search(len(a.vars), func(i int) bool { return a.vars[i].OID.cmp(o) > 0 })
	if i < len(a.vars) {
		return a.vars[i]
	}
	return Var{OID: o, Type: TypeEndOfMib}
}

func (a *Agent) answer(tag byte, oids []OID, nonRep, maxRep int) []Var {
	var out []Var
	switch tag {
	case pduGet:
		for _, o := range oids {
			v := Var{OID: o, Type: TypeNoSuchInst}
			for _, x := range a.vars {
				if x.OID.cmp(o) == 0 {
					v = x
				}
			}
			out = append(out, v)
		}
	case pduGetNext:
		for _, o := range oids {
			out = append(out, a.after(o))
		}
	case pduGetBulk:
		for i, o := range oids {
			if i < nonRep {
				out = append(out, a.after(o))
				continue
			}
			cur := o
			for k := 0; k < maxRep; k++ {
				v := a.after(cur)
				out = append(out, v)
				if v.Type == TypeEndOfMib {
					break
				}
				cur = v.OID
			}
		}
	}
	return out
}

func encValue(v Var) []byte {
	switch v.Type {
	case TypeInteger:
		return encInt(v.Int)
	case TypeOctetString, TypeIPAddress:
		return tlv(v.Type, v.Bytes)
	case TypeOID:
		return encOID(v.Obj)
	case TypeCounter32, TypeGauge32, TypeTimeTicks, TypeCounter64:
		b := encInt(v.Int)[2:]
		if len(b) > 1 && b[0] == 0 && b[1]&0x80 == 0 {
			b = b[1:]
		}
		return tlv(v.Type, b)
	}
	return []byte{v.Type, 0}
}

func encodeResponse(community string, id int64, vars []Var) []byte {
	var vbs []byte
	for _, v := range vars {
		vbs = append(vbs, tlv(0x30, append(encOID(v.OID), encValue(v)...))...)
	}
	body := append(encInt(id), encInt(0)...)
	body = append(body, encInt(0)...)
	body = append(body, tlv(0x30, vbs)...)
	msg := append(encInt(1), tlv(TypeOctetString, []byte(community))...)
	msg = append(msg, tlv(pduResponse, body)...)
	return tlv(0x30, msg)
}
