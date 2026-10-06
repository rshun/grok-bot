package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/rshun/grok-tg-bot/internal/config"
	"github.com/rshun/grok-tg-bot/internal/core"
	"github.com/rshun/grok-tg-bot/internal/grok"
	"github.com/rshun/grok-tg-bot/internal/platform/telegram"
	"github.com/rshun/grok-tg-bot/internal/store"
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
	engine := &grok.Client{
		Bin:        cfg.GrokBin,
		Workspace:  workspace,
		AuthPath:   cfg.AuthPath,
		BillingURL: cfg.BillingURL,
		UserURL:    cfg.UserURL,
		Timeout:    cfg.GrokTimeout,
	}
	svc := &core.Service{
		Store:        db,
		Engine:       engine,
		AllowedUsers: cfg.AllowedUsers,
		QueueLimit:   cfg.QueueLimit,
		Version:      cfg.Version,
	}
	bot, err := telegram.New(cfg.TelegramToken)
	if err != nil {
		slog.Error("telegram", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	slog.Info("listening", "version", version, "bot", bot.Username())
	if err := bot.Run(ctx, svc.Handle); err != nil && ctx.Err() == nil {
		slog.Error("poll", "err", err)
		os.Exit(1)
	}
}
