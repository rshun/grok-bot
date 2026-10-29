package telegram

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/rshun/grok-bot/internal/platform"
)

// inbound turns a private message into the core's Inbound. A photo or an image document makes
// the message usable even without text; other attachments are only noted through Skipped.
func (b *Bot) inbound(msg *tgbotapi.Message) platform.Inbound {
	text := strings.TrimSpace(msg.Text)
	images, skipped := b.images(msg)
	if len(images) > 0 {
		text = strings.TrimSpace(msg.Caption)
	}
	return platform.Inbound{
		Platform:    platform.Telegram,
		ChatID:      strconv.FormatInt(msg.Chat.ID, 10),
		MessageID:   strconv.Itoa(msg.MessageID),
		UserID:      strconv.FormatInt(msg.From.ID, 10),
		Text:        text,
		TextMessage: len(images) > 0 || (text != "" && !hasMedia(msg)),
		Images:      images,
		Skipped:     skipped,
	}
}

// images returns the pictures of one message and whether anything else was left out.
func (b *Bot) images(msg *tgbotapi.Message) (images []platform.Image, skipped bool) {
	if len(msg.Photo) > 0 {
		// Telegram lists the same photo in several sizes; only the largest is used.
		best := msg.Photo[0]
		for _, p := range msg.Photo[1:] {
			if photoRank(p) > photoRank(best) {
				best = p
			}
		}
		images = append(images, b.image(best.FileID, "image/jpeg", "", int64(best.FileSize)))
	}
	if doc := msg.Document; doc != nil {
		mime := strings.ToLower(strings.TrimSpace(doc.MimeType))
		if mime == "" {
			// Telegram leaves the type out for some clients; the name is the only hint then.
			mime = platform.ImageMIMEByName(doc.FileName)
		}
		if isImageMIME(mime) {
			images = append(images, b.image(doc.FileID, mime, doc.FileName, int64(doc.FileSize)))
		} else {
			skipped = true
		}
	}
	if msg.Voice != nil || msg.Audio != nil || msg.Video != nil || msg.Sticker != nil || msg.VideoNote != nil {
		skipped = true
	}
	if len(images) > platform.MaxImages {
		images, skipped = images[:platform.MaxImages], true
	}
	return images, skipped
}

func hasMedia(msg *tgbotapi.Message) bool {
	return msg.Photo != nil || msg.Voice != nil || msg.Audio != nil || msg.Video != nil || msg.Document != nil || msg.Sticker != nil || msg.VideoNote != nil
}

// photoRank orders the sizes of one photo; the declared byte size is optional, so pixels count too.
func photoRank(p tgbotapi.PhotoSize) int64 {
	return int64(p.FileSize)<<32 | int64(p.Width*p.Height)
}

func isImageMIME(mime string) bool {
	switch mime {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	}
	return false
}

func (b *Bot) image(fileID, mime, name string, size int64) platform.Image {
	return platform.Image{
		MIME: mime,
		Name: name,
		Size: size,
		Load: func(ctx context.Context) ([]byte, error) { return b.download(ctx, fileID) },
	}
}

// getFile is api.GetFile bounded by ctx. tgbotapi takes no context there, so the call runs on
// its own goroutine; when ctx ends first the caller moves on and the goroutine finishes
// whenever the HTTP call does.
func (b *Bot) getFile(ctx context.Context, fileID string) (tgbotapi.File, error) {
	type result struct {
		file tgbotapi.File
		err  error
	}
	done := make(chan result, 1)
	go func() {
		file, err := b.api.GetFile(tgbotapi.FileConfig{FileID: fileID})
		done <- result{file, err}
	}()
	select {
	case r := <-done:
		return r.file, r.err
	case <-ctx.Done():
		return tgbotapi.File{}, ctx.Err()
	}
}

// download fetches a file through getFile. The download URL holds the bot token, so no error
// returned from here contains a URL: transport errors are replaced by fixed texts.
func (b *Bot) download(ctx context.Context, fileID string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := b.getFile(ctx, fileID)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var apiErr *tgbotapi.Error
		if errors.As(err, &apiErr) {
			return nil, fmt.Errorf("telegram getFile failed: code %d", apiErr.Code)
		}
		return nil, errors.New("telegram getFile failed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, file.Link(b.api.Token), nil)
	if err != nil {
		return nil, errors.New("telegram download failed")
	}
	resp, err := b.api.Client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("telegram download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram download failed: status %d", resp.StatusCode)
	}
	data, err := platform.ReadImage(resp.Body)
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case errors.Is(err, platform.ErrImageTooLarge):
		return nil, err
	case err != nil:
		return nil, errors.New("telegram download failed")
	}
	return data, nil
}
