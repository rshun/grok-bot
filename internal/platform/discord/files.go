package discord

import (
	"context"

	"github.com/bwmarrin/discordgo"

	"github.com/rshun/grok-bot/internal/platform"
)

// FileLimit is the largest file in bytes a bot can attach in a direct message; Discord's default upload limit for bots is 10 MiB.
func (b *Bot) FileLimit() int64 { return 10 << 20 }

// SendFile attaches one file to a new message, quoting replyTo when it is set.
func (b *Bot) SendFile(ctx context.Context, chatID, replyTo string, f platform.OutFile) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	body, _, err := f.Open()
	if err != nil {
		return err
	}
	defer body.Close()
	out := &discordgo.MessageSend{
		Content:         f.Caption,
		Files:           []*discordgo.File{{Name: f.Name, Reader: body}},
		Flags:           discordgo.MessageFlagsSuppressEmbeds,
		AllowedMentions: nobody(),
	}
	if replyTo != "" {
		failIfMissing := false
		out.Reference = &discordgo.MessageReference{
			MessageID:       replyTo,
			ChannelID:       chatID,
			FailIfNotExists: &failIfMissing,
		}
	}
	_, err = b.session.ChannelMessageSendComplex(chatID, out, discordgo.WithContext(ctx))
	return err
}
