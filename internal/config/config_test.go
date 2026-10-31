package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadRequiresOnePlatform(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	t.Setenv("DISCORD_BOT_TOKEN", "")
	t.Setenv("ALLOWED_USER_IDS", "")
	t.Setenv("DISCORD_ALLOWED_USER_IDS", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadTelegramOnly(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "tg-token")
	t.Setenv("DISCORD_BOT_TOKEN", "")
	t.Setenv("ALLOWED_USER_IDS", "1, 2")
	t.Setenv("DISCORD_ALLOWED_USER_IDS", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TelegramToken != "tg-token" || cfg.DiscordToken != "" {
		t.Fatalf("%#v", cfg)
	}
	if _, ok := cfg.TelegramUsers["1"]; !ok {
		t.Fatalf("%#v", cfg.TelegramUsers)
	}
	if _, ok := cfg.TelegramUsers["2"]; !ok {
		t.Fatalf("%#v", cfg.TelegramUsers)
	}
	if cfg.DiscordUsers != nil {
		t.Fatalf("%#v", cfg.DiscordUsers)
	}
}

func TestLoadDiscordStripsBotPrefix(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	t.Setenv("DISCORD_BOT_TOKEN", "Bot abc.def")
	t.Setenv("ALLOWED_USER_IDS", "")
	t.Setenv("DISCORD_ALLOWED_USER_IDS", "99")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DiscordToken != "abc.def" {
		t.Fatalf("%q", cfg.DiscordToken)
	}
	if _, ok := cfg.DiscordUsers["99"]; !ok {
		t.Fatalf("%#v", cfg.DiscordUsers)
	}
}

func TestLoadRejectsNonNumericDiscordID(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	t.Setenv("DISCORD_BOT_TOKEN", "token")
	t.Setenv("DISCORD_ALLOWED_USER_IDS", "abc")
	if _, err := Load(); err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadWebSearch(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "tg-token")
	t.Setenv("DISCORD_BOT_TOKEN", "")
	t.Setenv("ALLOWED_USER_IDS", "1")
	t.Setenv("WEB_SEARCH", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.WebSearch {
		t.Fatal("web search should default to on")
	}
	t.Setenv("WEB_SEARCH", "false")
	if cfg, err = Load(); err != nil || cfg.WebSearch {
		t.Fatalf("WEB_SEARCH=false: %v %v", cfg.WebSearch, err)
	}
	t.Setenv("WEB_SEARCH", "maybe")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for WEB_SEARCH=maybe")
	}
}

func TestLoadEnvFile(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "tg-token")
	t.Setenv("DISCORD_BOT_TOKEN", "")
	t.Setenv("ALLOWED_USER_IDS", "1")
	t.Setenv("BOT_ENV_FILE", "")
	cfg, err := Load()
	if err != nil || cfg.EnvFile != "/etc/grok-tg-bot.env" {
		t.Fatalf("%q %v", cfg.EnvFile, err)
	}
	t.Setenv("BOT_ENV_FILE", "/opt/bot/bot.env")
	if cfg, err = Load(); err != nil || cfg.EnvFile != "/opt/bot/bot.env" {
		t.Fatalf("%q %v", cfg.EnvFile, err)
	}
}

func TestLoadTaskSettings(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "tg-token")
	t.Setenv("DISCORD_BOT_TOKEN", "")
	t.Setenv("ALLOWED_USER_IDS", "1")
	t.Setenv("TIMEZONE", "")
	t.Setenv("TASK_LIMIT", "")
	t.Setenv("TASK_MIN_INTERVAL", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Location != time.Local || cfg.TaskLimit != 20 || cfg.TaskMinInterval != time.Hour {
		t.Fatalf("%v %d %v", cfg.Location, cfg.TaskLimit, cfg.TaskMinInterval)
	}
	t.Setenv("TIMEZONE", "UTC")
	t.Setenv("TASK_LIMIT", "5")
	t.Setenv("TASK_MIN_INTERVAL", "90m")
	if cfg, err = Load(); err != nil || cfg.Location.String() != "UTC" || cfg.TaskLimit != 5 || cfg.TaskMinInterval != 90*time.Minute {
		t.Fatalf("%v %d %v %v", cfg.Location, cfg.TaskLimit, cfg.TaskMinInterval, err)
	}
	for env, value := range map[string]string{"TIMEZONE": "Nowhere/Zone", "TASK_LIMIT": "0", "TASK_MIN_INTERVAL": "0s", "GROK_MAX_TURNS": "-1"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv(env, value)
			if _, err := Load(); err == nil {
				t.Fatalf("%s=%s accepted", env, value)
			}
		})
	}
	t.Setenv("GROK_MAX_TURNS", "0")
	if cfg, err = Load(); err != nil || cfg.GrokMaxTurns != -1 {
		t.Fatalf("GROK_MAX_TURNS=0: %d %v", cfg.GrokMaxTurns, err)
	}
}

func TestLoadUserProfile(t *testing.T) {
	home := t.TempDir()
	profile := strings.Join([]string{
		`# user profile`,
		`export THESEUS_API_KEY="secret-theseus"`,
		`export RAG_MCP_TOKEN='secret$literal'`,
		`UNEXPORTED_MCP_KEY=plain_val # inline comment`,
		`export EXISTING_BOT_VAR="from_profile"`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(home, ".bash_profile"), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("THESEUS_API_KEY", "")
	t.Setenv("RAG_MCP_TOKEN", "")
	t.Setenv("UNEXPORTED_MCP_KEY", "")
	t.Setenv("EXISTING_BOT_VAR", "from_systemd")

	LoadUserProfile(home)

	if got := os.Getenv("THESEUS_API_KEY"); got != "secret-theseus" {
		t.Fatalf("THESEUS_API_KEY = %q", got)
	}
	if got := os.Getenv("RAG_MCP_TOKEN"); got != "secret$literal" {
		t.Fatalf("RAG_MCP_TOKEN = %q", got)
	}
	if got := os.Getenv("UNEXPORTED_MCP_KEY"); got != "plain_val" {
		t.Fatalf("UNEXPORTED_MCP_KEY = %q", got)
	}
	if got := os.Getenv("EXISTING_BOT_VAR"); got != "from_systemd" {
		t.Fatalf("EXISTING_BOT_VAR overwritten: %q", got)
	}
}
