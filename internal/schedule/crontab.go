package schedule

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	cronBegin = "# BEGIN GROK-BOT TASKS"
	cronEnd   = "# END GROK-BOT TASKS"
)

// CronEntry is one task rendered into the managed crontab block.
type CronEntry struct {
	ID      int64
	Spec    string
	Summary string
	NextRun time.Time
	Paused  bool
}

// CronExpr returns the 5-field cron expression for s in loc.
func (s Spec) CronExpr(loc *time.Location) (string, error) {
	if loc == nil {
		loc = time.Local
	}
	switch s.Kind {
	case "once":
		at, err := time.ParseInLocation(atLayout, s.At, loc)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d %d %d %d *", at.Minute(), at.Hour(), at.Day(), int(at.Month())), nil
	case "daily", "dates":
		hour, minute, err := clock(s.Time)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d %d * * *", minute, hour), nil
	case "weekly":
		hour, minute, err := clock(s.Time)
		if err != nil {
			return "", err
		}
		if len(s.Weekdays) == 0 {
			return "", errors.New("no weekdays")
		}
		days := make([]int, 0, len(s.Weekdays))
		seen := map[int]bool{}
		for _, d := range s.Weekdays {
			if d >= 0 && d <= 6 && !seen[d] {
				seen[d] = true
				days = append(days, d)
			}
		}
		sort.Ints(days)
		parts := make([]string, len(days))
		for i, d := range days {
			parts[i] = strconv.Itoa(d)
		}
		return fmt.Sprintf("%d %d * * %s", minute, hour, strings.Join(parts, ",")), nil
	case "interval":
		if s.EveryMinutes <= 0 {
			return "", errors.New("invalid interval")
		}
		if s.EveryMinutes < 60 {
			return fmt.Sprintf("*/%d * * * *", s.EveryMinutes), nil
		}
		if s.EveryMinutes == 60 {
			return "0 * * * *", nil
		}
		if s.EveryMinutes%60 == 0 && s.EveryMinutes/60 <= 23 {
			return fmt.Sprintf("0 */%d * * *", s.EveryMinutes/60), nil
		}
		return "* * * * *", nil
	default:
		return "", errors.New("unsupported schedule kind")
	}
}

// MergeCrontab replaces the GROK-BOT managed block in existing with entries.
func MergeCrontab(existing string, entries []CronEntry, loc *time.Location, commandPrefix string) string {
	if loc == nil {
		loc = time.Local
	}
	stripped := stripManagedBlock(existing)
	var block strings.Builder
	var count int
	for _, e := range entries {
		var spec Spec
		if err := json.Unmarshal([]byte(e.Spec), &spec); err != nil {
			continue
		}
		expr, err := spec.CronExpr(loc)
		if err != nil {
			continue
		}
		if count == 0 {
			block.WriteString(cronBegin + "\n")
		}
		count++
		summary := strings.ReplaceAll(strings.TrimSpace(e.Summary), "\n", " ")
		cmd := fmt.Sprintf("%s run-task %d", strings.TrimSpace(commandPrefix), e.ID)
		if e.Paused {
			fmt.Fprintf(&block, "# #%d [PAUSED] %s\n", e.ID, summary)
			fmt.Fprintf(&block, "# %s %s\n", expr, cmd)
			continue
		}
		extra := ""
		if spec.DateFile != "" {
			extra = fmt.Sprintf(", date_file: %s", spec.DateFile)
		}
		fmt.Fprintf(&block, "# #%d %s (next: %s%s)\n", e.ID, summary, e.NextRun.In(loc).Format(atLayout), extra)
		fmt.Fprintf(&block, "%s %s\n", expr, cmd)
	}
	if count > 0 {
		block.WriteString(cronEnd + "\n")
	}
	if stripped == "" {
		return block.String()
	}
	if block.Len() == 0 {
		return stripped + "\n"
	}
	return stripped + "\n\n" + block.String()
}

func stripManagedBlock(existing string) string {
	lines := strings.Split(strings.ReplaceAll(existing, "\r\n", "\n"), "\n")
	var kept []string
	inBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == cronBegin {
			inBlock = true
			continue
		}
		if trimmed == cronEnd {
			inBlock = false
			continue
		}
		if !inBlock {
			kept = append(kept, line)
		}
	}
	return strings.TrimRight(strings.Join(kept, "\n"), "\n")
}

// Crontab synchronizes scheduled tasks with the OS user crontab.
type Crontab struct {
	CommandPrefix string
	Read          func() (string, error)
	Write         func(string) error
}

// NewSystemCrontab returns a Crontab backed by the system `crontab` command,
// or nil if the `crontab` binary is not available on this machine.
func NewSystemCrontab(binPath, workDir, envFile string) *Crontab {
	if _, err := exec.LookPath("crontab"); err != nil {
		return nil
	}
	var prefix strings.Builder
	if workDir != "" {
		fmt.Fprintf(&prefix, "cd %s && ", shellQuote(workDir))
	}
	if envFile != "" {
		fmt.Fprintf(&prefix, "BOT_ENV_FILE=%s ", shellQuote(envFile))
	}
	prefix.WriteString(shellQuote(binPath))
	return &Crontab{
		CommandPrefix: prefix.String(),
		Read:          readSystemCrontab,
		Write:         writeSystemCrontab,
	}
}

// Sync updates the managed block in the user's crontab.
func (c *Crontab) Sync(entries []CronEntry, loc *time.Location) error {
	if c == nil || c.Read == nil || c.Write == nil {
		return nil
	}
	existing, err := c.Read()
	if err != nil {
		return err
	}
	updated := MergeCrontab(existing, entries, loc, c.CommandPrefix)
	if updated == existing {
		return nil
	}
	return c.Write(updated)
}

func readSystemCrontab() (string, error) {
	out, err := exec.Command("crontab", "-l").CombinedOutput()
	if err != nil {
		lower := strings.ToLower(string(out))
		if strings.Contains(lower, "no crontab for") {
			return "", nil
		}
		return "", fmt.Errorf("crontab -l: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func writeSystemCrontab(content string) error {
	cmd := exec.Command("crontab", "-")
	cmd.Stdin = strings.NewReader(content)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("crontab -: %w: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
