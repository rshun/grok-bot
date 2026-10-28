package platform

import (
	"context"
	"io"
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
	Text      string
	// TextMessage is false for photos, voice notes, and other unsupported payloads.
	TextMessage bool
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
