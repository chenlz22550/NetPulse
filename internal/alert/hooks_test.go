package alert

import (
	"errors"
	"testing"
	"time"

	"netping/internal/config"
	"netping/internal/monitor"
)

// 插件接入点：SetSender 替换发送函数，OnEvents 在事件写入历史后收到带发送结果的副本。
func TestSetSenderAndOnEvents(t *testing.T) {
	e, _ := newTestEngine(t)
	var contents []string
	fail := false
	e.SetSender(nil) // 忽略 nil
	e.SetSender(func(_ config.AlertConfig, content string) error {
		contents = append(contents, content)
		if fail {
			return errors.New("插件 demo: boom")
		}
		return nil
	})
	type batch struct {
		evs      []Event
		notified bool
	}
	var got []batch
	e.OnEvents = func(evs []Event, notified bool) { got = append(got, batch{evs, notified}) }

	now := time.Now()
	e.Evaluate(now, []monitor.Status{st("n-192-168-50-1", "node", 10, 0)})
	e.flushIfDue(now.Add(time.Minute))
	if len(contents) != 1 || len(got) != 1 || !got[0].notified || len(got[0].evs) != 1 {
		t.Fatalf("first batch: sent=%d got=%+v", len(contents), got)
	}
	if ev := got[0].evs[0]; ev.Type != EvDown || ev.TargetID != "n-192-168-50-1" || !ev.Sent || ev.Error != "" {
		t.Fatalf("event: %+v", ev)
	}
	// 修改回调收到的切片不影响历史。
	got[0].evs[0].Type = "changed"
	if e.History(1)[0].Type != EvDown {
		t.Fatal("OnEvents shares the history slice")
	}
	// 发送失败：事件仍然送达，带失败原因。
	fail = true
	e.Evaluate(now.Add(2*time.Minute), []monitor.Status{st("n-192-168-50-1", "node", 0, 10)})
	e.flushIfDue(now.Add(3 * time.Minute))
	if len(got) != 2 || got[1].notified || got[1].evs[0].Type != EvRecover || got[1].evs[0].Sent || got[1].evs[0].Error == "" {
		t.Fatalf("failed batch: %+v", got)
	}
}
