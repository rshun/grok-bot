package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/platform"
	"github.com/rshun/grok-bot/internal/store"
)

func toolUse(name, input string) grok.ToolUse {
	return grok.ToolUse{Name: name, Input: json.RawMessage(input)}
}

func TestProgressTextOneLine(t *testing.T) {
	long := toolUse("web_search", `{"query":"a\nb`+strings.Repeat("字", 200)+`"}`)
	got := progressText(long)
	if !strings.HasPrefix(got, "正在搜索：") || strings.ContainsAny(got, "\r\n") {
		t.Fatalf("%q", got)
	}
	if n := len([]rune(got)); n > len([]rune("正在搜索："))+80 {
		t.Fatalf("too long: %d", n)
	}

	cases := []struct {
		tu   grok.ToolUse
		want string
	}{
		{toolUse("web_search", `{"query":"go news"}`), "正在搜索：go news"},
		{toolUse("web_fetch", `{"url":"https://example.com/a"}`), "正在打开网页：https://example.com/a"},
		{toolUse("read_file", `{"target_file":"/tmp/a.txt"}`), "正在读取：/tmp/a.txt"},
		{toolUse("list_dir", `{"target_directory":"/tmp"}`), "正在读取：/tmp"},
		{toolUse("grep", `{"pattern":"x","path":"/src"}`), "正在读取：/src"},
		{toolUse("write", `{"file_path":"/tmp/b.txt","contents":"x"}`), "正在写入：/tmp/b.txt"},
		{toolUse("search_replace", `{"file_path":"/tmp/c.txt"}`), "正在写入：/tmp/c.txt"},
		{toolUse("use_tool", `{"tool_name":"mcp_x"}`), "正在调用工具：mcp_x"},
		{toolUse("search_tool", `{"query":"calendar"}`), "正在查找工具"},
		{toolUse("mystery", `{"query":"x"}`), "正在处理：mystery"},
		// A missing or unusable input degrades to the label.
		{toolUse("web_search", `{}`), "正在搜索"},
		{toolUse("web_fetch", ``), "正在打开网页"},
		{toolUse("read_file", `{"path":42}`), "正在读取"},
		{toolUse("write", `not json`), "正在写入"},
		{toolUse("use_tool", `{"tool_name":"  "}`), "正在调用工具"},
	}
	for _, c := range cases {
		if got := progressText(c.tu); got != c.want {
			t.Errorf("%s %s: got %q, want %q", c.tu.Name, c.tu.Input, got, c.want)
		}
	}
}

// progressEvents records everything a fake progress message receives, in order.
type progressEvents struct {
	mu      sync.Mutex
	events  []string
	startAt int
}

func (p *progressEvents) add(e string) {
	p.mu.Lock()
	p.events = append(p.events, e)
	p.mu.Unlock()
}

func (p *progressEvents) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.events...)
}

func (p *progressEvents) waitFor(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := p.snapshot(); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("got %#v, want %d events", p.snapshot(), n)
	return nil
}

type fakeProgressOut struct {
	ev       *progressEvents
	startErr error
	updErr   error
	finErr   error
}

func (f fakeProgressOut) Send(_ context.Context, _, text string) error {
	f.ev.add("send:" + text)
	return nil
}

func (f fakeProgressOut) MarkReceived(context.Context, platform.Inbound) error {
	f.ev.add("receipt")
	return nil
}

func (f fakeProgressOut) StartProgress(_ context.Context, _ platform.Inbound, text string) (platform.ProgressHandle, error) {
	if f.startErr != nil {
		f.ev.add("start-failed")
		return nil, f.startErr
	}
	f.ev.add("start:" + text)
	return fakeHandle{f}, nil
}

type fakeHandle struct{ out fakeProgressOut }

func (h fakeHandle) Update(_ context.Context, text string) error {
	h.out.ev.add("update:" + text)
	return h.out.updErr
}

func (h fakeHandle) Finish(_ context.Context, text string) error {
	h.out.ev.add("finish:" + text)
	return h.out.finErr
}

func (h fakeHandle) Delete(context.Context) error {
	h.out.ev.add("delete")
	return nil
}

