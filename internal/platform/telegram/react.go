package telegram

import (
	"context"
	"encoding/json"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/rshun/grok-bot/internal/actions"
	"github.com/rshun/grok-bot/internal/platform"
)

// maxReactionInput bounds what is looked at; the longest allowed reaction is far shorter.
const maxReactionInput = 32

// reactionList is the set a bot may use (ReactionTypeEmoji in the Bot API), without
// variation selectors. Joined emoji carry their U+200D joiner.
var reactionList = []string{
	"👍", "👎", "❤", "🔥", "🥰", "👏", "😁", "🤔", "🤯", "😱", "🤬", "😢", "🎉", "🤩", "🤮", "💩", "🙏", "👌", "🕊", "🤡",
	"🥱", "🥴", "😍", "🐳", "❤‍🔥", "🌚", "🌭", "💯", "🤣", "⚡", "🍌", "🏆", "💔", "🤨", "😐", "🍓", "🍾", "💋", "🖕", "😈",
	"😴", "😭", "🤓", "👻", "👨‍💻", "👀", "🎃", "🙈", "😇", "😨", "🤝", "✍", "🤗", "🫡", "🎅", "🎄", "☃", "💅", "🤪", "🗿",
	"🆒", "💘", "🙉", "🦄", "😘", "💊", "🙊", "😎", "👾", "🤷‍♂", "🤷", "🤷‍♀", "😡",
}

var (
	allowedReactions = func() map[string]struct{} {
		m := make(map[string]struct{}, len(reactionList))
		for _, e := range reactionList {
			m[e] = struct{}{}
		}
		return m
	}()
	notAllowedReaction = "not an allowed Telegram reaction; use one of: " + strings.Join(reactionList, " ")
)

// React puts one emoji reaction on the user's message. A bot has one reaction per message,
// so this replaces the receipt reaction.
func (b *Bot) React(ctx context.Context, in platform.Inbound, emoji string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	emoji = normalizeReaction(emoji)
	if _, ok := allowedReactions[emoji]; !ok {
		return &actions.ToolError{Msg: notAllowedReaction}
	}
	reaction, err := json.Marshal([]map[string]string{{"type": "emoji", "emoji": emoji}})
	if err != nil {
		return err
	}
	_, err = b.api.MakeRequest("setMessageReaction", tgbotapi.Params{
		"chat_id":    in.ChatID,
		"message_id": in.MessageID,
		"reaction":   string(reaction),
		"is_big":     "false",
	})
	return err
}

// normalizeReaction trims the input and drops variation selectors, which the allowed set omits.
// Input that is too long to be any reaction is returned empty.
func normalizeReaction(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxReactionInput {
		return ""
	}
	return strings.ReplaceAll(s, "️", "")
}
