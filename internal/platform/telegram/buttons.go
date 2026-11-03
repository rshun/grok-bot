package telegram

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/rshun/grok-bot/internal/platform"
)

const maxTelegramCallbackBytes = 64

type tgInlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

type tgInlineMarkup struct {
	InlineKeyboard [][]tgInlineButton `json:"inline_keyboard"`
}

func buildInlineMarkup(rows [][]platform.Button) string {
	var kb [][]tgInlineButton
	for _, row := range rows {
		var outRow []tgInlineButton
		for _, btn := range row {
			label := strings.TrimSpace(btn.Label)
			cmd := strings.TrimSpace(btn.Command)
			if label == "" || cmd == "" || len(cmd) > maxTelegramCallbackBytes {
				continue
			}
			outRow = append(outRow, tgInlineButton{
				Text:         label,
				CallbackData: cmd,
			})
			if len(outRow) >= 8 {
				break
			}
		}
		if len(outRow) > 0 {
			kb = append(kb, outRow)
		}
	}
	if len(kb) == 0 {
		return ""
	}
	raw, err := json.Marshal(tgInlineMarkup{InlineKeyboard: kb})
	if err != nil {
		return ""
	}
	return string(raw)
}

func (b *Bot) SendWithButtons(ctx context.Context, chatID, text string, rows [][]platform.Button) error {
	return b.sendWithButtons(ctx, chatID, text, 0, rows)
}

func (r *messageReply) SendWithButtons(ctx context.Context, chatID, text string, rows [][]platform.Button) error {
	return r.Bot.sendWithButtons(ctx, chatID, text, r.messageID, rows)
}

func (b *Bot) sendWithButtons(ctx context.Context, chatID, text string, messageID int, rows [][]platform.Button) error {
	markup := buildInlineMarkup(rows)
	if markup == "" {
		return b.send(ctx, chatID, text, messageID)
	}
	chunks := platform.Split(text, chunkLimit)
	for i, chunk := range chunks {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rowMarkup := ""
		if i == len(chunks)-1 {
			rowMarkup = markup
		}
		if _, err := b.sendOneMarkup(chatID, chunk, messageID, rowMarkup); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bot) handleCallback(ctx context.Context, cb *tgbotapi.CallbackQuery, handler func(context.Context, platform.Inbound, platform.Replier)) {
	if cb == nil || cb.From == nil || cb.Message == nil || cb.Message.Chat == nil {
		return
	}
	if cb.Message.Chat.Type != "private" {
		return
	}
	// Stop the client's loading spinner right away.
	if cb.ID != "" {
		_, _ = b.api.MakeRequest("answerCallbackQuery", tgbotapi.Params{
			"callback_query_id": cb.ID,
		})
	}
	data := strings.TrimSpace(cb.Data)
	if data == "" || handler == nil {
		return
	}
	in := platform.Inbound{
		Platform:    platform.Telegram,
		ChatID:      strconv.FormatInt(cb.Message.Chat.ID, 10),
		MessageID:   strconv.Itoa(cb.Message.MessageID),
		UserID:      strconv.FormatInt(cb.From.ID, 10),
		Text:        data,
		TextMessage: true,
	}
	handler(ctx, in, &messageReply{Bot: b, messageID: cb.Message.MessageID})
}
