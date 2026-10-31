package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/rshun/grok-bot/internal/platform"
)

// inbound turns a private message into the core's Inbound. Photos, image documents and general
// file documents make the message usable even without text; voice/video/stickers are noted through Skipped.
func (b *Bot) inbound(msg *tgbotapi.Message) platform.Inbound {
	text := strings.TrimSpace(msg.Text)
	images, files, skipped := b.media(msg)
	if len(images) > 0 || len(files) > 0 {
		text = strings.TrimSpace(msg.Caption)
	}
	return platform.Inbound{
		Platform:    platform.Telegram,
		ChatID:      strconv.FormatInt(msg.Chat.ID, 10),
		MessageID:   strconv.Itoa(msg.MessageID),
		UserID:      strconv.FormatInt(msg.From.ID, 10),
		Text:        text,
		ReplyToText: replyToText(msg.ReplyToMessage),
		TextMessage: len(images) > 0 || len(files) > 0 || (text != "" && !hasMedia(msg)),
		Images:      images,
		Files:       files,
		Skipped:     skipped,
	}
}

func replyToText(reply *tgbotapi.Message) string {
	if reply == nil {
		return ""
	}
	if t := strings.TrimSpace(reply.Text); t != "" {
		return t
	}
	return strings.TrimSpace(reply.Caption)
}

// media returns the pictures and document files of one message and whether anything else was left out.
func (b *Bot) media(msg *tgbotapi.Message) (images []platform.Image, files []platform.Attachment, skipped bool) {
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
		var mime string
		if strings.TrimSpace(doc.MimeType) != "" {
			mime = platform.ImageMIME(doc.MimeType)
		} else {
			// Telegram leaves the type out for some clients; the name is the only hint then.
			mime = platform.ImageMIMEByName(doc.FileName)
		}
		if mime != "" {
			images = append(images, b.image(doc.FileID, mime, doc.FileName, int64(doc.FileSize)))
		} else {
			name := strings.TrimSpace(doc.FileName)
			if name == "" {
				name = "attachment"
			}
			files = append(files, b.attachment(doc.FileID, strings.TrimSpace(doc.MimeType), name, int64(doc.FileSize)))
		}
	}
	if msg.Voice != nil || msg.Audio != nil || msg.Video != nil || msg.Sticker != nil || msg.VideoNote != nil {
		skipped = true
	}
	if len(images) > platform.MaxImages {
		images, skipped = images[:platform.MaxImages], true
	}
	if len(files) > platform.MaxFiles {
		files, skipped = files[:platform.MaxFiles], true
	}
	return images, files, skipped
}

func hasMedia(msg *tgbotapi.Message) bool {
	return msg.Photo != nil || msg.Voice != nil || msg.Audio != nil || msg.Video != nil || msg.Document != nil || msg.Sticker != nil || msg.VideoNote != nil
}

// photoRank orders the sizes of one photo; the declared byte size is optional, so pixels count too.
func photoRank(p tgbotapi.PhotoSize) int64 {
	return int64(p.FileSize)<<32 | int64(p.Width*p.Height)
}

func (b *Bot) image(fileID, mime, name string, size int64) platform.Image {
	return platform.Image{
		MIME: mime,
		Name: name,
		Size: size,
		Load: func(ctx context.Context) ([]byte, error) { return b.download(ctx, fileID, platform.ReadImage) },
	}
}

func (b *Bot) attachment(fileID, mime, name string, size int64) platform.Attachment {
	return platform.Attachment{
		MIME: mime,
		Name: name,
		Size: size,
		Load: func(ctx context.Context) ([]byte, error) { return b.download(ctx, fileID, platform.ReadAttachment) },
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
func (b *Bot) download(ctx context.Context, fileID string, readBody func(io.Reader) ([]byte, error)) ([]byte, error) {
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
	data, err := readBody(resp.Body)
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case errors.Is(err, platform.ErrImageTooLarge), errors.Is(err, platform.ErrFileTooLarge):
		return nil, err
	case err != nil:
		return nil, errors.New("telegram download failed")
	}
	return data, nil
}
