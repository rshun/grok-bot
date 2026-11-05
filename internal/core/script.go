package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"

	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/store"
)

const (
	// scriptOutputMax bounds the script output kept in memory for the log and the chat tail.
	scriptOutputMax = 256 << 10
	// scriptTailLines is how many trailing output lines are shown in chat.
	scriptTailLines = 15
	// scriptLogMax rotates a per-task script log to .1 once it reaches this size.
	scriptLogMax = 2 << 20
)

// scriptSecretEnv are bot secrets never passed to script tasks. Scripts that need to push
// messages call `grok-bot send`, which reads the env file itself.
var scriptSecretEnv = map[string]bool{
	"TELEGRAM_BOT_TOKEN":       true,
	"DISCORD_BOT_TOKEN":        true,
	"ALLOWED_USER_IDS":         true,
	"DISCORD_ALLOWED_USER_IDS": true,
}

func (s *Service) scriptTimeout() time.Duration {
	if s.ScriptTimeout <= 0 {
		return 30 * time.Minute
	}
	return s.ScriptTimeout
}

// CheckScript validates a script path for a script task and returns its real path.
// Only regular files inside ScriptDirs are accepted; error messages are meant for the chat user.
func (s *Service) CheckScript(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("请提供脚本的绝对路径。")
	}
	if strings.IndexFunc(raw, unicode.IsControl) >= 0 || strings.ContainsAny(raw, `"*?[`) {
		return "", errors.New("脚本路径里不能有引号、通配符或控制字符。")
	}
	if !filepath.IsAbs(raw) {
		return "", errors.New("请写脚本的绝对路径，例如 /home/grok/bin/job.py。")
	}
	if len(s.ScriptDirs) == 0 {
		return "", errors.New("未配置允许的脚本目录（SCRIPT_DIRS），脚本任务不可用。")
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(raw))
	if err != nil {
		return "", errors.New("找不到这个脚本文件。")
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", errors.New("找不到这个脚本文件。")
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("这不是普通文件。")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o002 != 0 {
		return "", errors.New("脚本对所有用户可写（world-writable），存在被篡改风险，请先 chmod o-w。")
	}
	for _, dir := range s.ScriptDirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		if resolved, rErr := filepath.EvalSymlinks(dir); rErr == nil {
			dir = resolved
		}
		if pathWithin(real, dir) && real != filepath.Clean(dir) {
			return real, nil
		}
	}
	return "", fmt.Errorf("脚本必须放在允许的目录内：%s。", strings.Join(s.ScriptDirs, "、"))
}

// pathWithin reports whether path is dir or inside dir.
func pathWithin(path, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// pythonInterpreter finds python3 on Unix; on Windows "python3" is often a Store stub, so
// "python" is tried first there.
func pythonInterpreter() (string, error) {
	names := []string{"python3", "python"}
	if runtime.GOOS == "windows" {
		names = []string{"python", "python3"}
	}
	for _, name := range names {
		if bin, err := exec.LookPath(name); err == nil {
			return bin, nil
		}
	}
	return "", errors.New("找不到 python3 解释器")
}

// scriptCommand picks the interpreter by extension; other files must be executable themselves.
func scriptCommand(ctx context.Context, path string) (*exec.Cmd, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".py":
		bin, err := pythonInterpreter()
		if err != nil {
			return nil, err
		}
		return exec.CommandContext(ctx, bin, path), nil
	case ".sh":
		return exec.CommandContext(ctx, "/bin/sh", path), nil
	}
	return exec.CommandContext(ctx, path), nil
}

// scriptEnv is the process environment without bot secrets, plus task context for the script.
func (s *Service) scriptEnv(t store.Task) []string {
	out := make([]string, 0, len(os.Environ())+len(s.ScriptEnv)+4)
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		if scriptSecretEnv[name] {
			continue
		}
		out = append(out, item)
	}
	out = append(out, s.ScriptEnv...)
	return append(out,
		fmt.Sprintf("GROK_BOT_TASK_ID=%d", t.ID),
		"PYTHONIOENCODING=utf-8",
		"PYTHONUNBUFFERED=1",
	)
}

// limitedBuffer keeps at most max bytes, dropping the oldest output first.
type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.buf.Write(p)
	if over := b.buf.Len() - b.max; over > 0 {
		b.buf.Next(over)
	}
	return n, nil
}

