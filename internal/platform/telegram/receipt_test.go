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

func TestMessageReplyKeepsOriginalAcrossChunks(t *testing.T) {
	var references []int
	api := &tgbotapi.BotAPI{Token: "your_token_here", Client: receiptHTTPClient{
		do: func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/sendMessage") {
				t.Fatal("unexpected reply request")
			}
			if err := req.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if req.Form.Get("chat_id") != "9" || req.Form.Get("disable_web_page_preview") != "true" {
				t.Fatal("reply lost its chat or preview setting")
			}
			var reply struct {
				MessageID    int  `json:"message_id"`
				AllowMissing bool `json:"allow_sending_without_reply"`
			}
			if params := req.Form.Get("reply_parameters"); params != "" {
				if err := json.Unmarshal([]byte(params), &reply); err != nil {
					t.Fatal(err)
				}
				if !reply.AllowMissing {
					t.Fatal("a deleted original message would prevent the reply")
				}
			}
			references = append(references, reply.MessageID)
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":20}}`))}, nil
		},
	}}
	api.SetAPIEndpoint("https://example.invalid/bot%s/%s")
	b := &Bot{api: api}
	first := &messageReply{Bot: b, messageID: 17}
	second := &messageReply{Bot: b, messageID: 18}
	var _ platform.Receipt = first
	ctx := context.Background()
	if err := first.Send(ctx, "9", strings.Repeat("x", chunkLimit+1)); err != nil {
		t.Fatal(err)
	}
	if err := second.Send(ctx, "9", "second result"); err != nil {
		t.Fatal(err)
	}
	if err := first.Send(ctx, "9", "first follow-up"); err != nil {
		t.Fatal(err)
	}
	if err := b.Send(ctx, "9", "unreferenced message"); err != nil {
		t.Fatal(err)
	}
	want := []int{17, 17, 18, 17, 0}
	if len(references) != len(want) {
		t.Fatalf("references=%v", references)
	}
	for i := range want {
		if references[i] != want[i] {
			t.Fatalf("references=%v, want %v", references, want)
		}
	}
}
