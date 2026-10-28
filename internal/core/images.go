package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/platform"
)

const (
	unsupportedText   = "目前只处理文字和图片。"
	skippedNote       = "其余附件暂不支持。"
	imageTooLargeText = "图片超过 10MB，暂不处理。"
	// imageFailedText is fixed on purpose: a download error can hold a URL with the bot token.
	imageFailedText = "图片下载失败，请重发。"
)

// parseInboundCommand is parseCommand for a message. A message with images is never a command,
// even when its caption starts with a slash.
func parseInboundCommand(in platform.Inbound) (cmd, arg string, ok bool) {
	if len(in.Images) > 0 {
		return "", "", false
	}
	return parseCommand(in.Text)
}

// limitImages keeps the first MaxImages images and marks the message when it had more.
func limitImages(in *platform.Inbound) {
	if len(in.Images) > platform.MaxImages {
		in.Images = in.Images[:platform.MaxImages]
		in.Skipped = true
	}
}

// oversize reports whether any image declares a size over the limit.
func oversize(images []platform.Image) bool {
	for _, img := range images {
		if img.Size > platform.MaxImageBytes {
			return true
		}
	}
	return false
}

// imagePrompt is the text sent with the pictures: the caption, or a stand-in when there is none.
func imagePrompt(text string, n int) string {
	switch {
	case text != "":
		return text
	case n == 1:
		return "（用户发送了一张图片）"
	default:
		return fmt.Sprintf("（用户发送了 %d 张图片）", n)
	}
}

// loadImages downloads the pictures of a message. When it cannot, failure is the fixed text to
// show the user. Only here are the pictures fetched, after the allowlist has passed.
func loadImages(ctx context.Context, in platform.Inbound) (images []grok.Image, failure string) {
	for _, img := range in.Images {
		if img.Load == nil {
			return nil, imageFailedText
		}
		data, err := img.Load(ctx)
		switch {
		case errors.Is(err, platform.ErrImageTooLarge):
			return nil, imageTooLargeText
		case err != nil:
			// Platforms keep URLs out of their errors; safeErr covers the ones that do not.
			slog.Warn("image download failed", "platform", in.Platform, "chat", in.ChatID, "err", safeErr(err))
			return nil, imageFailedText
		case len(data) > platform.MaxImageBytes:
			return nil, imageTooLargeText
		}
		images = append(images, grok.Image{MIME: img.MIME, Data: data})
	}
	return images, ""
}
