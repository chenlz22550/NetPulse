// Package pinger 提供单次 ICMP Echo 探测。
package pinger

import "time"

// Result 是一次 ping 的结果。
type Result struct {
	OK  bool
	RTT time.Duration
}

// Pinger 发送 ICMP Echo。每个监控协程持有一个实例，不要求并发安全。
type Pinger interface {
	Ping(ip string, timeout time.Duration, size int) (Result, error)
	Close() error
}

// Hop 是一次指定 TTL 的探测结果（路由追踪用）。
type Hop struct {
	From    string        // 回应者地址：中间路由器（TTL 超时）或目标本身；无回应时为空
	Reached bool          // 收到目标的 Echo Reply
	Status  string        // 非正常回应的说明，如“目标主机不可达”
	RTT     time.Duration // 有回应时的往返时间
}

// TTLPinger 支持指定 TTL 发送 ICMP Echo。
type TTLPinger interface {
	PingTTL(ip string, ttl int, timeout time.Duration) (Hop, error)
}
