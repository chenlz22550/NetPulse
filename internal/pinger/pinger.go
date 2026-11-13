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
