package telegram

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/rshun/grok-bot/internal/platform"
)

func privateMessage(m tgbotapi.Message) *tgbotapi.Message {
	m.MessageID = 7
	m.Chat = &tgbotapi.Chat{ID: 9, Type: "private"}
	m.From = &tgbotapi.User{ID: 1}
	return &m
}

func TestInboundPhotoUsesLargest(t *testing.T) {
	b := &Bot{api: &tgbotapi.BotAPI{Token: "your_token_here"}}
	in := b.inbound(privateMessage(tgbotapi.Message{
		Caption: " what is this ",
		Photo: []tgbotapi.PhotoSize{
			{FileID: "small", Width: 90, Height: 60, FileSize: 1000},
			{FileID: "big", Width: 1280, Height: 960, FileSize: 90000},
			{FileID: "mid", Width: 320, Height: 240, FileSize: 9000},
		},
	}))
	if !in.TextMessage || in.Text != "what is this" || in.Skipped {
		t.Fatalf("%#v", in)
	}
	if len(in.Images) != 1 || in.Images[0].MIME != "image/jpeg" || in.Images[0].Size != 90000 || in.Images[0].Load == nil {
		t.Fatalf("%#v", in.Images)
	}
	if in.ChatID != "9" || in.MessageID != "7" || in.UserID != "1" || in.Platform != platform.Telegram {
		t.Fatalf("%#v", in)
	}
}

func TestInboundImageDocument(t *testing.T) {
	b := &Bot{api: &tgbotapi.BotAPI{Token: "your_token_here"}}
	in := b.inbound(privateMessage(tgbotapi.Message{
		Document: &tgbotapi.Document{FileID: "d", FileName: "a.png", MimeType: "image/png", FileSize: 5},
	}))
	if !in.TextMessage || len(in.Images) != 1 || in.Images[0].MIME != "image/png" || in.Images[0].Name != "a.png" || in.Skipped {
		t.Fatalf("%#v", in)
	}

	in = b.inbound(privateMessage(tgbotapi.Message{
		Caption:        "read this",
		ReplyToMessage: &tgbotapi.Message{Text: "previous alert"},
		Document:       &tgbotapi.Document{FileID: "d", FileName: "a.pdf", MimeType: "application/pdf"},
	}))
	if !in.TextMessage || len(in.Images) != 0 || len(in.Files) != 1 || in.Files[0].Name != "a.pdf" || in.ReplyToText != "previous alert" {
		t.Fatalf("pdf document should be accepted as file attachment: %#v", in)
	}
	// Formats Grok cannot take as vision images become file attachments instead.
	in = b.inbound(privateMessage(tgbotapi.Message{
		Document: &tgbotapi.Document{FileID: "d", FileName: "icon.svg", MimeType: "image/svg+xml"},
	}))
	if !in.TextMessage || len(in.Images) != 0 || len(in.Files) != 1 || in.Files[0].Name != "icon.svg" {
		t.Fatalf("svg should be file attachment: %#v", in)
	}
}

func TestInboundTextAndOtherMedia(t *testing.T) {
	b := &Bot{api: &tgbotapi.BotAPI{Token: "your_token_here"}}
	in := b.inbound(privateMessage(tgbotapi.Message{Text: " hi "}))
	if !in.TextMessage || in.Text != "hi" || len(in.Images) != 0 {
		t.Fatalf("%#v", in)
	}
	if in = b.inbound(privateMessage(tgbotapi.Message{Voice: &tgbotapi.Voice{FileID: "v"}})); in.TextMessage {
		t.Fatalf("voice accepted: %#v", in)
	}
	if in = b.inbound(privateMessage(tgbotapi.Message{Text: "hi", Sticker: &tgbotapi.Sticker{FileID: "s"}})); in.TextMessage {
		t.Fatalf("text with a sticker is not plain text: %#v", in)
	}
}

// downloadBot serves getFile and the file download through fn, which sees the request path.
func downloadBot(fn func(req *http.Request) (*http.Response, error)) *Bot {
	api := &tgbotapi.BotAPI{Token: "your_token_here", Client: receiptHTTPClient{do: fn}}
	api.SetAPIEndpoint("https://example.invalid/bot%s/%s")
	return &Bot{api: api}
}

func httpReply(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
}

