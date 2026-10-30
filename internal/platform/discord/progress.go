package discord

import (
	"context"

	"github.com/bwmarrin/discordgo"

	"github.com/rshun/grok-bot/internal/platform"
)

// StartProgress sends a quoted progress message that Update, Finish and Delete later change.
func (r *messageReply) StartProgress(ctx context.Context, in platform.Inbound, text string) (platform.ProgressHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sent, err := r.Bot.sendOne(ctx, in.ChatID, firstChunk(text), r.messageID)
	if err != nil {
		return nil, err
	}
	return &progressMessage{bot: r.Bot, chatID: in.ChatID, replyTo: r.messageID, messageID: sent.ID}, nil
}

type progressMessage struct {
	bot       *Bot
	chatID    string
	replyTo   string
	messageID string
}

func (p *progressMessage) Update(ctx context.Context, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.edit(ctx, firstChunk(text))
}

func (p *progressMessage) Finish(ctx context.Context, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	chunks := platform.Split(text, chunkLimit)
	if err := p.edit(ctx, chunks[0]); err != nil {
		// The progress message cannot be edited, so deliver everything as new replies.
		if sendErr := p.bot.send(ctx, p.chatID, text, p.replyTo); sendErr != nil {
			return sendErr
		}
		// Best effort: do not leave a stale progress message behind.
		_ = p.bot.session.ChannelMessageDelete(p.chatID, p.messageID, discordgo.WithContext(ctx))
		return nil
	}
	for _, chunk := range chunks[1:] {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := p.bot.sendOne(ctx, p.chatID, chunk, p.replyTo); err != nil {
			return err
		}
	}
	return nil
}

func (p *progressMessage) Delete(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.bot.session.ChannelMessageDelete(p.chatID, p.messageID, discordgo.WithContext(ctx))
}

func (p *progressMessage) edit(ctx context.Context, text string) error {
	edit := discordgo.NewMessageEdit(p.chatID, p.messageID)
	edit.Content = &text
	edit.Flags = discordgo.MessageFlagsSuppressEmbeds
	edit.AllowedMentions = nobody()
	_, err := p.bot.session.ChannelMessageEditComplex(edit, discordgo.WithContext(ctx))
	return err
}

// sendOne sends a single quoted message and returns it.
func (b *Bot) sendOne(ctx context.Context, chatID, text, replyTo string) (*discordgo.Message, error) {
	out := &discordgo.MessageSend{
		Content:         text,
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
	return b.session.ChannelMessageSendComplex(chatID, out, discordgo.WithContext(ctx))
}

func firstChunk(text string) string {
	return platform.Split(text, chunkLimit)[0]
}
