package discord

import (
	"context"
	"log/slog"
	"strconv"
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
		{
			Name:        "dir",
			Description: "查看或管理 Grok 可以读写的目录",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "list", Description: "查看开放的目录"},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "add",
					Description: "开放一个目录",
					Options: []*discordgo.ApplicationCommandOption{
						{
							Type:        discordgo.ApplicationCommandOptionString,
							Name:        "mode",
							Description: "权限",
							Required:    true,
							Choices: []*discordgo.ApplicationCommandOptionChoice{
								{Name: "只读", Value: "ro"},
								{Name: "可读写", Value: "rw"},
							},
						},
						{Type: discordgo.ApplicationCommandOptionString, Name: "path", Description: "绝对路径", Required: true},
					},
				},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "del",
					Description: "移除一个目录",
					Options: []*discordgo.ApplicationCommandOption{
						{Type: discordgo.ApplicationCommandOptionString, Name: "path", Description: "绝对路径", Required: true},
					},
				},
			},
		},
		{
			Name:        "task",
			Description: "管理定时任务",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "add",
					Description: "用一句话创建定时任务",
					Options: []*discordgo.ApplicationCommandOption{
						{Type: discordgo.ApplicationCommandOptionString, Name: "text", Description: "例如：每天早上九点总结科技新闻", Required: true},
					},
				},
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "list", Description: "查看定时任务"},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "del",
					Description: "删除定时任务",
					Options: []*discordgo.ApplicationCommandOption{
						{Type: discordgo.ApplicationCommandOptionInteger, Name: "id", Description: "任务编号", Required: true},
					},
				},
			},
		},
		{
			Name:        "shell",
			Description: "查看或开关服务器命令执行权限",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "mode",
					Description: "开启或关闭命令执行",
					Required:    false,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "开启", Value: "on"},
						{Name: "关闭", Value: "off"},
					},
				},
			},
		},
		{
			Name:        "memory",
			Description: "管理永久保留的长期记忆（/reset 后仍保留）",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "list", Description: "查看长期记忆"},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "add",
					Description: "添加一条长期记忆",
					Options: []*discordgo.ApplicationCommandOption{
						{Type: discordgo.ApplicationCommandOptionString, Name: "text", Description: "例如：每次应答前面都要加上xxx", Required: true},
					},
				},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "del",
					Description: "删除一条长期记忆",
					Options: []*discordgo.ApplicationCommandOption{
						{Type: discordgo.ApplicationCommandOptionInteger, Name: "id", Description: "记忆编号", Required: true},
					},
				},
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "clear", Description: "清空所有长期记忆"},
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
	b.handler(b.context(), in, &messageReply{Bot: b, messageID: in.MessageID})
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
	// Discord requires acknowledgement before queued commands start running.
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	}); err != nil {
		slog.Warn("discord interaction acknowledgement failed")
		return
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
	return b.send(ctx, chatID, text, "")
}

// messageReply keeps the triggering message with its queued reply.
type messageReply struct {
	*Bot
	messageID string
}

func (r *messageReply) Send(ctx context.Context, chatID, text string) error {
	return r.Bot.send(ctx, chatID, text, r.messageID)
}

func (b *Bot) send(ctx context.Context, chatID, text, messageID string) error {
	for _, chunk := range platform.Split(text, chunkLimit) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := b.sendOne(ctx, chatID, chunk, messageID); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bot) MarkReceived(ctx context.Context, in platform.Inbound) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return b.session.ChannelTyping(in.ChatID, discordgo.WithContext(ctx))
}

// interactionReply replaces the deferred response with the first result.
// Additional chunks are sent as follow-ups.
type interactionReply struct {
	session     *discordgo.Session
	interaction *discordgo.Interaction
	responded   bool
}

func (r *interactionReply) Send(ctx context.Context, _, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	chunks := platform.Split(text, chunkLimit)
	if !r.responded {
		_, err := r.session.InteractionResponseEdit(r.interaction, &discordgo.WebhookEdit{
			Content:         &chunks[0],
			AllowedMentions: nobody(),
		}, discordgo.WithContext(ctx))
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
			Flags:           discordgo.MessageFlagsEphemeral | discordgo.MessageFlagsSuppressEmbeds,
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
	case "shell":
		for _, opt := range data.Options {
			if opt.Name == "mode" {
				if mode := strings.TrimSpace(opt.StringValue()); mode != "" {
					return "/shell " + mode
				}
			}
		}
		return "/shell"
	case "dir", "task", "memory":
		return subcommandText(data)
	default:
		if data.Name == "" {
			return "/"
		}
		return "/" + data.Name
	}
}

// subcommandText maps /dir, /task, and /memory subcommands to the text commands the core understands.
func subcommandText(data discordgo.ApplicationCommandInteractionData) string {
	base := "/" + data.Name
	if len(data.Options) == 0 {
		return base
	}
	sub := data.Options[0]
	values := map[string]string{}
	for _, opt := range sub.Options {
		switch opt.Type {
		case discordgo.ApplicationCommandOptionInteger:
			values[opt.Name] = strconv.FormatInt(opt.IntValue(), 10)
		case discordgo.ApplicationCommandOptionString:
			values[opt.Name] = strings.TrimSpace(opt.StringValue())
		}
	}
	switch {
	case data.Name == "dir" && sub.Name == "add":
		return strings.Join([]string{base, "add", values["mode"], values["path"]}, " ")
	case sub.Name == "del" && data.Name == "dir":
		return base + " del " + values["path"]
	case sub.Name == "del":
		return base + " del " + values["id"]
	case data.Name == "task" && sub.Name == "add":
		return base + " " + values["text"]
	case data.Name == "memory" && sub.Name == "add":
		return base + " add " + values["text"]
	case (data.Name == "task" || data.Name == "memory") && sub.Name == "list":
		return base + " list"
	case data.Name == "memory" && sub.Name == "clear":
		return base + " clear"
	default:
		return base
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
	images, files, skipped := splitAttachments(m.Attachments)
	other := len(m.StickerItems) > 0 || m.Poll != nil || m.Flags&discordgo.MessageFlagsIsVoiceMessage != 0
	media := len(m.Attachments) > 0 || other
	if text == "" && !media {
		return platform.Inbound{}, false
	}
	var replyText string
	if m.ReferencedMessage != nil {
		replyText = strings.TrimSpace(m.ReferencedMessage.Content)
	}
	return platform.Inbound{
		Platform:    platform.Discord,
		ChatID:      m.ChannelID,
		MessageID:   m.ID,
		UserID:      m.Author.ID,
		Text:        text,
		ReplyToText: replyText,
		TextMessage: len(images) > 0 || len(files) > 0 || (text != "" && !media),
		Images:      images,
		Files:       files,
		Skipped:     skipped || other,
	}, true
}
