//go:build !windows && !darwin

package discovery

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// ReadARP 读取系统 ARP 表（开发调试用：Linux 读 /proc/net/arp，macOS 解析 arp -an）。
func ReadARP() ([]ARPEntry, error) {
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile("/proc/net/arp")
		if err != nil {
			return nil, err
		}
		var out []ARPEntry
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Scan() // 表头
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) >= 4 && f[2] != "0x0" {
				if mac := normalizeMAC(f[3]); mac != "" {
					out = append(out, ARPEntry{IP: f[0], MAC: mac})
				}
			}
		}
		return out, nil
	}
	var data []byte
	var err error
	if f := os.Getenv("NETPING_ARP_FILE"); f != "" { // 开发调试：从文件读取 arp -an 格式的内容
		data, err = os.ReadFile(f)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		data, err = exec.CommandContext(ctx, "arp", "-an").Output()
	}
	if err != nil {
		return nil, err
	}
	return parseARPText(string(data)), nil
}
