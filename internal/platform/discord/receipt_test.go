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

func TestOnMessageKeepsOriginalAcrossQueuedAndLongReplies(t *testing.T) {
	s, err := discordgo.New("Bot your_token_here")
	if err != nil {
		t.Fatal(err)
	}
	var references []string
	s.Client = &http.Client{Transport: receiptTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/channels/9/messages") {
			t.Fatal("unexpected reply request")
		}
		var body struct {
			Reference       *discordgo.MessageReference       `json:"message_reference"`
			AllowedMentions *discordgo.MessageAllowedMentions `json:"allowed_mentions"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.AllowedMentions == nil || body.AllowedMentions.RepliedUser || len(body.AllowedMentions.Parse) != 0 {
			t.Fatal("reply enabled unwanted mentions")
		}
		reference := ""
		if body.Reference != nil {
			if body.Reference.ChannelID != "9" || body.Reference.FailIfNotExists == nil || *body.Reference.FailIfNotExists {
				t.Fatal("reply lost its channel or deleted-message fallback")
			}
			reference = body.Reference.MessageID
		}
		references = append(references, reference)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"20","channel_id":"9"}`))}, nil
	})}
	var replies []platform.Replier
	b := &Bot{session: s, userID: "bot", handler: func(_ context.Context, in platform.Inbound, out platform.Replier) {
		if _, ok := out.(platform.Receipt); !ok {
			t.Fatal("quoted replier lost its typing indicator")
		}
		replies = append(replies, out)
	}}
	for _, id := range []string{"17", "18"} {
		b.onMessage(s, &discordgo.MessageCreate{Message: &discordgo.Message{
			ID: id, ChannelID: "9", Content: "hello", Author: &discordgo.User{ID: "42"},
		}})
	}
	if len(replies) != 2 {
		t.Fatalf("got %d queued replies", len(replies))
	}
	ctx := context.Background()
	if err := replies[0].Send(ctx, "9", strings.Repeat("x", chunkLimit+1)); err != nil {
		t.Fatal(err)
	}
	if err := replies[1].Send(ctx, "9", "second result"); err != nil {
		t.Fatal(err)
	}
	if err := b.Send(ctx, "9", "unreferenced message"); err != nil {
		t.Fatal(err)
	}
	want := []string{"17", "17", "18", ""}
	if len(references) != len(want) {
		t.Fatalf("references=%v", references)
	}
	for i := range want {
		if references[i] != want[i] {
			t.Fatalf("references=%v, want %v", references, want)
		}
	}
}
