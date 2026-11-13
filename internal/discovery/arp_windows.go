//go:build windows

package discovery

import (
	"encoding/binary"
	"fmt"
	"net"
	"syscall"
	"unsafe"
)

var procGetIpNetTable = syscall.NewLazyDLL("iphlpapi.dll").NewProc("GetIpNetTable")

const (
	errInsufficientBuffer = 122
	errNoData             = 232
	mibIPNetTypeInvalid   = 2
	ipNetRowSize          = 24 // dwIndex, dwPhysAddrLen, bPhysAddr[8], dwAddr, dwType
)

// ReadARP 通过 GetIpNetTable 读取系统 ARP 表（Windows Server 2008 R2 起可用，无需管理员权限）。
func ReadARP() ([]ARPEntry, error) {
	var size uint32
	r, _, _ := procGetIpNetTable.Call(0, uintptr(unsafe.Pointer(&size)), 0)
	if r == errNoData {
		return nil, nil
	}
	if r != errInsufficientBuffer && r != 0 {
		return nil, fmt.Errorf("GetIpNetTable: 错误码 %d", r)
	}
	for attempt := 0; attempt < 3; attempt++ {
		buf := make([]byte, size+ipNetRowSize*16)
		size = uint32(len(buf))
		r, _, _ = procGetIpNetTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0)
		if r == errInsufficientBuffer {
			continue
		}
		if r == errNoData {
			return nil, nil
		}
		if r != 0 {
			return nil, fmt.Errorf("GetIpNetTable: 错误码 %d", r)
		}
		n := int(binary.LittleEndian.Uint32(buf[0:4]))
		var out []ARPEntry
		for i := 0; i < n; i++ {
			off := 4 + i*ipNetRowSize
			if off+ipNetRowSize > len(buf) {
				break
			}
			row := buf[off : off+ipNetRowSize]
			physLen := int(binary.LittleEndian.Uint32(row[4:8]))
			typ := binary.LittleEndian.Uint32(row[20:24])
			if typ == mibIPNetTypeInvalid || physLen != 6 {
				continue
			}
			mac := macString(row[8:14])
			if mac == "" {
				continue
			}
			ip := net.IPv4(row[16], row[17], row[18], row[19]).String()
			out = append(out, ARPEntry{IP: ip, MAC: mac})
		}
		return out, nil
	}
	return nil, fmt.Errorf("GetIpNetTable: 缓冲区大小不足")
}
