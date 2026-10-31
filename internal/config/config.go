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
	GrokMaxTurns  int
	QueueLimit    int
	WebSearch     bool
	AllowShell    bool
	ShowProgress  bool
	EnvFile       string
	// Location is the time zone for scheduled tasks.
	Location        *time.Location
	TaskLimit       int
	TaskMinInterval time.Duration
	Version         string
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
	timeout := 10 * time.Minute
	if raw := strings.TrimSpace(os.Getenv("GROK_TIMEOUT")); raw != "" {
		timeout, err = time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("GROK_TIMEOUT: %w", err)
		}
	}
	maxTurns := 50
	if raw := strings.TrimSpace(os.Getenv("GROK_MAX_TURNS")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return Config{}, fmt.Errorf("GROK_MAX_TURNS must be a non-negative integer")
		}
		if n == 0 {
			maxTurns = -1
		} else {
			maxTurns = n
		}
	}
	queueLimit := 8
	if raw := strings.TrimSpace(os.Getenv("QUEUE_LIMIT")); raw != "" {
		queueLimit, err = strconv.Atoi(raw)
		if err != nil || queueLimit < 1 {
			return Config{}, fmt.Errorf("QUEUE_LIMIT must be a positive integer")
		}
	}
	webSearch := true
	if raw := strings.TrimSpace(os.Getenv("WEB_SEARCH")); raw != "" {
		webSearch, err = strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("WEB_SEARCH must be true or false")
		}
	}
	allowShell := false
	if raw := strings.TrimSpace(os.Getenv("ALLOW_SHELL")); raw != "" {
		allowShell, err = strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("ALLOW_SHELL must be true or false")
		}
	}
	showProgress := false
	if raw := strings.TrimSpace(os.Getenv("SHOW_PROGRESS")); raw != "" {
		showProgress, err = strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("SHOW_PROGRESS must be true or false")
		}
	}
	envFile := strings.TrimSpace(os.Getenv("BOT_ENV_FILE"))
	if envFile == "" {
		envFile = "/etc/grok-tg-bot.env"
	}
	location := time.Local
	if raw := strings.TrimSpace(os.Getenv("TIMEZONE")); raw != "" {
		location, err = time.LoadLocation(raw)
		if err != nil {
			return Config{}, fmt.Errorf("TIMEZONE: %w", err)
		}
	}
	taskLimit := 20
	if raw := strings.TrimSpace(os.Getenv("TASK_LIMIT")); raw != "" {
		taskLimit, err = strconv.Atoi(raw)
		if err != nil || taskLimit < 1 {
			return Config{}, fmt.Errorf("TASK_LIMIT must be a positive integer")
		}
	}
	minInterval := time.Hour
	if raw := strings.TrimSpace(os.Getenv("TASK_MIN_INTERVAL")); raw != "" {
		minInterval, err = time.ParseDuration(raw)
		if err != nil || minInterval <= 0 {
			return Config{}, fmt.Errorf("TASK_MIN_INTERVAL must be a positive duration such as 60m")
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
		TelegramToken:   token,
		DiscordToken:    discordToken,
		TelegramUsers:   allowed,
		DiscordUsers:    discordUsers,
		DataDir:         dataDir,
		DefaultModel:    model,
		GrokBin:         bin,
		AuthPath:        auth,
		BillingURL:      billing,
		UserURL:         userURL,
		GrokTimeout:     timeout,
		GrokMaxTurns:    maxTurns,
		QueueLimit:      queueLimit,
		WebSearch:       webSearch,
		AllowShell:      allowShell,
		ShowProgress:    showProgress,
		EnvFile:         envFile,
		Location:        location,
		TaskLimit:       taskLimit,
		TaskMinInterval: minInterval,
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

// LoadEnvFile populates unset environment variables from the first readable KEY=VALUE file.
func LoadEnvFile(paths ...string) {
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, raw := range strings.Split(string(data), "\n") {
			line := strings.TrimSpace(raw)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			k = strings.TrimSpace(k)
			v = strings.Trim(strings.TrimSpace(v), `"'`)
			if k != "" && os.Getenv(k) == "" {
				_ = os.Setenv(k, v)
			}
		}
		return
	}
}