func TestProgressThrottle(t *testing.T) {
	ev := &progressEvents{}
	out := fakeProgressOut{ev: ev}
	handle, _ := out.StartProgress(context.Background(), platform.Inbound{}, "处理中…")
	view := &progressView{handle: handle, platform: "telegram", interval: time.Hour}
	ctx := context.Background()
	view.Show(ctx, "正在搜索：a")
	view.Show(ctx, "正在搜索：b")
	view.Show(ctx, "正在搜索：c")
	got := ev.snapshot()
	if len(got) != 2 || got[1] != "update:正在搜索：a" {
		t.Fatalf("%#v", got)
	}

	// The same text is not sent twice even after the interval passed.
	ev2 := &progressEvents{}
	handle, _ = fakeProgressOut{ev: ev2}.StartProgress(ctx, platform.Inbound{}, "处理中…")
	view = &progressView{handle: handle, platform: "telegram", interval: time.Nanosecond}
	view.Show(ctx, "正在搜索：a")
	time.Sleep(time.Millisecond)
	view.Show(ctx, "正在搜索：a")
	time.Sleep(time.Millisecond)
	view.Show(ctx, "正在搜索：b")
	got = ev2.snapshot()
	if len(got) != 3 || got[1] != "update:正在搜索：a" || got[2] != "update:正在搜索：b" {
		t.Fatalf("%#v", got)
	}
}

func progressService(t *testing.T) (*Service, *fakeEngine) {
	svc, engine := testService(t)
	svc.ProgressInterval = time.Nanosecond
	return svc, engine
}

func handleHello(svc *Service, out platform.Replier, text string) {
	svc.Handle(context.Background(), platform.Inbound{
		Platform: "telegram", ChatID: "9", UserID: "1", Text: text, TextMessage: true,
	}, out)
}

func TestChatUsesProgressMessage(t *testing.T) {
	svc, engine := progressService(t)
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		if opts.OnTool == nil {
			return grok.HeadlessResult{}, errors.New("no OnTool")
		}
		opts.OnTool(toolUse("web_search", `{"query":"go news"}`))
		return grok.HeadlessResult{Text: "pong", SessionID: opts.SessionID}, nil
	}
	ev := &progressEvents{}
	handleHello(svc, fakeProgressOut{ev: ev}, "hello")
	got := ev.waitFor(t, 4)
	time.Sleep(50 * time.Millisecond)
	got = ev.snapshot()
	want := []string{"receipt", "start:处理中…", "update:正在搜索：go news", "finish:pong"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestChatPassesOnToolOnResumedSession(t *testing.T) {
	svc, engine := progressService(t)
	if err := svc.Store.SetSession(context.Background(), "telegram", "9", "old"); err != nil {
		t.Fatal(err)
	}
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		if opts.OnTool == nil {
			return grok.HeadlessResult{}, errors.New("no OnTool")
		}
		opts.OnTool(toolUse("web_fetch", `{"url":"https://example.com"}`))
		return grok.HeadlessResult{Text: "pong", SessionID: opts.SessionID}, nil
	}
	ev := &progressEvents{}
	handleHello(svc, fakeProgressOut{ev: ev}, "hello")
	got := ev.waitFor(t, 4)
	if got[2] != "update:正在打开网页：https://example.com" || got[3] != "finish:pong" {
		t.Fatalf("%#v", got)
	}
}

func TestCommandHasNoProgress(t *testing.T) {
	svc, _ := progressService(t)
	ev := &progressEvents{}
	handleHello(svc, fakeProgressOut{ev: ev}, "/usage")
	got := ev.waitFor(t, 1)
	time.Sleep(50 * time.Millisecond)
	got = ev.snapshot()
	if len(got) != 1 || !strings.HasPrefix(got[0], "send:") {
		t.Fatalf("%#v", got)
	}
}

func TestProgressShowsErrorOnFailure(t *testing.T) {
	svc, engine := progressService(t)
	engine.replyFor = func(grok.ReplyOptions) (grok.HeadlessResult, error) {
		return grok.HeadlessResult{}, context.DeadlineExceeded
	}
	ev := &progressEvents{}
	handleHello(svc, fakeProgressOut{ev: ev}, "hello")
	got := ev.waitFor(t, 3)
	want := "finish:" + grok.UserFacing(context.DeadlineExceeded)
	if got[2] != want {
		t.Fatalf("%#v", got)
	}
}

func TestProgressEmptyReplyFinishesWithFailure(t *testing.T) {
	svc, engine := progressService(t)
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		return grok.HeadlessResult{SessionID: opts.SessionID}, nil
	}
	ev := &progressEvents{}
	handleHello(svc, fakeProgressOut{ev: ev}, "hello")
	got := ev.waitFor(t, 3)
	if got[2] != "finish:处理失败：没有得到回复。" {
		t.Fatalf("%#v", got)
	}
}

