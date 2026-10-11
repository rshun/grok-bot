package discord

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/rshun/grok-bot/internal/platform"
)

type receiptTransport func(*http.Request) (*http.Response, error)

func (f receiptTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestSlashDefersBeforeWorkAndEditsResult(t *testing.T) {
	for _, accepted := range []bool{true, false} {
		t.Run(map[bool]string{true: "accepted", false: "rejected"}[accepted], func(t *testing.T) {
			s, err := discordgo.New("Bot your_token_here")
			if err != nil {
				t.Fatal(err)
			}
			calls, handled := 0, false
			s.Client = &http.Client{Transport: receiptTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				var body struct {
					Type    int    `json:"type"`
					Content string `json:"content"`
					Data    struct {
						Flags int `json:"flags"`
					} `json:"data"`
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				status, response := http.StatusOK, `{"id":"7","channel_id":"9"}`
				switch calls {
				case 1:
					if handled || req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/callback") || body.Type != 5 || body.Data.Flags != 64 {
						t.Fatal("command started without deferred acknowledgement")
					}
					if !accepted {
						status, response = http.StatusBadRequest, `{"message":"synthetic failure","code":0}`
					}
				case 2:
					if !handled || req.Method != http.MethodPatch || !strings.HasSuffix(req.URL.Path, "/messages/@original") || body.Content != "pong" {
						t.Fatal("result did not replace the deferred response")
					}
				default:
					t.Fatal("unexpected extra receipt or reply")
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
			})}
			b := &Bot{userID: "bot", handler: func(ctx context.Context, in platform.Inbound, out platform.Replier) {
				handled = true
				if in.Text != "/help" {
					t.Fatalf("unexpected command: %q", in.Text)
				}
				if err := out.Send(ctx, in.ChatID, "pong"); err != nil {
					t.Fatal(err)
				}
			}}
			b.onInteraction(s, &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
				ID: "1", AppID: "2", Token: "your_token_here", ChannelID: "9",
				Type: discordgo.InteractionApplicationCommand, User: &discordgo.User{ID: "42"},
				Data: discordgo.ApplicationCommandInteractionData{Name: "help"},
			}})
			if handled != accepted || calls != map[bool]int{true: 2, false: 1}[accepted] {
				t.Fatalf("handled=%v calls=%d", handled, calls)
			}
		})
	}
}

func TestMarkReceivedUsesTyping(t *testing.T) {
	s, err := discordgo.New("Bot your_token_here")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.Client = &http.Client{Transport: receiptTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/channels/9/typing") {
			t.Fatal("unexpected typing request")
		}
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	b := &Bot{session: s}
	if err := b.MarkReceived(context.Background(), platform.Inbound{ChatID: "9"}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("unexpected request count: %d", calls)
	}
}
