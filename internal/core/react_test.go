package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rshun/grok-bot/internal/actions"
	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/platform"
)

// reactingOut is a progress-capable replier that can also react.
type reactingOut struct {
	fakeProgressOut
	reactErr error
}

func (r reactingOut) React(_ context.Context, in platform.Inbound, emoji string) error {
	r.ev.add("react:" + emoji + "@" + in.MessageID)
	return r.reactErr
}

// reactingPlain has no progress message: results go through Send.
type reactingPlain struct{ ev *progressEvents }

func (r reactingPlain) Send(_ context.Context, _, text string) error {
	r.ev.add("send:" + text)
	return nil
}

func (r reactingPlain) MarkReceived(context.Context, platform.Inbound) error {
	r.ev.add("receipt")
	return nil
}

func (r reactingPlain) React(_ context.Context, in platform.Inbound, emoji string) error {
	r.ev.add("react:" + emoji + "@" + in.MessageID)
	return nil
}

// callReact calls the react tool the way grok does and returns the tool result text.
func callReact(t *testing.T, ts *httptest.Server, token, emoji string) (text string, isError bool) {
	t.Helper()
	args, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "react", "arguments": map[string]string{"emoji": emoji}},
	})
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(string(args)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var reply struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		t.Fatal(err)
	}
	if len(reply.Result.Content) != 1 {
		t.Fatalf("unexpected tool reply: %+v", reply)
	}
	return reply.Result.Content[0].Text, reply.Result.IsError
}

// reactThenReply makes the fake engine react once and answer with text.
func reactThenReply(t *testing.T, engine *fakeEngine, ts *httptest.Server, emoji, text string) {
	t.Helper()
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		if !opts.React {
			t.Error("react tool not offered")
		}
		callReact(t, ts, tokenFrom(t, opts.Env), emoji)
		return grok.HeadlessResult{Text: text, SessionID: opts.SessionID}, nil
	}
}

func handleMessage(svc *Service, out platform.Replier, text string) {
	svc.Handle(context.Background(), platform.Inbound{
		Platform: "telegram", ChatID: "9", MessageID: "5", UserID: "1", Text: text, TextMessage: true,
	}, out)
}

func joined(events []string) string { return strings.Join(events, "|") }

func TestReactOnlyDeletesProgress(t *testing.T) {
	svc, engine, ts := actionsService(t)
	svc.ProgressInterval = time.Nanosecond
	reactThenReply(t, engine, ts, "👍", noReply)
	ev := &progressEvents{}
	handleMessage(svc, reactingOut{fakeProgressOut: fakeProgressOut{ev: ev}}, "谢谢")
	ev.waitFor(t, 4)
	time.Sleep(50 * time.Millisecond)
	want := "receipt|start:处理中…|react:👍@5|delete"
	if got := joined(ev.snapshot()); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	// The new session is kept even though there was no text.
	chat, err := svc.Store.Chat(context.Background(), "telegram", "9")
	if err != nil {
		t.Fatal(err)
	}
	engine.mu.Lock()
	sent := engine.calls[0].SessionID
	engine.mu.Unlock()
	if sent == "" || chat.SessionID != sent {
		t.Fatalf("session not saved: stored %q, sent %q", chat.SessionID, sent)
	}
}

