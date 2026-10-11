package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/rshun/grok-bot/internal/platform"
)

type receiptHTTPClient struct {
	do func(*http.Request) (*http.Response, error)
}

func (c receiptHTTPClient) Do(req *http.Request) (*http.Response, error) {
	return c.do(req)
}

func TestMarkReceivedUsesOriginalMessageReaction(t *testing.T) {
	calls := 0
	api := &tgbotapi.BotAPI{Token: "your_token_here", Client: receiptHTTPClient{
		do: func(req *http.Request) (*http.Response, error) {
			calls++
			if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/setMessageReaction") {
				t.Fatal("unexpected receipt request")
			}
			if err := req.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if req.Form.Get("chat_id") != "9" || req.Form.Get("message_id") != "17" || req.Form.Get("is_big") != "false" {
				t.Fatal("receipt did not target the original message")
			}
			var reaction []struct{ Type, Emoji string }
			if err := json.Unmarshal([]byte(req.Form.Get("reaction")), &reaction); err != nil {
				t.Fatal(err)
			}
			if len(reaction) != 1 || reaction[0].Type != "emoji" || reaction[0].Emoji != "👀" {
				t.Fatalf("unexpected reaction: %#v", reaction)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`))}, nil
		},
	}}
	api.SetAPIEndpoint("https://example.invalid/bot%s/%s")
	b := &Bot{api: api}
	in := platform.Inbound{ChatID: "9", MessageID: "17"}
	if err := b.MarkReceived(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.MarkReceived(ctx, in); err != context.Canceled {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if err := b.MarkReceived(context.Background(), platform.Inbound{ChatID: "9"}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("unexpected request count: %d", calls)
	}
}
