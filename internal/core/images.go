package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/platform"
)

const (
	unsupportedText   = "目前只处理文字、图片和文件。"
	skippedNote       = "其余附件暂不支持。"
	imageTooLargeText = "图片超过 10MB，暂不处理。"
	// imageFailedText is fixed on purpose: a download error can hold a URL with the bot token.
	imageFailedText  = "图片下载失败，请重发。"
	fileTooLargeText = "文件超过 20MB，暂不处理。"
	fileFailedText   = "文件下载失败，请重发。"
	inlineFileMax    = 32 << 10
)

// parseInboundCommand is parseCommand for a message. A message with images or files is never a
// command, even when its caption starts with a slash.
func parseInboundCommand(in platform.Inbound) (cmd, arg string, ok bool) {
	if len(in.Images) > 0 || len(in.Files) > 0 {
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

// limitFiles keeps the first MaxFiles files and marks the message when it had more.
func limitFiles(in *platform.Inbound) {
	if len(in.Files) > platform.MaxFiles {
		in.Files = in.Files[:platform.MaxFiles]
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

// oversizeFiles reports whether any file declares a size over MaxFileBytes.
func oversizeFiles(files []platform.Attachment) bool {
	for _, f := range files {
		if f.Size > platform.MaxFileBytes {
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

// defaultImageLoadTimeout bounds downloading all pictures of one message. The handler context
// has no deadline and a stalled transfer would otherwise hold the chat's lane forever.
const defaultImageLoadTimeout = 3 * time.Minute

// loadImages downloads the pictures of a message. When it cannot, failure is the fixed text to
// show the user. Only here are the pictures fetched, after the allowlist has passed.
func (s *Service) loadImages(ctx context.Context, in platform.Inbound) (images []grok.Image, failure string) {
	timeout := s.ImageLoadTimeout
	if timeout <= 0 {
		timeout = defaultImageLoadTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
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

type uploadedFile struct {
	Name string
	Path string
	Data []byte
}

func (s *Service) uploadRoot() string {
	if s.UploadDir != "" {
		return s.UploadDir
	}
	return filepath.Join(os.TempDir(), "grok-bot-uploads")
}

// saveUploads downloads attached files and writes them under UploadDir/<platform-chat>/ so Grok
// (and custom date-file schedules) can read them by absolute path.
func (s *Service) saveUploads(ctx context.Context, in platform.Inbound) ([]uploadedFile, string) {
	if len(in.Files) == 0 {
		return nil, ""
	}
	timeout := s.ImageLoadTimeout
	if timeout <= 0 {
		timeout = defaultImageLoadTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	chatDir := filepath.Join(s.uploadRoot(), sanitizeSegment(in.Platform+"-"+in.ChatID))
	if abs, err := filepath.Abs(chatDir); err == nil {
		chatDir = abs
	}
	if err := os.MkdirAll(chatDir, 0o750); err != nil {
		slog.Error("create upload dir", "err", err)
		return nil, fileFailedText
	}
	var out []uploadedFile
	for _, f := range in.Files {
		if f.Load == nil {
			return nil, fileFailedText
		}
		data, err := f.Load(ctx)
		switch {
		case errors.Is(err, platform.ErrFileTooLarge):
			return nil, fileTooLargeText
		case err != nil:
			slog.Warn("file download failed", "platform", in.Platform, "chat", in.ChatID, "err", safeErr(err))
			return nil, fileFailedText
		case len(data) > platform.MaxFileBytes:
			return nil, fileTooLargeText
		}
		name := sanitizeUploadName(f.Name)
		dest := filepath.Join(chatDir, name)
		if err := os.WriteFile(dest, data, 0o640); err != nil {
			slog.Error("write uploaded file", "err", err)
			return nil, fileFailedText
		}
		out = append(out, uploadedFile{Name: name, Path: dest, Data: data})
	}
	return out, ""
}

func sanitizeSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "chat"
	}
	return b.String()
}

func sanitizeUploadName(name string) string {
	base := filepath.Base(strings.TrimSpace(name))
	if base == "" || base == "." || base == string(filepath.Separator) {
		base = "attachment"
	}
	base = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, base)
	lower := strings.ToLower(base)
	// Avoid matching sandbox deny rules (**/.env and **/*.pem).
	if lower == ".env" {
		base = "uploaded_env.txt"
	} else if strings.HasSuffix(lower, ".pem") {
		base = strings.TrimSuffix(base, filepath.Ext(base)) + "_pem.txt"
	}
	return base
}

func formatUploadedFilesPrompt(text string, files []uploadedFile, inlineContent bool) string {
	if len(files) == 0 {
		return text
	}
	var b strings.Builder
	for _, f := range files {
		fmt.Fprintf(&b, "[用户上传了文件: %s]\n", f.Path)
		if inlineContent && len(f.Data) <= inlineFileMax && utf8.Valid(f.Data) && !strings.ContainsRune(string(f.Data), 0) {
			fmt.Fprintf(&b, "```text\n%s\n```\n", strings.TrimRight(string(f.Data), "\r\n"))
		}
	}
	if strings.TrimSpace(text) == "" {
		b.WriteString("\n请查看并处理上面上传的文件。")
	} else {
		b.WriteString("\n" + text)
	}
	return b.String()
}
