package discord

import (
	"context"
	"net/url"
	"strings"

	"github.com/rshun/grok-bot/internal/actions"
	"github.com/rshun/grok-bot/internal/platform"
)

// maxReactionInput bounds the emoji text in bytes; ZWJ sequences with skin tones reach about 35.
const maxReactionInput = 64

// React puts a Unicode emoji reaction on the user's message. Custom emoji are not supported.
func (b *Bot) React(ctx context.Context, in platform.Inbound, emoji string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	emoji = strings.TrimSpace(emoji)
	if msg := reactionProblem(emoji); msg != "" {
		return &actions.ToolError{Msg: msg}
	}
	// The emoji is a path segment, so escape it; this also keeps a keycap's # out of the fragment.
	return b.session.MessageReactionAdd(in.ChatID, in.MessageID, url.PathEscape(emoji))
}

// reactionProblem explains why emoji cannot be used, or returns "" when it can.
func reactionProblem(emoji string) string {
	switch {
	case emoji == "":
		return "emoji is required; use a Unicode emoji such as 👍"
	case len(emoji) > maxReactionInput:
		return "emoji is too long; use a single Unicode emoji such as 👍"
	case strings.ContainsAny(emoji, "<>:"):
		return "custom emoji are not supported; use a Unicode emoji such as 👍"
	case !hasNonASCII(emoji):
		return "not an emoji; use a Unicode emoji such as 👍"
	}
	return ""
}

func hasNonASCII(s string) bool {
	for _, r := range s {
		if r > 0x7f {
			return true
		}
	}
	return false
}
