package telegram

import (
	"context"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/rshun/grok-bot/internal/platform"
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
	group := newAlbums(ctx, handler)
	defer group.stop()
	for update := range stream {
		msg := update.Message
		if msg == nil || msg.From == nil {
			continue
		}
		if msg.Chat.Type != "private" {
			continue
		}
		in := b.inbound(msg)
		if msg.MediaGroupID != "" {
			// Album parts carry the caption on one message only; keep it for the merge even
			// when that part is not a picture.
			in.Text = strings.TrimSpace(msg.Caption)
		}
		group.add(in, msg.MediaGroupID, &messageReply{Bot: b, messageID: msg.MessageID})
	}
	return ctx.Err()
}

func (b *Bot) Send(ctx context.Context, chatID, text string) error {
	return b.send(ctx, chatID, text, 0)
}

// messageReply keeps the triggering message with its queued reply.
type messageReply struct {
	*Bot
	messageID int
}

func (r *messageReply) Send(ctx context.Context, chatID, text string) error {
	return r.Bot.send(ctx, chatID, text, r.messageID)
}

func (b *Bot) send(ctx context.Context, chatID, text string, messageID int) error {
	for _, chunk := range platform.Split(text, chunkLimit) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := b.sendOne(chatID, chunk, messageID); err != nil {
			return err
		}
	}
	return nil
}

// MarkReceived uses the same receipt reaction as codex-tg-bot.
func (b *Bot) MarkReceived(ctx context.Context, in platform.Inbound) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if in.MessageID == "" {
		return nil
	}
	_, err := b.api.MakeRequest("setMessageReaction", tgbotapi.Params{
		"chat_id":    in.ChatID,
		"message_id": in.MessageID,
		"reaction":   `[{"type":"emoji","emoji":"👀"}]`,
		"is_big":     "false",
	})
	return err
}
