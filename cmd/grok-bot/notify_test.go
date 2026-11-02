package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rshun/grok-bot/internal/config"
	"github.com/rshun/grok-bot/internal/platform"
)

func TestParsePushArgs(t *testing.T) {
	req, err := parsePushArgs([]string{"-p", "tg", "--chat", "123", "-f", "/srv/a.csv", "盘前检查", "无异常"}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if req.Platform != "telegram" || req.ChatID != "123" || len(req.Files) != 1 || req.Files[0] != "/srv/a.csv" || req.Text != "盘前检查 无异常" {
		t.Fatalf("unexpected req: %#v", req)
	}

	// Read from piped stdin when no positional text is passed.
	pipedReq, err := parsePushArgs(nil, strings.NewReader("来自管道的通知\n"), true)
	if err != nil || pipedReq.Text != "来自管道的通知" {
		t.Fatalf("piped got %#v, err %v", pipedReq, err)
	}

	// Missing both text and file fails cleanly.
	if _, err := parsePushArgs(nil, nil, false); err == nil {
		t.Fatal("expected error when both text and file are empty")
	}
}

func TestParseCLI(t *testing.T) {
	// No args -> daemon mode.
	inv, err := parseCLI(nil)
	if err != nil || inv.Mode != modeDaemon {
		t.Fatalf("empty args: %#v, err=%v", inv, err)
	}

	// Version and help flags.
	for _, flag := range []string{"-v", "--version", "version"} {
		if inv, err := parseCLI([]string{flag}); err != nil || inv.Mode != modeVersion {
			t.Fatalf("%s: %#v, err=%v", flag, inv, err)
		}
	}
	for _, flag := range []string{"-h", "--help", "help"} {
		if inv, err := parseCLI([]string{flag}); err != nil || inv.Mode != modeHelp {
			t.Fatalf("%s: %#v, err=%v", flag, inv, err)
		}
	}

	// Standard `send` subcommand and flexible flag-before-subcommand (`grok-bot -p dc send "hi"`).
	inv, err = parseCLI([]string{"send", "-p", "dc", "升级成功"})
	if err != nil || inv.Mode != modePush || !reflect.DeepEqual(inv.PushArgs, []string{"-p", "dc", "升级成功"}) {
		t.Fatalf("send after: %#v, err=%v", inv, err)
	}
	inv, err = parseCLI([]string{"-p", "dc", "send", "升级成功"})
	if err != nil || inv.Mode != modePush || !reflect.DeepEqual(inv.PushArgs, []string{"-p", "dc", "升级成功"}) {
		t.Fatalf("flags before send: %#v, err=%v", inv, err)
	}
	inv, err = parseCLI([]string{"--platform=tg", "-f", "/srv/a.csv", "notify", "日报"})
	if err != nil || inv.Mode != modePush || !reflect.DeepEqual(inv.PushArgs, []string{"--platform=tg", "-f", "/srv/a.csv", "日报"}) {
		t.Fatalf("flags before notify: %#v, err=%v", inv, err)
	}

	// Valid and invalid `run-task`.
	inv, err = parseCLI([]string{"run-task", "42"})
	if err != nil || inv.Mode != modeRunTask || inv.TaskID != 42 {
		t.Fatalf("run-task: %#v, err=%v", inv, err)
	}
	if _, err := parseCLI([]string{"run-task"}); err == nil {
		t.Fatal("expected error for bare run-task")
	}
	if _, err := parseCLI([]string{"run-task", "abc"}); err == nil {
		t.Fatal("expected error for non-numeric task id")
	}

	// Unknown args MUST fail instead of falling through to daemon mode!
	for _, bad := range [][]string{
		{"--unknown-flag"},
		{"pushtest", "hello"},
		{"-p", "dc", "unknown", "hello"},
	} {
		if _, err := parseCLI(bad); err == nil {
			t.Fatalf("expected error for %v, got nil", bad)
		}
	}
}

func TestTargetPlatformsDefaultDualPush(t *testing.T) {
	ctx := context.Background()
	dualCfg := config.Config{
		TelegramToken: "tg-placeholder",
		DiscordToken:  "dc-placeholder",
		TelegramUsers: map[string]struct{}{"10001": {}},
		DiscordUsers:  map[string]struct{}{"20002": {}},
	}

	// Default `grok-bot send "msg"` pushes to BOTH configured platforms.
	plats, err := targetPlatforms(ctx, dualCfg, nil, pushRequest{Text: "hello"})
	if err != nil || !reflect.DeepEqual(plats, []string{platform.Telegram, platform.Discord}) {
		t.Fatalf("dual default got %v, err=%v", plats, err)
	}

	// Explicit `-p dc` or `-p tg` targets only that platform.
	plats, err = targetPlatforms(ctx, dualCfg, nil, pushRequest{Platform: platform.Discord, Text: "hello"})
	if err != nil || !reflect.DeepEqual(plats, []string{platform.Discord}) {
		t.Fatalf("explicit dc got %v, err=%v", plats, err)
	}
	plats, err = targetPlatforms(ctx, dualCfg, nil, pushRequest{Platform: platform.Telegram, Text: "hello"})
	if err != nil || !reflect.DeepEqual(plats, []string{platform.Telegram}) {
		t.Fatalf("explicit tg got %v, err=%v", plats, err)
	}

	// `--chat` matching an allowed user on one platform routes exclusively to that platform.
	plats, err = targetPlatforms(ctx, dualCfg, nil, pushRequest{ChatID: "20002", Text: "hello"})
	if err != nil || !reflect.DeepEqual(plats, []string{platform.Discord}) {
		t.Fatalf("chat infer dc got %v, err=%v", plats, err)
	}

	// Ambiguous `--chat` without `-p` on dual-platform setup returns an error.
	if _, err := targetPlatforms(ctx, dualCfg, nil, pushRequest{ChatID: "99999", Text: "hello"}); err == nil {
		t.Fatal("expected error for unknown --chat without -p on dual platform")
	}
}

func TestEnvCandidateAndResolveDataDir(t *testing.T) {
	exeDir := t.TempDir()
	paths := envCandidatePaths("", []string{exeDir})
	wantEnv := filepath.Join(exeDir, ".env")
	if paths[len(paths)-1] != wantEnv {
		t.Fatalf("expected exe .env %q in %v", wantEnv, paths)
	}

	dataDir := filepath.Join(exeDir, "data")
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "bot.db"), []byte("sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	nonExistentDefault := filepath.Join(t.TempDir(), "missing-data")
	if got := resolveDataDir(nonExistentDefault, wantEnv, []string{exeDir}); got != dataDir {
		t.Fatalf("resolveDataDir got %q, want %q", got, dataDir)
	}
}
