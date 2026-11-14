package core

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o750); err != nil {
		t.Fatal(err)
	}
	return path
}

func requirePython(t *testing.T) {
	t.Helper()
	bin, err := pythonInterpreter()
	if err != nil {
		t.Skip("python not available")
	}
	if out, err := exec.Command(bin, "-c", "print(1)").CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "1" {
		t.Skip("python not runnable")
	}
}

func TestCheckScriptRestrictsToScriptDirs(t *testing.T) {
	svc, _, _ := taskService(t, dailyNews)
	allowed := t.TempDir()
	outside := t.TempDir()
	svc.ScriptDirs = []string{allowed}
	ok := writeScript(t, allowed, "job.py", "print('ok')\n")
	bad := writeScript(t, outside, "evil.py", "print('x')\n")

	real, err := svc.CheckScript(ok)
	if err != nil {
		t.Fatalf("allowed script rejected: %v", err)
	}
	if resolved, _ := filepath.EvalSymlinks(ok); real != resolved {
		t.Fatalf("real=%q want %q", real, resolved)
	}
	for _, raw := range []string{bad, "job.py", filepath.Join(allowed, "missing.py"), allowed, ""} {
		if _, err := svc.CheckScript(raw); err == nil {
			t.Fatalf("expected rejection for %q", raw)
		}
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(ok, 0o777); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.CheckScript(ok); err == nil || !strings.Contains(err.Error(), "world-writable") {
			t.Fatalf("world-writable script accepted: %v", err)
		}
	}
	svc.ScriptDirs = nil
	if _, err := svc.CheckScript(ok); err == nil {
		t.Fatal("script accepted without SCRIPT_DIRS")
	}
}

func TestScriptTaskCreateRunsWithoutGrokAndAlertsOnFailure(t *testing.T) {
	requirePython(t)
	svc, engine, sent := taskService(t, dailyNews)
	dir := t.TempDir()
	svc.ScriptDirs = []string{dir}
	svc.ScriptLogDir = filepath.Join(t.TempDir(), "task-logs")
	okScript := writeScript(t, dir, "ok.py", "import os\nprint('hello', os.environ.get('GROK_BOT_TASK_ID'), os.environ.get('TELEGRAM_BOT_TOKEN'))\n")
	failScript := writeScript(t, dir, "fail.py", "print('boom')\nraise SystemExit(3)\n")
	t.Setenv("TELEGRAM_BOT_TOKEN", "secret-token-should-not-leak")

	rec := &recorder{}
	say(svc, rec, "/task script 0 9 * * * "+okScript+" 工作日早餐下单")
	got := rec.wait(t, 1)
	if !strings.Contains(got[0], "已创建 #1") || !strings.Contains(got[0], "📜 脚本任务") {
		t.Fatalf("%#v", got)
	}
	say(svc, rec, "/task list")
	got = rec.wait(t, 2)
	if !strings.Contains(got[1], "#1 📜") {
		t.Fatalf("list missing script mark: %#v", got)
	}

	// Scheduled run: success is silent and never calls Grok.
	svc.Now = func() time.Time { return time.Date(2026, 10, 19, 9, 0, 0, 0, taskZone) }
	if err := svc.RunTaskByID(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	engine.mu.Lock()
	calls := len(engine.calls)
	engine.mu.Unlock()
	if calls != 0 {
		t.Fatalf("script task called Grok %d times", calls)
	}
	sent.mu.Lock()
	pushed := len(sent.msgs)
	sent.mu.Unlock()
	if pushed != 0 {
		t.Fatalf("successful script pushed messages: %#v", sent.msgs)
	}
	logData, err := os.ReadFile(filepath.Join(svc.ScriptLogDir, "task-1.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "hello 1 None") || !strings.Contains(string(logData), "exit=0") {
		t.Fatalf("log=%q", logData)
	}
	if strings.Contains(string(logData), "secret-token") {
		t.Fatal("bot token leaked into script environment")
	}

	// Bind the task to a failing script, keeping the schedule; the failure is pushed.
	say(svc, rec, "/task bind 1 "+failScript)
	got = rec.wait(t, 3)
	if !strings.Contains(got[2], "绑定为脚本任务") {
		t.Fatalf("%#v", got)
	}
	svc.Now = func() time.Time { return time.Date(2026, 10, 20, 9, 0, 0, 0, taskZone) }
	if err := svc.RunTaskByID(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	alert := sent.wait(t, 1)
	if !strings.Contains(alert[0], "⏰ 定时任务 #1") || !strings.Contains(alert[0], "退出码 3") || !strings.Contains(alert[0], "boom") {
		t.Fatalf("alert=%#v", alert)
	}

	// Unbind turns it back into a Grok prompt task.
	say(svc, rec, "/task unbind 1")
	got = rec.wait(t, 4)
	if !strings.Contains(got[3], "恢复为 Grok 指令任务") {
		t.Fatalf("%#v", got)
	}
	task, _, _ := svc.Store.TaskByID(context.Background(), 1)
	if task.Script != "" {
		t.Fatalf("script not cleared: %#v", task)
	}
}

func TestScriptTaskRejectsPathOutsideScriptDirs(t *testing.T) {
	svc, _, _ := taskService(t, dailyNews)
	svc.ScriptDirs = []string{t.TempDir()}
	outside := writeScript(t, t.TempDir(), "x.py", "print(1)\n")
	rec := &recorder{}
	say(svc, rec, "/task script 0 9 * * * "+outside)
	got := rec.wait(t, 1)
	if !strings.Contains(got[0], "允许的目录") {
		t.Fatalf("%#v", got)
	}
	if all, _ := svc.Store.AllTasks(context.Background()); len(all) != 0 {
		t.Fatalf("task created: %#v", all)
	}
}

func TestScriptTaskSinglePlatformEnvAndFailureAlert(t *testing.T) {
	requirePython(t)
	svc, _, tgSent := taskService(t, dailyNews)
	dcRec := &recorder{}
	svc.AllowedUsers["discord"] = map[string]struct{}{"dc_u1": {}}
	svc.Senders["discord"] = &fakeDiscordSender{rec: dcRec}

	dir := t.TempDir()
	svc.ScriptDirs = []string{dir}
	svc.ScriptLogDir = filepath.Join(t.TempDir(), "task-logs")
	script := writeScript(t, dir, "plat.py", "import os\nprint('plat=' + str(os.environ.get('GROK_BOT_TASK_PLATFORM')))\nraise SystemExit(2)\n")

	rec := &recorder{}
	say(svc, rec, "/task script -p dc 0 9 * * * "+script+" 仅DC脚本检查")
	got := rec.wait(t, 1)
	if !strings.Contains(got[0], "仅推送 Discord") {
		t.Fatalf("create reply: %#v", got)
	}

	svc.Now = func() time.Time { return time.Date(2026, 10, 19, 9, 0, 0, 0, taskZone) }
	if err := svc.RunTaskByID(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	dcAlert := dcRec.wait(t, 1)
	if !strings.Contains(dcAlert[0], "plat=discord") || !strings.Contains(dcAlert[0], "退出码 2") {
		t.Fatalf("discord alert: %#v", dcAlert)
	}
	tgSent.mu.Lock()
	tgCount := len(tgSent.msgs)
	tgSent.mu.Unlock()
	if tgCount != 0 {
		t.Fatalf("TG should not receive DC-only script alert, got %#v", tgSent.msgs)
	}
}
