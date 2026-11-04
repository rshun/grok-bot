package schedule

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

const (
	winTaskFolder = `\GrokBot\`
	winTaskPrefix = `\GrokBot\Task_`
	winTimeLayout = "2006-01-02T15:04:05-07:00"
)

var (
	winWeekdayNames = [7]string{
		"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday",
	}
	winMonthNames = [13]string{
		"",
		"January", "February", "March", "April", "May", "June",
		"July", "August", "September", "October", "November", "December",
	}
)

// WindowsScheduler synchronizes scheduled tasks with Windows Task Scheduler (`schtasks.exe`).
type WindowsScheduler struct {
	Cfg      SystemConfig
	StateDir string
	// RunCmd executes `schtasks <args...>`. Injectable for unit testing on any OS.
	RunCmd func(args ...string) (string, error)
}

// Name returns the display name of the Windows Task Scheduler backend.
func (w *WindowsScheduler) Name() string {
	return "Windows 任务计划程序 (schtasks)"
}

// NewWindowsScheduler constructs a Windows Task Scheduler backend using `schtasks.exe`.
func NewWindowsScheduler(cfg SystemConfig) (*WindowsScheduler, error) {
	if _, err := exec.LookPath("schtasks"); err != nil {
		return nil, errors.New("未找到 Windows schtasks 命令")
	}
	stateDir := ""
	if strings.TrimSpace(cfg.DataDir) != "" {
		stateDir = filepath.Join(cfg.DataDir, "win-tasks")
	}
	return &WindowsScheduler{
		Cfg:      cfg,
		StateDir: stateDir,
		RunCmd:   runSystemSchtasks,
	}, nil
}

// Sync creates/updates active tasks under `\GrokBot\Task_<id>` and deletes removed or paused tasks.
func (w *WindowsScheduler) Sync(entries []CronEntry, loc *time.Location) error {
	if w == nil || w.RunCmd == nil {
		return nil
	}
	if loc == nil {
		loc = time.Local
	}
	stateDir := strings.TrimSpace(w.StateDir)
	if stateDir == "" {
		if strings.TrimSpace(w.Cfg.DataDir) != "" {
			stateDir = filepath.Join(w.Cfg.DataDir, "win-tasks")
		} else {
			stateDir = filepath.Join(os.TempDir(), "grok-bot-win-tasks")
		}
	}
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		return fmt.Errorf("创建 Windows 任务缓存目录失败: %w", err)
	}

	desired := make(map[int64]string)
	var ids []int64
	var errs []error
	for _, e := range entries {
		if e.Paused || e.ID <= 0 {
			continue
		}
		xmlText, err := BuildWindowsTaskXML(w.Cfg, e, loc)
		if err != nil {
			errs = append(errs, fmt.Errorf("任务 #%d 生成 Windows 计划任务配置失败: %w", e.ID, err))
			continue
		}
		desired[e.ID] = xmlText
		ids = append(ids, e.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	existingInOS := w.queryExistingTasks()

	// Delete tasks in OS Task Scheduler that are no longer active.
	var existingIDs []int64
	for id := range existingInOS {
		existingIDs = append(existingIDs, id)
	}
	sort.Slice(existingIDs, func(i, j int) bool { return existingIDs[i] < existingIDs[j] })
	for _, id := range existingIDs {
		if _, keep := desired[id]; !keep {
			if out, delErr := w.RunCmd("/Delete", "/TN", winTaskName(id), "/F"); delErr != nil {
				errs = append(errs, fmt.Errorf("删除计划任务 %s 失败: %w (%s)", winTaskName(id), delErr, strings.TrimSpace(out)))
				continue
			}
			_ = os.Remove(filepath.Join(stateDir, fmt.Sprintf("Task_%d.xml", id)))
		}
	}

	// Also clean up any orphan cached XML files not in desired.
	if dirItems, readErr := os.ReadDir(stateDir); readErr == nil {
		for _, item := range dirItems {
			if id, ok := parseWinCacheFilename(item.Name()); ok {
				if _, keep := desired[id]; !keep {
					_ = os.Remove(filepath.Join(stateDir, item.Name()))
				}
			}
		}
	}

	// Create or update active tasks.
	for _, id := range ids {
		encoded := EncodeUTF16LE(desired[id])
		xmlPath := filepath.Join(stateDir, fmt.Sprintf("Task_%d.xml", id))
		if existingInOS[id] {
			if prev, readErr := os.ReadFile(xmlPath); readErr == nil && bytes.Equal(prev, encoded) {
				continue
			}
		}
		if writeErr := os.WriteFile(xmlPath, encoded, 0o600); writeErr != nil {
			errs = append(errs, fmt.Errorf("写入计划任务 XML 失败: %w", writeErr))
			continue
		}
		if out, createErr := w.RunCmd("/Create", "/TN", winTaskName(id), "/XML", xmlPath, "/F"); createErr != nil {
			errs = append(errs, fmt.Errorf("注册计划任务 %s 失败: %w (%s)", winTaskName(id), createErr, strings.TrimSpace(out)))
		}
	}
	return errors.Join(errs...)
}

func (w *WindowsScheduler) queryExistingTasks() map[int64]bool {
	found := make(map[int64]bool)
	out, err := w.RunCmd("/Query", "/TN", winTaskFolder, "/FO", "CSV", "/NH")
	if err != nil {
		// When `\GrokBot\` does not exist yet, schtasks exits 1 with a localized message.
		return found
	}
	r := csv.NewReader(strings.NewReader(out))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	records, _ := r.ReadAll()
	for _, rec := range records {
		if len(rec) == 0 {
			continue
		}
		name := strings.TrimSpace(rec[0])
		if strings.HasPrefix(strings.ToLower(name), strings.ToLower(winTaskPrefix)) {
			numStr := name[len(winTaskPrefix):]
			if id, parseErr := strconv.ParseInt(numStr, 10, 64); parseErr == nil && id > 0 {
				found[id] = true
			}
		}
	}
	return found
}

// BuildWindowsTaskXML generates a Windows Task Scheduler 1.2 XML definition for task e.
func BuildWindowsTaskXML(cfg SystemConfig, e CronEntry, loc *time.Location) (string, error) {
	if loc == nil {
		loc = time.Local
	}
	var spec Spec
	if err := json.Unmarshal([]byte(e.Spec), &spec); err != nil {
		return "", err
	}
	triggersXML, err := buildWinTriggersXML(spec, e.NextRun, loc)
	if err != nil {
		return "", err
	}

	desc := fmt.Sprintf("#%d %s", e.ID, strings.ReplaceAll(strings.TrimSpace(e.Summary), "\n", " "))
	rawArgs := RunTaskArgs(e.ID, cfg.DataDir, cfg.EnvFile)
	quotedArgs := make([]string, len(rawArgs))
	for i, a := range rawArgs {
		quotedArgs[i] = winQuoteArg(a)
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-16"?>` + "\r\n")
	b.WriteString(`<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">` + "\r\n")
	b.WriteString("  <RegistrationInfo>\r\n")
	b.WriteString("    <Author>grok-bot</Author>\r\n")
	fmt.Fprintf(&b, "    <Description>%s</Description>\r\n", xmlEscape(desc))
	b.WriteString("  </RegistrationInfo>\r\n")
	b.WriteString("  <Triggers>\r\n")
	b.WriteString(triggersXML)
	b.WriteString("  </Triggers>\r\n")
	b.WriteString("  <Settings>\r\n")
	b.WriteString("    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>\r\n")
	b.WriteString("    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>\r\n")
	b.WriteString("    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>\r\n")
	b.WriteString("    <AllowHardTerminate>true</AllowHardTerminate>\r\n")
	b.WriteString("    <StartWhenAvailable>false</StartWhenAvailable>\r\n")
	b.WriteString("    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>\r\n")
	b.WriteString("    <AllowStartOnDemand>true</AllowStartOnDemand>\r\n")
	b.WriteString("    <Enabled>true</Enabled>\r\n")
	b.WriteString("    <Hidden>false</Hidden>\r\n")
	b.WriteString("    <RunOnlyIfIdle>false</RunOnlyIfIdle>\r\n")
	b.WriteString("    <WakeToRun>false</WakeToRun>\r\n")
	b.WriteString("    <ExecutionTimeLimit>PT1H</ExecutionTimeLimit>\r\n")
	b.WriteString("  </Settings>\r\n")
	b.WriteString("  <Actions Context=\"Author\">\r\n")
	b.WriteString("    <Exec>\r\n")
	fmt.Fprintf(&b, "      <Command>%s</Command>\r\n", xmlEscape(cfg.BinPath))
	fmt.Fprintf(&b, "      <Arguments>%s</Arguments>\r\n", xmlEscape(strings.Join(quotedArgs, " ")))
	if wd := strings.TrimSpace(cfg.WorkDir); wd != "" {
		fmt.Fprintf(&b, "      <WorkingDirectory>%s</WorkingDirectory>\r\n", xmlEscape(wd))
	}
	b.WriteString("    </Exec>\r\n")
	b.WriteString("  </Actions>\r\n")
	b.WriteString("</Task>\r\n")
	return b.String(), nil
}

