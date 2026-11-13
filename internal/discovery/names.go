package discovery

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// 主机名解析：对每个在线地址同时尝试 NetBIOS（Windows 计算机名）、mDNS（苹果 / Linux / 打印机等的 xxx.local）
// 和反向 DNS，按 NetBIOS > mDNS > DNS 取第一个有效结果。全部是普通 UDP 查询，不需要管理员权限。

const nameTimeout = 700 * time.Millisecond

// ResolveNames 为 hosts 填写 Hostname（已有的保留）。
func ResolveNames(hosts []Host) {
	sem := make(chan struct{}, 32)
	var wg sync.WaitGroup
	for i := range hosts {
		h := &hosts[i]
		if h.Hostname != "" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			h.Hostname = lookupHost(h.IP)
		}()
	}
	wg.Wait()
}

func lookupHost(ip string) string {
	var nb, md, dn string
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); nb, _ = netbiosName(ip, nameTimeout) }()
	go func() { defer wg.Done(); md, _ = mdnsName(ip, nameTimeout) }()
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), nameTimeout+100*time.Millisecond)
		defer cancel()
		if names, err := net.DefaultResolver.LookupAddr(ctx, ip); err == nil && len(names) > 0 {
			dn = names[0]
		}
	}()
	wg.Wait()
	for _, n := range []string{nb, md, dn} {
		if n = CleanHostname(n, ip); n != "" {
			return n
		}
	}
	return ""
}

var (
	localSuffix = regexp.MustCompile(`(?i)\.(local|lan|home|localdomain|home\.arpa|internal|router)$`)
	ipInName    = regexp.MustCompile(`\d{1,3}[-.]\d{1,3}[-.]\d{1,3}[-.]\d{1,3}`)
)

// CleanHostname 去掉末尾的点和常见局域网后缀；名字只是 IP 的变形（如 192-168-1-5.isp.net）时视为无效。
func CleanHostname(name, ip string) string {
	name = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(name), "."))
	name = localSuffix.ReplaceAllString(name, "")
	if name == "" || name == ip || ipInName.MatchString(name) || strings.EqualFold(name, "localhost") {
		return ""
	}
	if len(name) > 100 {
		name = name[:100]
	}
	// 只保留可打印字符
	b := strings.Builder{}
	for _, r := range name {
		if r >= 0x20 && r != 0x7f {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// ---------------------------------------------------------------------------
// NetBIOS 节点状态查询（UDP 137，NBSTAT）

func netbiosName(ip string, timeout time.Duration) (string, error) {
	c, err := net.DialTimeout("udp4", net.JoinHostPort(ip, "137"), timeout)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))
	id := uint16(rand.Intn(0xffff))
	q := make([]byte, 0, 50)
	q = binary.BigEndian.AppendUint16(q, id)
	q = append(q, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0) // flags=0, QD=1
	q = append(q, 0x20)
	name := [16]byte{'*'}
	for _, ch := range name {
		q = append(q, 'A'+ch>>4, 'A'+ch&0x0f)
	}
	q = append(q, 0, 0, 0x21, 0, 1) // NBSTAT / IN
	if _, err := c.Write(q); err != nil {
		return "", err
	}
	buf := make([]byte, 1500)
	n, err := c.Read(buf)
	if err != nil {
		return "", err
	}
	return parseNBSTAT(buf[:n], id)
}

func parseNBSTAT(b []byte, id uint16) (string, error) {
	if len(b) < 12 || binary.BigEndian.Uint16(b) != id || binary.BigEndian.Uint16(b[6:]) == 0 {
		return "", errors.New("无效的 NetBIOS 响应")
	}
	off, err := skipName(b, 12)
	if err != nil {
		return "", err
	}
	off += 10 // type class ttl rdlength
	if off >= len(b) {
		return "", errors.New("NetBIOS 响应过短")
	}
	cnt := int(b[off])
	off++
	var group string
	for i := 0; i < cnt && off+18 <= len(b); i, off = i+1, off+18 {
		nm := strings.TrimRight(string(b[off:off+15]), " \x00")
		suffix := b[off+15]
		flags := binary.BigEndian.Uint16(b[off+16:])
		if suffix != 0x00 || nm == "" {
			continue
		}
		if flags&0x8000 == 0 { // 唯一名 = 计算机名；组名是工作组
			return nm, nil
		}
		if group == "" {
			group = nm
		}
	}
	return "", errors.New("没有计算机名")
}

