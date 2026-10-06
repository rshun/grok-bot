package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/rshun/grok-tg-bot/internal/platform"
)

const chunkLimit = 4000

// Bot receives private Telegram text messages and sends replies.
type Bot struct {
	api *tgbotapi.BotAPI
}

func New(token string) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, err
	}
	api.Debug = false
	return &Bot{api: api}, nil
}

func (b *Bot) Username() string {
	return b.api.Self.UserName
}

// Run polls until ctx is cancelled. handler should return quickly.
func (b *Bot) Run(ctx context.Context, handler func(context.Context, platform.Inbound, platform.Replier)) error {
	updates := tgbotapi.NewUpdate(0)
	updates.Timeout = 30
	updates.AllowedUpdates = []string{"message"}
	stream := b.api.GetUpdatesChan(updates)
	go func() {
		<-ctx.Done()
		b.api.StopReceivingUpdates()
	}()
	for update := range stream {
		msg := update.Message
		if msg == nil || msg.From == nil {
			continue
		}
		if msg.Chat.Type != "private" {
			continue
		}
		text := strings.TrimSpace(msg.Text)
		media := msg.Photo != nil || msg.Voice != nil || msg.Audio != nil || msg.Video != nil || msg.Document != nil || msg.Sticker != nil || msg.VideoNote != nil
		in := platform.Inbound{
			Platform:    "telegram",
			ChatID:      strconv.FormatInt(msg.Chat.ID, 10),
			UserID:      strconv.FormatInt(msg.From.ID, 10),
			Text:        text,
			TextMessage: text != "" && !media,
		}
		handler(ctx, in, b)
	}
	return ctx.Err()
}

func (b *Bot) Send(ctx context.Context, chatID, text string) error {
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return fmt.Errorf("chat id: %w", err)
	}
	for _, chunk := range split(text, chunkLimit) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		out := tgbotapi.NewMessage(id, chunk)
		out.DisableWebPagePreview = true
		if _, err := b.api.Send(out); err != nil {
			return err
		}
	}
	return nil
}

func split(text string, limit int) []string {
	runes := []rune(text)
	if len(runes) == 0 {
		return []string{" "}
	}
	if limit < 1 {
		limit = chunkLimit
	}
	var parts []string
	for len(runes) > limit {
		cut := limit
		for i := limit; i > limit/2; i-- {
			if runes[i-1] == '\n' {
				cut = i - 1
				break
			}
		}
		if cut < 1 {
			cut = limit
		}
		parts = append(parts, string(runes[:cut]))
		runes = runes[cut:]
		for len(runes) > 0 && runes[0] == '\n' {
			runes = runes[1:]
		}
	}
	if len(runes) > 0 {
		parts = append(parts, string(runes))
	}
	if len(parts) == 0 {
		return []string{text}
	}
	return parts
}
