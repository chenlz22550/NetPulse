package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Window 一个监控时段。
type Window struct {
	// Days 星期几生效：1=周一 … 7=周日；为空表示每天。
	Days []int `yaml:"days,omitempty" json:"days"`
	// Start / End 为 "HH:MM"；End 小于等于 Start 表示跨过午夜（如 22:00-06:00）。
	Start string `yaml:"start" json:"start"`
	End   string `yaml:"end" json:"end"`
}

// Schedule 全局监控时段。未启用时全天候监控；启用后只在任一时段内 ping 和告警。
type Schedule struct {
	Enabled bool     `yaml:"enabled" json:"enabled"`
	Windows []Window `yaml:"windows" json:"windows"`
}

func parseHM(s string) (int, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return 0, fmt.Errorf("时间 %q 格式应为 HH:MM", s)
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 24 || mm < 0 || mm > 59 || (hh == 24 && mm != 0) {
		return 0, fmt.Errorf("时间 %q 格式应为 HH:MM（00:00~24:00）", s)
	}
	return hh*60 + mm, nil
}

// Validate 检查时段配置。
func (s Schedule) Validate() error {
	if s.Enabled && len(s.Windows) == 0 {
		return fmt.Errorf("启用监控时段后至少需要一个时段")
	}
	for i, w := range s.Windows {
		a, err := parseHM(w.Start)
		if err != nil {
			return fmt.Errorf("第 %d 个时段开始%v", i+1, err)
		}
		b, err := parseHM(w.End)
		if err != nil {
			return fmt.Errorf("第 %d 个时段结束%v", i+1, err)
		}
		if a == b {
			return fmt.Errorf("第 %d 个时段开始和结束时间相同", i+1)
		}
		for _, d := range w.Days {
			if d < 1 || d > 7 {
				return fmt.Errorf("第 %d 个时段 days 只能是 1~7（1=周一，7=周日）", i+1)
			}
		}
	}
	return nil
}

func isoWeekday(t time.Time) int {
	d := int(t.Weekday())
	if d == 0 {
		return 7
	}
	return d
}

func dayIn(days []int, d int) bool {
	if len(days) == 0 {
		return true
	}
	for _, x := range days {
		if x == d {
			return true
		}
	}
	return false
}

// Active 判断 t（本地时间）是否处于监控时段内。
func (s Schedule) Active(t time.Time) bool {
	if !s.Enabled {
		return true
	}
	now := t.Hour()*60 + t.Minute()
	today := isoWeekday(t)
	yesterday := isoWeekday(t.AddDate(0, 0, -1))
	for _, w := range s.Windows {
		a, err1 := parseHM(w.Start)
		b, err2 := parseHM(w.End)
		if err1 != nil || err2 != nil {
			continue
		}
		if a < b {
			if dayIn(w.Days, today) && now >= a && now < b {
				return true
			}
			continue
		}
		// 跨午夜：开始当天的 [a, 24:00) 以及次日的 [00:00, b)
		if dayIn(w.Days, today) && now >= a {
			return true
		}
		if dayIn(w.Days, yesterday) && now < b {
			return true
		}
	}
	return false
}

var weekdayName = []string{"", "一", "二", "三", "四", "五", "六", "日"}

// Describe 返回时段的简短中文描述。
func (s Schedule) Describe() string {
	if !s.Enabled {
		return "全天候监控"
	}
	var parts []string
	for _, w := range s.Windows {
		days := "每天"
		if len(w.Days) > 0 && len(w.Days) < 7 {
			var names []string
			for _, d := range w.Days {
				if d >= 1 && d <= 7 {
					names = append(names, weekdayName[d])
				}
			}
			days = "周" + strings.Join(names, "、")
		}
		parts = append(parts, fmt.Sprintf("%s %s-%s", days, w.Start, w.End))
	}
	return strings.Join(parts, "；")
}
