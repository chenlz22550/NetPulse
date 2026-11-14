package pinger

import (
	"testing"
	"time"
)

func TestLoopbackPingAndTTL(t *testing.T) {
	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	res, err := p.Ping("127.0.0.1", time.Second, 32)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Skip("系统 ping 不可用，跳过回环测试")
	}
	if res.RTT < 0 || res.RTT > time.Second {
		t.Fatalf("unexpected RTT: %v", res.RTT)
	}

	if tp, ok := p.(TTLPinger); ok {
		hop, err := tp.PingTTL("127.0.0.1", 1, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if !hop.Reached || hop.From != "127.0.0.1" {
			t.Fatalf("unexpected TTL hop: %+v", hop)
		}
	}
}
