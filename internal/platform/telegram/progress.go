package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/rshun/grok-bot/internal/platform"
)

// StartProgress sends a quoted progress message that Update, Finish and Delete later change.
func (r *messageReply) StartProgress(ctx context.Context, in platform.Inbound, text string) (platform.ProgressHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, err := r.Bot.sendOne(in.ChatID, firstChunk(text), r.messageID)
	if err != nil {
		return nil, err
	}
	return &progressMessage{bot: r.Bot, chatID: in.ChatID, replyTo: r.messageID, messageID: id}, nil
}

type progressMessage struct {
	bot       *Bot
	chatID    string
	replyTo   int
	messageID int
}

func (p *progressMessage) Update(ctx context.Context, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.edit(firstChunk(text))
}

func (p *progressMessage) Finish(ctx context.Context, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	chunks := platform.Split(text, chunkLimit)
	if err := p.edit(chunks[0]); err != nil {
		// The progress message cannot be edited, so deliver everything as new replies.
		if sendErr := p.bot.send(ctx, p.chatID, text, p.replyTo); sendErr != nil {
			return sendErr
		}
		// Best effort: do not leave a stale progress message behind.
		_ = p.delete()
		return nil
	}
	for _, chunk := range chunks[1:] {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := p.bot.sendOne(p.chatID, chunk, p.replyTo); err != nil {
			return err
		}
	}
	return nil
}

func (p *progressMessage) Delete(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.delete()
}

func (p *progressMessage) edit(text string) error {
	_, err := p.bot.api.MakeRequest("editMessageText", tgbotapi.Params{
		"chat_id":                  p.chatID,
		"message_id":               strconv.Itoa(p.messageID),
		"text":                     formatHTML(text),
		"parse_mode":               "HTML",
		"disable_web_page_preview": "true",
	})
	if err != nil && strings.Contains(err.Error(), "can't parse entities") {
		_, err = p.bot.api.MakeRequest("editMessageText", tgbotapi.Params{
			"chat_id":                  p.chatID,
			"message_id":               strconv.Itoa(p.messageID),
			"text":                     text,
			"disable_web_page_preview": "true",
		})
	}
	if err != nil && strings.Contains(err.Error(), "message is not modified") {
		return nil
	}
	return err
}

func (p *progressMessage) delete() error {
	_, err := p.bot.api.MakeRequest("deleteMessage", tgbotapi.Params{
		"chat_id":    p.chatID,
		"message_id": strconv.Itoa(p.messageID),
	})
	return err
}

// sendOne sends a single message and returns its ID.
func (b *Bot) sendOne(chatID, text string, replyTo int) (int, error) {
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("chat id: %w", err)
	}
	params := tgbotapi.Params{
		"chat_id":                  strconv.FormatInt(id, 10),
		"text":                     formatHTML(text),
		"parse_mode":               "HTML",
		"disable_web_page_preview": "true",
	}
	if replyTo > 0 {
		params["reply_parameters"] = fmt.Sprintf(`{"message_id":%d,"allow_sending_without_reply":true}`, replyTo)
	}
	resp, err := b.api.MakeRequest("sendMessage", params)
	if err != nil && strings.Contains(err.Error(), "can't parse entities") {
		delete(params, "parse_mode")
		params["text"] = text
		resp, err = b.api.MakeRequest("sendMessage", params)
	}
	if err != nil {
		return 0, err
	}
	var sent struct {
		MessageID int `json:"message_id"`
	}
	if err := json.Unmarshal(resp.Result, &sent); err != nil || sent.MessageID == 0 {
		return 0, fmt.Errorf("sendMessage returned no message id")
	}
	return sent.MessageID, nil
}

func firstChunk(text string) string {
	return platform.Split(text, chunkLimit)[0]
}
