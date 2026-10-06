package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultModel   = "grok-4.7"
	defaultBilling = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"
	defaultUserURL = "https://cli-chat-proxy.grok.com/v1/user?include=subscription"
)

// Config is the process configuration. Secrets come from the environment,
// which systemd loads from /etc/grok-tg-bot.env.
type Config struct {
	TelegramToken string
	DiscordToken  string
	TelegramUsers map[string]struct{}
	DiscordUsers  map[string]struct{}
	DataDir       string
	DefaultModel  string
	GrokBin       string
	AuthPath      string
	BillingURL    string
	UserURL       string
	GrokTimeout   time.Duration
	QueueLimit    int
	Version       string
}

func Load() (Config, error) {
	token := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	discordToken := strings.TrimSpace(os.Getenv("DISCORD_BOT_TOKEN"))
	discordToken = strings.TrimPrefix(discordToken, "Bot ")
	if token == "" && discordToken == "" {
		return Config{}, fmt.Errorf("TELEGRAM_BOT_TOKEN or DISCORD_BOT_TOKEN is required")
	}
	var allowed map[string]struct{}
	var discordUsers map[string]struct{}
	var err error
	if token != "" {
		allowed, err = parseUsers(os.Getenv("ALLOWED_USER_IDS"), "ALLOWED_USER_IDS", "Telegram")
		if err != nil {
			return Config{}, err
		}
	}
	if discordToken != "" {
		discordUsers, err = parseUsers(os.Getenv("DISCORD_ALLOWED_USER_IDS"), "DISCORD_ALLOWED_USER_IDS", "Discord")
		if err != nil {
			return Config{}, err
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, fmt.Errorf("home directory: %w", err)
	}
	dataDir := strings.TrimSpace(os.Getenv("DATA_DIR"))
	if dataDir == "" {
		dataDir = "data"
	}
	timeout := 4 * time.Minute
	if raw := strings.TrimSpace(os.Getenv("GROK_TIMEOUT")); raw != "" {
		timeout, err = time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("GROK_TIMEOUT: %w", err)
		}
	}
	queueLimit := 8
	if raw := strings.TrimSpace(os.Getenv("QUEUE_LIMIT")); raw != "" {
		queueLimit, err = strconv.Atoi(raw)
		if err != nil || queueLimit < 1 {
			return Config{}, fmt.Errorf("QUEUE_LIMIT must be a positive integer")
		}
	}
	model := strings.TrimSpace(os.Getenv("DEFAULT_MODEL"))
	if model == "" {
		model = defaultModel
	}
	bin := strings.TrimSpace(os.Getenv("GROK_BIN"))
	if bin == "" {
		bin = "grok"
	}
	auth := strings.TrimSpace(os.Getenv("GROK_AUTH_PATH"))
	if auth == "" {
		auth = filepath.Join(home, ".grok", "auth.json")
	}
	billing := strings.TrimSpace(os.Getenv("GROK_BILLING_URL"))
	if billing == "" {
		billing = defaultBilling
	}
	userURL := strings.TrimSpace(os.Getenv("GROK_USER_URL"))
	if userURL == "" {
		userURL = defaultUserURL
	}
	return Config{
		TelegramToken: token,
		DiscordToken:  discordToken,
		TelegramUsers: allowed,
		DiscordUsers:  discordUsers,
		DataDir:       dataDir,
		DefaultModel:  model,
		GrokBin:       bin,
		AuthPath:      auth,
		BillingURL:    billing,
		UserURL:       userURL,
		GrokTimeout:   timeout,
		QueueLimit:    queueLimit,
	}, nil
}

func parseUsers(raw, envName, platformName string) (map[string]struct{}, error) {
	out := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		id := strings.TrimSpace(part)
		if id == "" {
			continue
		}
		if !digits(id) {
			return nil, fmt.Errorf("%s contains %q, which is not a numeric %s user id", envName, id, platformName)
		}
		out[id] = struct{}{}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s is required", envName)
	}
	return out, nil
}

func digits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
