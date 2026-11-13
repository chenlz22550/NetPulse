// Package snmp 是一个精简的 SNMP v2c 只读客户端（Get / GetBulk 遍历），用于读取交换机的 MAC 地址表和端口名。
// 不依赖第三方库，兼容 Go 1.20 / Windows Server 2008 R2。
package snmp

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"time"
)

// 值类型。
const (
	TypeInteger     = 0x02
	TypeOctetString = 0x04
	TypeNull        = 0x05
	TypeOID         = 0x06
	TypeIPAddress   = 0x40
	TypeCounter32   = 0x41
	TypeGauge32     = 0x42
	TypeTimeTicks   = 0x43
	TypeCounter64   = 0x46
	TypeNoSuchObj   = 0x80
	TypeNoSuchInst  = 0x81
	TypeEndOfMib    = 0x82

	pduGet      = 0xa0
	pduGetNext  = 0xa1
	pduResponse = 0xa2
	pduGetBulk  = 0xa5
)

// Var 是一个变量绑定。
type Var struct {
	OID   OID
	Type  byte
	Int   int64  // INTEGER / Counter / Gauge / TimeTicks
	Bytes []byte // OCTET STRING / IpAddress
	Obj   OID    // OBJECT IDENTIFIER 值
}

// String 以可读形式返回值。
func (v Var) String() string {
	switch v.Type {
	case TypeOctetString:
		return string(v.Bytes)
	case TypeIPAddress:
		if len(v.Bytes) == 4 {
			return net.IP(v.Bytes).String()
		}
	case TypeOID:
		return v.Obj.String()
	case TypeNull, TypeNoSuchObj, TypeNoSuchInst, TypeEndOfMib:
		return ""
	}
	return strconv.FormatInt(v.Int, 10)
}

// OID 是对象标识。
type OID []uint32

// ParseOID 解析 "1.3.6.1..." 形式的 OID。
func ParseOID(s string) OID {
	s = strings.TrimPrefix(s, ".")
	var o OID
	for _, p := range strings.Split(s, ".") {
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return nil
		}
		o = append(o, uint32(n))
	}
	return o
}

func (o OID) String() string {
	parts := make([]string, len(o))
	for i, n := range o {
		parts[i] = strconv.FormatUint(uint64(n), 10)
	}
	return strings.Join(parts, ".")
}

// HasPrefix 判断 o 是否在 p 子树下（不含 p 本身）。
func (o OID) HasPrefix(p OID) bool {
	if len(o) <= len(p) {
		return false
	}
	for i := range p {
		if o[i] != p[i] {
			return false
		}
	}
	return true
}

func (o OID) cmp(b OID) int {
	for i := 0; i < len(o) && i < len(b); i++ {
		if o[i] != b[i] {
			if o[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return len(o) - len(b)
}

// Client 是 SNMP v2c 客户端，不是并发安全的。
type Client struct {
	Host      string // IP 或主机名，可带端口
	Community string
	Timeout   time.Duration
	Retries   int
	MaxRep    int // GetBulk 每次最多返回的行数

	conn  net.Conn
	reqID int32
}

// Connect 建立 UDP 连接（默认端口 161）。
func (c *Client) Connect() error {
	addr := c.Host
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "161")
	}
	if c.Timeout <= 0 {
		c.Timeout = 2 * time.Second
	}
	if c.MaxRep <= 0 {
		c.MaxRep = 25
	}
	conn, err := net.DialTimeout("udp", addr, c.Timeout)
	if err != nil {
		return err
	}
	c.conn = conn
	c.reqID = rand.Int31()
	return nil
}

// Close 关闭连接。
func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// ErrTimeout 表示设备没有响应（地址错误、团体名错误或未开启 SNMP 时都表现为超时）。
var ErrTimeout = errors.New("SNMP 无响应（请检查管理 IP、团体名，以及交换机是否开启 SNMP 并允许本机访问）")

func (c *Client) request(pdu byte, oids []OID, a, b int) ([]Var, error) {
	if c.conn == nil {
		return nil, errors.New("未连接")
	}
	c.reqID++
	id := c.reqID & 0x7fffffff
	msg := encodeMessage(c.Community, pdu, id, a, b, oids)
	buf := make([]byte, 65536)
	for try := 0; try <= c.Retries; try++ {
		if _, err := c.conn.Write(msg); err != nil {
			return nil, err
		}
		deadline := time.Now().Add(c.Timeout)
		for {
			_ = c.conn.SetReadDeadline(deadline)
			n, err := c.conn.Read(buf)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					break
				}
				return nil, err
			}
			rid, errStat, errIdx, vars, err := decodeResponse(buf[:n])
			if err != nil || rid != id {
				continue // 不是本次请求的响应
			}
			if errStat != 0 {
				return nil, fmt.Errorf("SNMP 错误 %s（第 %d 项）", errStatusText(errStat), errIdx)
			}
			return vars, nil
		}
	}
	return nil, ErrTimeout
}

func errStatusText(s int) string {
	names := map[int]string{1: "tooBig", 2: "noSuchName", 3: "badValue", 4: "readOnly", 5: "genErr", 6: "noAccess", 16: "authorizationError"}
	if n, ok := names[s]; ok {
		return n
	}
	return strconv.Itoa(s)
}

// Get 读取若干标量。
func (c *Client) Get(oids ...OID) ([]Var, error) {
	return c.request(pduGet, oids, 0, 0)
}

