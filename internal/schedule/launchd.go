package schedule

import (
	"bytes"
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
)

const (
	launchdLabelPrefix = "com.grokbot.task."
	launchdManagedMark = "<!-- managed by grok-bot -->"
)

// Launchd synchronizes scheduled tasks with macOS's native launchd (~/Library/LaunchAgents).
type Launchd struct {
	Cfg       SystemConfig
	AgentsDir string
	UID       int
	PathEnv   string
	// RunCtl executes `launchctl <args...>`. Injectable for unit testing on any OS.
	RunCtl func(args ...string) (string, error)
}

// Name returns the display name of the macOS launchd backend.
func (l *Launchd) Name() string {
	return "macOS launchd (LaunchAgents)"
}

// NewLaunchd constructs a macOS launchd scheduler backend targeting ~/Library/LaunchAgents.
func NewLaunchd(cfg SystemConfig) (*Launchd, error) {
	if _, err := exec.LookPath("launchctl"); err != nil {
		return nil, errors.New("未找到 macOS launchctl 命令")
	}
	home := strings.TrimSpace(cfg.HomeDir)
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil || home == "" {
			return nil, errors.New("无法确定用户主目录以写入 ~/Library/LaunchAgents")
		}
		cfg.HomeDir = home
	}
	return &Launchd{
		Cfg:       cfg,
		AgentsDir: filepath.Join(home, "Library", "LaunchAgents"),
		UID:       os.Getuid(),
		PathEnv:   buildUnixSchedulerPath(os.Getenv("PATH"), home),
		RunCtl:    runSystemLaunchctl,
	}, nil
}

