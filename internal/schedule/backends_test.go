package schedule

import (
	"bytes"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func TestCronExprForEntryAnchorsInterval(t *testing.T) {
	// 60m interval created at 10:25 -> NextRun 11:25 -> cron fires at minute 25 (`25 * * * *`).
	got60, err := CronExprForEntry(Spec{Kind: "interval", EveryMinutes: 60}, at(19, 11, 25, 0), loc)
	if err != nil || got60 != "25 * * * *" {
		t.Fatalf("60m anchored got %q, err=%v", got60, err)
	}
	// 120m interval with NextRun 11:25 (odd hour) -> `25 1-23/2 * * *`.
	got120, err := CronExprForEntry(Spec{Kind: "interval", EveryMinutes: 120}, at(19, 11, 25, 0), loc)
	if err != nil || got120 != "25 1-23/2 * * *" {
		t.Fatalf("120m odd-hour anchored got %q, err=%v", got120, err)
	}
	// 120m interval with NextRun 12:10 (even hour) -> `10 */2 * * *`.
	got120Even, err := CronExprForEntry(Spec{Kind: "interval", EveryMinutes: 120}, at(19, 12, 10, 0), loc)
	if err != nil || got120Even != "10 */2 * * *" {
		t.Fatalf("120m even-hour anchored got %q, err=%v", got120Even, err)
	}
}

func TestLaunchdPlistAndSyncLifecycle(t *testing.T) {
	agentsDir := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "data")
	cfg := SystemConfig{
		GOOS:    "darwin",
		BinPath: "/usr/local/bin/grok-bot",
		WorkDir: "/Users/grok/grok-bot",
		DataDir: dataDir,
		EnvFile: "/Users/grok/grok-bot/.env",
		HomeDir: "/Users/grok",
	}

	// Place an unmanaged plist in LaunchAgents; Sync MUST leave it untouched.
	unmanagedPath := filepath.Join(agentsDir, "com.grokbot.task.999.plist")
	if err := os.WriteFile(unmanagedPath, []byte("<plist>user-owned</plist>"), 0o644); err != nil {
		t.Fatal(err)
	}

	var ctlCalls [][]string
	ld := &Launchd{
		Cfg:       cfg,
		AgentsDir: agentsDir,
		UID:       501,
		PathEnv:   "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin",
		RunCtl: func(args ...string) (string, error) {
			cp := append([]string(nil), args...)
			ctlCalls = append(ctlCalls, cp)
			return "", nil
		},
	}

	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		shanghai = loc
	}

	entries := []CronEntry{
		{ID: 1, Spec: `{"kind":"daily","time":"09:30"}`, Summary: "日报", NextRun: at(20, 9, 30, 0)},
		{ID: 2, Spec: `{"kind":"weekly","time":"08:00","weekdays":[1,3,5]}`, Summary: "晨会", NextRun: at(21, 8, 0, 0)},
		{ID: 3, Spec: `{"kind":"monthly","time":"18:00","month_days":[1,-1]}`, Summary: "月报", NextRun: at(31, 18, 0, 0)},
		{ID: 4, Spec: `{"kind":"interval","every_minutes":90}`, Summary: "巡检", NextRun: at(19, 11, 30, 0)},
		{ID: 5, Spec: `{"kind":"cron","cron":"30 9 * * 1-5"}`, Summary: "工作日开盘", NextRun: at(20, 9, 30, 0)},
		{ID: 6, Spec: `{"kind":"once","at":"2026-10-20 15:00"}`, Summary: "一次性提醒", NextRun: at(20, 15, 0, 0)},
	}
	if err := ld.Sync(entries, shanghai); err != nil {
		t.Fatalf("Launchd.Sync create: %v", err)
	}
	if len(ctlCalls) != 6 {
		t.Fatalf("expected 6 bootstrap calls, got %d: %v", len(ctlCalls), ctlCalls)
	}

	// Verify XML well-formedness and key content on generated plists.
	for _, e := range entries {
		p := filepath.Join(agentsDir, launchdFilename(e.ID))
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("missing plist %s: %v", p, err)
		}
		assertWellFormedXML(t, raw)
		text := string(raw)
		for _, want := range []string{
			launchdManagedMark,
			"<string>/usr/local/bin/grok-bot</string>",
			"<string>run-task</string>",
			"<string>--data-dir</string>",
			"<string>--env-file</string>",
			"<key>RunAtLoad</key>",
		} {
			if !strings.Contains(text, want) {
				t.Fatalf("task #%d plist missing %q:\n%s", e.ID, want, text)
			}
		}
	}
	plist4, _ := os.ReadFile(filepath.Join(agentsDir, launchdFilename(4)))
	if !strings.Contains(string(plist4), "<key>StartInterval</key>\n  <integer>5400</integer>") {
		t.Fatalf("interval plist unexpected:\n%s", string(plist4))
	}

	// Re-syncing identical entries must make ZERO launchctl calls.
	ctlCalls = nil
	if err := ld.Sync(entries, shanghai); err != nil {
		t.Fatalf("Launchd.Sync idempotent: %v", err)
	}
	if len(ctlCalls) != 0 {
		t.Fatalf("expected 0 launchctl calls on unchanged sync, got %v", ctlCalls)
	}

	// Pausing #2 and deleting #3..#6 unloads & removes their plists while keeping #1 and unmanaged #999.
	entries[1].Paused = true
	if err := ld.Sync(entries[:2], shanghai); err != nil {
		t.Fatalf("Launchd.Sync prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(agentsDir, launchdFilename(1))); err != nil {
		t.Fatal("active task #1 plist should still exist")
	}
	for _, removedID := range []int64{2, 3, 4, 5, 6} {
		if _, err := os.Stat(filepath.Join(agentsDir, launchdFilename(removedID))); !os.IsNotExist(err) {
			t.Fatalf("task #%d plist should have been removed", removedID)
		}
	}
	if _, err := os.Stat(unmanagedPath); err != nil {
		t.Fatal("unmanaged plist must not be touched")
	}
}

