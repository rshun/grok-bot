package telegram

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/rshun/grok-bot/internal/platform"
)

func TestBuildInlineMarkupAndCallbackHandling(t *testing.T) {
	rows := [][]platform.Button{
		{
			{Label: "✓ grok-4.7", Command: "/model grok-4.7", Style: platform.ButtonSuccess},
			{Label: "grok-4.6", Command: "/model grok-4.6", Style: platform.ButtonSecondary},
			{Label: "too-long", Command: "/" + strings.Repeat("x", 70)}, // skipped (>64 bytes)
		},
	}
	markup := buildInlineMarkup(rows)
	if markup == "" {
		t.Fatal("expected non-empty inline_keyboard JSON")
	}
	var parsed tgInlineMarkup
	if err := json.Unmarshal([]byte(markup), &parsed); err != nil {
		t.Fatalf("unmarshal markup: %v", err)
	}
	if len(parsed.InlineKeyboard) != 1 || len(parsed.InlineKeyboard[0]) != 2 {
		t.Fatalf("unexpected keyboard layout: %#v", parsed.InlineKeyboard)
	}
	if parsed.InlineKeyboard[0][0].CallbackData != "/model grok-4.7" {
		t.Fatalf("unexpected callback_data: %#v", parsed.InlineKeyboard[0][0])
	}

	var calls []progressCall
	bot := progressBot(t, &calls, func(method string, _ int) string {
		if method == "answerCallbackQuery" {
			return `{"ok":true,"result":true}`
		}
		return ""
	})

	reply := &messageReply{Bot: bot, messageID: 55}
	if err := reply.SendWithButtons(context.Background(), "1001", "选择模型", rows); err != nil {
		t.Fatalf("SendWithButtons: %v", err)
	}
	if len(calls) != 1 || calls[0].method != "sendMessage" || calls[0].form["reply_markup"] != markup {
		t.Fatalf("unexpected sendMessage calls: %#v", calls)
	}

	var gotInbound platform.Inbound
	bot.handleCallback(context.Background(), &tgbotapi.CallbackQuery{
		ID:   "cb-1",
		From: &tgbotapi.User{ID: 42},
		Message: &tgbotapi.Message{
			MessageID: 88,
			Chat:      &tgbotapi.Chat{ID: 1001, Type: "private"},
		},
		Data: "/model grok-4.6",
	}, func(_ context.Context, in platform.Inbound, _ platform.Replier) {
		gotInbound = in
	})

	if len(calls) != 2 || calls[1].method != "answerCallbackQuery" || calls[1].form["callback_query_id"] != "cb-1" {
		t.Fatalf("expected answerCallbackQuery call, got %#v", calls)
	}
	if gotInbound.Platform != platform.Telegram || gotInbound.ChatID != "1001" || gotInbound.UserID != "42" || gotInbound.Text != "/model grok-4.6" || !gotInbound.TextMessage {
		t.Fatalf("unexpected callback inbound: %#v", gotInbound)
	}
}
