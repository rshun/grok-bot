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

// CalendarSlot represents one calendar trigger combination; nil fields mean wildcard (*).
type CalendarSlot struct {
	Month   *int // 1..12
	Day     *int // 1..31
	Weekday *int // 0..6 (Sunday=0)
	Hour    *int // 0..23
	Minute  *int // 0..59
}

// CronStructure exposes the compiled boolean bitmaps of a 5-field cron expression
// for OS-native scheduler translation (launchd, Windows Task Scheduler).
type CronStructure struct {
	Minutes  []int // 0..59 (empty means all 60 minutes = wildcard)
	Hours    []int // 0..23 (empty means all 24 hours = wildcard)
	Days     []int // 1..31 (empty when DOMStar is true)
	Months   []int // 1..12 (empty means all 12 months = wildcard)
	Weekdays []int // 0..6  (empty when DOWStar is true)
	DOMStar  bool
	DOWStar  bool
}

// AnalyzeCron compiles a 5-field cron expression into its active values and wildcard flags.
func AnalyzeCron(expr string) (CronStructure, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return CronStructure{}, errors.New("Cron 表达式需要 5 段（分 时 日 月 周）")
	}
	c, err := compileCron(fields)
	if err != nil {
		return CronStructure{}, err
	}
	st := CronStructure{
		Minutes: collapseFull(collectTrue(c.minutes[:], 0, 59), 60),
		Hours:   collapseFull(collectTrue(c.hours[:], 0, 23), 24),
		Months:  collapseFull(collectTrue(c.months[:], 1, 12), 12),
		DOMStar: c.domStar,
		DOWStar: c.dowStar,
	}
	if !c.domStar {
		st.Days = collectTrue(c.days[:], 1, 31)
	}
	if !c.dowStar {
		st.Weekdays = collectTrue(c.weekdays[:], 0, 6)
	}
	return st, nil
}

// ExpandCronSlots expands a 5-field cron expression into CalendarSlot entries suitable
// for macOS launchd StartCalendarInterval. Returns ok=false when the Cartesian product
// exceeds maxSlots so callers can fall back to a 60-second interval trigger.
func ExpandCronSlots(expr string, maxSlots int) (slots []CalendarSlot, ok bool, err error) {
	st, err := AnalyzeCron(expr)
	if err != nil {
		return nil, false, err
	}
	if maxSlots <= 0 {
		maxSlots = 128
	}
	mons := optionalInts(st.Months)
	hrs := optionalInts(st.Hours)
	mins := optionalInts(st.Minutes)

	type dayPair struct {
		day     *int
		weekday *int
	}
	var pairs []dayPair
	switch {
	case st.DOMStar && st.DOWStar:
		pairs = []dayPair{{}}
	case !st.DOMStar && st.DOWStar:
		for _, d := range st.Days {
			v := d
			pairs = append(pairs, dayPair{day: &v})
		}
	case st.DOMStar && !st.DOWStar:
		for _, w := range st.Weekdays {
			v := w
			pairs = append(pairs, dayPair{weekday: &v})
		}
	default:
		// Standard Vixie cron OR semantics when both DOM and DOW are restricted.
		for _, d := range st.Days {
			v := d
			pairs = append(pairs, dayPair{day: &v})
		}
		for _, w := range st.Weekdays {
			v := w
			pairs = append(pairs, dayPair{weekday: &v})
		}
	}

	total := len(mons) * len(pairs) * len(hrs) * len(mins)
	if total == 0 || total > maxSlots {
		return nil, false, nil
	}
	slots = make([]CalendarSlot, 0, total)
	for _, mon := range mons {
		for _, dp := range pairs {
			for _, hr := range hrs {
				for _, min := range mins {
					slots = append(slots, CalendarSlot{
						Month:   mon,
						Day:     dp.day,
						Weekday: dp.weekday,
						Hour:    hr,
						Minute:  min,
					})
				}
			}
		}
	}
	return slots, true, nil
}

func collectTrue(bits []bool, lo, hi int) []int {
	var out []int
	for i := lo; i <= hi && i < len(bits); i++ {
		if bits[i] {
			out = append(out, i)
		}
	}
	return out
}

func collapseFull(vals []int, full int) []int {
	if len(vals) >= full {
		return nil
	}
	return vals
}

func optionalInts(vals []int) []*int {
	if len(vals) == 0 {
		return []*int{nil}
	}
	out := make([]*int, len(vals))
	for i, v := range vals {
		val := v
		out[i] = &val
	}
	return out
}
