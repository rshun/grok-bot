package core

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/platform"
)

// loaded counts how often an image was downloaded.
type loaded struct{ n atomic.Int32 }

func (l *loaded) image(mime string, size int64, data string) platform.Image {
	return platform.Image{MIME: mime, Name: "pic", Size: size, Load: func(context.Context) ([]byte, error) {
		l.n.Add(1)
		return []byte(data), nil
	}}
}

func sendImages(svc *Service, rec *recorder, userID, text string, images ...platform.Image) {
	svc.Handle(context.Background(), platform.Inbound{
		Platform: "telegram", ChatID: "9", MessageID: "5", UserID: userID, Text: text, TextMessage: len(images) > 0 || text != "", Images: images,
	}, capture{rec})
}

func TestImageMessageReachesEngine(t *testing.T) {
	svc, engine := testService(t)
	rec := &recorder{}
	var l loaded
	sendImages(svc, rec, "1", "what is this", l.image("image/png", 3, "one"))
	rec.wait(t, 1)
	sendImages(svc, rec, "1", "", l.image("image/jpeg", 3, "two"))
	rec.wait(t, 2)
	sendImages(svc, rec, "1", "", l.image("image/png", 3, "a"), l.image("image/webp", 3, "b"))
	rec.wait(t, 3)

	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.calls) != 3 {
		t.Fatalf("%d calls", len(engine.calls))
	}
	first := engine.calls[0]
	if first.Prompt != "what is this" || len(first.Images) != 1 || first.Images[0].MIME != "image/png" || string(first.Images[0].Data) != "one" {
		t.Fatalf("%#v", first)
	}
	if engine.calls[1].Prompt != "（用户发送了一张图片）" || len(engine.calls[1].Images) != 1 {
		t.Fatalf("%#v", engine.calls[1])
	}
	if engine.calls[2].Prompt != "（用户发送了 2 张图片）" || len(engine.calls[2].Images) != 2 || string(engine.calls[2].Images[1].Data) != "b" {
		t.Fatalf("%#v", engine.calls[2])
	}
}

func TestImageNotLoadedForStranger(t *testing.T) {
	svc, engine := testService(t)
	rec := &recorder{}
	var l loaded
	sendImages(svc, rec, "2", "hi", l.image("image/png", 3, "x"))
	got := rec.wait(t, 1)
	if got[0] != "没有权限。" || l.n.Load() != 0 || len(engine.calls) != 0 {
		t.Fatalf("%#v loaded=%d calls=%d", got, l.n.Load(), len(engine.calls))
	}
}

func TestImageTooLarge(t *testing.T) {
	svc, engine := testService(t)
	rec := &recorder{}
	var l loaded
	sendImages(svc, rec, "1", "hi", l.image("image/png", platform.MaxImageBytes+1, "x"))
	got := rec.wait(t, 1)
	if got[0] != "图片超过 10MB，暂不处理。" || l.n.Load() != 0 || len(engine.calls) != 0 {
		t.Fatalf("%#v loaded=%d calls=%d", got, l.n.Load(), len(engine.calls))
	}

	// The limit is also checked on what was downloaded.
	over := platform.Image{MIME: "image/png", Load: func(context.Context) ([]byte, error) { return nil, platform.ErrImageTooLarge }}
	sendImages(svc, rec, "1", "hi", over)
	got = rec.wait(t, 2)
	if got[1] != "图片超过 10MB，暂不处理。" || len(engine.calls) != 0 {
		t.Fatalf("%#v calls=%d", got, len(engine.calls))
	}
	exactly := l.image("image/png", platform.MaxImageBytes, "x")
	sendImages(svc, rec, "1", "hi", exactly)
	rec.wait(t, 3)
	if len(engine.calls) != 1 {
		t.Fatal("an image of exactly the limit must pass")
	}
}