func buildWinTriggersXML(s Spec, nextRun time.Time, loc *time.Location) (string, error) {
	var b strings.Builder
	switch s.Kind {
	case "once":
		at, err := time.ParseInLocation(atLayout, s.At, loc)
		if err != nil {
			return "", err
		}
		writeWinTimeTrigger(&b, at, "")
		return b.String(), nil

	case "daily", "dates":
		hr, min, err := clock(s.Time)
		if err != nil {
			return "", err
		}
		writeWinDailyTrigger(&b, winCalendarAnchor(hr, min, loc))
		return b.String(), nil

	case "weekly":
		hr, min, err := clock(s.Time)
		if err != nil {
			return "", err
		}
		days := uniqueSortedInts(s.Weekdays, 0, 6)
		if len(days) == 0 {
			return "", errors.New("no weekdays")
		}
		writeWinWeeklyTrigger(&b, winCalendarAnchor(hr, min, loc), days)
		return b.String(), nil

	case "monthly":
		hr, min, err := clock(s.Time)
		if err != nil {
			return "", err
		}
		if len(s.MonthDays) == 0 {
			return "", errors.New("no month_days")
		}
		var doms []int
		hasLast := false
		seen := map[int]bool{}
		for _, d := range s.MonthDays {
			if d == -1 {
				hasLast = true
			} else if d >= 1 && d <= 31 && !seen[d] {
				seen[d] = true
				doms = append(doms, d)
			}
		}
		sort.Ints(doms)
		if len(doms) == 0 && !hasLast {
			return "", errors.New("invalid month_days")
		}
		writeWinMonthlyTrigger(&b, winCalendarAnchor(hr, min, loc), doms, hasLast, nil)
		return b.String(), nil

	case "interval":
		if s.EveryMinutes <= 0 {
			return "", errors.New("invalid interval")
		}
		anchor := winCalendarAnchor(0, 0, loc)
		if !nextRun.IsZero() {
			anchor = nextRun.In(loc).Truncate(time.Minute)
		}
		writeWinTimeTrigger(&b, anchor, formatWinDuration(s.EveryMinutes))
		return b.String(), nil

	case "cron":
		return buildWinCronTriggers(s.Cron, loc)

	default:
		return "", errors.New("unsupported schedule kind")
	}
}

