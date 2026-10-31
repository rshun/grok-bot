// Package schedule turns Grok's reading of a scheduling request into run times.
package schedule

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	atLayout      = "2006-01-02 15:04"
	clockLayout   = "15:04"
	isoDateLayout = "2006-01-02"
	compactLayout = "20060102"
)

// Spec is when a task runs.
type Spec struct {
	// Kind is once, daily, weekly, interval or dates.
	Kind string `json:"kind"`
	// At is the local date and time of a once task.
	At string `json:"at,omitempty"`
	// Time is the local HH:MM of a daily, weekly or dates task.
	Time string `json:"time,omitempty"`
	// Weekdays uses 0 for Sunday through 6 for Saturday.
	Weekdays     []int  `json:"weekdays,omitempty"`
	EveryMinutes int    `json:"every_minutes,omitempty"`
	DateFile     string `json:"date_file,omitempty"`
}

// Parsed is Grok's structured answer to a scheduling request.
type Parsed struct {
	Spec
	// Action is create (default), skip, pause, resume or delete.
	Action  string `json:"action,omitempty"`
	TaskID  int64  `json:"task_id,omitempty"`
	Prompt  string `json:"prompt"`
	Summary string `json:"summary"`
	Error   string `json:"error,omitempty"`
}

// ExistingTask describes a task already scheduled in the chat so Grok can
// match natural-language skip, pause, resume or delete requests without an ID.
type ExistingTask struct {
	ID      int64
	Summary string
	Prompt  string
	NextRun time.Time
	Paused  bool
}

// Schema is passed to grok with --json-schema.
const Schema = `{"type":"object","properties":{` +
	`"action":{"type":"string","enum":["create","skip","pause","resume","delete"]},` +
	`"task_id":{"type":"integer","description":"ID of the existing task when action is skip, pause, resume or delete"},` +
	`"kind":{"type":"string","enum":["once","daily","weekly","interval","dates"]},` +
	`"at":{"type":"string","description":"once: local time YYYY-MM-DD HH:MM"},` +
	`"time":{"type":"string","description":"daily, weekly or dates: local HH:MM"},` +
	`"weekdays":{"type":"array","items":{"type":"integer","minimum":0,"maximum":6}},` +
	`"every_minutes":{"type":"integer","minimum":1},` +
	`"date_file":{"type":"string","description":"dates: absolute path to a file of YYYY-MM-DD or YYYYMMDD dates"},` +
	`"prompt":{"type":"string"},` +
	`"summary":{"type":"string"},` +
	`"error":{"type":"string"}}}`

// Prompt asks Grok to read text as a schedule or task management request relative to now.
func Prompt(text string, now time.Time, loc *time.Location, tasks ...ExistingTask) string {
	local := now.In(loc)
	var taskBlock strings.Builder
	if len(tasks) > 0 {
		taskBlock.WriteString("\nExisting tasks in this chat:\n")
		for _, t := range tasks {
			status := "active, next " + t.NextRun.In(loc).Format(atLayout)
			if t.Paused {
				status = "paused"
			}
			fmt.Fprintf(&taskBlock, "- #%d: %s (prompt: %s; %s)\n", t.ID, t.Summary, t.Prompt, status)
		}
	}
	return fmt.Sprintf(`Current local time: %s %s (%s, %s).%s
Turn the scheduling request below into JSON.
- To create a new scheduled task, set "action" to "create":
  - kind "once": set "at" to the local time as YYYY-MM-DD HH:MM.
  - kind "daily": set "time" to HH:MM.
  - kind "weekly": set "time" to HH:MM and "weekdays" (0 = Sunday ... 6 = Saturday).
  - kind "interval": set "every_minutes".
  - kind "dates": when the request says to run according to dates in a file, set "date_file" to the file path and "time" to HH:MM (default "09:00" if no time of day is specified).
  - "prompt": what Grok should do each time the task runs, as an instruction in the user's language, without the schedule.
  - "summary": one short Chinese line such as "每天 09:00：总结科技新闻".
- To manage an existing task in natural language:
  - "action" = "skip": cancel/skip only the next run (e.g. "取消明天的定时任务", "明天不用执行了", "跳过下次"), keeping future runs.
  - "action" = "pause": pause an active task until resumed (e.g. "暂停定时任务", "先停一下").
  - "action" = "resume": resume a paused task (e.g. "恢复定时任务", "继续执行").
  - "action" = "delete": permanently cancel/remove a task (e.g. "删除定时任务", "取消这个定时任务").
  - Set "task_id" to the matching task's ID from the existing tasks list (if there is only one existing task, use its ID; if multiple tasks exist and the request is ambiguous, return {"error": "请说明要操作哪个定时任务"}).
If the request gives no clear time or action, return only {"error": "<short reason in Chinese>"}.

Request: %s`, local.Format(atLayout), local.Weekday(), local.Format("MST"), loc.String(), taskBlock.String(), text)
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
	p.DateFile = strings.TrimSpace(p.DateFile)
	if p.DateFile != "" {
		p.Kind = "dates"
	}
	if p.Kind == "dates" && strings.TrimSpace(p.Time) == "" {
		p.Time = "09:00"
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
	case "dates":
		if _, _, err := clock(s.Time); err != nil {
			return err
		}
		if _, err := readDateFile(s.DateFile, loc); err != nil {
			return err
		}
		if _, ok := s.Next(now, loc); !ok {
			return errors.New("日期文件中没有未来的执行时间")
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
	case "dates":
		hour, minute, err := clock(s.Time)
		if err != nil {
			return time.Time{}, false
		}
		dates, err := readDateFile(s.DateFile, loc)
		if err != nil {
			return time.Time{}, false
		}
		var best time.Time
		for _, d := range dates {
			candidate := time.Date(d.Year(), d.Month(), d.Day(), hour, minute, 0, 0, loc)
			if candidate.After(after) && (best.IsZero() || candidate.Before(best)) {
				best = candidate
			}
		}
		if best.IsZero() {
			return time.Time{}, false
		}
		return best, true
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

func readDateFile(path string, loc *time.Location) ([]time.Time, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("没有指定日期文件")
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("日期文件需使用绝对路径")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("无法读取日期文件")
	}
	content := strings.TrimPrefix(string(data), "\ufeff")
	lines := strings.Split(content, "\n")
	var dates []time.Time
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		d, err := parseDateLine(line, loc)
		if err != nil {
			return nil, fmt.Errorf("日期文件第 %d 行格式不对（需为 YYYY-MM-DD 或 YYYYMMDD）", i+1)
		}
		dates = append(dates, d)
	}
	if len(dates) == 0 {
		return nil, errors.New("日期文件为空")
	}
	return dates, nil
}

func parseDateLine(line string, loc *time.Location) (time.Time, error) {
	switch len(line) {
	case len(isoDateLayout):
		return time.ParseInLocation(isoDateLayout, line, loc)
	case len(compactLayout):
		return time.ParseInLocation(compactLayout, line, loc)
	default:
		return time.Time{}, errors.New("invalid date format")
	}
}
