package server

import (
	"testing"
	"testing/quick"
	"time"

	"netping/internal/monitor"
	"netping/internal/store"
)

// 性质测试：任意样本列表按 bucketSamples 分桶后，各桶时间单调递增、总发送数守恒，且 0 <= Loss <= 100。
func TestBucketSamplesFormalProperties(t *testing.T) {
	if err := quick.Check(func(rtts []int8, stepMs uint8) bool {
		if len(rtts) == 0 {
			return len(bucketSamples(nil, time.Second)) == 0
		}
		step := int64(stepMs%250) + 50
		samples := make([]monitor.Sample, len(rtts))
		var cur int64 = 10_000
		for i, r := range rtts {
			cur += step
			rtt := float64(r)
			if r < 0 {
				rtt = -1
			}
			samples[i] = monitor.Sample{T: cur, RTT: rtt}
		}
		pts := bucketSamples(samples, time.Second)
		totalSent := 0
		for i, p := range pts {
			if p.Sent <= 0 || p.Loss < 0 || p.Loss > 100 {
				return false
			}
			if i > 0 && p.T <= pts[i-1].T {
				return false
			}
			if p.Avg != nil && (*p.Min > *p.Avg+0.01 || *p.Avg > *p.Max+0.01) {
				return false
			}
			totalSent += p.Sent
		}
		return totalSent == len(samples)
	}, &quick.Config{MaxCount: 150}); err != nil {
		t.Fatal(err)
	}
}

// 性质测试：任意乱序分钟数据经 bucketMinutes 分桶后，桶时间严格递增、总发送数守恒，且不出现 NaN/越界丢包率。
func TestBucketMinutesFormalProperties(t *testing.T) {
	if err := quick.Check(func(offsets []uint8, sents []uint8, recvs []uint8) bool {
		n := len(offsets)
		if len(sents) < n {
			n = len(sents)
		}
		if len(recvs) < n {
			n = len(recvs)
		}
		var recs []store.Minute
		expectedSent := 0
		for i := 0; i < n; i++ {
			s := int(sents[i]%60) + 1
			r := int(recvs[i]) % (s + 1)
			m := store.Minute{T: int64(offsets[i]%20) * 60, Sent: s, Recv: r}
			if r > 0 {
				m.Min, m.Avg, m.Max = 1, 5, 10
			}
			recs = append(recs, m)
			expectedSent += s
		}
		pts := bucketMinutes(recs, 300)
		gotSent := 0
		for i, p := range pts {
			if p.Loss < 0 || p.Loss > 100 {
				return false
			}
			if i > 0 && p.T <= pts[i-1].T {
				return false
			}
			gotSent += p.Sent
		}
		return gotSent == expectedSent
	}, &quick.Config{MaxCount: 150}); err != nil {
		t.Fatal(err)
	}
}