func buildWinCronTriggers(expr string, loc *time.Location) (string, error) {
	st, err := AnalyzeCron(expr)
	if err != nil {
		return "", err
	}
	var b strings.Builder

	// Hourly at a single fixed minute on all days (`M * * * *`).
	if len(st.Hours) == 0 && len(st.Minutes) == 1 && st.DOMStar && st.DOWStar && len(st.Months) == 0 {
		writeWinTimeTrigger(&b, winCalendarAnchor(0, st.Minutes[0], loc), "PT1H")
		return b.String(), nil
	}

	// Minute-wildcard cron (`* ...`): fall back to 1-minute repetition gated by MatchesRun.
	if len(st.Minutes) == 0 {
		writeWinTimeTrigger(&b, winCalendarAnchor(0, 0, loc), "PT1M")
		return b.String(), nil
	}

	hrs := st.Hours
	if len(hrs) == 0 {
		hrs = make([]int, 24)
		for i := 0; i < 24; i++ {
			hrs[i] = i
		}
	}

	// Restricted weekday combined with restricted month cannot be represented directly in
	// ScheduleByWeek; if DOM is also restricted with restricted month, fall back to PT1M.
	if !st.DOWStar && len(st.Months) > 0 {
		writeWinTimeTrigger(&b, winCalendarAnchor(0, 0, loc), "PT1M")
		return b.String(), nil
	}

	branches := 1
	if !st.DOMStar && !st.DOWStar {
		branches = 2
	}
	if len(hrs)*len(st.Minutes)*branches > 40 {
		writeWinTimeTrigger(&b, winCalendarAnchor(0, 0, loc), "PT1M")
		return b.String(), nil
	}

	allDays := make([]int, 31)
	for i := 0; i < 31; i++ {
		allDays[i] = i + 1
	}

	for _, hr := range hrs {
		for _, min := range st.Minutes {
			anchor := winCalendarAnchor(hr, min, loc)
			switch {
			case st.DOMStar && st.DOWStar && len(st.Months) == 0:
				writeWinDailyTrigger(&b, anchor)
			case st.DOMStar && !st.DOWStar:
				writeWinWeeklyTrigger(&b, anchor, st.Weekdays)
			case !st.DOMStar && st.DOWStar:
				writeWinMonthlyTrigger(&b, anchor, st.Days, false, st.Months)
			case st.DOMStar && st.DOWStar && len(st.Months) > 0:
				writeWinMonthlyTrigger(&b, anchor, allDays, false, st.Months)
			default:
				// Both DOM and DOW restricted (months == all): Vixie OR -> monthly + weekly triggers.
				writeWinMonthlyTrigger(&b, anchor, st.Days, false, nil)
				writeWinWeeklyTrigger(&b, anchor, st.Weekdays)
			}
		}
	}
	return b.String(), nil
}

