package core

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/platform"
	"github.com/rshun/grok-bot/internal/store"
)

type fakeEngine struct {
	mu       sync.Mutex
	calls    []grok.ReplyOptions
	block    chan struct{}
	replyFor func(grok.ReplyOptions) (grok.HeadlessResult, error)
}

func (f *fakeEngine) Reply(ctx context.Context, opts grok.ReplyOptions) (grok.HeadlessResult, error) {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return grok.HeadlessResult{}, ctx.Err()
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, opts)
	f.mu.Unlock()
	if f.replyFor != nil {
		return f.replyFor(opts)
	}
	return grok.HeadlessResult{Text: "pong", SessionID: opts.SessionID}, nil
}

func (f *fakeEngine) Models(context.Context) (grok.ModelList, error) {
	return grok.ModelList{Default: "grok-4.7", Names: []string{"grok-4.7", "grok-4.6"}}, nil
}

func (f *fakeEngine) Allowance(context.Context) (grok.Allowance, error) {
	return grok.Allowance{Tier: "GrokPro", UsedPercent: 2, HasUsedPercent: true, PeriodKind: "USAGE_PERIOD_TYPE_WEEKLY"}, nil
}

type recorder struct {
	mu   sync.Mutex
	msgs []string
}

func (r *recorder) Send(context.Context, string, string) error {
	return nil
}

func (r *recorder) sendText(_ context.Context, _, text string) error {
	r.mu.Lock()
	r.msgs = append(r.msgs, text)
	r.mu.Unlock()
	return nil
}

func (r *recorder) wait(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		if len(r.msgs) >= n {
			out := append([]string(nil), r.msgs...)
			r.mu.Unlock()
			return out
		}
		r.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t.Fatalf("got %d messages, want %d: %#v", len(r.msgs), n, r.msgs)
	return nil
}

type capture struct {
	rec *recorder
}

func (c capture) Send(ctx context.Context, chatID, text string) error {
	return c.rec.sendText(ctx, chatID, text)
}

func TestAckThenResult(t *testing.T) {
	svc, engine := testService(t)
	rec := &recorder{}
	svc.Handle(context.Background(), platform.Inbound{
		Platform: "telegram", ChatID: "9", UserID: "1", Text: "hello", TextMessage: true,
	}, capture{rec})
	got := rec.wait(t, 2)
	if got[0] != ackText || got[1] != "pong" {
		t.Fatalf("%#v", got)
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.calls) != 1 || !engine.calls[0].NewSession {
		t.Fatalf("%#v", engine.calls)
	}
}

func TestQueueReportsAhead(t *testing.T) {
	svc, engine := testService(t)
	engine.block = make(chan struct{})
	rec := &recorder{}
	in := func(text string) {
		svc.Handle(context.Background(), platform.Inbound{
			Platform: "telegram", ChatID: "9", UserID: "1", Text: text, TextMessage: true,
		}, capture{rec})
	}
	in("one")
	rec.wait(t, 1)
	in("two")
	got := rec.wait(t, 2)
	if got[1] != ackText+"前面还有 1 条。" {
		t.Fatalf("%#v", got)
	}
	close(engine.block)
	got = rec.wait(t, 4)
	if got[2] != "pong" || got[3] != "pong" {
		t.Fatalf("%#v", got)
	}
}

func TestResetClearsSession(t *testing.T) {
	svc, engine := testService(t)
	rec := &recorder{}
	send := func(text string) {
		svc.Handle(context.Background(), platform.Inbound{
			Platform: "telegram", ChatID: "9", UserID: "1", Text: text, TextMessage: true,
		}, capture{rec})
	}
	send("hello")
	rec.wait(t, 2)
	send("/reset")
	rec.wait(t, 4)
	send("again")
	rec.wait(t, 6)
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.calls) != 2 || !engine.calls[0].NewSession || !engine.calls[1].NewSession {
		t.Fatalf("%#v", engine.calls)
	}
	if engine.calls[0].SessionID == engine.calls[1].SessionID {
		t.Fatal("reset reused the session")
	}
}

func TestUsageAndModel(t *testing.T) {
	svc, _ := testService(t)
	rec := &recorder{}
	svc.Handle(context.Background(), platform.Inbound{
		Platform: "telegram", ChatID: "9", UserID: "1", Text: "/usage", TextMessage: true,
	}, capture{rec})
	got := rec.wait(t, 2)
	if got[1] == "" || !strings.Contains(got[1], "本期剩余：98%") {
		t.Fatalf("%#v", got)
	}
	svc.Handle(context.Background(), platform.Inbound{
		Platform: "telegram", ChatID: "9", UserID: "1", Text: "/model grok-4.6", TextMessage: true,
	}, capture{rec})
	got = rec.wait(t, 4)
	if got[3] != "已切换到 grok-4.6。下一条消息开始使用。" {
		t.Fatalf("%#v", got)
	}
}

func TestRejectsOtherUsers(t *testing.T) {
	svc, _ := testService(t)
	rec := &recorder{}
	svc.Handle(context.Background(), platform.Inbound{
		Platform: "telegram", ChatID: "9", UserID: "2", Text: "hi", TextMessage: true,
	}, capture{rec})
	got := rec.wait(t, 1)
	if got[0] != "没有权限。" {
		t.Fatalf("%#v", got)
	}
}

func testService(t *testing.T) (*Service, *fakeEngine) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "data"), "grok-4.7")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	engine := &fakeEngine{}
	return &Service{
		Store:  db,
		Engine: engine,
		AllowedUsers: map[string]map[string]struct{}{
			"telegram": {"1": {}},
		},
		QueueLimit: 8,
	}, engine
}

func TestAllowlistIsPerPlatform(t *testing.T) {
	svc, _ := testService(t)
	svc.AllowedUsers["discord"] = map[string]struct{}{"7": {}}
	rec := &recorder{}
	svc.Handle(context.Background(), platform.Inbound{
		Platform: "discord", ChatID: "c", UserID: "1", Text: "hi", TextMessage: true,
	}, capture{rec})
	got := rec.wait(t, 1)
	if got[0] != "没有权限。" {
		t.Fatalf("%#v", got)
	}
	svc.Handle(context.Background(), platform.Inbound{
		Platform: "discord", ChatID: "c", UserID: "7", Text: "hi", TextMessage: true,
	}, capture{rec})
	got = rec.wait(t, 3)
	if got[1] != ackText || got[2] != "pong" {
		t.Fatalf("%#v", got)
	}
}
