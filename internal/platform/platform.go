package platform

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
)

const (
	// Telegram and Discord are the platform names stored with each chat.
	Telegram = "telegram"
	Discord  = "discord"
)

// Inbound is one user message from Telegram or Discord.
type Inbound struct {
	Platform  string
	ChatID    string
	MessageID string
	UserID    string
	// Text is the message text, or the caption when the message carries images.
	Text string
	// TextMessage is true when the message has text or images; it is false for voice notes and
	// other payloads the bot cannot use.
	TextMessage bool
	// Images are the pictures of the message, at most MaxImages.
	Images []Image
	// Skipped is true when the message also carried attachments that are not images, or more
	// images than MaxImages.
	Skipped bool
}

const (
	// MaxImages is the most images taken from one message; the rest count as skipped.
	MaxImages = 10
	// MaxImageBytes is the largest single image the bot accepts.
	MaxImageBytes = 10 << 20
)

// ErrImageTooLarge is returned by an Image Load when the picture is over MaxImageBytes.
var ErrImageTooLarge = errors.New("image too large")

// Image is a picture attached to a message. Nothing is downloaded until Load is called, and
// the caller calls it only for messages from allowed users.
type Image struct {
	MIME, Name string
	// Size is the size the platform declared, 0 when unknown.
	Size int64
	// Load downloads the picture. Its errors never contain the download URL, which can hold a
	// bot token, so they are safe to log.
	Load func(ctx context.Context) ([]byte, error)
}

// ReadImage reads a downloaded picture, stopping one byte after the limit.
func ReadImage(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxImageBytes {
		return nil, ErrImageTooLarge
	}
	return data, nil
}

// ImageMIMEByName returns the image type Grok takes for a file name's extension, or "" when
// the extension is not one of them. It is only for attachments that state no type.
func ImageMIMEByName(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	}
	return ""
}

// Replier sends a message back to the chat that produced an inbound message.
type Replier interface {
	Send(ctx context.Context, chatID, text string) error
}

// Receipt marks an accepted message without sending a separate text reply.
type Receipt interface {
	MarkReceived(ctx context.Context, in Inbound) error
}

// Reactor lets a replier put an emoji reaction on a user's message.
// React returns an *actions.ToolError for input the platform cannot use, whose text is safe to
// show to Grok; any other error is a platform failure and must not be shown.
type Reactor interface {
	React(ctx context.Context, in Inbound, emoji string) error
}

// Progress lets a replier show an editable "working" message for one inbound message.
type Progress interface {
	StartProgress(ctx context.Context, in Inbound, text string) (ProgressHandle, error)
}

// ProgressHandle controls one progress message. Core ends it with exactly one of Finish or Delete.
type ProgressHandle interface {
	Update(ctx context.Context, text string) error
	// Finish 把进度消息改成第一段，其余段落新发并引用原消息。编辑失败时自己改为全部新发，只有新发也失败才返回错误。
	Finish(ctx context.Context, text string) error
	Delete(ctx context.Context) error
}

// OutFile is one file to send to a chat. The file is read through Open instead of a path, so
// the bytes sent are the bytes that were checked.
type OutFile struct {
	// Name is the file name shown in the chat.
	Name string
	// Caption is text shown with the file; the platform may refuse one that is too long.
	Caption string
	// Open returns a freshly validated reader and its size. Each call re-checks the file and
	// must be closed by the caller; a platform that retries a different way opens it again.
	Open func() (io.ReadCloser, int64, error)
}

// FileSender lets a replier send files to a chat.
// replyTo is the message to quote, or empty to send without quoting.
type FileSender interface {
	SendFile(ctx context.Context, chatID, replyTo string, f OutFile) error
	// FileLimit is the largest file in bytes the platform accepts.
	FileLimit() int64
}
