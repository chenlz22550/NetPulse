//go:build darwin

package discovery

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// 与 golang.org/x/net/route 相同的做法：直接按 MIB 调用 sysctl。
//
//go:linkname sysctl syscall.sysctl
func sysctl(mib []int32, old *byte, oldlen *uintptr, new *byte, newlen uintptr) error

func sysctlMIB(mib []int32) ([]byte, error) {
	for try := 0; try < 3; try++ {
		var n uintptr
		if err := sysctl(mib, nil, &n, nil, 0); err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, nil
		}
		n += n / 8 // 两次调用之间表项可能增加
		buf := make([]byte, n)
		if err := sysctl(mib, &buf[0], &n, nil, 0); err != nil {
			if err == unix.ENOMEM {
				continue
			}
			return nil, err
		}
		return buf[:n], nil
	}
	return nil, unix.ENOMEM
}

var _ unsafe.Pointer

func init() {
	arpEmptyHint = "未读取到 ARP 记录（无法获得 MAC）。macOS 需要授予本地网络权限：系统设置 → 隐私与安全性 → 本地网络，打开 netPing 后重新扫描"
}

// ReadARP 通过 sysctl(net.route, NET_RT_FLAGS, RTF_LLINFO) 读取系统 ARP 表，
// 不调用外部 arp 命令，也不需要管理员权限。
func ReadARP() ([]ARPEntry, error) {
	if f := os.Getenv("NETPING_ARP_FILE"); f != "" { // 开发调试：从文件读取 arp -an 格式的内容
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		return parseARPText(string(data)), nil
	}
	buf, err := sysctlMIB([]int32{unix.CTL_NET, unix.AF_ROUTE, 0, unix.AF_INET, unix.NET_RT_FLAGS, unix.RTF_LLINFO})
	if err != nil {
		return nil, fmt.Errorf("读取 ARP 表失败: %w", err)
	}
	return parseRouteMsgs(buf), nil
}

// parseRouteMsgs 解析 rt_msghdr + sockaddr 序列（每个 sockaddr 按 4 字节对齐）。
func parseRouteMsgs(buf []byte) []ARPEntry {
	var out []ARPEntry
	for len(buf) >= unix.SizeofRtMsghdr {
		msglen := int(binary.LittleEndian.Uint16(buf[0:2]))
		if msglen < unix.SizeofRtMsghdr || msglen > len(buf) {
			break
		}
		msg := buf[:msglen]
		buf = buf[msglen:]
		addrs := int32(binary.LittleEndian.Uint32(msg[12:16]))
		b := msg[unix.SizeofRtMsghdr:]
		var ip net.IP
		var mac []byte
		for i := 0; i < unix.RTAX_MAX && len(b) > 0; i++ {
			if addrs&(1<<uint(i)) == 0 {
				continue
			}
			l := int(b[0])
			step := 4
			if l > 0 {
				step = (l + 3) &^ 3
			}
			if step > len(b) {
				break
			}
			sa := b[:step]
			switch i {
			case unix.RTAX_DST:
				if l >= 8 && sa[1] == unix.AF_INET {
					ip = net.IPv4(sa[4], sa[5], sa[6], sa[7])
				}
			case unix.RTAX_GATEWAY:
				if l >= 8 && sa[1] == unix.AF_LINK {
					nlen, alen := int(sa[5]), int(sa[6])
					if alen == 6 && 8+nlen+alen <= l {
						mac = sa[8+nlen : 8+nlen+alen]
					}
				}
			}
			b = b[step:]
		}
		if ip != nil && mac != nil {
			if m := macString(mac); m != "" {
				out = append(out, ARPEntry{IP: ip.String(), MAC: m})
			}
		}
	}
	return out
}
