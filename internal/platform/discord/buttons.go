package discord

import (
	"context"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"github.com/rshun/grok-bot/internal/platform"
)

const (
	buttonCustomIDPrefix = "cmd:"
	maxDiscordCustomID   = 100
	maxDiscordLabelRunes = 80
	maxDiscordRows       = 5
	maxDiscordRowButtons = 5
)

func discordButtonStyle(s platform.ButtonStyle) discordgo.ButtonStyle {
	switch s {
	case platform.ButtonPrimary:
		return discordgo.PrimaryButton
	case platform.ButtonSuccess:
		return discordgo.SuccessButton
	case platform.ButtonDanger:
		return discordgo.DangerButton
	default:
		return discordgo.SecondaryButton
	}
}

func truncateDiscordLabel(label string) string {
	label = strings.TrimSpace(label)
	if utf8.RuneCountInString(label) <= maxDiscordLabelRunes {
		return label
	}
	runes := []rune(label)
	return string(runes[:maxDiscordLabelRunes-1]) + "…"
}

func buildComponents(rows [][]platform.Button) []discordgo.MessageComponent {
	var out []discordgo.MessageComponent
	seen := make(map[string]bool)
	for _, row := range rows {
		if len(out) >= maxDiscordRows {
			break
		}
		var buttons []discordgo.MessageComponent
		for _, btn := range row {
			label := truncateDiscordLabel(btn.Label)
			cmd := strings.TrimSpace(btn.Command)
			if label == "" || cmd == "" {
				continue
			}
			customID := buttonCustomIDPrefix + cmd
			if len(customID) > maxDiscordCustomID || seen[customID] {
				continue
			}
			seen[customID] = true
			buttons = append(buttons, discordgo.Button{
				Label:    label,
				Style:    discordButtonStyle(btn.Style),
				CustomID: customID,
			})
			if len(buttons) >= maxDiscordRowButtons {
				break
			}
		}
		if len(buttons) > 0 {
			out = append(out, discordgo.ActionsRow{Components: buttons})
		}
	}
	return out
}

func (b *Bot) SendWithButtons(ctx context.Context, chatID, text string, rows [][]platform.Button) error {
	return b.sendWithButtons(ctx, chatID, text, "", rows)
}

func (r *messageReply) SendWithButtons(ctx context.Context, chatID, text string, rows [][]platform.Button) error {
	return r.Bot.sendWithButtons(ctx, chatID, text, r.messageID, rows)
}

func (b *Bot) sendWithButtons(ctx context.Context, chatID, text, messageID string, rows [][]platform.Button) error {
	components := buildComponents(rows)
	if len(components) == 0 {
		return b.send(ctx, chatID, text, messageID)
	}
	chunks := platform.Split(text, chunkLimit)
	for i, chunk := range chunks {
		if err := ctx.Err(); err != nil {
			return err
		}
		var comps []discordgo.MessageComponent
		if i == len(chunks)-1 {
			comps = components
		}
		if _, err := b.sendOneComponents(ctx, chatID, chunk, messageID, comps); err != nil {
			return err
		}
	}
	return nil
}

func (r *interactionReply) SendWithButtons(ctx context.Context, chatID, text string, rows [][]platform.Button) error {
	components := buildComponents(rows)
	if len(components) == 0 {
		return r.Send(ctx, chatID, text)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	chunks := platform.Split(text, chunkLimit)
	start := 0
	if !r.responded {
		edit := &discordgo.WebhookEdit{
			Content:         &chunks[0],
			AllowedMentions: nobody(),
		}
		if len(chunks) == 1 {
			edit.Components = &components
		}
		_, err := r.session.InteractionResponseEdit(r.interaction, edit, discordgo.WithContext(ctx))
		r.responded = err == nil
		if err != nil {
			return err
		}
		start = 1
	}
	for i := start; i < len(chunks); i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		params := &discordgo.WebhookParams{
			Content:         chunks[i],
			Flags:           discordgo.MessageFlagsEphemeral | discordgo.MessageFlagsSuppressEmbeds,
			AllowedMentions: nobody(),
		}
		if i == len(chunks)-1 {
			params.Components = components
		}
		if _, err := r.session.FollowupMessageCreate(r.interaction, true, params); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bot) onComponentInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.GuildID != "" {
		_ = s.InteractionRespond(i.Interaction, privateOnlyResponse())
		return
	}
	user := interactionUser(i)
	if user == nil || user.Bot || user.ID == "" || user.ID == b.userID || b.handler == nil {
		return
	}
	data := i.MessageComponentData()
	if !strings.HasPrefix(data.CustomID, buttonCustomIDPrefix) {
		return
	}
	cmd := strings.TrimSpace(strings.TrimPrefix(data.CustomID, buttonCustomIDPrefix))
	if cmd == "" {
		return
	}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredMessageUpdate,
	}); err != nil {
		slog.Warn("discord component interaction acknowledgement failed")
		return
	}
	var msgID string
	if i.Message != nil && i.Message.Flags&discordgo.MessageFlagsEphemeral == 0 {
		msgID = i.Message.ID
	}
	in := platform.Inbound{
		Platform:    platform.Discord,
		ChatID:      i.ChannelID,
		MessageID:   msgID,
		UserID:      user.ID,
		Text:        cmd,
		TextMessage: true,
	}
	b.handler(b.context(), in, &messageReply{Bot: b, messageID: msgID})
}
