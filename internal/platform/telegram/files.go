package telegram

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/rshun/grok-bot/internal/platform"
)

const (
	// fileLimit is the largest file a bot can upload.
	fileLimit = 50 << 20
	// photoLimit is the largest picture sendPhoto takes; bigger ones go as documents.
	photoLimit = 10 << 20
)

// FileLimit is the largest file in bytes Telegram accepts from a bot.
func (b *Bot) FileLimit() int64 { return fileLimit }

// SendFile uploads one file. Pictures go as photos and everything else as documents; a photo
// Telegram refuses is sent again as a document, which accepts any bytes.
func (b *Bot) SendFile(ctx context.Context, chatID, replyTo string, f platform.OutFile) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return fmt.Errorf("chat id: %w", err)
	}
	var reply int
	if replyTo != "" {
		if reply, err = strconv.Atoi(replyTo); err != nil {
			return fmt.Errorf("reply id: %w", err)
		}
	}
	body, size, err := f.Open()
	if err != nil {
		return err
	}
	if isPhoto(f.Name) && size <= photoLimit {
		cfg := tgbotapi.NewPhoto(id, tgbotapi.FileReader{Name: f.Name, Reader: body})
		cfg.Caption, cfg.ReplyToMessageID, cfg.AllowSendingWithoutReply = f.Caption, reply, true
		_, err := b.api.Send(cfg)
		body.Close()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// The error is not logged here: it can embed the request URL, which holds the bot token.
		slog.Warn("sendPhoto failed, sending as document", "chat", chatID)
		// The upload consumed the first reader, so open the file again.
		if body, _, err = f.Open(); err != nil {
			return err
		}
	}
	return b.sendDocument(id, reply, f, body)
}

func (b *Bot) sendDocument(chatID int64, reply int, f platform.OutFile, body io.ReadCloser) error {
	defer body.Close()
	cfg := tgbotapi.NewDocument(chatID, tgbotapi.FileReader{Name: f.Name, Reader: body})
	cfg.Caption, cfg.ReplyToMessageID, cfg.AllowSendingWithoutReply = f.Caption, reply, true
	_, err := b.api.Send(cfg)
	return err
}

// isPhoto reports whether the name is a picture type sendPhoto accepts.
func isPhoto(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp":
		return true
	}
	return false
}
