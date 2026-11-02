package schedule

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

type parsedCron struct {
	minutes  [60]bool
	hours    [24]bool
	days     [32]bool // 1..31
	months   [13]bool // 1..12
	weekdays [7]bool  // 0..6 (Sunday=0)
	domStar  bool
	dowStar  bool
}

// ParseCron parses a standard 5-field cron expression ("min hour dom month dow").
func ParseCron(expr string) (string, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return "", errors.New("Cron 表达式需要 5 段（分 时 日 月 周）")
	}
	if _, err := compileCron(fields); err != nil {
		return "", err
	}
	return strings.Join(fields, " "), nil
}

// NextCron returns the first run strictly after after that matches expr in loc.
func NextCron(expr string, after time.Time, loc *time.Location) (time.Time, bool) {
	if loc == nil {
		loc = time.Local
	}
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return time.Time{}, false
	}
	c, err := compileCron(fields)
	if err != nil {
		return time.Time{}, false
	}
	// Start at the next whole minute strictly after `after`.
	cur := after.In(loc).Truncate(time.Minute).Add(time.Minute)
	limit := cur.AddDate(5, 0, 0)
	for cur.Before(limit) {
		if !c.months[int(cur.Month())] {
			cur = time.Date(cur.Year(), cur.Month()+1, 1, 0, 0, 0, 0, loc)
			continue
		}
		if !c.matchDay(cur) {
			cur = time.Date(cur.Year(), cur.Month(), cur.Day()+1, 0, 0, 0, 0, loc)
			continue
		}
		if !c.hours[cur.Hour()] {
			cur = time.Date(cur.Year(), cur.Month(), cur.Day(), cur.Hour()+1, 0, 0, 0, loc)
			continue
		}
		if !c.minutes[cur.Minute()] {
			cur = cur.Add(time.Minute)
			continue
		}
		return cur, true
	}
	return time.Time{}, false
}

func (c parsedCron) matchDay(t time.Time) bool {
	domOK := c.days[t.Day()]
	dowOK := c.weekdays[int(t.Weekday())]
	// Standard Vixie cron semantics: when both DOM and DOW are restricted, either matching triggers.
	if !c.domStar && !c.dowStar {
		return domOK || dowOK
	}
	return domOK && dowOK
}

func compileCron(fields []string) (parsedCron, error) {
	var c parsedCron
	mins, _, err := parseCronField(fields[0], 0, 59, false)
	if err != nil {
		return c, errors.New("Cron 分钟段格式不对")
	}
	copy(c.minutes[:], mins)

	hrs, _, err := parseCronField(fields[1], 0, 23, false)
	if err != nil {
		return c, errors.New("Cron 小时段格式不对")
	}
	copy(c.hours[:], hrs)

	doms, domStar, err := parseCronField(fields[2], 1, 31, false)
	if err != nil {
		return c, errors.New("Cron 日期段格式不对")
	}
	copy(c.days[:], doms)
	c.domStar = domStar

	mons, _, err := parseCronField(fields[3], 1, 12, false)
	if err != nil {
		return c, errors.New("Cron 月份段格式不对")
	}
	copy(c.months[:], mons)

	dows, dowStar, err := parseCronField(fields[4], 0, 7, true)
	if err != nil {
		return c, errors.New("Cron 星期段格式不对")
	}
	copy(c.weekdays[:], dows[:7])
	c.dowStar = dowStar
	return c, nil
}

func parseCronField(field string, minVal, maxVal int, mapSevenToZero bool) ([]bool, bool, error) {
	out := make([]bool, maxVal+1)
	if field == "*" {
		for i := minVal; i <= maxVal; i++ {
			idx := i
			if mapSevenToZero && idx == 7 {
				idx = 0
			}
			out[idx] = true
		}
		return out, true, nil
	}
	parts := strings.Split(field, ",")
	anySet := false
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, false, errors.New("empty cron token")
		}
		step := 1
		rangePart := part
		if base, stepStr, hasStep := strings.Cut(part, "/"); hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n <= 0 {
				return nil, false, errors.New("bad cron step")
			}
			step = n
			rangePart = base
		}
		var lo, hi int
		switch {
		case rangePart == "*":
			lo, hi = minVal, maxVal
		case strings.Contains(rangePart, "-"):
			aStr, bStr, _ := strings.Cut(rangePart, "-")
			a, err1 := strconv.Atoi(aStr)
			b, err2 := strconv.Atoi(bStr)
			if err1 != nil || err2 != nil || a < minVal || b > maxVal || a > b {
				return nil, false, errors.New("bad cron range")
			}
			lo, hi = a, b
		default:
			v, err := strconv.Atoi(rangePart)
			if err != nil || v < minVal || v > maxVal {
				return nil, false, errors.New("bad cron value")
			}
			lo, hi = v, v
			if strings.Contains(part, "/") {
				hi = maxVal
			}
		}
		for i := lo; i <= hi; i += step {
			idx := i
			if mapSevenToZero && idx == 7 {
				idx = 0
			}
			out[idx] = true
			anySet = true
		}
	}
	if !anySet {
		return nil, false, errors.New("empty cron set")
	}
	return out, false, nil
}
