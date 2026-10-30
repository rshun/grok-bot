package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/rshun/grok-bot/internal/actions"
	"github.com/rshun/grok-bot/internal/platform"
)

func reactBot(t *testing.T, requests *[]map[string]string) *Bot {
	t.Helper()
	api := &tgbotapi.BotAPI{Token: "your_token_here", Client: receiptHTTPClient{
		do: func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/setMessageReaction") {
				t.Fatal("unexpected reaction request")
			}
			if err := req.ParseForm(); err != nil {
				t.Fatal(err)
			}
			*requests = append(*requests, map[string]string{
				"chat_id":    req.Form.Get("chat_id"),
				"message_id": req.Form.Get("message_id"),
				"reaction":   req.Form.Get("reaction"),
			})
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`))}, nil
		},
	}}
	api.SetAPIEndpoint("https://example.invalid/bot%s/%s")
	return &Bot{api: api}
}

func sentEmoji(t *testing.T, raw string) string {
	t.Helper()
	var reaction []struct{ Type, Emoji string }
	if err := json.Unmarshal([]byte(raw), &reaction); err != nil {
		t.Fatal(err)
	}
	if len(reaction) != 1 || reaction[0].Type != "emoji" {
		t.Fatalf("unexpected reaction: %#v", reaction)
	}
	return reaction[0].Emoji
}

func TestTelegramReactionNormalizes(t *testing.T) {
	var requests []map[string]string
	b := reactBot(t, &requests)
	in := platform.Inbound{ChatID: "9", MessageID: "17"}
	ctx := context.Background()

	if err := b.React(ctx, in, "👍"); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0]["chat_id"] != "9" || requests[0]["message_id"] != "17" || sentEmoji(t, requests[0]["reaction"]) != "👍" {
		t.Fatalf("requests=%v", requests)
	}

	// A variation selector is dropped, so the heart goes out in its allowed form.
	if err := b.React(ctx, in, " ❤️ "); err != nil {
		t.Fatal(err)
	}
	if got := sentEmoji(t, requests[1]["reaction"]); got != "❤" {
		t.Fatalf("sent %q", got)
	}
	// Joined emoji keep their joiner.
	if err := b.React(ctx, in, "❤️‍🔥"); err != nil {
		t.Fatal(err)
	}
	if got := sentEmoji(t, requests[2]["reaction"]); got != "❤‍🔥" {
		t.Fatalf("sent %q", got)
	}

	// Skin-tone variants, text and oversize input are refused with a message Grok may see.
	for _, bad := range []string{"👍🏻", "", "ok", "🦊", strings.Repeat("👍", 10)} {
		err := b.React(ctx, in, bad)
		var toolErr *actions.ToolError
		if !errors.As(err, &toolErr) {
			t.Fatalf("%q: want *actions.ToolError, got %v", bad, err)
		}
		if !strings.Contains(toolErr.Msg, "👍") || !strings.Contains(toolErr.Msg, "😡") {
			t.Fatalf("%q: message lacks the allowed set: %s", bad, toolErr.Msg)
		}
	}
	if len(requests) != 3 {
		t.Fatalf("a refused emoji reached Telegram: %v", requests)
	}
}

func TestTelegramReactionAllowedSetIsComplete(t *testing.T) {
	if len(allowedReactions) != 73 {
		t.Fatalf("got %d allowed reactions", len(allowedReactions))
	}
	for _, e := range []string{"❤‍🔥", "👨‍💻", "🤷‍♂", "🤷‍♀", "🕊", "✍", "☃", "⚡"} {
		if _, ok := allowedReactions[e]; !ok {
			t.Errorf("%q missing", e)
		}
	}
}

func TestTelegramReactionErrorsStayPlain(t *testing.T) {
	api := &tgbotapi.BotAPI{Token: "your_token_here", Client: receiptHTTPClient{
		do: func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(`{"ok":false,"error_code":400,"description":"Bad Request: REACTION_INVALID"}`))}, nil
		},
	}}
	api.SetAPIEndpoint("https://example.invalid/bot%s/%s")
	b := &Bot{api: api}
	err := b.React(context.Background(), platform.Inbound{ChatID: "9", MessageID: "17"}, "👍")
	var toolErr *actions.ToolError
	if err == nil || errors.As(err, &toolErr) {
		t.Fatalf("a platform failure must not be shown to Grok: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.React(ctx, platform.Inbound{ChatID: "9", MessageID: "17"}, "👍"); err != context.Canceled {
		t.Fatalf("got %v", err)
	}
	var _ platform.Reactor = &messageReply{Bot: b}
}
