package discord

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/rshun/grok-bot/internal/actions"
	"github.com/rshun/grok-bot/internal/platform"
)

func TestDiscordReactAddsUnicode(t *testing.T) {
	s, err := discordgo.New("Bot your_token_here")
	if err != nil {
		t.Fatal(err)
	}
	var methods, paths, raw []string
	s.Client = &http.Client{Transport: receiptTransport(func(req *http.Request) (*http.Response, error) {
		methods = append(methods, req.Method)
		paths = append(paths, req.URL.Path)
		raw = append(raw, req.URL.EscapedPath())
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	b := &Bot{session: s}
	in := platform.Inbound{ChatID: "9", MessageID: "17"}
	ctx := context.Background()
	for _, emoji := range []string{"👍", " ❤️ ", "#️⃣"} {
		if err := b.React(ctx, in, emoji); err != nil {
			t.Fatalf("%q: %v", emoji, err)
		}
	}
	if len(methods) != 3 {
		t.Fatalf("requests: %v", paths)
	}
	for _, m := range methods {
		if m != http.MethodPut {
			t.Fatalf("method %s", m)
		}
	}
	if !strings.HasSuffix(paths[0], "/channels/9/messages/17/reactions/👍/@me") {
		t.Fatalf("path %s", paths[0])
	}
	if !strings.HasSuffix(raw[0], "/reactions/%F0%9F%91%8D/@me") {
		t.Fatalf("escaped path %s", raw[0])
	}
	if !strings.HasSuffix(paths[1], "/reactions/❤️/@me") {
		t.Fatalf("path %s", paths[1])
	}
	// The keycap's # must not turn into a URL fragment.
	if !strings.HasSuffix(paths[2], "/reactions/#️⃣/@me") {
		t.Fatalf("path %s", paths[2])
	}
	var _ platform.Reactor = &messageReply{Bot: b}
}

func TestDiscordReactRefusesUnusableEmoji(t *testing.T) {
	s, err := discordgo.New("Bot your_token_here")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.Client = &http.Client{Transport: receiptTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	b := &Bot{session: s}
	in := platform.Inbound{ChatID: "9", MessageID: "17"}
	for _, bad := range []string{"", "  ", "<:blob:123456789>", "<a:blob:123456789>", "blob:123456789", "thumbsup", "a/b", strings.Repeat("👍", 9)} {
		err := b.React(context.Background(), in, bad)
		var toolErr *actions.ToolError
		if !errors.As(err, &toolErr) || toolErr.Msg == "" {
			t.Fatalf("%q: want *actions.ToolError, got %v", bad, err)
		}
	}
	if calls != 0 {
		t.Fatalf("a refused emoji reached Discord: %d calls", calls)
	}
}

func TestDiscordReactErrorsStayPlain(t *testing.T) {
	s, err := discordgo.New("Bot your_token_here")
	if err != nil {
		t.Fatal(err)
	}
	s.Client = &http.Client{Transport: receiptTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"message":"Missing Access","code":50001}`))}, nil
	})}
	b := &Bot{session: s}
	err = b.React(context.Background(), platform.Inbound{ChatID: "9", MessageID: "17"}, "👍")
	var toolErr *actions.ToolError
	if err == nil || errors.As(err, &toolErr) {
		t.Fatalf("a platform failure must not be shown to Grok: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.React(ctx, platform.Inbound{ChatID: "9", MessageID: "17"}, "👍"); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