// Walk 用 GetBulk 遍历 root 子树，对每个变量调用 fn；fn 返回 false 时停止。
func (c *Client) Walk(root OID, fn func(Var) bool) error {
	cur := root
	for {
		vars, err := c.request(pduGetBulk, []OID{cur}, 0, c.MaxRep)
		if err != nil {
			return err
		}
		if len(vars) == 0 {
			return nil
		}
		for _, v := range vars {
			if v.Type == TypeEndOfMib || !v.OID.HasPrefix(root) {
				return nil
			}
			if v.OID.cmp(cur) <= 0 {
				return errors.New("SNMP 遍历顺序异常（设备返回的 OID 未递增）")
			}
			if !fn(v) {
				return nil
			}
			cur = v.OID
		}
	}
}

// ---------------------------------------------------------------------------
// BER 编解码

func encLen(n int) []byte {
	if n < 0x80 {
		return []byte{byte(n)}
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte(n)}, b...)
		n >>= 8
	}
	return append([]byte{0x80 | byte(len(b))}, b...)
}

func tlv(tag byte, val []byte) []byte {
	out := append([]byte{tag}, encLen(len(val))...)
	return append(out, val...)
}

func encInt(v int64) []byte {
	b := []byte{byte(v)}
	for v > 127 || v < -128 {
		v >>= 8
		b = append([]byte{byte(v)}, b...)
	}
	return tlv(TypeInteger, b)
}

func encOID(o OID) []byte {
	if len(o) < 2 {
		o = OID{1, 3}
	}
	b := []byte{byte(o[0]*40 + o[1])}
	for _, n := range o[2:] {
		var tmp []byte
		tmp = append(tmp, byte(n&0x7f))
		n >>= 7
		for n > 0 {
			tmp = append([]byte{byte(n&0x7f) | 0x80}, tmp...)
			n >>= 7
		}
		b = append(b, tmp...)
	}
	return tlv(TypeOID, b)
}

func encodeMessage(community string, pdu byte, id int32, a, b int, oids []OID) []byte {
	var vbs []byte
	for _, o := range oids {
		vbs = append(vbs, tlv(0x30, append(encOID(o), TypeNull, 0))...)
	}
	body := append(encInt(int64(id)), encInt(int64(a))...)
	body = append(body, encInt(int64(b))...)
	body = append(body, tlv(0x30, vbs)...)
	msg := append(encInt(1), tlv(TypeOctetString, []byte(community))...) // version 1 = v2c
	msg = append(msg, tlv(pdu, body)...)
	return tlv(0x30, msg)
}

type reader struct {
	b   []byte
	pos int
}

var errBER = errors.New("SNMP 报文格式错误")

func (r *reader) next() (byte, []byte, error) {
	if r.pos+2 > len(r.b) {
		return 0, nil, errBER
	}
	tag := r.b[r.pos]
	l := int(r.b[r.pos+1])
	p := r.pos + 2
	if l&0x80 != 0 {
		nb := l & 0x7f
		if nb == 0 || nb > 4 || p+nb > len(r.b) {
			return 0, nil, errBER
		}
		l = 0
		for i := 0; i < nb; i++ {
			l = l<<8 | int(r.b[p+i])
		}
		p += nb
	}
	if l < 0 || p+l > len(r.b) {
		return 0, nil, errBER
	}
	r.pos = p + l
	return tag, r.b[p : p+l], nil
}

func decInt(b []byte) int64 {
	var v int64
	for i, x := range b {
		if i == 0 && x&0x80 != 0 {
			v = -1
		}
		v = v<<8 | int64(x)
	}
	return v
}

func decUint(b []byte) int64 {
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return int64(v)
}

func decOID(b []byte) OID {
	if len(b) == 0 {
		return nil
	}
	o := OID{uint32(b[0]) / 40, uint32(b[0]) % 40}
	var n uint32
	for _, x := range b[1:] {
		n = n<<7 | uint32(x&0x7f)
		if x&0x80 == 0 {
			o = append(o, n)
			n = 0
		}
	}
	return o
}

func decodeResponse(data []byte) (id int32, errStat, errIdx int, vars []Var, err error) {
	top := &reader{b: data}
	tag, msg, err := top.next()
	if err != nil || tag != 0x30 {
		return 0, 0, 0, nil, errBER
	}
	r := &reader{b: msg}
	if _, _, err = r.next(); err != nil { // version
		return
	}
	if _, _, err = r.next(); err != nil { // community
		return
	}
	tag, pdu, err := r.next()
	if err != nil || tag != pduResponse {
		return 0, 0, 0, nil, errBER
	}
	p := &reader{b: pdu}
	var f [3][]byte
	for i := range f {
		if _, f[i], err = p.next(); err != nil {
			return
		}
	}
	id, errStat, errIdx = int32(decInt(f[0])), int(decInt(f[1])), int(decInt(f[2]))
	_, list, err := p.next()
	if err != nil {
		return
	}
	lr := &reader{b: list}
	for lr.pos < len(list) {
		_, vb, e := lr.next()
		if e != nil {
			return 0, 0, 0, nil, e
		}
		vr := &reader{b: vb}
		_, ob, e := vr.next()
		if e != nil {
			return 0, 0, 0, nil, e
		}
		t, val, e := vr.next()
		if e != nil {
			return 0, 0, 0, nil, e
		}
		v := Var{OID: decOID(ob), Type: t}
		switch t {
		case TypeInteger:
			v.Int = decInt(val)
		case TypeCounter32, TypeGauge32, TypeTimeTicks, TypeCounter64:
			v.Int = decUint(val)
		case TypeOctetString, TypeIPAddress:
			v.Bytes = bytes.Clone(val)
		case TypeOID:
			v.Obj = decOID(val)
		}
		vars = append(vars, v)
	}
	return id, errStat, errIdx, vars, nil
}
