package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/rshun/grok-bot/internal/actions"
	"github.com/rshun/grok-bot/internal/config"
	"github.com/rshun/grok-bot/internal/core"
	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/platform"
	"github.com/rshun/grok-bot/internal/platform/discord"
	"github.com/rshun/grok-bot/internal/platform/telegram"
	"github.com/rshun/grok-bot/internal/sandbox"
	"github.com/rshun/grok-bot/internal/schedule"
	"github.com/rshun/grok-bot/internal/store"
)

var version = "dev"

func main() {
	inv, err := parseCLI(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error()+"\n\n"+cliUsage)
		os.Exit(2)
	}
	switch inv.Mode {
	case modeVersion:
		fmt.Printf("grok-bot %s\n", version)
		return
	case modeHelp:
		fmt.Println(cliUsage)
		return
	case modePush:
		if isPushHelp(inv.PushArgs) {
			fmt.Println(pushUsage)
			return
		}
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	exeDirs := executableDirs()
	var loadedEnv string
	if os.Getenv("TELEGRAM_BOT_TOKEN") == "" && os.Getenv("DISCORD_BOT_TOKEN") == "" {
		loadedEnv = config.LoadEnvFile(envCandidatePaths(os.Getenv("BOT_ENV_FILE"), exeDirs)...)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		config.LoadUserProfile(home)
	}
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}
	cfg.Version = version
	if os.Getenv("DATA_DIR") == "" {
		cfg.DataDir = resolveDataDir(cfg.DataDir, loadedEnv, exeDirs)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		slog.Error("home dir", "err", err)
		os.Exit(1)
	}
	dataDir, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		slog.Error("data dir", "err", err)
		os.Exit(1)
	}
	policy := sandbox.Policy{
		Home:     home,
		DataDir:  dataDir,
		EnvFile:  cfg.EnvFile,
		AuthPath: cfg.AuthPath,
	}
	if inv.Mode == modePush {
		var db *store.Store
		if fileExists(filepath.Join(cfg.DataDir, "bot.db")) {
			if opened, openErr := store.Open(cfg.DataDir, cfg.DefaultModel); openErr == nil {
				db = opened
				defer db.Close()
			}
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if err := runPushCommand(ctx, cfg, db, policy, inv.PushArgs); err != nil {
			os.Exit(1)
		}
		return
	}

	isTaskRun := inv.Mode == modeRunTask
	workspaceName := "workspace"
	if isTaskRun {
		// Isolated workspace so one-shot cron runs do not overwrite the daemon's MCP port config.
		workspaceName = "task-workspace"
	}
	workspace := filepath.Join(cfg.DataDir, workspaceName)
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		slog.Error("data dir", "err", err)
		os.Exit(1)
	}
	// Prompt files of turns with images live here only while grok runs; only the bot may read them.
	inbox := filepath.Join(cfg.DataDir, "inbox")
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		slog.Error("inbox dir", "err", err)
		os.Exit(1)
	}
	// MkdirAll keeps the mode of a directory that already exists.
	if err := os.Chmod(inbox, 0o700); err != nil {
		slog.Warn("inbox dir mode", "err", err)
	}
	db, err := store.Open(cfg.DataDir, cfg.DefaultModel)
	if err != nil {
		slog.Error("store", "err", err)
		os.Exit(1)
	}
	defer db.Close()
	sandboxManager := &sandbox.Manager{
		Path:          filepath.Join(home, ".grok", "sandbox.toml"),
		Enabled:       sandbox.Supported(),
		DenyAvailable: sandbox.BwrapAvailable,
		Policy:        policy,
	}
	engine := &grok.Client{
		Bin:        cfg.GrokBin,
		Workspace:  workspace,
		AuthPath:   cfg.AuthPath,
		BillingURL: cfg.BillingURL,
		UserURL:    cfg.UserURL,
		Timeout:    cfg.GrokTimeout,
		MaxTurns:   cfg.GrokMaxTurns,
		InboxDir:   inbox,
	}
	if !isTaskRun {
		engine.ResetLeader(context.Background())
	}
	// The local MCP server lets grok act on the chat during a turn. Chat keeps working without it.
	// actionsForSvc stays nil when the server cannot listen or grok cannot be pointed at it.
	var actionsForSvc *actions.Server
	if actionsSrv, err := actions.Listen("127.0.0.1:0"); err != nil {
		slog.Error("actions listen; chat actions are off", "err", err)
	} else {
		switch err := actions.WriteConfig(filepath.Join(workspace, ".grok", "config.toml"), actionsSrv.Port()); {
		case err == nil:
			actionsForSvc = actionsSrv
		case errors.Is(err, actions.ErrUnmanaged):
			slog.Warn("workspace .grok/config.toml was not written by the bot; chat actions are off")
		default:
			slog.Error("write grok config; chat actions are off", "err", err)
		}
		if actionsForSvc == nil {
			_ = actionsSrv.Close()
		}
	}
	if actionsForSvc != nil {
		defer actionsForSvc.Close()
	}
	svc := &core.Service{
		Actions:             actionsForSvc,
		Store:               db,
		Engine:              engine,
		AllowedUsers:        map[string]map[string]struct{}{},
		QueueLimit:          cfg.QueueLimit,
		Version:             cfg.Version,
		WebSearch:           cfg.WebSearch,
		DisableLinkPrefetch: !cfg.LinkPrefetch,
		AllowShell:          cfg.AllowShell,
		ShowProgress:        cfg.ShowProgress,
		UploadDir:           filepath.Join(dataDir, "uploads"),
		Sandbox:             sandboxManager,
		Senders:             map[string]platform.Replier{},
		Location:            cfg.Location,
		TaskLimit:           cfg.TaskLimit,
		TaskMinInterval:     cfg.TaskMinInterval,
		Crontab:             buildCrontab(cfg.EnvFile),
	}
	type runner struct {
		name string
		run  func(context.Context) error
	}
	var runners []runner
	logArgs := []any{"version", version}
	if cfg.TelegramToken != "" {
		bot, err := telegram.New(cfg.TelegramToken)
		if err != nil {
			slog.Error("telegram", "err", err)
			os.Exit(1)
		}
		svc.AllowedUsers[platform.Telegram] = cfg.TelegramUsers
		svc.Senders[platform.Telegram] = bot
		logArgs = append(logArgs, "telegram", bot.Username())
		runners = append(runners, runner{name: "telegram", run: func(ctx context.Context) error {
			return bot.Run(ctx, svc.Handle)
		}})
	}
	if cfg.DiscordToken != "" {
		bot, err := discord.New(cfg.DiscordToken)
		if err != nil {
			slog.Error("discord", "err", err)
			os.Exit(1)
		}
		svc.AllowedUsers[platform.Discord] = cfg.DiscordUsers
		svc.Senders[platform.Discord] = bot
		logArgs = append(logArgs, "discord", bot.Username())
		runners = append(runners, runner{name: "discord", run: func(ctx context.Context) error {
			return bot.Run(ctx, svc.Handle)
		}})
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if actionsForSvc != nil {
		go func() {
			if err := actionsForSvc.Serve(ctx); err != nil {
				slog.Error("actions server", "err", err)
			}
		}()
	}
	if isTaskRun {
		if err := svc.RunTaskByID(ctx, inv.TaskID); err != nil {
			slog.Error("run-task", "id", inv.TaskID, "err", err)
			os.Exit(1)
		}
		return
	}
	slog.Info("listening", logArgs...)
	go svc.RunScheduler(ctx, 30*time.Second)
	errCh := make(chan error, len(runners))
	for _, item := range runners {
		go func(item runner) {
			err := item.run(ctx)
			if err != nil && ctx.Err() == nil {
				slog.Error(item.name, "err", err)
				stop()
				errCh <- err
				return
			}
			errCh <- nil
		}(item)
	}
	failed := false
	for range runners {
		if err := <-errCh; err != nil {
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
}

func executableDirs() []string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return nil
	}
	dirs := []string{filepath.Dir(exe)}
	if real, err := filepath.EvalSymlinks(exe); err == nil && real != "" {
		realDir := filepath.Dir(real)
		if realDir != dirs[0] {
			dirs = append(dirs, realDir)
		}
	}
	return dirs
}

func envCandidatePaths(botEnvFile string, exeDirs []string) []string {
	paths := []string{botEnvFile, "/etc/grok-tg-bot.env", ".env"}
	for _, dir := range exeDirs {
		if dir != "" {
			paths = append(paths, filepath.Join(dir, ".env"))
		}
	}
	return paths
}

func resolveDataDir(defaultDir, loadedEnv string, exeDirs []string) string {
	if fileExists(filepath.Join(defaultDir, "bot.db")) {
		return defaultDir
	}
	if loadedEnv != "" {
		cand := filepath.Join(filepath.Dir(loadedEnv), "data")
		if fileExists(filepath.Join(cand, "bot.db")) {
			return cand
		}
	}
	for _, dir := range exeDirs {
		cand := filepath.Join(dir, "data")
		if fileExists(filepath.Join(cand, "bot.db")) {
			return cand
		}
	}
	return defaultDir
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func buildCrontab(envFile string) *schedule.Crontab {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	wd, _ := os.Getwd()
	if envFile != "" {
		if _, err := os.Stat(envFile); err != nil {
			envFile = ""
		}
	}
	return schedule.NewSystemCrontab(exe, wd, envFile)
}
