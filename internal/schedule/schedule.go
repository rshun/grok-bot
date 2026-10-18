// Package schedule turns Grok's reading of a scheduling request into run times.
package schedule

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	atLayout    = "2006-01-02 15:04"
	clockLayout = "15:04"
)

// Spec is when a task runs.
type Spec struct {
	// Kind is once, daily, weekly or interval.
	Kind string `json:"kind"`
	// At is the local date and time of a once task.
	At string `json:"at,omitempty"`
	// Time is the local HH:MM of a daily or weekly task.
	Time string `json:"time,omitempty"`
	// Weekdays uses 0 for Sunday through 6 for Saturday.
	Weekdays     []int `json:"weekdays,omitempty"`
	EveryMinutes int   `json:"every_minutes,omitempty"`
}

// Parsed is Grok's structured answer to a scheduling request.
type Parsed struct {
	Spec
	Prompt  string `json:"prompt"`
	Summary string `json:"summary"`
	Error   string `json:"error,omitempty"`
}

// Schema is passed to grok with --json-schema.
const Schema = `{"type":"object","properties":{` +
	`"kind":{"type":"string","enum":["once","daily","weekly","interval"]},` +
	`"at":{"type":"string","description":"once: local time YYYY-MM-DD HH:MM"},` +
	`"time":{"type":"string","description":"daily or weekly: local HH:MM"},` +
	`"weekdays":{"type":"array","items":{"type":"integer","minimum":0,"maximum":6}},` +
	`"every_minutes":{"type":"integer","minimum":1},` +
	`"prompt":{"type":"string"},` +
	`"summary":{"type":"string"},` +
	`"error":{"type":"string"}}}`

// Prompt asks Grok to read text as a schedule relative to now.
func Prompt(text string, now time.Time, loc *time.Location) string {
	local := now.In(loc)
	return fmt.Sprintf(`Current local time: %s (%s, %s).
Turn the scheduling request below into JSON.
- kind "once": set "at" to the local time as YYYY-MM-DD HH:MM.
- kind "daily": set "time" to HH:MM.
- kind "weekly": set "time" to HH:MM and "weekdays" (0 = Sunday ... 6 = Saturday).
- kind "interval": set "every_minutes".
- "prompt": what Grok should do each time the task runs, as an instruction in the user's language, without the schedule.
- "summary": one short Chinese line such as "每天 09:00：总结科技新闻".
If the request gives no clear time, return only {"error": "<short reason in Chinese>"}.

Request: %s`, local.Format(atLayout), local.Format("MST"), loc.String(), text)
}

// Decode reads the structured output, falling back to JSON in the reply text.
func Decode(structured json.RawMessage, text string) (Parsed, error) {
	raw := []byte(strings.TrimSpace(string(structured)))
	if len(raw) == 0 || string(raw) == "null" {
		raw = []byte(stripFence(text))
	}
	var p Parsed
	if err := json.Unmarshal(raw, &p); err != nil {
		return Parsed{}, fmt.Errorf("decode schedule: %w", err)
	}
	return p, nil
}

func stripFence(text string) string {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "```") {
		return text
	}
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[i+1:]
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), "```"))
}

// Validate checks a spec before it is saved. Errors are short Chinese reasons.
func (s Spec) Validate(now time.Time, loc *time.Location, minInterval time.Duration) error {
	switch s.Kind {
	case "once":
		at, err := time.ParseInLocation(atLayout, s.At, loc)
		if err != nil {
			return errors.New("时间格式不对")
		}
		if !at.After(now) {
			return errors.New("这个时间已经过去了")
		}
	case "daily":
		if _, _, err := clock(s.Time); err != nil {
			return err
		}
	case "weekly":
		if _, _, err := clock(s.Time); err != nil {
			return err
		}
		if len(s.Weekdays) == 0 {
			return errors.New("没有说是星期几")
		}
		for _, d := range s.Weekdays {
			if d < 0 || d > 6 {
				return errors.New("星期几不对")
			}
		}
	case "interval":
		if s.EveryMinutes <= 0 {
			return errors.New("没有说间隔多久")
		}
		if time.Duration(s.EveryMinutes)*time.Minute < minInterval {
			return fmt.Errorf("周期任务的间隔不能少于 %d 分钟", int(minInterval/time.Minute))
		}
	default:
		return errors.New("不支持这种时间规则")
	}
	return nil
}

func clock(text string) (hour, minute int, err error) {
	t, err := time.Parse(clockLayout, text)
	if err != nil {
		return 0, 0, errors.New("时刻格式不对")
	}
	return t.Hour(), t.Minute(), nil
}

// Next returns the first run strictly after after. ok is false when a once task has passed.
func (s Spec) Next(after time.Time, loc *time.Location) (time.Time, bool) {
	switch s.Kind {
	case "once":
		at, err := time.ParseInLocation(atLayout, s.At, loc)
		if err != nil || !at.After(after) {
			return time.Time{}, false
		}
		return at, true
	case "interval":
		if s.EveryMinutes <= 0 {
			return time.Time{}, false
		}
		return after.Add(time.Duration(s.EveryMinutes) * time.Minute), true
	case "daily", "weekly":
		hour, minute, err := clock(s.Time)
		if err != nil {
			return time.Time{}, false
		}
		local := after.In(loc)
		// Building each day with time.Date keeps the wall clock right across DST changes.
		for i := 0; i <= 7; i++ {
			day := local.AddDate(0, 0, i)
			candidate := time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, loc)
			if candidate.After(after) && (s.Kind == "daily" || s.onWeekday(candidate.Weekday())) {
				return candidate, true
			}
		}
	}
	return time.Time{}, false
}

func (s Spec) onWeekday(day time.Weekday) bool {
	for _, d := range s.Weekdays {
		if time.Weekday(d) == day {
			return true
		}
	}
	return false
}
