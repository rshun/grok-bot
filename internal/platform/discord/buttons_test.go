package discord

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"github.com/rshun/grok-bot/internal/platform"
)

func TestBuildComponentsAndTaskSubcommands(t *testing.T) {
	rows := [][]platform.Button{
		{
			{Label: "▶️ 试跑 #1", Command: "/task run 1", Style: platform.ButtonPrimary},
			{Label: "⏸ 暂停 #1", Command: "/task pause 1", Style: platform.ButtonSecondary},
			{Label: "重复命令会被自动去重", Command: "/task pause 1", Style: platform.ButtonSecondary},
			{Label: strings.Repeat("长标题", 40), Command: "/sessions 1", Style: platform.ButtonSuccess},
		},
	}
	comps := buildComponents(rows)
	if len(comps) != 1 {
		t.Fatalf("expected 1 ActionsRow, got %d", len(comps))
	}
	ar, ok := comps[0].(discordgo.ActionsRow)
	if !ok || len(ar.Components) != 3 {
		t.Fatalf("expected 3 deduplicated buttons, got %#v", comps[0])
	}
	b0 := ar.Components[0].(discordgo.Button)
	if b0.CustomID != "cmd:/task run 1" || b0.Style != discordgo.PrimaryButton {
		t.Fatalf("unexpected button 0: %#v", b0)
	}
	b2 := ar.Components[2].(discordgo.Button)
	if utf8.RuneCountInString(b2.Label) > maxDiscordLabelRunes || b2.Style != discordgo.SuccessButton {
		t.Fatalf("unexpected button 2: %#v", b2)
	}

	idOpt := &discordgo.ApplicationCommandInteractionDataOption{
		Name: "id", Type: discordgo.ApplicationCommandOptionInteger, Value: float64(5),
	}
	for subName, want := range map[string]string{
		"run":    "/task run 5",
		"pause":  "/task pause 5",
		"resume": "/task resume 5",
	} {
		got := commandText(discordgo.ApplicationCommandInteractionData{
			Name: "task",
			Options: []*discordgo.ApplicationCommandInteractionDataOption{
				{Name: subName, Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandInteractionDataOption{idOpt}},
			},
		})
		if got != want {
			t.Fatalf("subcommand %s: got %q, want %q", subName, got, want)
		}
	}
}