// ---------------------------------------------------------------------------
// mDNS 单播反向查询（直接发到设备的 UDP 5353，按 RFC 6762 的 legacy unicast 回复）

func reverseName(ip string) (string, error) {
	p := net.ParseIP(ip).To4()
	if p == nil {
		return "", fmt.Errorf("不是 IPv4 地址: %s", ip)
	}
	return fmt.Sprintf("%d.%d.%d.%d.in-addr.arpa", p[3], p[2], p[1], p[0]), nil
}

func mdnsName(ip string, timeout time.Duration) (string, error) {
	rn, err := reverseName(ip)
	if err != nil {
		return "", err
	}
	c, err := net.DialTimeout("udp4", net.JoinHostPort(ip, "5353"), timeout)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))
	id := uint16(rand.Intn(0xffff))
	q := binary.BigEndian.AppendUint16(nil, id)
	q = append(q, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0)
	for _, l := range strings.Split(rn, ".") {
		q = append(q, byte(len(l)))
		q = append(q, l...)
	}
	q = append(q, 0, 0, 12, 0, 1) // PTR / IN
	if _, err := c.Write(q); err != nil {
		return "", err
	}
	buf := make([]byte, 9000)
	n, err := c.Read(buf)
	if err != nil {
		return "", err
	}
	return parsePTR(buf[:n])
}

// parsePTR 从 DNS 响应中取第一个 PTR 记录的目标名。
func parsePTR(b []byte) (string, error) {
	if len(b) < 12 {
		return "", errors.New("响应过短")
	}
	qd, an := int(binary.BigEndian.Uint16(b[4:])), int(binary.BigEndian.Uint16(b[6:]))
	off := 12
	var err error
	for i := 0; i < qd; i++ {
		if off, err = skipName(b, off); err != nil {
			return "", err
		}
		off += 4
	}
	for i := 0; i < an; i++ {
		if off, err = skipName(b, off); err != nil {
			return "", err
		}
		if off+10 > len(b) {
			return "", errors.New("响应过短")
		}
		typ := binary.BigEndian.Uint16(b[off:])
		rdlen := int(binary.BigEndian.Uint16(b[off+8:]))
		off += 10
		if off+rdlen > len(b) {
			return "", errors.New("响应过短")
		}
		if typ == 12 {
			return readName(b, off)
		}
		off += rdlen
	}
	return "", errors.New("没有 PTR 记录")
}

func skipName(b []byte, off int) (int, error) {
	for off < len(b) {
		l := int(b[off])
		switch {
		case l == 0:
			return off + 1, nil
		case l&0xc0 == 0xc0:
			return off + 2, nil
		default:
			off += 1 + l
		}
	}
	return 0, errors.New("名字越界")
}

func readName(b []byte, off int) (string, error) {
	var parts []string
	for jumps := 0; jumps < 20; {
		if off >= len(b) {
			return "", errors.New("名字越界")
		}
		l := int(b[off])
		switch {
		case l == 0:
			return strings.Join(parts, "."), nil
		case l&0xc0 == 0xc0:
			if off+1 >= len(b) {
				return "", errors.New("名字越界")
			}
			off = int(binary.BigEndian.Uint16(b[off:]) & 0x3fff)
			jumps++
		default:
			if off+1+l > len(b) {
				return "", errors.New("名字越界")
			}
			parts = append(parts, string(b[off+1:off+1+l]))
			off += 1 + l
		}
	}
	return "", errors.New("名字指针过多")
}

// localHostname 运行 netPing 的本机名。
func localHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return CleanHostname(h, "")
}
