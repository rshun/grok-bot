package platform

import "context"

const (
	// Telegram and Discord are the platform names stored with each chat.
	Telegram = "telegram"
	Discord  = "discord"
)

// Inbound is one user message from Telegram or Discord.
type Inbound struct {
	Platform string
	ChatID   string
	UserID   string
	Text     string
	// TextMessage is false for photos, voice notes, and other unsupported payloads.
	TextMessage bool
}

// Replier sends a message back to the chat that produced an inbound message.
type Replier interface {
	Send(ctx context.Context, chatID, text string) error
}
