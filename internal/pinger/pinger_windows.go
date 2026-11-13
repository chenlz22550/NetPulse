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

func (p *winPinger) Ping(ip string, timeout time.Duration, size int) (Result, error) {
	addr := net.ParseIP(ip).To4()
	if addr == nil {
		return Result{}, errors.New("invalid IPv4: " + ip)
	}
	if len(p.payload) != size {
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
		0,
		uintptr(unsafe.Pointer(&p.reply[0])),
		uintptr(len(p.reply)),
		uintptr(ms),
	)
	elapsed := time.Since(start)
	if n == 0 {
		// 超时、主机不可达等都视为丢包。
		return Result{OK: false}, nil
	}
	r := (*icmpEchoReply)(unsafe.Pointer(&p.reply[0]))
	if r.Status != ipSuccess {
		return Result{OK: false}, nil
	}
	// RoundTripTime 精度只有 1ms；用本地计时补足亚毫秒精度，但不超过系统报告值 +1ms。
	rtt := elapsed
	reported := time.Duration(r.RoundTripTime) * time.Millisecond
	if rtt > reported+time.Millisecond {
		rtt = reported
	}
	return Result{OK: true, RTT: rtt}, nil
}

func (p *winPinger) Close() error {
	if p.handle != 0 {
		procIcmpCloseHandle.Call(p.handle)
		p.handle = 0
	}
	return nil
}