func TestImageLoadFailureHidesDetails(t *testing.T) {
	svc, engine := testService(t)
	rec := &recorder{}
	broken := platform.Image{MIME: "image/png", Load: func(context.Context) ([]byte, error) {
		return nil, errors.New("Get https://api.telegram.org/file/botyour_token_here/photos/a.jpg: boom")
	}}
	sendImages(svc, rec, "1", "hi", broken)
	got := rec.wait(t, 1)
	if got[0] != "图片下载失败，请重发。" || len(engine.calls) != 0 {
		t.Fatalf("%#v calls=%d", got, len(engine.calls))
	}
	// One bad image of two fails the whole message.
	var l loaded
	sendImages(svc, rec, "1", "hi", l.image("image/png", 1, "x"), broken)
	got = rec.wait(t, 2)
	if got[1] != "图片下载失败，请重发。" || len(engine.calls) != 0 {
		t.Fatalf("%#v calls=%d", got, len(engine.calls))
	}
}

func TestSkippedAttachmentNote(t *testing.T) {
	svc, _ := testService(t)
	rec := &recorder{}
	var l loaded
	svc.Handle(context.Background(), platform.Inbound{
		Platform: "telegram", ChatID: "9", UserID: "1", Text: "hi", TextMessage: true, Skipped: true,
		Images: []platform.Image{l.image("image/png", 1, "x")},
	}, capture{rec})
	got := rec.wait(t, 1)
	if got[0] != "其余附件暂不支持。\n\npong" {
		t.Fatalf("%#v", got)
	}
}

func TestExtraImagesAreSkipped(t *testing.T) {
	svc, engine := testService(t)
	rec := &recorder{}
	var l loaded
	var images []platform.Image
	for i := 0; i < platform.MaxImages+3; i++ {
		images = append(images, l.image("image/png", 1, "x"))
	}
	sendImages(svc, rec, "1", "hi", images...)
	got := rec.wait(t, 1)
	if !strings.HasPrefix(got[0], "其余附件暂不支持。") || len(engine.calls[0].Images) != platform.MaxImages || l.n.Load() != platform.MaxImages {
		t.Fatalf("%#v images=%d loaded=%d", got, len(engine.calls[0].Images), l.n.Load())
	}
}

func TestUnsupportedOnly(t *testing.T) {
	svc, engine := testService(t)
	rec := &recorder{}
	svc.Handle(context.Background(), platform.Inbound{
		Platform: "telegram", ChatID: "9", UserID: "1", Skipped: true,
	}, capture{rec})
	got := rec.wait(t, 1)
	if got[0] != "目前只处理文字和图片。" || len(engine.calls) != 0 {
		t.Fatalf("%#v", got)
	}
}

func TestImageCaptionIsNeverACommand(t *testing.T) {
	svc, engine := testService(t)
	rec := &recorder{}
	var l loaded
	sendImages(svc, rec, "1", "/reset", l.image("image/png", 1, "x"))
	got := rec.wait(t, 1)
	if len(got) != 1 || got[0] != "pong" || len(engine.calls) != 1 || engine.calls[0].Prompt != "/reset" {
		t.Fatalf("%#v calls=%#v", got, engine.calls)
	}
	if rec.receiptCount() != 1 {
		t.Fatalf("receipts=%d", rec.receiptCount())
	}
}

func TestCaptionGrantApplies(t *testing.T) {
	svc, _, _ := sandboxService(t)
	dir := t.TempDir()
	rec := &recorder{}
	var l loaded
	sendImages(svc, rec, "1", "允许写入 "+dir, l.image("image/png", 1, "x"))
	got := rec.wait(t, 1)
	if !strings.Contains(got[0], "已允许写入") {
		t.Fatalf("%#v", got)
	}
}

func TestImagesResentWhenSessionIsRebuilt(t *testing.T) {
	svc, engine := testService(t)
	if err := svc.Store.SetSession(context.Background(), "telegram", "9", "old"); err != nil {
		t.Fatal(err)
	}
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		if !opts.NewSession {
			return grok.HeadlessResult{}, errors.New("Session old not found")
		}
		return grok.HeadlessResult{Text: "pong", SessionID: opts.SessionID}, nil
	}
	rec := &recorder{}
	var l loaded
	sendImages(svc, rec, "1", "hi", l.image("image/png", 1, "x"))
	rec.wait(t, 1)
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.calls) != 2 || len(engine.calls[0].Images) != 1 || len(engine.calls[1].Images) != 1 || l.n.Load() != 1 {
		t.Fatalf("calls=%#v loaded=%d", engine.calls, l.n.Load())
	}
}