func TestReactOnlyWithEmptyTextDeletesProgress(t *testing.T) {
	svc, engine, ts := actionsService(t)
	svc.ProgressInterval = time.Nanosecond
	reactThenReply(t, engine, ts, "🔥", "  \n")
	ev := &progressEvents{}
	handleMessage(svc, reactingOut{fakeProgressOut: fakeProgressOut{ev: ev}}, "好耶")
	ev.waitFor(t, 4)
	time.Sleep(50 * time.Millisecond)
	if got, want := joined(ev.snapshot()), "receipt|start:处理中…|react:🔥@5|delete"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestReactOnlyWithoutProgressSendsNothing(t *testing.T) {
	svc, engine, ts := actionsService(t)
	reactThenReply(t, engine, ts, "👍", " "+noReply+"\n")
	ev := &progressEvents{}
	out := reactingPlain{ev: ev}
	handleMessage(svc, out, "谢谢")
	ev.waitFor(t, 2)
	// The lane is serial: once the next answer arrives, the first turn has finished.
	engine.replyFor = nil
	handleMessage(svc, out, "再来")
	ev.waitFor(t, 4)
	time.Sleep(50 * time.Millisecond)
	if got, want := joined(ev.snapshot()), "receipt|react:👍@5|receipt|send:pong"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

// grantService is a service with both a sandbox and the chat-action server.
func grantService(t *testing.T) (*Service, *fakeEngine, *httptest.Server) {
	t.Helper()
	svc, engine, _ := sandboxService(t)
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

func TestReactOnlyKeepsGrantNote(t *testing.T) {
	svc, engine, ts := grantService(t)
	svc.ProgressInterval = time.Nanosecond
	reactThenReply(t, engine, ts, "👍", noReply)

	// With a progress message the confirmation replaces it; it is not deleted.
	dir, real := realDir(t)
	ev := &progressEvents{}
	handleMessage(svc, reactingOut{fakeProgressOut: fakeProgressOut{ev: ev}}, "允许写入 "+dir)
	ev.waitFor(t, 4)
	time.Sleep(50 * time.Millisecond)
	if got, want := joined(ev.snapshot()), "receipt|start:处理中…|react:👍@5|finish:已允许写入 "+real+"。"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}

	// Without one the confirmation is sent on its own.
	dir2, real2 := realDir(t)
	ev2 := &progressEvents{}
	handleMessage(svc, reactingPlain{ev: ev2}, "允许写入 "+dir2)
	ev2.waitFor(t, 3)
	time.Sleep(50 * time.Millisecond)
	if got, want := joined(ev2.snapshot()), "receipt|react:👍@5|send:已允许写入 "+real2+"。"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestNoReplyWithoutReactIsEmpty(t *testing.T) {
	svc, engine, _ := actionsService(t)
	svc.ProgressInterval = time.Nanosecond
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		return grok.HeadlessResult{Text: noReply, SessionID: opts.SessionID}, nil
	}
	ev := &progressEvents{}
	handleMessage(svc, reactingOut{fakeProgressOut: fakeProgressOut{ev: ev}}, "你好")
	ev.waitFor(t, 3)
	time.Sleep(50 * time.Millisecond)
	if got, want := joined(ev.snapshot()), "receipt|start:处理中…|finish:"+emptyReplyText; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestFailedReactWithMarkerIsEmpty(t *testing.T) {
	svc, engine, ts := actionsService(t)
	svc.ProgressInterval = time.Nanosecond
	var result string
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		result, _ = callReact(t, ts, tokenFrom(t, opts.Env), "👍")
		return grok.HeadlessResult{Text: noReply, SessionID: opts.SessionID}, nil
	}
	ev := &progressEvents{}
	handleMessage(svc, reactingOut{fakeProgressOut: fakeProgressOut{ev: ev}, reactErr: errors.New("boom")}, "你好")
	ev.waitFor(t, 4)
	time.Sleep(50 * time.Millisecond)
	if result != "could not react" {
		t.Fatalf("grok was told %q", result)
	}
	if got, want := joined(ev.snapshot()), "receipt|start:处理中…|react:👍@5|finish:"+emptyReplyText; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestReactToolErrorReachesGrok(t *testing.T) {
	svc, engine, ts := actionsService(t)
	var result string
	var isErr bool
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		result, isErr = callReact(t, ts, tokenFrom(t, opts.Env), "👍🏻")
		return grok.HeadlessResult{Text: "ok", SessionID: opts.SessionID}, nil
	}
	ev := &progressEvents{}
	out := reactToolErrOut{reactingPlain{ev: ev}}
	handleMessage(svc, out, "你好")
	ev.waitFor(t, 2)
	if !isErr || result != "not an allowed reaction" {
		t.Fatalf("grok was told %q (error=%v)", result, isErr)
	}
}

// reactToolErrOut refuses every emoji with a message meant for Grok.
type reactToolErrOut struct{ reactingPlain }

func (reactToolErrOut) React(context.Context, platform.Inbound, string) error {
	return &actions.ToolError{Msg: "not an allowed reaction"}
}

func TestMarkerInsideTextKept(t *testing.T) {
	svc, engine, ts := actionsService(t)
	svc.ProgressInterval = time.Nanosecond
	reactThenReply(t, engine, ts, "👍", "好的 [[NO_REPLY]] 呀")
	ev := &progressEvents{}
	handleMessage(svc, reactingOut{fakeProgressOut: fakeProgressOut{ev: ev}}, "你好")
	ev.waitFor(t, 4)
	time.Sleep(50 * time.Millisecond)
	if got, want := joined(ev.snapshot()), "receipt|start:处理中…|react:👍@5|finish:好的 [[NO_REPLY]] 呀"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestReactWithTextStillAnswers(t *testing.T) {
	svc, engine, ts := actionsService(t)
	svc.ProgressInterval = time.Nanosecond
	reactThenReply(t, engine, ts, "👍", "收到")
	ev := &progressEvents{}
	handleMessage(svc, reactingOut{fakeProgressOut: fakeProgressOut{ev: ev}}, "你好")
	ev.waitFor(t, 4)
	time.Sleep(50 * time.Millisecond)
	if got, want := joined(ev.snapshot()), "receipt|start:处理中…|react:👍@5|finish:收到"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestReactOnlyAfterSessionRebuildKeepsNotice(t *testing.T) {
	svc, engine, ts := actionsService(t)
	svc.ProgressInterval = time.Nanosecond
	sendChat(t, svc, "first")
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		if !opts.NewSession {
			return grok.HeadlessResult{}, &grok.RunError{Message: "session not found"}
		}
		callReact(t, ts, tokenFrom(t, opts.Env), "👍")
		return grok.HeadlessResult{Text: noReply, SessionID: opts.SessionID}, nil
	}
	ev := &progressEvents{}
	handleMessage(svc, reactingOut{fakeProgressOut: fakeProgressOut{ev: ev}}, "second")
	ev.waitFor(t, 4)
	time.Sleep(50 * time.Millisecond)
	want := "receipt|start:处理中…|react:👍@5|finish:之前的上下文已经失效，这是新的一段对话。"
	if got := joined(ev.snapshot()); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestTaskTurnHasNoReact(t *testing.T) {
	svc, _, _ := actionsService(t)
	ev := &progressEvents{}
	out := reactingPlain{ev: ev}

	turn, _, end := svc.beginTurn(platform.Inbound{Platform: "telegram", ChatID: "9"}, out, false)
	end()
	if turn == nil || turn.React != nil {
		t.Fatal("a turn with no message must not offer react")
	}
	turn, _, end = svc.beginTurn(platform.Inbound{Platform: "telegram", ChatID: "9", MessageID: "5"}, out, false)
	end()
	if turn == nil || turn.React == nil {
		t.Fatal("a chat message on a reacting platform must offer react")
	}
	turn, _, end = svc.beginTurn(platform.Inbound{Platform: "telegram", ChatID: "9", MessageID: "5"}, capture{&recorder{}}, false)
	end()
	if turn == nil || turn.React != nil {
		t.Fatal("a replier that cannot react must not offer react")
	}

	// End to end: a scheduled run reaches the engine without the react tool.
	engine := svc.Engine.(*fakeEngine)
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		if opts.React {
			t.Error("task run offered react")
		}
		return grok.HeadlessResult{Text: "done", SessionID: opts.SessionID}, nil
	}
	svc.Senders = map[string]platform.Replier{"telegram": out}
	svc.Location = taskZone
	svc.Now = func() time.Time { return taskNow }
	addTask(t, svc, "9", dailyNews, "每天 09:00：看新闻", taskNow.Add(-10*time.Second))
	svc.runDue(context.Background())
	ev.waitFor(t, 1)
}

func TestReactOnlyKeepsResetReminder(t *testing.T) {
	svc, engine, ts := grantService(t)
	svc.ProgressInterval = time.Nanosecond
	if err := svc.Store.SetSession(context.Background(), "telegram", "9", "old"); err != nil {
		t.Fatal(err)
	}
	reactThenReply(t, engine, ts, "👍", noReply)
	dir, _ := realDir(t)

	// With a progress message the reminder replaces it.
	ev := &progressEvents{}
	handleMessage(svc, reactingOut{fakeProgressOut: fakeProgressOut{ev: ev}}, "看看 "+dir)
	ev.waitFor(t, 4)
	time.Sleep(50 * time.Millisecond)
	if got, want := joined(ev.snapshot()), "receipt|start:处理中…|react:👍@5|finish:"+resetReminder; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}

	// Without one it is sent on its own.
	ev2 := &progressEvents{}
	handleMessage(svc, reactingPlain{ev: ev2}, "看看 "+dir)
	ev2.waitFor(t, 3)
	time.Sleep(50 * time.Millisecond)
	if got, want := joined(ev2.snapshot()), "receipt|react:👍@5|send:"+resetReminder; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
