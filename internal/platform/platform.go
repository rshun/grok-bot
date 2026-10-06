package platform

import "context"

// Inbound is one user message from Telegram or, later, Discord.
type Inbound struct {
	Platform string
	ChatID   string
	UserID   string
	Text     string
	// Text is false for photos, voice notes, and other unsupported payloads.
	TextMessage bool
}

// Replier sends a message back to the chat that produced an inbound message.
type Replier interface {
	Send(ctx context.Context, chatID, text string) error
}
