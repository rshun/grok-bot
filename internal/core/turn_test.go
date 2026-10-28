package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rshun/grok-bot/internal/actions"
	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/platform"
)

func actionsService(t *testing.T) (*Service, *fakeEngine, *httptest.Server) {
	t.Helper()
	svc, engine := testService(t)
	srv, err := actions.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	svc.Actions = srv
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return svc, engine, ts
}

func tokenFrom(t *testing.T, env []string) string {
	t.Helper()
	var found string
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, actions.TokenEnv+"="); ok {
			found = v
		}
	}
	return found
}

func statusFor(t *testing.T, ts *httptest.Server, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func sendChat(t *testing.T, svc *Service, text string) {
	t.Helper()
	rec := &recorder{}
	svc.Handle(context.Background(), platform.Inbound{
		Platform: "telegram", ChatID: "9", UserID: "1", Text: text, TextMessage: true,
	}, capture{rec})
	rec.wait(t, 1)
}

func TestChatTurnPassesTokenEnv(t *testing.T) {
	svc, engine, _ := actionsService(t)
	sendChat(t, svc, "hello")
	if len(engine.calls) != 1 {
		t.Fatalf("calls: %d", len(engine.calls))
	}
	opts := engine.calls[0]
	if !opts.Actions {
		t.Fatal("Actions not set")
	}
	if opts.React {
		t.Fatal("React set before any turn callback exists")
	}
	if len(tokenFrom(t, opts.Env)) != 32 {
		t.Fatalf("token env missing: %d entries", len(opts.Env))
	}

	plain, plainEngine := testService(t)
	sendChat(t, plain, "hello")
	got := plainEngine.calls[0]
	if got.Actions || got.React || len(got.Env) != 0 {
		t.Fatalf("channel options without a server: actions=%v react=%v env=%d", got.Actions, got.React, len(got.Env))
	}
}

func TestTokenEndsAfterTurn(t *testing.T) {
	svc, engine, ts := actionsService(t)
	var mu sync.Mutex
	var token string
	var during int
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		mu.Lock()
		token = tokenFrom(t, opts.Env)
		mu.Unlock()
		during = statusFor(t, ts, token)
		return grok.HeadlessResult{Text: "pong", SessionID: opts.SessionID}, nil
	}
	sendChat(t, svc, "hello")
	if during != http.StatusOK {
		t.Fatalf("token rejected during the turn: %d", during)
	}
	// dispatch sends the reply after the turn ends, so the token must already be gone.
	if got := statusFor(t, ts, token); got != http.StatusUnauthorized {
		t.Fatalf("token still valid after the turn: %d", got)
	}
}

func TestTokenEndsAfterFailedTurn(t *testing.T) {
	svc, engine, ts := actionsService(t)
	var token string
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		token = tokenFrom(t, opts.Env)
		return grok.HeadlessResult{}, &grok.RunError{Message: "boom"}
	}
	sendChat(t, svc, "hello")
	if token == "" {
		t.Fatal("no token passed")
	}
	if got := statusFor(t, ts, token); got != http.StatusUnauthorized {
		t.Fatalf("token still valid after a failed turn: %d", got)
	}
}

func TestSessionRebuildTokensEnd(t *testing.T) {
	svc, engine, ts := actionsService(t)
	sendChat(t, svc, "first")
	var tokens []string
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		tokens = append(tokens, tokenFrom(t, opts.Env))
		if !opts.NewSession {
			return grok.HeadlessResult{}, &grok.RunError{Message: "session not found"}
		}
		return grok.HeadlessResult{Text: "fresh", SessionID: opts.SessionID}, nil
	}
	sendChat(t, svc, "second")
	if len(tokens) != 2 || tokens[0] == "" || tokens[1] == "" {
		t.Fatalf("tokens: %q", tokens)
	}
	for _, token := range tokens {
		if got := statusFor(t, ts, token); got != http.StatusUnauthorized {
			t.Fatalf("token still valid after rebuild: %d", got)
		}
	}
}

func TestTaskRunGetsToken(t *testing.T) {
	svc, engine, ts := actionsService(t)
	var token string
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		token = tokenFrom(t, opts.Env)
		if !opts.Actions || opts.React {
			t.Errorf("task options: actions=%v react=%v", opts.Actions, opts.React)
		}
		return grok.HeadlessResult{Text: "done", SessionID: opts.SessionID}, nil
	}
	rec := &recorder{}
	svc.Senders = map[string]platform.Replier{"telegram": capture{rec}}
	svc.Location = taskZone
	svc.Now = func() time.Time { return taskNow }
	addTask(t, svc, "9", dailyNews, "每天 09:00：看新闻", taskNow.Add(-10*time.Second))
	svc.runDue(context.Background())
	rec.wait(t, 1)
	if token == "" {
		t.Fatal("task run got no token")
	}
	if got := statusFor(t, ts, token); got != http.StatusUnauthorized {
		t.Fatalf("token still valid after the task: %d", got)
	}
}
