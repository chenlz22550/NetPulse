//go:build !windows

package pinger

import (
	"context"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"time"
)

// 非 Windows 平台（开发调试用）调用系统 ping 命令，避免需要 root 权限。

var timeRe = regexp.MustCompile(`time[=<]([0-9.]+)\s*ms`)

type execPinger struct{}

// New 创建一个 Pinger。
func New() (Pinger, error) { return execPinger{}, nil }

func (execPinger) Ping(ip string, timeout time.Duration, size int) (Result, error) {
	ms := int(timeout / time.Millisecond)
	if ms < 1 {
		ms = 1
	}
	var args []string
	if runtime.GOOS == "darwin" {
		args = []string{"-n", "-c", "1", "-W", strconv.Itoa(ms), "-s", strconv.Itoa(size), ip}
	} else {
		sec := (ms + 999) / 1000
		args = []string{"-n", "-c", "1", "-W", strconv.Itoa(sec), "-s", strconv.Itoa(size), ip}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout+2*time.Second)
	defer cancel()
	start := time.Now()
	out, err := exec.CommandContext(ctx, "ping", args...).Output()
	elapsed := time.Since(start)
	if err != nil {
		return Result{OK: false}, nil
	}
	if m := timeRe.FindSubmatch(out); m != nil {
		if v, perr := strconv.ParseFloat(string(m[1]), 64); perr == nil {
			d := time.Duration(v * float64(time.Millisecond))
			if d > timeout {
				return Result{OK: false}, nil
			}
			return Result{OK: true, RTT: d}, nil
		}
	}
	return Result{OK: true, RTT: elapsed}, nil
}

var (
	ttlExceededRe = regexp.MustCompile(`(?i)from ([0-9.]+)[: ].*(time to live exceeded|ttl exceeded)`)
	unreachableRe = regexp.MustCompile(`(?i)from ([0-9.]+)[: ].*unreachable`)
	replyFromRe   = regexp.MustCompile(`bytes from ([0-9.]+)`)
)

// PingTTL 调用系统 ping 并指定 TTL（开发调试用）。
func (execPinger) PingTTL(ip string, ttl int, timeout time.Duration) (Hop, error) {
	ms := int(timeout / time.Millisecond)
	if ms < 1 {
		ms = 1
	}
	var args []string
	if runtime.GOOS == "darwin" {
		args = []string{"-n", "-c", "1", "-m", strconv.Itoa(ttl), "-W", strconv.Itoa(ms), ip}
	} else {
		args = []string{"-n", "-c", "1", "-t", strconv.Itoa(ttl), "-W", strconv.Itoa((ms + 999) / 1000), ip}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout+2*time.Second)
	defer cancel()
	start := time.Now()
	out, _ := exec.CommandContext(ctx, "ping", args...).CombinedOutput()
	elapsed := time.Since(start)
	if m := ttlExceededRe.FindSubmatch(out); m != nil {
		return Hop{From: string(m[1]), RTT: elapsed}, nil
	}
	if m := unreachableRe.FindSubmatch(out); m != nil {
		return Hop{From: string(m[1]), Status: "目标不可达", RTT: elapsed}, nil
	}
	if m := replyFromRe.FindSubmatch(out); m != nil {
		h := Hop{From: string(m[1]), Reached: true, RTT: elapsed}
		if t := timeRe.FindSubmatch(out); t != nil {
			if v, err := strconv.ParseFloat(string(t[1]), 64); err == nil {
				h.RTT = time.Duration(v * float64(time.Millisecond))
			}
		}
		return h, nil
	}
	return Hop{}, nil
}

func (execPinger) Close() error { return nil }