func TestProgressStartFailureFallsBack(t *testing.T) {
	svc, _ := progressService(t)
	ev := &progressEvents{}
	handleHello(svc, fakeProgressOut{ev: ev, startErr: errors.New("synthetic start failure")}, "hello")
	got := ev.waitFor(t, 3)
	want := []string{"receipt", "start-failed", "send:pong"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestProgressFinishFailureDoesNotResend(t *testing.T) {
	svc, _ := progressService(t)
	ev := &progressEvents{}
	handleHello(svc, fakeProgressOut{ev: ev, finErr: errors.New("synthetic finish failure")}, "hello")
	ev.waitFor(t, 3)
	time.Sleep(50 * time.Millisecond)
	for _, e := range ev.snapshot() {
		if strings.HasPrefix(e, "send:") {
			t.Fatalf("resent after finish failure: %#v", ev.snapshot())
		}
	}
}

func TestProgressUpdateFailureStillFinishes(t *testing.T) {
	svc, engine := progressService(t)
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		opts.OnTool(toolUse("web_search", `{"query":"x"}`))
		return grok.HeadlessResult{Text: "pong", SessionID: opts.SessionID}, nil
	}
	ev := &progressEvents{}
	handleHello(svc, fakeProgressOut{ev: ev, updErr: errors.New("synthetic update failure")}, "hello")
	got := ev.waitFor(t, 4)
	if got[3] != "finish:pong" {
		t.Fatalf("%#v", got)
	}
}

func TestDiscordReMarksReceivedAtStart(t *testing.T) {
	svc, _ := progressService(t)
	svc.AllowedUsers["discord"] = map[string]struct{}{"7": {}}
	ev := &progressEvents{}
	svc.Handle(context.Background(), platform.Inbound{
		Platform: "discord", ChatID: "c", UserID: "7", Text: "hi", TextMessage: true,
	}, fakeProgressOut{ev: ev})
	got := ev.waitFor(t, 4)
	want := []string{"receipt", "receipt", "start:处理中…", "finish:pong"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestTelegramDoesNotReMarkReceived(t *testing.T) {
	svc, _ := progressService(t)
	ev := &progressEvents{}
	handleHello(svc, fakeProgressOut{ev: ev}, "hello")
	ev.waitFor(t, 3)
	n := 0
	for _, e := range ev.snapshot() {
		if e == "receipt" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%#v", ev.snapshot())
	}
}

func TestScheduledTaskHasNoProgress(t *testing.T) {
	svc, _ := progressService(t)
	ev := &progressEvents{}
	svc.dispatch(item{
		in:   platform.Inbound{Platform: "telegram", ChatID: "9", UserID: "1"},
		out:  fakeProgressOut{ev: ev},
		task: &store.Task{ID: 1, Platform: "telegram", ChatID: "9", Prompt: "do it"},
	})
	for _, e := range ev.snapshot() {
		if strings.HasPrefix(e, "start") || strings.HasPrefix(e, "finish") {
			t.Fatalf("%#v", ev.snapshot())
		}
	}
}

func TestSafeErrHidesRequestURL(t *testing.T) {
	err := &url.Error{
		Op:  "Post",
		URL: "https://api.telegram.org/botyour_token_here/sendMessage",
		Err: errors.New("boom"),
	}
	for _, e := range []error{err, fmt.Errorf("send: %w", err)} {
		got := safeErr(e)
		if !strings.Contains(got, "boom") || strings.Contains(got, "your_token_here") || strings.Contains(got, "api.telegram.org/bot") {
			t.Fatalf("%q", got)
		}
	}
	if got := safeErr(errors.New("plain")); got != "plain" {
		t.Fatalf("%q", got)
	}
}

// ctxHandle records whether the context it received was already cancelled.
type ctxHandle struct {
	finishErr chan error
	deleteErr chan error
}

func (ctxHandle) Update(context.Context, string) error { return nil }
func (h ctxHandle) Finish(ctx context.Context, _ string) error {
	h.finishErr <- ctx.Err()
	return nil
}
func (h ctxHandle) Delete(ctx context.Context) error {
	h.deleteErr <- ctx.Err()
	return nil
}

func TestFinishAndDeleteSurviveCancelledTurn(t *testing.T) {
	h := ctxHandle{finishErr: make(chan error, 1), deleteErr: make(chan error, 1)}
	view := &progressView{handle: h, platform: "telegram", interval: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = view.Finish(ctx, "x")
	_ = view.Delete(ctx)
	if err := <-h.finishErr; err != nil {
		t.Fatalf("Finish got a cancelled context: %v", err)
	}
	if err := <-h.deleteErr; err != nil {
		t.Fatalf("Delete got a cancelled context: %v", err)
	}
}
