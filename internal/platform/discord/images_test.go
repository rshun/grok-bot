package discord

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/rshun/grok-bot/internal/platform"
)

func attachmentMessage(content string, atts ...*discordgo.MessageAttachment) *discordgo.MessageCreate {
	return &discordgo.MessageCreate{Message: &discordgo.Message{
		ID: "m1", ChannelID: "c1", Content: content,
		Author:      &discordgo.User{ID: "42"},
		Attachments: atts,
	}}
}

func TestInboundImageAttachments(t *testing.T) {
	in, ok := messageInbound(attachmentMessage(" look ",
		&discordgo.MessageAttachment{ID: "1", Filename: "a.png", ContentType: "image/png", Size: 10, URL: "https://cdn.example.invalid/a.png"},
		&discordgo.MessageAttachment{ID: "2", Filename: "b.jpg", ContentType: "image/jpeg; charset=binary", Size: 20, URL: "https://cdn.example.invalid/b.jpg"},
		&discordgo.MessageAttachment{ID: "3", Filename: "c.zip", ContentType: "application/zip", Size: 30},
	), "bot")
	if !ok || !in.TextMessage || !in.Skipped || in.Text != "look" || len(in.Images) != 2 {
		t.Fatalf("ok=%v in=%#v", ok, in)
	}
	if in.Images[0].MIME != "image/png" || in.Images[0].Name != "a.png" || in.Images[0].Size != 10 || in.Images[0].Load == nil {
		t.Fatalf("%#v", in.Images[0])
	}
	if in.Images[1].MIME != "image/jpeg" || in.Images[1].Size != 20 {
		t.Fatalf("%#v", in.Images[1])
	}

	in, ok = messageInbound(attachmentMessage("caption",
		&discordgo.MessageAttachment{ID: "3", Filename: "c.zip", ContentType: "application/zip"},
	), "bot")
	if !ok || in.TextMessage || len(in.Images) != 0 {
		t.Fatalf("ok=%v in=%#v", ok, in)
	}

	// Images alone, no text.
	in, ok = messageInbound(attachmentMessage("",
		&discordgo.MessageAttachment{ID: "1", Filename: "a.gif", ContentType: "image/gif"},
	), "bot")
	if !ok || !in.TextMessage || in.Skipped || len(in.Images) != 1 {
		t.Fatalf("ok=%v in=%#v", ok, in)
	}
}

func TestInboundImageByExtension(t *testing.T) {
	in, _ := messageInbound(attachmentMessage("",
		&discordgo.MessageAttachment{ID: "1", Filename: "Photo.JPG"},
		&discordgo.MessageAttachment{ID: "2", Filename: "x.webp"},
		&discordgo.MessageAttachment{ID: "3", Filename: "x.png", ContentType: "application/octet-stream"}, // a stated type wins
		&discordgo.MessageAttachment{ID: "4", Filename: "x.svg"},
	), "bot")
	if len(in.Images) != 2 || in.Images[0].MIME != "image/jpeg" || in.Images[1].MIME != "image/webp" || !in.Skipped {
		t.Fatalf("%#v", in)
	}
}

func TestInboundImageCap(t *testing.T) {
	var atts []*discordgo.MessageAttachment
	for i := 0; i < platform.MaxImages+2; i++ {
		atts = append(atts, &discordgo.MessageAttachment{ID: "x", Filename: "a.png", ContentType: "image/png"})
	}
	in, _ := messageInbound(attachmentMessage("", atts...), "bot")
	if len(in.Images) != platform.MaxImages || !in.Skipped {
		t.Fatalf("%d images, skipped=%v", len(in.Images), in.Skipped)
	}
}

func TestInboundStickerWithImageIsSkipped(t *testing.T) {
	m := attachmentMessage("", &discordgo.MessageAttachment{ID: "1", Filename: "a.png", ContentType: "image/png"})
	m.StickerItems = []*discordgo.StickerItem{{ID: "s"}}
	in, _ := messageInbound(m, "bot")
	if !in.TextMessage || !in.Skipped {
		t.Fatalf("%#v", in)
	}
}

func TestImageLoad(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.png":
			_, _ = w.Write([]byte("png bytes"))
		case "/big.png":
			_, _ = w.Write([]byte(strings.Repeat("x", platform.MaxImageBytes+1)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	load := func(name string) ([]byte, error) {
		in, _ := messageInbound(attachmentMessage("", &discordgo.MessageAttachment{
			ID: "1", Filename: name, ContentType: "image/png", URL: srv.URL + "/" + name + "?sig=your_token_here",
		}), "bot")
		return in.Images[0].Load(context.Background())
	}
	if data, err := load("ok.png"); err != nil || string(data) != "png bytes" {
		t.Fatalf("got %q, %v", data, err)
	}
	if _, err := load("big.png"); !errors.Is(err, platform.ErrImageTooLarge) {
		t.Fatalf("got %v", err)
	}
	_, err := load("gone.png")
	if err == nil {
		t.Fatal("expected an error for a 404")
	}
	for _, leak := range []string{srv.URL, "sig=", "your_token_here", "gone.png"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error %q contains %q", err, leak)
		}
	}

	// A connection failure is a *url.Error whose text holds the URL.
	in, _ := messageInbound(attachmentMessage("", &discordgo.MessageAttachment{
		ID: "1", Filename: "a.png", ContentType: "image/png", URL: "http://127.0.0.1:1/a.png?sig=your_token_here",
	}), "bot")
	_, err = in.Images[0].Load(context.Background())
	if err == nil || strings.Contains(err.Error(), "sig=") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := in.Images[0].Load(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