func TestWindowsSchedulerXMLAndSyncLifecycle(t *testing.T) {
	stateDir := t.TempDir()
	cfg := SystemConfig{
		GOOS:    "windows",
		BinPath: `C:\Program Files\GrokBot\grok-bot.exe`,
		WorkDir: `C:\Program Files\GrokBot`,
		DataDir: `C:\Users\grok user\AppData\Local\GrokBot\data\`,
		EnvFile: `C:\Program Files\GrokBot\.env`,
	}

	registered := map[string][]byte{}
	var cmds [][]string
	ws := &WindowsScheduler{
		Cfg:      cfg,
		StateDir: stateDir,
		RunCmd: func(args ...string) (string, error) {
			cp := append([]string(nil), args...)
			cmds = append(cmds, cp)
			switch args[0] {
			case "/Query":
				if len(registered) == 0 {
					return "ERROR: not found", os.ErrNotExist
				}
				var sb strings.Builder
				for name := range registered {
					sb.WriteString(`"` + name + `","2026/10/20 09:00:00","Ready"` + "\r\n")
				}
				return sb.String(), nil
			case "/Create":
				// /Create /TN <name> /XML <path> /F
				tn, xmlPath := args[2], args[4]
				raw, err := os.ReadFile(xmlPath)
				if err != nil {
					return "", err
				}
				registered[tn] = raw
				return "SUCCESS", nil
			case "/Delete":
				delete(registered, args[2])
				return "SUCCESS", nil
			default:
				t.Fatalf("unexpected schtasks command: %v", args)
				return "", nil
			}
		},
	}

	entries := []CronEntry{
		{ID: 1, Spec: `{"kind":"once","at":"2026-10-20 15:00"}`, Summary: "中文提醒 & <测试>", NextRun: at(20, 15, 0, 0)},
		{ID: 2, Spec: `{"kind":"weekly","time":"09:00","weekdays":[1,2,3,4,5]}`, Summary: "工作日早报", NextRun: at(20, 9, 0, 0)},
		{ID: 3, Spec: `{"kind":"monthly","time":"18:00","month_days":[1,-1]}`, Summary: "月初月末账单", NextRun: at(31, 18, 0, 0)},
		{ID: 4, Spec: `{"kind":"interval","every_minutes":90}`, Summary: "90分钟巡检", NextRun: at(19, 11, 30, 0)},
		{ID: 5, Spec: `{"kind":"cron","cron":"0 9,18 1,15 * *"}`, Summary: "半月双次检查", NextRun: at(20, 9, 0, 0)},
	}
	if err := ws.Sync(entries, loc); err != nil {
		t.Fatalf("WindowsScheduler.Sync create: %v", err)
	}
	if len(registered) != 5 {
		t.Fatalf("expected 5 registered Windows tasks, got %d", len(registered))
	}

	// Verify UTF-16LE BOM, valid XML structure, <Day>Last</Day>, PT1H30M, and escaped trailing backslash!
	for tn, raw := range registered {
		decoded := decodeUTF16LE(t, raw)
		assertWellFormedXML(t, []byte(strings.Replace(decoded, `encoding="UTF-16"`, `encoding="UTF-8"`, 1)))
		if !strings.Contains(decoded, `data\\&quot;`) {
			t.Fatalf("%s missing CommandLineToArgvW escaped trailing backslash in Arguments:\n%s", tn, decoded)
		}
	}
	monthlyXML := decodeUTF16LE(t, registered[`\GrokBot\Task_3`])
	if !strings.Contains(monthlyXML, "<Day>1</Day>") || !strings.Contains(monthlyXML, "<Day>Last</Day>") {
		t.Fatalf("monthly XML missing <Day>1</Day> or <Day>Last</Day>:\n%s", monthlyXML)
	}
	intervalXML := decodeUTF16LE(t, registered[`\GrokBot\Task_4`])
	if !strings.Contains(intervalXML, "<Interval>PT1H30M</Interval>") {
		t.Fatalf("interval XML missing PT1H30M:\n%s", intervalXML)
	}

	// Re-syncing identical entries skips /Create calls (only 1 /Query).
	cmds = nil
	if err := ws.Sync(entries, loc); err != nil {
		t.Fatalf("WindowsScheduler.Sync idempotent: %v", err)
	}
	if len(cmds) != 1 || cmds[0][0] != "/Query" {
		t.Fatalf("expected only /Query on unchanged sync, got %v", cmds)
	}

	// Pausing #1 and removing #3..#5 deletes them via schtasks /Delete /TN ... /F.
	entries[0].Paused = true
	if err := ws.Sync(entries[:2], loc); err != nil {
		t.Fatalf("WindowsScheduler.Sync delete: %v", err)
	}
	if len(registered) != 1 || registered[`\GrokBot\Task_2`] == nil {
		t.Fatalf("expected only Task_2 to remain registered, got %v", len(registered))
	}
}

func decodeUTF16LE(t *testing.T, raw []byte) string {
	t.Helper()
	if len(raw) < 2 || raw[0] != 0xFF || raw[1] != 0xFE || len(raw)%2 != 0 {
		t.Fatalf("invalid UTF-16LE BOM buffer (len=%d)", len(raw))
	}
	u16 := make([]uint16, (len(raw)-2)/2)
	for i := range u16 {
		u16[i] = uint16(raw[2+i*2]) | uint16(raw[2+i*2+1])<<8
	}
	return string(utf16.Decode(u16))
}

func assertWellFormedXML(t *testing.T, raw []byte) {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(raw))
	for {
		_, err := dec.Token()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("malformed XML: %v\n%s", err, string(raw))
		}
	}
}
