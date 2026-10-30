package telegram

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/rshun/grok-bot/internal/platform"
)

type fileRequest struct {
	method string
	form   map[string]string
	field  string // name of the file field
	name   string // file name in the upload
	body   string
}

// fileBot answers every upload with fail(method) and records what it received.
func fileBot(t *testing.T, requests *[]fileRequest, fail func(method string) bool) *Bot {
	t.Helper()
	api := &tgbotapi.BotAPI{Token: "your_token_here", Client: receiptHTTPClient{
		do: func(req *http.Request) (*http.Response, error) {
			if err := req.ParseMultipartForm(1 << 20); err != nil {
				t.Fatalf("request is not multipart: %v", err)
			}
			got := fileRequest{method: req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:], form: map[string]string{}}
			for k, v := range req.MultipartForm.Value {
				got.form[k] = v[0]
			}
			for field, headers := range req.MultipartForm.File {
				got.field, got.name = field, headers[0].Filename
				f, err := headers[0].Open()
				if err != nil {
					t.Fatal(err)
				}
				data, _ := io.ReadAll(f)
				f.Close()
				got.body = string(data)
			}
			*requests = append(*requests, got)
			if fail != nil && fail(got.method) {
				return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(`{"ok":false,"error_code":400,"description":"synthetic failure"}`))}, nil
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":3,"date":1,"chat":{"id":9,"type":"private"}}}`))}, nil
		},
	}}
	api.SetAPIEndpoint("https://example.invalid/bot%s/%s")
	return &Bot{api: api}
}

// memFile returns an Open function that serves body and counts how often it was opened.
func memFile(body string, size int64, opens *int) func() (io.ReadCloser, int64, error) {
	return func() (io.ReadCloser, int64, error) {
		*opens++
		return io.NopCloser(strings.NewReader(body)), size, nil
	}
}

func TestSendFilePhotoVsDocument(t *testing.T) {
	var requests []fileRequest
	b := fileBot(t, &requests, nil)
	ctx := context.Background()
	opens := 0

	if err := b.SendFile(ctx, "9", "17", platform.OutFile{Name: "chart.PNG", Caption: "图表", Open: memFile("pngdata", 7, &opens)}); err != nil {
		t.Fatal(err)
	}
	if err := b.SendFile(ctx, "9", "17", platform.OutFile{Name: "report.pdf", Open: memFile("pdfdata", 7, &opens)}); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests: %+v", requests)
	}
	photo, doc := requests[0], requests[1]
	if photo.method != "sendPhoto" || photo.field != "photo" || photo.name != "chart.PNG" || photo.body != "pngdata" {
		t.Fatalf("photo request: %+v", photo)
	}
	if photo.form["caption"] != "图表" || photo.form["chat_id"] != "9" || photo.form["reply_to_message_id"] != "17" || photo.form["allow_sending_without_reply"] != "true" {
		t.Fatalf("photo form: %v", photo.form)
	}
	if doc.method != "sendDocument" || doc.field != "document" || doc.name != "report.pdf" || doc.body != "pdfdata" {
		t.Fatalf("document request: %+v", doc)
	}
	if _, ok := doc.form["caption"]; ok {
		t.Fatalf("empty caption was sent: %v", doc.form)
	}
	if opens != 2 {
		t.Fatalf("opens: %d", opens)
	}
}

func TestSendFileWithoutReplyTo(t *testing.T) {
	var requests []fileRequest
	b := fileBot(t, &requests, nil)
	opens := 0
	if err := b.SendFile(context.Background(), "9", "", platform.OutFile{Name: "a.txt", Open: memFile("x", 1, &opens)}); err != nil {
		t.Fatal(err)
	}
	if _, ok := requests[0].form["reply_to_message_id"]; ok {
		t.Fatalf("quoted nothing: %v", requests[0].form)
	}
}

func TestSendFileLargeImageGoesAsDocument(t *testing.T) {
	var requests []fileRequest
	b := fileBot(t, &requests, nil)
	opens := 0
	// Telegram rejects photos over 10 MB, so a bigger image keeps its bytes as a document.
	if err := b.SendFile(context.Background(), "9", "17", platform.OutFile{Name: "big.jpg", Open: memFile("jpgdata", 10<<20+1, &opens)}); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0].method != "sendDocument" || requests[0].body != "jpgdata" {
		t.Fatalf("requests: %+v", requests)
	}
	// Exactly 10 MB still goes as a photo.
	requests = nil
	if err := b.SendFile(context.Background(), "9", "17", platform.OutFile{Name: "ok.webp", Open: memFile("webp", 10<<20, &opens)}); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0].method != "sendPhoto" {
		t.Fatalf("requests: %+v", requests)
	}
}

func TestSendFilePhotoFailureFallsBackToDocument(t *testing.T) {
	var requests []fileRequest
	b := fileBot(t, &requests, func(method string) bool { return method == "sendPhoto" })
	opens := 0
	if err := b.SendFile(context.Background(), "9", "17", platform.OutFile{Name: "odd.png", Caption: "图", Open: memFile("pngdata", 7, &opens)}); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[0].method != "sendPhoto" || requests[1].method != "sendDocument" {
		t.Fatalf("requests: %+v", requests)
	}
	// The first reader was consumed by the photo upload, so the document is read from a new one.
	if opens != 2 || requests[1].body != "pngdata" || requests[1].form["caption"] != "图" || requests[1].form["reply_to_message_id"] != "17" {
		t.Fatalf("opens=%d request=%+v", opens, requests[1])
	}
}

func TestSendFileReportsOpenAndSendFailures(t *testing.T) {
	var requests []fileRequest
	b := fileBot(t, &requests, func(string) bool { return true })
	ctx := context.Background()
	refused := errors.New("这个文件不允许发送。")
	err := b.SendFile(ctx, "9", "17", platform.OutFile{Name: "a.txt", Open: func() (io.ReadCloser, int64, error) { return nil, 0, refused }})
	if !errors.Is(err, refused) || len(requests) != 0 {
		t.Fatalf("err=%v requests=%d", err, len(requests))
	}
	opens := 0
	if err := b.SendFile(ctx, "9", "17", platform.OutFile{Name: "a.txt", Open: memFile("x", 1, &opens)}); err == nil {
		t.Fatal("a rejected upload was reported as sent")
	}
	if err := b.SendFile(ctx, "bad", "17", platform.OutFile{Name: "a.txt", Open: memFile("x", 1, &opens)}); err == nil {
		t.Fatal("bad chat id accepted")
	}
}

func TestTelegramFileSenderLimits(t *testing.T) {
	var _ platform.FileSender = &messageReply{Bot: &Bot{}}
	if got := (&Bot{}).FileLimit(); got != 50<<20 {
		t.Fatalf("limit %d", got)
	}
}