func writeWinTimeTrigger(b *strings.Builder, start time.Time, repeatISO string) {
	b.WriteString("    <TimeTrigger>\r\n")
	fmt.Fprintf(b, "      <StartBoundary>%s</StartBoundary>\r\n", start.Format(winTimeLayout))
	b.WriteString("      <Enabled>true</Enabled>\r\n")
	if repeatISO != "" {
		b.WriteString("      <Repetition>\r\n")
		fmt.Fprintf(b, "        <Interval>%s</Interval>\r\n", repeatISO)
		b.WriteString("        <StopAtDurationEnd>false</StopAtDurationEnd>\r\n")
		b.WriteString("      </Repetition>\r\n")
	}
	b.WriteString("    </TimeTrigger>\r\n")
}

func writeWinDailyTrigger(b *strings.Builder, start time.Time) {
	b.WriteString("    <CalendarTrigger>\r\n")
	fmt.Fprintf(b, "      <StartBoundary>%s</StartBoundary>\r\n", start.Format(winTimeLayout))
	b.WriteString("      <Enabled>true</Enabled>\r\n")
	b.WriteString("      <ScheduleByDay>\r\n")
	b.WriteString("        <DaysInterval>1</DaysInterval>\r\n")
	b.WriteString("      </ScheduleByDay>\r\n")
	b.WriteString("    </CalendarTrigger>\r\n")
}

func writeWinWeeklyTrigger(b *strings.Builder, start time.Time, weekdays []int) {
	b.WriteString("    <CalendarTrigger>\r\n")
	fmt.Fprintf(b, "      <StartBoundary>%s</StartBoundary>\r\n", start.Format(winTimeLayout))
	b.WriteString("      <Enabled>true</Enabled>\r\n")
	b.WriteString("      <ScheduleByWeek>\r\n")
	b.WriteString("        <WeeksInterval>1</WeeksInterval>\r\n")
	b.WriteString("        <DaysOfWeek>\r\n")
	for _, wd := range weekdays {
		if wd >= 0 && wd <= 6 {
			fmt.Fprintf(b, "          <%s/>\r\n", winWeekdayNames[wd])
		}
	}
	b.WriteString("        </DaysOfWeek>\r\n")
	b.WriteString("      </ScheduleByWeek>\r\n")
	b.WriteString("    </CalendarTrigger>\r\n")
}

