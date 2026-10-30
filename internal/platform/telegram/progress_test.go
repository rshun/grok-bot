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

type progressCall struct {
	method string
	form   map[string]string
}

// progressBot fakes the Bot API. respond may return a non-empty body per call.
func progressBot(t *testing.T, calls *[]progressCall, respond func(method string, n int) string) *Bot {
	t.Helper()
	api := &tgbotapi.BotAPI{Token: "your_token_here", Client: receiptHTTPClient{
		do: func(req *http.Request) (*http.Response, error) {
			if err := req.ParseForm(); err != nil {
				t.Fatal(err)
			}
			form := map[string]string{}
			for k := range req.Form {
				form[k] = req.Form.Get(k)
			}
			method := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
			*calls = append(*calls, progressCall{method: method, form: form})
			body := respond(method, len(*calls))
			if body == "" {
				body = `{"ok":true,"result":{"message_id":20}}`
			}
			status := http.StatusOK
			if strings.Contains(body, `"ok":false`) {
				status = http.StatusBadRequest
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
		},
	}}
	api.SetAPIEndpoint("https://example.invalid/bot%s/%s")
	return &Bot{api: api}
}

func replyMessageID(t *testing.T, call progressCall) int {
	t.Helper()
	params := call.form["reply_parameters"]
	if params == "" {
		return 0
	}
	var reply struct {
		MessageID    int  `json:"message_id"`
		AllowMissing bool `json:"allow_sending_without_reply"`
	}
	if err := json.Unmarshal([]byte(params), &reply); err != nil {
		t.Fatal(err)
	}
	if !reply.AllowMissing {
		t.Fatal("a deleted original message would prevent the reply")
	}
	return reply.MessageID
}

func startProgress(t *testing.T, b *Bot) platform.ProgressHandle {
	t.Helper()
	var replier platform.Replier = &messageReply{Bot: b, messageID: 17}
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

func TestProgressStartEditsAndFinishes(t *testing.T) {
	var calls []progressCall
	b := progressBot(t, &calls, func(string, int) string { return "" })
	h := startProgress(t, b)
	ctx := context.Background()

	if len(calls) != 1 || calls[0].method != "sendMessage" {
		t.Fatalf("calls=%v", calls)
	}
	if c := calls[0]; c.form["chat_id"] != "9" || c.form["text"] != "处理中…" || c.form["disable_web_page_preview"] != "true" || replyMessageID(t, c) != 17 {
		t.Fatalf("unexpected start request: %v", c.form)
	}

	if err := h.Update(ctx, "仍在处理"); err != nil {
		t.Fatal(err)
	}
	if c := calls[1]; c.method != "editMessageText" || c.form["chat_id"] != "9" || c.form["message_id"] != "20" || c.form["text"] != "仍在处理" {
		t.Fatalf("unexpected update request: %v %v", c.method, c.form)
	}

	long := strings.Repeat("x", chunkLimit) + strings.Repeat("y", chunkLimit)
	if err := h.Finish(ctx, long); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 4 {
		t.Fatalf("calls=%v", calls)
	}
	edit, send := calls[2], calls[3]
	if edit.method != "editMessageText" || edit.form["message_id"] != "20" || edit.form["text"] != strings.Repeat("x", chunkLimit) || edit.form["disable_web_page_preview"] != "true" {
		t.Fatalf("first chunk was not edited into the progress message: %v %v", edit.method, edit.form["message_id"])
	}
	if send.method != "sendMessage" || send.form["text"] != strings.Repeat("y", chunkLimit) || replyMessageID(t, send) != 17 {
		t.Fatalf("remaining chunk was not sent as a quoted reply: %v", send.method)
	}
}

func TestProgressNotModifiedIsSuccess(t *testing.T) {
	var calls []progressCall
	b := progressBot(t, &calls, func(method string, _ int) string {
		if method == "editMessageText" {
			return `{"ok":false,"error_code":400,"description":"Bad Request: message is not modified"}`
		}
		return ""
	})
	h := startProgress(t, b)
	ctx := context.Background()
	if err := h.Update(ctx, "处理中…"); err != nil {
		t.Fatalf("not modified should be success: %v", err)
	}
	if err := h.Finish(ctx, "处理中…"); err != nil {
		t.Fatalf("not modified should be success on finish: %v", err)
	}
	if len(calls) != 3 {
		t.Fatalf("finish must not resend after not modified: %v", calls)
	}
}

func TestFinishFallsBackWhenEditFails(t *testing.T) {
	var calls []progressCall
	b := progressBot(t, &calls, func(method string, _ int) string {
		if method == "editMessageText" {
			return `{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`
		}
		return ""
	})
	h := startProgress(t, b)
	text := strings.Repeat("x", chunkLimit) + strings.Repeat("y", chunkLimit)
	if err := h.Finish(context.Background(), text); err != nil {
		t.Fatalf("fallback should succeed: %v", err)
	}
	var sent []progressCall
	for _, c := range calls[1:] {
		if c.method == "sendMessage" {
			sent = append(sent, c)
		}
	}
	if len(sent) != 2 || sent[0].form["text"] != strings.Repeat("x", chunkLimit) || sent[1].form["text"] != strings.Repeat("y", chunkLimit) {
		t.Fatalf("all chunks should be sent as new messages: %v", calls)
	}
	for _, c := range sent {
		if replyMessageID(t, c) != 17 {
			t.Fatal("fallback message lost its quote")
		}
	}
}

func TestFinishReportsErrorWhenSendFailsToo(t *testing.T) {
	var calls []progressCall
	b := progressBot(t, &calls, func(method string, n int) string {
		if n > 1 {
			return `{"ok":false,"error_code":400,"description":"Bad Request: boom"}`
		}
		return ""
	})
	h := startProgress(t, b)
	if err := h.Finish(context.Background(), "done"); err == nil {
		t.Fatal("expected an error when both edit and send fail")
	}
}

func TestProgressDelete(t *testing.T) {
	var calls []progressCall
	b := progressBot(t, &calls, func(method string, _ int) string {
		if method == "deleteMessage" {
			return `{"ok":true,"result":true}`
		}
		return ""
	})
	h := startProgress(t, b)
	if err := h.Delete(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := calls[1]; c.method != "deleteMessage" || c.form["chat_id"] != "9" || c.form["message_id"] != "20" {
		t.Fatalf("unexpected delete request: %v %v", c.method, c.form)
	}
}
