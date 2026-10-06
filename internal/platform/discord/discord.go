package discord

import (
	"context"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/rshun/grok-bot/internal/platform"
)

const chunkLimit = 2000

// Bot receives Discord direct messages and the same text commands as Telegram.
type Bot struct {
	session  *discordgo.Session
	userID   string
	username string

	ctx     context.Context
	handler func(context.Context, platform.Inbound, platform.Replier)
}

func New(token string) (*Bot, error) {
	token = strings.TrimSpace(token)
	token = strings.TrimPrefix(token, "Bot ")
	session, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	// Direct messages include their text without the privileged message-content intent.
	session.Identify.Intents = discordgo.IntentDirectMessages
	user, err := session.User("@me")
	if err != nil {
		return nil, err
	}
	return &Bot{session: session, userID: user.ID, username: user.Username}, nil
}

func (b *Bot) Username() string {
	return b.username
}

// Run opens the gateway until ctx is cancelled. handler should return quickly.
func (b *Bot) Run(ctx context.Context, handler func(context.Context, platform.Inbound, platform.Replier)) error {
	b.ctx = ctx
	b.handler = handler
	if err := b.registerCommands(); err != nil {
		return err
	}
	b.session.AddHandler(b.onMessage)
	b.session.AddHandler(b.onInteraction)
	if err := b.session.Open(); err != nil {
		return err
	}
	defer b.session.Close()
	<-ctx.Done()
	return nil
}

func (b *Bot) registerCommands() error {
	_, err := b.session.ApplicationCommandBulkOverwrite(b.userID, "", []*discordgo.ApplicationCommand{
		{Name: "help", Description: "查看说明"},
		{Name: "usage", Description: "查看订阅剩余额度"},
		{Name: "reset", Description: "清空这个聊天的上下文"},
		{
			Name:        "model",
			Description: "查看或切换这个聊天使用的模型",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "name",
					Description: "要切换的模型名",
					Required:    false,
				},
			},
		},
	})
	return err
}

func (b *Bot) onMessage(_ *discordgo.Session, m *discordgo.MessageCreate) {
	in, ok := messageInbound(m, b.userID)
	if !ok || b.handler == nil {
		return
	}
	b.handler(b.context(), in, b)
}

func (b *Bot) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i == nil || i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	if i.GuildID != "" {
		_ = s.InteractionRespond(i.Interaction, privateOnlyResponse())
		return
	}
	user := interactionUser(i)
	if user == nil || user.Bot || user.ID == "" || user.ID == b.userID || b.handler == nil {
		return
	}
	in := platform.Inbound{
		Platform:    platform.Discord,
		ChatID:      i.ChannelID,
		UserID:      user.ID,
		Text:        commandText(i.ApplicationCommandData()),
		TextMessage: true,
	}
	b.handler(b.context(), in, &interactionReply{session: s, interaction: i.Interaction})
}

func (b *Bot) context() context.Context {
	if b.ctx != nil {
		return b.ctx
	}
	return context.Background()
}

func (b *Bot) Send(ctx context.Context, chatID, text string) error {
	for _, chunk := range platform.Split(text, chunkLimit) {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := b.session.ChannelMessageSendComplex(chatID, &discordgo.MessageSend{
			Content:         chunk,
			Flags:           discordgo.MessageFlagsSuppressEmbeds,
			AllowedMentions: nobody(),
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// interactionReply turns the core's first send into the interaction response
// and every later send into a follow-up. Discord requires that response.
type interactionReply struct {
	session     *discordgo.Session
	interaction *discordgo.Interaction
	responded   bool
}

func (r *interactionReply) Send(ctx context.Context, _, text string) error {
	chunks := platform.Split(text, chunkLimit)
	if !r.responded {
		err := r.session.InteractionRespond(r.interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content:         chunks[0],
				Flags:           discordgo.MessageFlagsSuppressEmbeds,
				AllowedMentions: nobody(),
			},
		})
		r.responded = err == nil
		if err != nil {
			return err
		}
		chunks = chunks[1:]
	}
	for _, chunk := range chunks {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := r.session.FollowupMessageCreate(r.interaction, true, &discordgo.WebhookParams{
			Content:         chunk,
			Flags:           discordgo.MessageFlagsSuppressEmbeds,
			AllowedMentions: nobody(),
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func privateOnlyResponse() *discordgo.InteractionResponse {
	return &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: "只处理私聊。",
			Flags:   discordgo.MessageFlagsEphemeral | discordgo.MessageFlagsSuppressEmbeds,
		},
	}
}

func nobody() *discordgo.MessageAllowedMentions {
	return &discordgo.MessageAllowedMentions{}
}

func interactionUser(i *discordgo.InteractionCreate) *discordgo.User {
	if i.User != nil {
		return i.User
	}
	if i.Member != nil {
		return i.Member.User
	}
	return nil
}

func commandText(data discordgo.ApplicationCommandInteractionData) string {
	switch data.Name {
	case "model":
		for _, opt := range data.Options {
			if opt.Name != "name" {
				continue
			}
			name := strings.TrimSpace(opt.StringValue())
			if name != "" {
				return "/model " + name
			}
		}
		return "/model"
	default:
		if data.Name == "" {
			return "/"
		}
		return "/" + data.Name
	}
}

func messageInbound(m *discordgo.MessageCreate, selfID string) (platform.Inbound, bool) {
	if m == nil || m.Message == nil || m.Author == nil {
		return platform.Inbound{}, false
	}
	if m.Author.Bot || m.Author.ID == "" || m.Author.ID == selfID || m.WebhookID != "" {
		return platform.Inbound{}, false
	}
	if m.GuildID != "" {
		return platform.Inbound{}, false
	}
	switch m.Type {
	case discordgo.MessageTypeDefault, discordgo.MessageTypeReply:
	default:
		return platform.Inbound{}, false
	}
	text := strings.TrimSpace(m.Content)
	media := len(m.Attachments) > 0 || len(m.StickerItems) > 0 || m.Poll != nil || m.Flags&discordgo.MessageFlagsIsVoiceMessage != 0
	if text == "" && !media {
		return platform.Inbound{}, false
	}
	return platform.Inbound{
		Platform:    platform.Discord,
		ChatID:      m.ChannelID,
		UserID:      m.Author.ID,
		Text:        text,
		TextMessage: text != "" && !media,
	}, true
}