// Sync writes/updates active task plists in ~/Library/LaunchAgents and unloads removed or paused tasks.
func (l *Launchd) Sync(entries []CronEntry, loc *time.Location) error {
	if l == nil {
		return nil
	}
	if loc == nil {
		loc = time.Local
	}
	if strings.TrimSpace(l.AgentsDir) == "" {
		return errors.New("LaunchAgents 目录未配置")
	}
	if err := os.MkdirAll(l.AgentsDir, 0o755); err != nil {
		return fmt.Errorf("创建 LaunchAgents 目录失败: %w", err)
	}

	pathEnv := l.PathEnv
	if pathEnv == "" {
		pathEnv = buildUnixSchedulerPath(os.Getenv("PATH"), l.Cfg.HomeDir)
	}

	desired := make(map[int64]string)
	var ids []int64
	var errs []error
	for _, e := range entries {
		if e.Paused || e.ID <= 0 {
			continue
		}
		plist, err := BuildLaunchdPlist(l.Cfg, e, loc, pathEnv)
		if err != nil {
			errs = append(errs, fmt.Errorf("任务 #%d 生成 launchd 配置失败: %w", e.ID, err))
			continue
		}
		desired[e.ID] = plist
		ids = append(ids, e.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	dirEntries, err := os.ReadDir(l.AgentsDir)
	if err != nil {
		return fmt.Errorf("读取 LaunchAgents 目录失败: %w", err)
	}
	existingManaged := make(map[int64]string)
	for _, de := range dirEntries {
		if de.IsDir() {
			continue
		}
		id, ok := parseLaunchdFilename(de.Name())
		if !ok {
			continue
		}
		fullPath := filepath.Join(l.AgentsDir, de.Name())
		raw, readErr := os.ReadFile(fullPath)
		if readErr != nil {
			continue
		}
		if !strings.Contains(string(raw), launchdManagedMark) {
			continue
		}
		existingManaged[id] = string(raw)
		if _, keep := desired[id]; !keep {
			_ = os.Remove(fullPath)
			l.unload(id, fullPath)
		}
	}

	for _, id := range ids {
		content := desired[id]
		fullPath := filepath.Join(l.AgentsDir, launchdFilename(id))
		if prev, existed := existingManaged[id]; existed {
			if prev == content {
				continue
			}
			l.unload(id, fullPath)
		}
		if writeErr := os.WriteFile(fullPath, []byte(content), 0o644); writeErr != nil {
			errs = append(errs, fmt.Errorf("写入 %s 失败: %w", fullPath, writeErr))
			continue
		}
		if loadErr := l.load(id, fullPath); loadErr != nil {
			errs = append(errs, fmt.Errorf("加载任务 #%d 到 launchd 失败: %w", id, loadErr))
		}
	}
	return errors.Join(errs...)
}

func (l *Launchd) load(id int64, plistPath string) error {
	if l.RunCtl == nil {
		return nil
	}
	if l.UID >= 0 {
		if _, err := l.RunCtl("bootstrap", fmt.Sprintf("gui/%d", l.UID), plistPath); err == nil {
			return nil
		}
		if _, err := l.RunCtl("bootstrap", fmt.Sprintf("user/%d", l.UID), plistPath); err == nil {
			return nil
		}
	}
	out, err := l.RunCtl("load", "-w", plistPath)
	if err != nil {
		return fmt.Errorf("%w (%s)", err, strings.TrimSpace(out))
	}
	return nil
}

func (l *Launchd) unload(id int64, plistPath string) {
	if l.RunCtl == nil {
		return
	}
	label := launchdLabel(id)
	if l.UID >= 0 {
		if _, err := l.RunCtl("bootout", fmt.Sprintf("gui/%d/%s", l.UID, label)); err == nil {
			return
		}
		if _, err := l.RunCtl("bootout", fmt.Sprintf("user/%d/%s", l.UID, label)); err == nil {
			return
		}
	}
	_, _ = l.RunCtl("unload", "-w", plistPath)
}

// BuildLaunchdPlist renders a macOS LaunchAgent XML property list for one scheduled task.
func BuildLaunchdPlist(cfg SystemConfig, e CronEntry, loc *time.Location, pathEnv string) (string, error) {
	if loc == nil {
		loc = time.Local
	}
	var spec Spec
	if err := json.Unmarshal([]byte(e.Spec), &spec); err != nil {
		return "", err
	}
	slots, intervalSec, err := launchdSchedule(spec, loc)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(launchdManagedMark + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	b.WriteString("<dict>\n")

	fmt.Fprintf(&b, "  <key>Label</key>\n  <string>%s</string>\n", xmlEscape(launchdLabel(e.ID)))

	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	fmt.Fprintf(&b, "    <string>%s</string>\n", xmlEscape(cfg.BinPath))
	for _, arg := range RunTaskArgs(e.ID, cfg.DataDir, cfg.EnvFile) {
		fmt.Fprintf(&b, "    <string>%s</string>\n", xmlEscape(arg))
	}
	b.WriteString("  </array>\n")

	if wd := strings.TrimSpace(cfg.WorkDir); wd != "" {
		fmt.Fprintf(&b, "  <key>WorkingDirectory</key>\n  <string>%s</string>\n", xmlEscape(wd))
	}

	b.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
	if pathEnv != "" {
		fmt.Fprintf(&b, "    <key>PATH</key>\n    <string>%s</string>\n", xmlEscape(pathEnv))
	}
	if home := strings.TrimSpace(cfg.HomeDir); home != "" {
		fmt.Fprintf(&b, "    <key>HOME</key>\n    <string>%s</string>\n", xmlEscape(home))
	}
	if dataDir := strings.TrimSpace(cfg.DataDir); dataDir != "" {
		fmt.Fprintf(&b, "    <key>DATA_DIR</key>\n    <string>%s</string>\n", xmlEscape(dataDir))
	}
	if envFile := strings.TrimSpace(cfg.EnvFile); envFile != "" {
		fmt.Fprintf(&b, "    <key>BOT_ENV_FILE</key>\n    <string>%s</string>\n", xmlEscape(envFile))
	}
	if tz := loc.String(); strings.Contains(tz, "/") {
		fmt.Fprintf(&b, "    <key>TZ</key>\n    <string>%s</string>\n", xmlEscape(tz))
	}
	b.WriteString("  </dict>\n")

	b.WriteString("  <key>RunAtLoad</key>\n  <false/>\n")

	if intervalSec > 0 {
		fmt.Fprintf(&b, "  <key>StartInterval</key>\n  <integer>%d</integer>\n", intervalSec)
	} else {
		b.WriteString("  <key>StartCalendarInterval</key>\n")
		if len(slots) == 1 {
			writeLaunchdSlot(&b, "  ", slots[0])
		} else {
			b.WriteString("  <array>\n")
			for _, slot := range slots {
				writeLaunchdSlot(&b, "    ", slot)
			}
			b.WriteString("  </array>\n")
		}
	}

	if dataDir := strings.TrimSpace(cfg.DataDir); dataDir != "" {
		logPath := xmlEscape(filepath.Join(dataDir, "task-scheduler.log"))
		fmt.Fprintf(&b, "  <key>StandardOutPath</key>\n  <string>%s</string>\n", logPath)
		fmt.Fprintf(&b, "  <key>StandardErrorPath</key>\n  <string>%s</string>\n", logPath)
	}

	b.WriteString("</dict>\n")
	b.WriteString("</plist>\n")
	return b.String(), nil
}

func writeLaunchdSlot(b *strings.Builder, indent string, s CalendarSlot) {
	b.WriteString(indent + "<dict>\n")
	if s.Month != nil {
		fmt.Fprintf(b, "%s  <key>Month</key>\n%s  <integer>%d</integer>\n", indent, indent, *s.Month)
	}
	if s.Day != nil {
		fmt.Fprintf(b, "%s  <key>Day</key>\n%s  <integer>%d</integer>\n", indent, indent, *s.Day)
	}
	if s.Weekday != nil {
		fmt.Fprintf(b, "%s  <key>Weekday</key>\n%s  <integer>%d</integer>\n", indent, indent, *s.Weekday)
	}
	if s.Hour != nil {
		fmt.Fprintf(b, "%s  <key>Hour</key>\n%s  <integer>%d</integer>\n", indent, indent, *s.Hour)
	}
	if s.Minute != nil {
		fmt.Fprintf(b, "%s  <key>Minute</key>\n%s  <integer>%d</integer>\n", indent, indent, *s.Minute)
	}
	b.WriteString(indent + "</dict>\n")
}

func launchdSchedule(s Spec, loc *time.Location) (slots []CalendarSlot, intervalSec int, err error) {
	switch s.Kind {
	case "once":
		at, err := time.ParseInLocation(atLayout, s.At, loc)
		if err != nil {
			return nil, 0, err
		}
		mon, day, hr, min := int(at.Month()), at.Day(), at.Hour(), at.Minute()
		return []CalendarSlot{{Month: &mon, Day: &day, Hour: &hr, Minute: &min}}, 0, nil
	case "daily", "dates":
		hr, min, err := clock(s.Time)
		if err != nil {
			return nil, 0, err
		}
		return []CalendarSlot{{Hour: &hr, Minute: &min}}, 0, nil
	case "weekly":
		hr, min, err := clock(s.Time)
		if err != nil {
			return nil, 0, err
		}
		days := uniqueSortedInts(s.Weekdays, 0, 6)
		if len(days) == 0 {
			return nil, 0, errors.New("no weekdays")
		}
		out := make([]CalendarSlot, len(days))
		for i, wd := range days {
			w, h, m := wd, hr, min
			out[i] = CalendarSlot{Weekday: &w, Hour: &h, Minute: &m}
		}
		return out, 0, nil
	case "monthly":
		hr, min, err := clock(s.Time)
		if err != nil {
			return nil, 0, err
		}
		days := expandMonthDays(s.MonthDays)
		if len(days) == 0 {
			return nil, 0, errors.New("no month_days")
		}
		out := make([]CalendarSlot, len(days))
		for i, dom := range days {
			d, h, m := dom, hr, min
			out[i] = CalendarSlot{Day: &d, Hour: &h, Minute: &m}
		}
		return out, 0, nil
	case "interval":
		if s.EveryMinutes <= 0 {
			return nil, 0, errors.New("invalid interval")
		}
		return nil, s.EveryMinutes * 60, nil
	case "cron":
		expanded, ok, err := ExpandCronSlots(s.Cron, 128)
		if err != nil {
			return nil, 0, err
		}
		if ok {
			return expanded, 0, nil
		}
		// High-cardinality cron fallback: trigger every 60s; RunTaskByID gates via MatchesRun.
		return nil, 60, nil
	default:
		return nil, 0, errors.New("unsupported schedule kind")
	}
}

func launchdLabel(id int64) string {
	return fmt.Sprintf("%s%d", launchdLabelPrefix, id)
}

func launchdFilename(id int64) string {
	return launchdLabel(id) + ".plist"
}

func parseLaunchdFilename(name string) (int64, bool) {
	if !strings.HasPrefix(name, launchdLabelPrefix) || !strings.HasSuffix(name, ".plist") {
		return 0, false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(name, launchdLabelPrefix), ".plist")
	id, err := strconv.ParseInt(mid, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func runSystemLaunchctl(args ...string) (string, error) {
	var out bytes.Buffer
	cmd := exec.Command("launchctl", args...)
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func buildUnixSchedulerPath(current, home string) string {
	seen := map[string]bool{}
	var parts []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p != "" && !seen[p] {
			seen[p] = true
			parts = append(parts, p)
		}
	}
	for _, p := range strings.Split(current, ":") {
		add(p)
	}
	if home != "" {
		add(home + "/.local/bin")
		add(home + "/.bun/bin")
	}
	for _, std := range []string{"/opt/homebrew/bin", "/opt/homebrew/sbin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"} {
		add(std)
	}
	return strings.Join(parts, ":")
}

func uniqueSortedInts(vals []int, lo, hi int) []int {
	seen := map[int]bool{}
	var out []int
	for _, v := range vals {
		if v >= lo && v <= hi && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Ints(out)
	return out
}

func expandMonthDays(monthDays []int) []int {
	seen := map[int]bool{}
	for _, d := range monthDays {
		if d == -1 {
			for last := 28; last <= 31; last++ {
				seen[last] = true
			}
		} else if d >= 1 && d <= 31 {
			seen[d] = true
		}
	}
	out := make([]int, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Ints(out)
	return out
}

func xmlEscape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&apos;")
		default:
			if r >= 0x20 || r == '\t' || r == '\n' || r == '\r' {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}