func TestLoadUsesGetFile(t *testing.T) {
	var paths []string
	b := downloadBot(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		if strings.HasSuffix(req.URL.Path, "/getFile") {
			if err := req.ParseForm(); err != nil || req.Form.Get("file_id") != "big" {
				t.Fatalf("getFile asked for %q (%v)", req.Form.Get("file_id"), err)
			}
			return httpReply(200, `{"ok":true,"result":{"file_id":"big","file_path":"photos/file_1.jpg"}}`), nil
		}
		if req.Method != http.MethodGet || !strings.HasSuffix(req.URL.Path, "/photos/file_1.jpg") {
			t.Fatalf("unexpected download %s %s", req.Method, req.URL.Path)
		}
		return httpReply(200, "jpeg bytes"), nil
	})
	in := b.inbound(privateMessage(tgbotapi.Message{Photo: []tgbotapi.PhotoSize{{FileID: "big", Width: 5, Height: 5}}}))
	data, err := in.Images[0].Load(context.Background())
	if err != nil || string(data) != "jpeg bytes" {
		t.Fatalf("got %q, %v", data, err)
	}
	if len(paths) != 2 || !strings.HasSuffix(paths[0], "/getFile") {
		t.Fatalf("getFile must come first: %v", paths)
	}
}

func TestLoadErrorsHideTheToken(t *testing.T) {
	transport := func(req *http.Request) error {
		return &url.Error{Op: "Get", URL: req.URL.String(), Err: errors.New("connection reset")}
	}
	cases := map[string]func(req *http.Request) (*http.Response, error){
		"getFile fails": func(req *http.Request) (*http.Response, error) { return nil, transport(req) },
		"getFile rejected": func(req *http.Request) (*http.Response, error) {
			return httpReply(400, `{"ok":false,"error_code":400,"description":"file is too big"}`), nil
		},
		"download fails": func(req *http.Request) (*http.Response, error) {
			if strings.HasSuffix(req.URL.Path, "/getFile") {
				return httpReply(200, `{"ok":true,"result":{"file_id":"f","file_path":"photos/a.jpg"}}`), nil
			}
			return nil, transport(req)
		},
		"download status": func(req *http.Request) (*http.Response, error) {
			if strings.HasSuffix(req.URL.Path, "/getFile") {
				return httpReply(200, `{"ok":true,"result":{"file_id":"f","file_path":"photos/a.jpg"}}`), nil
			}
			return httpReply(404, "not found"), nil
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			b := downloadBot(fn)
			in := b.inbound(privateMessage(tgbotapi.Message{Photo: []tgbotapi.PhotoSize{{FileID: "f", Width: 5, Height: 5}}}))
			_, err := in.Images[0].Load(context.Background())
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, secret := range []string{"your_token_here", "example.invalid", "api.telegram.org", "photos/a.jpg"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error %q contains %q", err, secret)
				}
			}
		})
	}
}

func TestLoadTooLarge(t *testing.T) {
	b := downloadBot(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/getFile") {
			return httpReply(200, `{"ok":true,"result":{"file_id":"f","file_path":"photos/a.jpg"}}`), nil
		}
		return httpReply(200, strings.Repeat("x", platform.MaxImageBytes+1)), nil
	})
	in := b.inbound(privateMessage(tgbotapi.Message{Photo: []tgbotapi.PhotoSize{{FileID: "f", Width: 5, Height: 5}}}))
	if _, err := in.Images[0].Load(context.Background()); !errors.Is(err, platform.ErrImageTooLarge) {
		t.Fatalf("got %v", err)
	}
}

func TestLoadCancelled(t *testing.T) {
	b := downloadBot(func(req *http.Request) (*http.Response, error) {
		return nil, req.Context().Err()
	})
	in := b.inbound(privateMessage(tgbotapi.Message{Photo: []tgbotapi.PhotoSize{{FileID: "f", Width: 5, Height: 5}}}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := in.Images[0].Load(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestLoadStalledGetFileEndsWithContext(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	b := downloadBot(func(req *http.Request) (*http.Response, error) {
		<-release // getFile takes no ctx in tgbotapi; it hangs until the test ends
		return nil, errors.New("released")
	})
	in := b.inbound(privateMessage(tgbotapi.Message{Photo: []tgbotapi.PhotoSize{{FileID: "f", Width: 5, Height: 5}}}))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := in.Images[0].Load(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Load did not return after ctx ended")
	}
}

func TestInboundImageDocumentWithoutMIME(t *testing.T) {
	b := &Bot{api: &tgbotapi.BotAPI{Token: "your_token_here"}}
	in := b.inbound(privateMessage(tgbotapi.Message{
		Document: &tgbotapi.Document{FileID: "d", FileName: "Shot.PNG"},
	}))
	if !in.TextMessage || len(in.Images) != 1 || in.Images[0].MIME != "image/png" || in.Skipped {
		t.Fatalf("%#v", in)
	}
	in = b.inbound(privateMessage(tgbotapi.Message{
		Document: &tgbotapi.Document{FileID: "d", FileName: "notes.txt"},
	}))
	if !in.TextMessage || len(in.Images) != 0 || len(in.Files) != 1 || in.Files[0].Name != "notes.txt" || in.Skipped {
		t.Fatalf("%#v", in)
	}
}