// runScriptTask runs a script task and returns the chat message and whether it succeeded.
// It never involves Grok, so it uses no Grok session or quota.
func (s *Service) runScriptTask(ctx context.Context, t store.Task) (string, bool) {
	header := taskHeader(t)
	real, err := s.CheckScript(t.Script)
	if err != nil {
		return header + "\n\n⚠️ 脚本校验未通过，未执行：" + err.Error(), false
	}
	runCtx, cancel := context.WithTimeout(ctx, s.scriptTimeout())
	defer cancel()
	cmd, err := scriptCommand(runCtx, real)
	if err != nil {
		return header + "\n\n⚠️ 脚本无法启动：" + err.Error(), false
	}
	cmd.Dir = filepath.Dir(real)
	cmd.Env = s.scriptEnv(t)
	out := &limitedBuffer{max: scriptOutputMax}
	cmd.Stdout = out
	cmd.Stderr = out
	// Do not hang on grandchildren that keep the output pipe open after a timeout kill.
	cmd.WaitDelay = 10 * time.Second

	started := s.now()
	runErr := cmd.Run()
	elapsed := s.now().Sub(started).Round(100 * time.Millisecond)
	output := out.buf.String()
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
	s.appendScriptLog(t, real, started, exitCode, timedOut, runErr, output)

	var status string
	ok := runErr == nil
	switch {
	case timedOut:
		status = fmt.Sprintf("⚠️ 脚本执行超时（超过 %s 已终止）", s.scriptTimeout())
		ok = false
	case runErr != nil && exitCode > 0:
		status = fmt.Sprintf("⚠️ 脚本执行失败（退出码 %d，耗时 %s）", exitCode, elapsed)
	case runErr != nil:
		status = "⚠️ 脚本执行失败：" + grok.RedactSensitive(runErr.Error())
	default:
		status = fmt.Sprintf("✅ 脚本执行完成（退出码 0，耗时 %s）", elapsed)
	}
	if ok {
		slog.Info("script task finished", "task", t.ID, "script", real, "elapsed", elapsed)
	} else {
		slog.Error("script task failed", "task", t.ID, "script", real, "exit", exitCode, "timeout", timedOut, "err", runErr)
	}
	msg := header + "\n\n" + status + "\n脚本：" + real
	if tail := outputTail(output, scriptTailLines); tail != "" {
		msg += "\n\n输出（末尾 " + fmt.Sprint(scriptTailLines) + " 行）：\n```\n" + grok.RedactSensitive(tail) + "\n```"
	}
	return msg, ok
}

func outputTail(output string, lines int) string {
	output = strings.TrimRight(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	if strings.TrimSpace(output) == "" {
		return ""
	}
	parts := strings.Split(output, "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	tail := strings.Join(parts, "\n")
	if r := []rune(tail); len(r) > 1500 {
		tail = "…" + string(r[len(r)-1500:])
	}
	return strings.ReplaceAll(tail, "```", "'''")
}

// appendScriptLog keeps each script task's full output in <DATA_DIR>/task-logs/task-<id>.log.
func (s *Service) appendScriptLog(t store.Task, script string, started time.Time, exitCode int, timedOut bool, runErr error, output string) {
	if s.ScriptLogDir == "" {
		return
	}
	if err := os.MkdirAll(s.ScriptLogDir, 0o750); err != nil {
		slog.Warn("script log dir", "err", err)
		return
	}
	path := filepath.Join(s.ScriptLogDir, fmt.Sprintf("task-%d.log", t.ID))
	if info, err := os.Stat(path); err == nil && info.Size() >= scriptLogMax {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		slog.Warn("script log open", "err", err)
		return
	}
	defer f.Close()
	result := fmt.Sprintf("exit=%d", exitCode)
	if timedOut {
		result += " timeout"
	} else if runErr != nil && exitCode <= 0 {
		result += " err=" + runErr.Error()
	}
	loc := s.location()
	fmt.Fprintf(f, "=== %s start %s ===\n%s", started.In(loc).Format("2006-01-02 15:04:05"), script, output)
	if output != "" && !strings.HasSuffix(output, "\n") {
		fmt.Fprintln(f)
	}
	fmt.Fprintf(f, "=== %s end %s ===\n", s.now().In(loc).Format("2006-01-02 15:04:05"), result)
}

// startScriptTrial runs a script task in the background for /task run so long scripts never
// block the chat lane; the result is pushed to every configured platform when it finishes.
func (s *Service) startScriptTrial(t store.Task) string {
	go func() {
		ctx := context.Background()
		text, _ := s.runScriptTask(ctx, t)
		s.deliverTask(ctx, t, "", "", nil, text, nil)
	}()
	return fmt.Sprintf("▶️ 已在后台启动脚本任务 #%d：%s。\n脚本：%s\n执行完成后会推送结果（不占用当前会话）。", t.ID, t.Summary, t.Script)
}
