package main

import (
	"context"
	"errors"
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
	"github.com/rshun/grok-bot/internal/store"
)

var version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}
	cfg.Version = version
	workspace := filepath.Join(cfg.DataDir, "workspace")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		slog.Error("data dir", "err", err)
		os.Exit(1)
	}
	db, err := store.Open(cfg.DataDir, cfg.DefaultModel)
	if err != nil {
		slog.Error("store", "err", err)
		os.Exit(1)
	}
	defer db.Close()
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
	sandboxManager := &sandbox.Manager{
		Path:          filepath.Join(home, ".grok", "sandbox.toml"),
		Enabled:       sandbox.Supported(),
		DenyAvailable: sandbox.BwrapAvailable,
		Policy: sandbox.Policy{
			Home:     home,
			DataDir:  dataDir,
			EnvFile:  cfg.EnvFile,
			AuthPath: cfg.AuthPath,
		},
	}
	engine := &grok.Client{
		Bin:        cfg.GrokBin,
		Workspace:  workspace,
		AuthPath:   cfg.AuthPath,
		BillingURL: cfg.BillingURL,
		UserURL:    cfg.UserURL,
		Timeout:    cfg.GrokTimeout,
	}
	// The local MCP server lets grok act on the chat during a turn. Chat keeps working without it.
	actionsSrv, err := actions.Listen("127.0.0.1:0")
	if err != nil {
		slog.Error("actions listen", "err", err)
		os.Exit(1)
	}
	defer actionsSrv.Close()
	var actionsForSvc *actions.Server
	switch err := actions.WriteConfig(filepath.Join(workspace, ".grok", "config.toml"), actionsSrv.Port()); {
	case err == nil:
		actionsForSvc = actionsSrv
	case errors.Is(err, actions.ErrUnmanaged):
		slog.Warn("workspace .grok/config.toml was not written by the bot; chat actions are off")
	default:
		slog.Error("write grok config; chat actions are off", "err", err)
	}
	svc := &core.Service{
		Actions:         actionsForSvc,
		Store:           db,
		Engine:          engine,
		AllowedUsers:    map[string]map[string]struct{}{},
		QueueLimit:      cfg.QueueLimit,
		Version:         cfg.Version,
		WebSearch:       cfg.WebSearch,
		Sandbox:         sandboxManager,
		Senders:         map[string]platform.Replier{},
		Location:        cfg.Location,
		TaskLimit:       cfg.TaskLimit,
		TaskMinInterval: cfg.TaskMinInterval,
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
	slog.Info("listening", logArgs...)
	go func() {
		if err := actionsSrv.Serve(ctx); err != nil {
			slog.Error("actions server", "err", err)
		}
	}()
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
