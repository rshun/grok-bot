package discord

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/rshun/grok-bot/internal/platform"
)

func TestSendFileAttaches(t *testing.T) {
	s, err := discordgo.New("Bot your_token_here")
	if err != nil {
		t.Fatal(err)
	}
	var path, payload, fileName, fileBody string
	s.Client = &http.Client{Transport: receiptTransport(func(req *http.Request) (*http.Response, error) {
		path = req.URL.Path
		payload = ""
		_, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
		if err != nil {
			t.Fatalf("content type: %v", err)
		}
		reader := multipart.NewReader(req.Body, params["boundary"])
		for {
			part, err := reader.NextPart()
			if err != nil {
				break
			}
			data, _ := io.ReadAll(part)
			if part.FormName() == "payload_json" {
				payload = string(data)
			} else {
				fileName, fileBody = part.FileName(), string(data)
			}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"7","channel_id":"9"}`))}, nil
	})}
	b := &Bot{session: s}
	opens := 0
	f := platform.OutFile{Name: "report.pdf", Caption: "报告", Open: func() (io.ReadCloser, int64, error) {
		opens++
		return io.NopCloser(strings.NewReader("pdfdata")), 7, nil
	}}
	if err := b.SendFile(context.Background(), "9", "17", f); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, "/channels/9/messages") || fileName != "report.pdf" || fileBody != "pdfdata" || opens != 1 {
		t.Fatalf("path=%s file=%q body=%q opens=%d", path, fileName, fileBody, opens)
	}
	var body struct {
		Content   string `json:"content"`
		Reference struct {
			MessageID string `json:"message_id"`
			ChannelID string `json:"channel_id"`
			Fail      *bool  `json:"fail_if_not_exists"`
		} `json:"message_reference"`
		Mentions struct {
			Parse []string `json:"parse"`
		} `json:"allowed_mentions"`
	}
	if err := json.Unmarshal([]byte(payload), &body); err != nil {
		t.Fatalf("payload %q: %v", payload, err)
	}
	if body.Content != "报告" || body.Reference.MessageID != "17" || body.Reference.ChannelID != "9" || body.Reference.Fail == nil || *body.Reference.Fail || len(body.Mentions.Parse) != 0 {
		t.Fatalf("payload: %s", payload)
	}

	// With nothing to quote there is no reference.
	if err := b.SendFile(context.Background(), "9", "", f); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, "message_reference") {
		t.Fatalf("quoted nothing: %s", payload)
	}
}

func TestSendFileOpenFailure(t *testing.T) {
	s, err := discordgo.New("Bot your_token_here")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.Client = &http.Client{Transport: receiptTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}
	b := &Bot{session: s}
	refused := errors.New("这个文件不允许发送。")
	err = b.SendFile(context.Background(), "9", "17", platform.OutFile{Name: "a", Open: func() (io.ReadCloser, int64, error) { return nil, 0, refused }})
	if !errors.Is(err, refused) || calls != 0 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestDiscordFileSenderLimits(t *testing.T) {
	var _ platform.FileSender = &messageReply{Bot: &Bot{}}
	if got := (&Bot{}).FileLimit(); got != 25<<20 {
		t.Fatalf("limit %d", got)
	}
}
