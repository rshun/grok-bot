package discord

import (
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/rshun/grok-bot/internal/platform"
)

func TestMessageInbound(t *testing.T) {
	in, ok := messageInbound(&discordgo.MessageCreate{Message: &discordgo.Message{
		ChannelID: "c1",
		Content:   " hello ",
		Author:    &discordgo.User{ID: "42"},
	}}, "bot")
	if !ok || in.Platform != platform.Discord || in.ChatID != "c1" || in.UserID != "42" || in.Text != "hello" || !in.TextMessage {
		t.Fatalf("ok=%v in=%#v", ok, in)
	}

	if _, ok := messageInbound(&discordgo.MessageCreate{Message: &discordgo.Message{
		Content: "hi",
		Author:  &discordgo.User{ID: "42", Bot: true},
	}}, "bot"); ok {
		t.Fatal("accepted a bot")
	}
	if _, ok := messageInbound(&discordgo.MessageCreate{Message: &discordgo.Message{
		Content: "hi",
		Author:  &discordgo.User{ID: "bot"},
	}}, "bot"); ok {
		t.Fatal("accepted self")
	}
	if _, ok := messageInbound(&discordgo.MessageCreate{Message: &discordgo.Message{
		GuildID: "g1",
		Content: "hi",
		Author:  &discordgo.User{ID: "42"},
	}}, "bot"); ok {
		t.Fatal("accepted a guild message")
	}

	in, ok = messageInbound(&discordgo.MessageCreate{Message: &discordgo.Message{
		ChannelID:   "c1",
		Content:     "caption",
		Author:      &discordgo.User{ID: "42"},
		Attachments: []*discordgo.MessageAttachment{{ID: "a"}},
	}}, "bot")
	if !ok || in.TextMessage {
		t.Fatalf("ok=%v in=%#v", ok, in)
	}
	if _, ok := messageInbound(&discordgo.MessageCreate{Message: &discordgo.Message{
		Content: "   ",
		Author:  &discordgo.User{ID: "42"},
	}}, "bot"); ok {
		t.Fatal("accepted empty text")
	}
}

func TestCommandText(t *testing.T) {
	if got := commandText(discordgo.ApplicationCommandInteractionData{Name: "usage"}); got != "/usage" {
		t.Fatalf("%q", got)
	}
	if got := commandText(discordgo.ApplicationCommandInteractionData{Name: "model"}); got != "/model" {
		t.Fatalf("%q", got)
	}
	got := commandText(discordgo.ApplicationCommandInteractionData{
		Name: "model",
		Options: []*discordgo.ApplicationCommandInteractionDataOption{
			{Name: "name", Type: discordgo.ApplicationCommandOptionString, Value: " grok-4.6 "},
		},
	})
	if got != "/model grok-4.6" {
		t.Fatalf("%q", got)
	}
}
