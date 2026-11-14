package telegram

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

var (
	reBotToken     = regexp.MustCompile(`\bbot\d+:[A-Za-z0-9_-]+`)
	reBareBotToken = regexp.MustCompile(`\b\d{6,}:[A-Za-z0-9_-]{20,}`)
)

func init() {
	_ = tgbotapi.SetLogger(botLogger{})
}

// RedactBotToken masks Telegram bot tokens in URLs and error messages.
func RedactBotToken(text, token string) string {
	if text == "" {
		return ""
	}
	out := text
	if token != "" {
		out = strings.ReplaceAll(out, token, "****")
	}
	out = reBotToken.ReplaceAllString(out, "bot****")
	out = reBareBotToken.ReplaceAllString(out, "****")
	return out
}

func sanitizeErr(err error, token string) error {
	if err == nil {
		return nil
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		cp := *urlErr
		cp.URL = RedactBotToken(cp.URL, token)
		cp.Err = sanitizeErr(cp.Err, token)
		return &cp
	}
	msg := err.Error()
	clean := RedactBotToken(msg, token)
	if clean != msg {
		return errors.New(clean)
	}
	return err
}

type safeHTTPClient struct {
	base  tgbotapi.HTTPClient
	token string
}

func (c *safeHTTPClient) Do(req *http.Request) (*http.Response, error) {
	resp, err := c.base.Do(req)
	if err != nil {
		return nil, sanitizeErr(err, c.token)
	}
	return resp, nil
}

type botLogger struct {
	warn func(msg string, args ...any)
}

func (l botLogger) logText(raw string) {
	clean := strings.TrimSpace(RedactBotToken(raw, ""))
	if clean == "" {
		return
	}
	if l.warn != nil {
		l.warn("telegram", "detail", clean)
		return
	}
	slog.Warn("telegram", "detail", clean)
}

func (l botLogger) Println(v ...any) {
	l.logText(fmt.Sprintln(v...))
}

func (l botLogger) Printf(format string, v ...any) {
	l.logText(fmt.Sprintf(format, v...))
}
