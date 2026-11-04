package schedule

import (
	"errors"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Backend registers and removes scheduled tasks in the host OS's native scheduler
// (Linux/Unix crontab, macOS launchd LaunchAgents, Windows Task Scheduler schtasks).
type Backend interface {
	Name() string
	Sync(entries []CronEntry, loc *time.Location) error
}

// SystemConfig describes the local paths needed so OS-triggered `grok-bot run-task <id>`
// runs with the exact same binary, working directory, data directory, and .env file.
type SystemConfig struct {
	GOOS    string
	BinPath string
	WorkDir string
	DataDir string
	EnvFile string
	HomeDir string
}

// NewSystemScheduler returns the native OS scheduler backend for cfg.GOOS (defaults to runtime.GOOS).
func NewSystemScheduler(cfg SystemConfig) (Backend, error) {
	if strings.TrimSpace(cfg.BinPath) == "" {
		return nil, errors.New("未找到 grok-bot 可执行文件路径")
	}
	goos := strings.ToLower(strings.TrimSpace(cfg.GOOS))
	if goos == "" {
		goos = runtime.GOOS
	}
	cfg.GOOS = goos
	switch goos {
	case "darwin":
		return NewLaunchd(cfg)
	case "windows":
		return NewWindowsScheduler(cfg)
	default:
		return NewSystemCrontabFromConfig(cfg)
	}
}

// RunTaskArgs returns the CLI arguments passed to `grok-bot` when an OS scheduler fires task id.
func RunTaskArgs(id int64, dataDir, envFile string) []string {
	args := []string{"run-task", strconv.FormatInt(id, 10)}
	if d := strings.TrimSpace(dataDir); d != "" {
		args = append(args, "--data-dir", d)
	}
	if e := strings.TrimSpace(envFile); e != "" {
		args = append(args, "--env-file", e)
	}
	return args
}
