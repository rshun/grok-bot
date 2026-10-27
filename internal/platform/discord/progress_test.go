package discord

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/rshun/grok-bot/internal/platform"
)

type progressCall struct {
	method string
	path   string
	body   struct {
		Content         string                            `json:"content"`
		Flags           int                               `json:"flags"`
		Reference       *discordgo.MessageReference       `json:"message_reference"`
		AllowedMentions *discordgo.MessageAllowedMentions `json:"allowed_mentions"`
	}
}

func progressBot(t *testing.T, calls *[]progressCall, status func(c progressCall) int) *Bot {
	t.Helper()
	s, err := discordgo.New("Bot your_token_here")
	if err != nil {
		t.Fatal(err)
	}
	s.Client = &http.Client{Transport: receiptTransport(func(req *http.Request) (*http.Response, error) {
		c := progressCall{method: req.Method, path: req.URL.Path}
		if req.Body != nil {
			raw, _ := io.ReadAll(req.Body)
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &c.body); err != nil {
					t.Fatal(err)
				}
			}
		}
		*calls = append(*calls, c)
		code := status(c)
		body := `{"id":"20","channel_id":"9"}`
		switch {
		case code == http.StatusNoContent:
			body = ""
		case code >= 400:
			body = `{"message":"synthetic failure","code":0}`
		}
		return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	return &Bot{session: s, userID: "bot"}
}

func startProgress(t *testing.T, b *Bot) platform.ProgressHandle {
	t.Helper()
	var replier platform.Replier = &messageReply{Bot: b, messageID: "17"}
	p, ok := replier.(platform.Progress)
	if !ok {
		t.Fatal("messageReply does not implement platform.Progress")
	}
	h, err := p.StartProgress(context.Background(), platform.Inbound{ChatID: "9", MessageID: "17"}, "处理中…")
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func checkQuietQuote(t *testing.T, c progressCall) {
	t.Helper()
	if c.body.Reference == nil || c.body.Reference.MessageID != "17" || c.body.Reference.ChannelID != "9" || c.body.Reference.FailIfNotExists == nil || *c.body.Reference.FailIfNotExists {
		t.Fatalf("message lost its quote: %+v", c.body.Reference)
	}
	checkQuiet(t, c)
}

func checkQuiet(t *testing.T, c progressCall) {
	t.Helper()
	if m := c.body.AllowedMentions; m == nil || m.RepliedUser || len(m.Parse) != 0 {
		t.Fatal("message enabled unwanted mentions")
	}
	if c.body.Flags&int(discordgo.MessageFlagsSuppressEmbeds) == 0 {
		t.Fatal("message did not suppress embeds")
	}
}

func TestProgressStartEditsAndFinishes(t *testing.T) {
	var calls []progressCall
	b := progressBot(t, &calls, func(progressCall) int { return http.StatusOK })
	h := startProgress(t, b)
	ctx := context.Background()

	if len(calls) != 1 || calls[0].method != http.MethodPost || !strings.HasSuffix(calls[0].path, "/channels/9/messages") || calls[0].body.Content != "处理中…" {
		t.Fatalf("unexpected start request: %+v", calls)
	}
	checkQuietQuote(t, calls[0])

	if err := h.Update(ctx, "仍在处理"); err != nil {
		t.Fatal(err)
	}
	if c := calls[1]; c.method != http.MethodPatch || !strings.HasSuffix(c.path, "/channels/9/messages/20") || c.body.Content != "仍在处理" {
		t.Fatalf("unexpected update request: %+v", c)
	}
	checkQuiet(t, calls[1])

	long := strings.Repeat("x", chunkLimit) + strings.Repeat("y", chunkLimit)
	if err := h.Finish(ctx, long); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 4 {
		t.Fatalf("calls=%+v", calls)
	}
	edit, send := calls[2], calls[3]
	if edit.method != http.MethodPatch || !strings.HasSuffix(edit.path, "/channels/9/messages/20") || edit.body.Content != strings.Repeat("x", chunkLimit) {
		t.Fatalf("first chunk was not edited into the progress message: %+v", edit)
	}
	checkQuiet(t, edit)
	if send.method != http.MethodPost || !strings.HasSuffix(send.path, "/channels/9/messages") || send.body.Content != strings.Repeat("y", chunkLimit) {
		t.Fatalf("remaining chunk was not sent as a new message: %+v", send)
	}
	checkQuietQuote(t, send)
}

func TestFinishFallsBackWhenEditFails(t *testing.T) {
	var calls []progressCall
	b := progressBot(t, &calls, func(c progressCall) int {
		if c.method == http.MethodPatch {
			return http.StatusBadRequest
		}
		return http.StatusOK
	})
	h := startProgress(t, b)
	text := strings.Repeat("x", chunkLimit) + strings.Repeat("y", chunkLimit)
	if err := h.Finish(context.Background(), text); err != nil {
		t.Fatalf("fallback should succeed: %v", err)
	}
	var sent []progressCall
	for _, c := range calls[1:] {
		if c.method == http.MethodPost {
			sent = append(sent, c)
		}
	}
	if len(sent) != 2 || sent[0].body.Content != strings.Repeat("x", chunkLimit) || sent[1].body.Content != strings.Repeat("y", chunkLimit) {
		t.Fatalf("all chunks should be sent as new messages: %+v", calls)
	}
	for _, c := range sent {
		checkQuietQuote(t, c)
	}
}

func TestFinishReportsErrorWhenSendFailsToo(t *testing.T) {
	var calls []progressCall
	b := progressBot(t, &calls, func(progressCall) int {
		if len(calls) > 1 {
			return http.StatusBadRequest
		}
		return http.StatusOK
	})
	h := startProgress(t, b)
	if err := h.Finish(context.Background(), "done"); err == nil {
		t.Fatal("expected an error when both edit and send fail")
	}
}

func TestProgressDelete(t *testing.T) {
	var calls []progressCall
	b := progressBot(t, &calls, func(c progressCall) int {
		if c.method == http.MethodDelete {
			return http.StatusNoContent
		}
		return http.StatusOK
	})
	h := startProgress(t, b)
	if err := h.Delete(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := calls[1]; c.method != http.MethodDelete || !strings.HasSuffix(c.path, "/channels/9/messages/20") {
		t.Fatalf("unexpected delete request: %+v", c)
	}
}

func TestInteractionReplyHasNoProgress(t *testing.T) {
	var replier platform.Replier = &interactionReply{}
	if _, ok := replier.(platform.Progress); ok {
		t.Fatal("ephemeral interaction replies cannot be edited as progress")
	}
}
