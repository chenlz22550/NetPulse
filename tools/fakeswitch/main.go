// fakeswitch 启动一台模拟交换机（SNMP v2c 只读），用于在没有真实交换机时演示和调试端口定位。
//
//	go run ./tools/fakeswitch -listen 127.0.0.1:16100 -community public -macs "2:0:0:0:0:12=1,02:00:00:00:00:08=1,02:00:00:00:00:17=26"
package main

import (
	"flag"
	"fmt"
	"log"
	"strings"

	"netping/internal/snmp"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:16100", "监听地址")
	community := flag.String("community", "public", "只读团体名")
	ports := flag.Int("ports", 26, "端口数量")
	name := flag.String("name", "fake-switch", "sysName")
	prefix := flag.String("ifname", "GigabitEthernet 0/", "端口名前缀")
	macs := flag.String("macs", "", "MAC=端口,MAC=端口,…")
	tail := flag.Int("tail", 0, "最后 N 个端口使用 -tailprefix 命名（如万兆光口）")
	tailPrefix := flag.String("tailprefix", "TenGigabitEthernet 0/", "尾部端口名前缀")
	flag.Parse()

	a := &snmp.Agent{Community: *community}
	a.SetString("1.3.6.1.2.1.1.1.0", "Ruijie Networks (simulated)")
	a.SetString("1.3.6.1.2.1.1.5.0", *name)
	for i := 1; i <= *ports; i++ {
		pf := *prefix
		if i > *ports-*tail {
			pf = *tailPrefix
		}
		a.SetString(fmt.Sprintf("1.3.6.1.2.1.31.1.1.1.1.%d", i), fmt.Sprintf("%s%d", pf, i))
		a.SetInt(fmt.Sprintf("1.3.6.1.2.1.17.1.4.1.2.%d", i), int64(i))
	}
	for _, kv := range strings.Split(*macs, ",") {
		p := strings.SplitN(strings.TrimSpace(kv), "=", 2)
		if len(p) != 2 {
			continue
		}
		var b [6]int
		var port int
		fmt.Sscanf(p[0], "%x:%x:%x:%x:%x:%x", &b[0], &b[1], &b[2], &b[3], &b[4], &b[5])
		fmt.Sscanf(p[1], "%d", &port)
		idx := fmt.Sprintf("1.%d.%d.%d.%d.%d.%d", b[0], b[1], b[2], b[3], b[4], b[5])
		a.SetInt("1.3.6.1.2.1.17.7.1.2.2.1.2."+idx, int64(port))
		a.SetInt("1.3.6.1.2.1.17.7.1.2.2.1.3."+idx, 3)
	}
	addr, err := a.Listen(*listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("模拟交换机 %s 监听 %s（团体名 %s）", *name, addr, *community)
	select {}
}
