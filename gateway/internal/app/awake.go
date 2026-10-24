package app

import (
	"fmt"
	"strings"
	"time"
)

// dayWindow 是每天的一段时间（醒着的时间），可以跨午夜（22:00-06:00）。start == end 表示全天。
type dayWindow struct{ start, end int } // 从零点算起的分钟

func parseWindow(s string) (dayWindow, error) {
	a, b, ok := strings.Cut(strings.ReplaceAll(strings.TrimSpace(s), "～", "-"), "-")
	if !ok {
		return dayWindow{}, fmt.Errorf("时间段写成 08:00-23:30 这样：%q", s)
	}
	start, err1 := parseClock(a)
	end, err2 := parseClock(b)
	if err1 != nil || err2 != nil {
		return dayWindow{}, fmt.Errorf("时间段写成 08:00-23:30 这样：%q", s)
	}
	return dayWindow{start, end}, nil
}

// parseClock 读 08:00、8:30、24:00
func parseClock(s string) (int, error) {
	var h, m int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d:%d", &h, &m); err != nil || h < 0 || h > 24 || m < 0 || m > 59 || h == 24 && m != 0 {
		return 0, fmt.Errorf("时间写成 08:00 这样：%q", s)
	}
	return h*60 + m, nil
}

func minuteOf(t time.Time) int { return t.Hour()*60 + t.Minute() }

func (w dayWindow) always() bool { return w.start%(24*60) == w.end%(24*60) }

// contains 是 t 在这段时间里（醒着）。
func (w dayWindow) contains(t time.Time) bool {
	if w.always() {
		return true
	}
	m := minuteOf(t)
	if w.start < w.end {
		return m >= w.start && m < w.end
	}
	return m >= w.start || m < w.end
}

func at(day time.Time, minute int) time.Time {
	y, mo, d := day.Date()
	return time.Date(y, mo, d, 0, 0, 0, 0, day.Location()).Add(time.Duration(minute) * time.Minute)
}

// lastEnd 是 t 之前（含 t）最近一次这段时间结束（开始睡觉）的时刻；全天时是零值。
func (w dayWindow) lastEnd(t time.Time) time.Time {
	if w.always() {
		return time.Time{}
	}
	e := at(t, w.end%(24*60))
	if e.After(t) {
		e = e.AddDate(0, 0, -1)
	}
	return e
}

// nextStart 是 t 之后最近一次这段时间开始（醒来）的时刻。
func (w dayWindow) nextStart(t time.Time) time.Time {
	s := at(t, w.start%(24*60))
	if !s.After(t) {
		s = s.AddDate(0, 0, 1)
	}
	return s
}
