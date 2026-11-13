//go:build windows

package pinger

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"
	"unsafe"
)

// Windows 使用 iphlpapi.dll 的 IcmpSendEcho：Windows Server 2008 R2 起即可用，
// 且不需要管理员权限（原始套接字需要）。

var (
	iphlpapi            = syscall.NewLazyDLL("iphlpapi.dll")
	procIcmpCreateFile  = iphlpapi.NewProc("IcmpCreateFile")
	procIcmpCloseHandle = iphlpapi.NewProc("IcmpCloseHandle")
	procIcmpSendEcho    = iphlpapi.NewProc("IcmpSendEcho")
)

const ipSuccess = 0

type ipOptionInformation struct {
	TTL         uint8
	Tos         uint8
	Flags       uint8
	OptionsSize uint8
	OptionsData uintptr
}

type icmpEchoReply struct {
	Address       uint32
	Status        uint32
	RoundTripTime uint32
	DataSize      uint16
	Reserved      uint16
	Data          uintptr
	Options       ipOptionInformation
}

type winPinger struct {
	handle  uintptr
	payload []byte
	reply   []byte
}

// New 创建一个 Pinger。
func New() (Pinger, error) {
	h, _, err := procIcmpCreateFile.Call()
	if h == 0 || h == uintptr(syscall.InvalidHandle) {
		return nil, fmt.Errorf("IcmpCreateFile: %v", err)
	}
	return &winPinger{handle: h}, nil
}

// send 发送一次 Echo；opts 为 IP_OPTION_INFORMATION 指针（0 表示默认选项）。返回回应数与耗时。
func (p *winPinger) send(addr net.IP, size int, timeout time.Duration, opts uintptr) (uintptr, time.Duration) {
	if len(p.payload) != size || p.reply == nil {
		p.payload = make([]byte, size)
		for i := range p.payload {
			p.payload[i] = byte('a' + i%23)
		}
		p.reply = make([]byte, int(unsafe.Sizeof(icmpEchoReply{}))+size+8+64)
	}
	ms := uint32(timeout / time.Millisecond)
	if ms == 0 {
		ms = 1
	}
	var dataPtr uintptr
	if size > 0 {
		dataPtr = uintptr(unsafe.Pointer(&p.payload[0]))
	}
	start := time.Now()
	n, _, _ := procIcmpSendEcho.Call(
		p.handle,
		uintptr(binary.LittleEndian.Uint32(addr)),
		dataPtr,
		uintptr(size),
		opts,
		uintptr(unsafe.Pointer(&p.reply[0])),
		uintptr(len(p.reply)),
		uintptr(ms),
	)
	return n, time.Since(start)
}

// rtt 用本地计时补足亚毫秒精度，但不超过系统报告值 +1ms（RoundTripTime 精度只有 1ms）。
func rtt(elapsed time.Duration, reportedMs uint32) time.Duration {
	reported := time.Duration(reportedMs) * time.Millisecond
	if elapsed > reported+time.Millisecond {
		return reported
	}
	return elapsed
}

func (p *winPinger) Ping(ip string, timeout time.Duration, size int) (Result, error) {
	addr := net.ParseIP(ip).To4()
	if addr == nil {
		return Result{}, errors.New("invalid IPv4: " + ip)
	}
	n, elapsed := p.send(addr, size, timeout, 0)
	if n == 0 {
		// 超时、主机不可达等都视为丢包。
		return Result{OK: false}, nil
	}
	r := (*icmpEchoReply)(unsafe.Pointer(&p.reply[0]))
	if r.Status != ipSuccess {
		return Result{OK: false}, nil
	}
	return Result{OK: true, RTT: rtt(elapsed, r.RoundTripTime)}, nil
}

// IcmpSendEcho 回应状态（ipexport.h）。
var icmpStatusText = map[uint32]string{
	11002: "目标网络不可达", 11003: "目标主机不可达", 11004: "目标协议不可达", 11005: "目标端口不可达",
	11009: "数据包过大（需要分片）", 11012: "参数错误", 11015: "源抑制",
}

const ipTTLExpiredTransit = 11013

// PingTTL 以指定 TTL 发送 Echo：中间路由器回应 TTL 超时（IP_TTL_EXPIRED_TRANSIT），目标回应 Echo Reply。
func (p *winPinger) PingTTL(ip string, ttl int, timeout time.Duration) (Hop, error) {
	addr := net.ParseIP(ip).To4()
	if addr == nil {
		return Hop{}, errors.New("invalid IPv4: " + ip)
	}
	opts := ipOptionInformation{TTL: uint8(ttl)}
	n, elapsed := p.send(addr, 32, timeout, uintptr(unsafe.Pointer(&opts)))
	if n == 0 {
		return Hop{}, nil
	}
	r := (*icmpEchoReply)(unsafe.Pointer(&p.reply[0]))
	var from [4]byte
	binary.LittleEndian.PutUint32(from[:], r.Address)
	h := Hop{From: net.IP(from[:]).String(), RTT: rtt(elapsed, r.RoundTripTime)}
	switch r.Status {
	case ipSuccess:
		h.Reached = true
	case ipTTLExpiredTransit:
	default:
		h.Status = icmpStatusText[r.Status]
		if h.Status == "" {
			h.Status = fmt.Sprintf("ICMP 状态 %d", r.Status)
		}
	}
	return h, nil
}

func (p *winPinger) Close() error {
	if p.handle != 0 {
		procIcmpCloseHandle.Call(p.handle)
		p.handle = 0
	}
	return nil
}