func writeWinMonthlyTrigger(b *strings.Builder, start time.Time, doms []int, hasLast bool, months []int) {
	b.WriteString("    <CalendarTrigger>\r\n")
	fmt.Fprintf(b, "      <StartBoundary>%s</StartBoundary>\r\n", start.Format(winTimeLayout))
	b.WriteString("      <Enabled>true</Enabled>\r\n")
	b.WriteString("      <ScheduleByMonth>\r\n")
	b.WriteString("        <DaysOfMonth>\r\n")
	for _, d := range doms {
		if d >= 1 && d <= 31 {
			fmt.Fprintf(b, "          <Day>%d</Day>\r\n", d)
		}
	}
	if hasLast {
		b.WriteString("          <Day>Last</Day>\r\n")
	}
	b.WriteString("        </DaysOfMonth>\r\n")
	b.WriteString("        <Months>\r\n")
	if len(months) == 0 {
		for m := 1; m <= 12; m++ {
			fmt.Fprintf(b, "          <%s/>\r\n", winMonthNames[m])
		}
	} else {
		for _, m := range months {
			if m >= 1 && m <= 12 {
				fmt.Fprintf(b, "          <%s/>\r\n", winMonthNames[m])
			}
		}
	}
	b.WriteString("        </Months>\r\n")
	b.WriteString("      </ScheduleByMonth>\r\n")
	b.WriteString("    </CalendarTrigger>\r\n")
}

func winCalendarAnchor(hr, min int, loc *time.Location) time.Time {
	return time.Date(2026, 1, 1, hr, min, 0, 0, loc)
}

func formatWinDuration(everyMinutes int) string {
	if everyMinutes <= 0 {
		return "PT1M"
	}
	// Windows Task Scheduler Repetition.Interval allows up to P31D.
	if everyMinutes > 31*24*60 {
		everyMinutes = 31 * 24 * 60
	}
	days := everyMinutes / (24 * 60)
	rem := everyMinutes % (24 * 60)
	hours := rem / 60
	mins := rem % 60

	var b strings.Builder
	b.WriteByte('P')
	if days > 0 {
		fmt.Fprintf(&b, "%dD", days)
	}
	if hours > 0 || mins > 0 || days == 0 {
		b.WriteByte('T')
		if hours > 0 {
			fmt.Fprintf(&b, "%dH", hours)
		}
		if mins > 0 || (days == 0 && hours == 0) {
			fmt.Fprintf(&b, "%dM", mins)
		}
	}
	return b.String()
}

// EncodeUTF16LE encodes s as UTF-16LE with a leading 0xFF 0xFE BOM as required by `schtasks /XML`.
func EncodeUTF16LE(s string) []byte {
	u16 := utf16.Encode([]rune(s))
	out := make([]byte, 2+len(u16)*2)
	out[0] = 0xFF
	out[1] = 0xFE
	for i, cp := range u16 {
		out[2+i*2] = byte(cp)
		out[2+i*2+1] = byte(cp >> 8)
	}
	return out
}

func winTaskName(id int64) string {
	return fmt.Sprintf("%s%d", winTaskPrefix, id)
}

func parseWinCacheFilename(name string) (int64, bool) {
	if !strings.HasPrefix(name, "Task_") || !strings.HasSuffix(name, ".xml") {
		return 0, false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(name, "Task_"), ".xml")
	id, err := strconv.ParseInt(mid, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// winQuoteArg quotes an argument following Windows CommandLineToArgvW rules.
func winQuoteArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			slashes++
		case '"':
			for j := 0; j < slashes*2+1; j++ {
				b.WriteByte('\\')
			}
			b.WriteByte('"')
			slashes = 0
		default:
			for j := 0; j < slashes; j++ {
				b.WriteByte('\\')
			}
			b.WriteByte(c)
			slashes = 0
		}
	}
	for j := 0; j < slashes*2; j++ {
		b.WriteByte('\\')
	}
	b.WriteByte('"')
	return b.String()
}

func runSystemSchtasks(args ...string) (string, error) {
	var out bytes.Buffer
	cmd := exec.Command("schtasks", args...)
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}
