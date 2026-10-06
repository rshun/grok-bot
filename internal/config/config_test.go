package config

import "testing"

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
