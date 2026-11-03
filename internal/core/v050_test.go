package core

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rshun/grok-bot/internal/platform"
	"github.com/rshun/grok-bot/internal/store"
)

type buttonCaptureOut struct {
	mu      sync.Mutex
	msgs    []string
	buttons [][][]platform.Button
}

func (o *buttonCaptureOut) Send(_ context.Context, _, text string) error {
	o.mu.Lock()
	o.msgs = append(o.msgs, text)
	o.buttons = append(o.buttons, nil)
	o.mu.Unlock()
	return nil
}

func (o *buttonCaptureOut) SendWithButtons(_ context.Context, _, text string, rows [][]platform.Button) error {
	o.mu.Lock()
	o.msgs = append(o.msgs, text)
	o.buttons = append(o.buttons, rows)
	o.mu.Unlock()
	return nil
}

func (o *buttonCaptureOut) last() (string, [][]platform.Button) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.msgs) == 0 {
		return "", nil
	}
	i := len(o.msgs) - 1
	return o.msgs[i], o.buttons[i]
}

func (o *buttonCaptureOut) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.msgs)
}

func waitIdle(t *testing.T, svc *Service, plat, chatID string) {
	t.Helper()
	ln := svc.lane(plat, chatID)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !ln.busy() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("lane %s/%s did not become idle", plat, chatID)
}

func hasButtonCommand(rows [][]platform.Button, cmd string) bool {
	for _, r := range rows {
		for _, b := range r {
			if b.Command == cmd {
				return true
			}
		}
	}
	return false
}

func TestCrossPlatformSessionHandoff(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data"), "grok-4.7")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	eng := &memoryTestEngine{}
	tgOut := &buttonCaptureOut{}
	dcOut := &buttonCaptureOut{}
	svc := &Service{
		Store:  st,
		Engine: eng,
		AllowedUsers: map[string]map[string]struct{}{
			platform.Telegram: {"u-tg": {}},
			platform.Discord:  {"u-dc": {}},
		},
		Senders: map[string]platform.Replier{
			platform.Telegram: tgOut,
			platform.Discord:  dcOut,
		},
	}

	// 1. Start a conversation on Telegram.
	svc.Handle(context.Background(), platform.Inbound{
		Platform: platform.Telegram, ChatID: "tg-chat", UserID: "u-tg", Text: "排查线上 Redis 延迟", TextMessage: true,
	}, tgOut)
	waitIdle(t, svc, platform.Telegram, "tg-chat")

	tgChat, err := st.Chat(context.Background(), platform.Telegram, "tg-chat")
	if err != nil || tgChat.SessionID == "" {
		t.Fatalf("expected Telegram active session, got %#v err=%v", tgChat, err)
	}

	// 2. Open Discord and list `/sessions`: must see Telegram's session #1 tagged `[来自 TG]` + handoff button.
	svc.Handle(context.Background(), platform.Inbound{
		Platform: platform.Discord, ChatID: "dc-dm", UserID: "u-dc", Text: "/sessions", TextMessage: true,
	}, dcOut)
	waitIdle(t, svc, platform.Discord, "dc-dm")

	listText, listBtns := dcOut.last()
	if !strings.Contains(listText, "#1 [来自 TG] 排查线上 Redis 延迟") {
		t.Fatalf("expected cross-platform TG session in Discord /sessions, got:\n%s", listText)
	}
	if !hasButtonCommand(listBtns, "/sessions 1") || !hasButtonCommand(listBtns, "/new") {
		t.Fatalf("expected /sessions 1 and /new buttons, got: %#v", listBtns)
	}

	// 3. Hand off session #1 onto Discord via `/sessions 1`.
	svc.Handle(context.Background(), platform.Inbound{
		Platform: platform.Discord, ChatID: "dc-dm", UserID: "u-dc", Text: "/sessions 1", TextMessage: true,
	}, dcOut)
	waitIdle(t, svc, platform.Discord, "dc-dm")

	swText, _ := dcOut.last()
	if !strings.Contains(swText, "已切回会话 #1：排查线上 Redis 延迟") || !strings.Contains(swText, "跨平台接力自 Telegram") {
		t.Fatalf("unexpected handoff reply: %s", swText)
	}

	// 4. Continue chatting on Discord: must resume the exact Grok SessionID and ONLY reply on Discord!
	tgBefore := tgOut.count()
	svc.Handle(context.Background(), platform.Inbound{
		Platform: platform.Discord, ChatID: "dc-dm", UserID: "u-dc", Text: "接着刚才的慢查询日志继续看", TextMessage: true,
	}, dcOut)
	waitIdle(t, svc, platform.Discord, "dc-dm")

	if len(eng.calls) != 2 {
		t.Fatalf("expected 2 engine calls, got %d", len(eng.calls))
	}
	if eng.calls[1].SessionID != tgChat.SessionID || eng.calls[1].NewSession {
		t.Fatalf("expected Discord to resume Telegram session %q, got %#v", tgChat.SessionID, eng.calls[1])
	}
	if tgOut.count() != tgBefore {
		t.Fatalf("regular chat on Discord must not push to Telegram (before=%d after=%d)", tgBefore, tgOut.count())
	}
}

func TestLinkPrefetchAndSSRFProtection(t *testing.T) {
	// 1. URL extraction with balanced parens & Chinese punctuation.
	gotURLs := extractURLs("帮我看看这篇 https://en.wikipedia.org/wiki/Go_(programming_language)，还有（https://github.com/acme/demo/pull/42）！以及 http://user:123@example.com/doc")
	if len(gotURLs) != 2 || gotURLs[0] != "https://en.wikipedia.org/wiki/Go_(programming_language)" || gotURLs[1] != "https://github.com/acme/demo/pull/42" {
		t.Fatalf("unexpected extracted URLs: %#v", gotURLs)
	}

	// 2. SSRF IP & hostname guards.
	for _, blocked := range []string{"127.0.0.1", "::1", "10.0.0.8", "172.16.1.2", "192.168.1.1", "169.254.169.254", "100.64.10.2"} {
		if isPublicIP(net.ParseIP(blocked)) {
			t.Fatalf("expected %s to be rejected as non-public IP", blocked)
		}
	}
	for _, pub := range []string{"1.1.1.1", "8.8.8.8"} {
		if !isPublicIP(net.ParseIP(pub)) {
			t.Fatalf("expected %s to be accepted as public IP", pub)
		}
	}
	for _, badHost := range []string{"localhost", "metadata.internal", "nas.local", "intranet", "127.0.0.1"} {
		if !isBlockedHostname(badHost) {
			t.Fatalf("expected hostname %q to be blocked", badHost)
		}
	}

	// 3. GitHub PR + X/Twitter + HTML extractors against a local test server.
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/demo/pulls/42", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number":42,"title":"Fix race in worker lane","state":"open","additions":18,"deletions":4,"changed_files":2,"body":"Resolves lane deadlock under /stop.","user":{"login":"alice"},"base":{"ref":"master"},"head":{"ref":"fix/lane"}}`))
	})
	mux.HandleFunc("/repos/acme/demo/pulls/42/files", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"filename":"internal/core/service.go","status":"modified","additions":18,"deletions":4}]`))
	})
	mux.HandleFunc("/alice/status/999", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tweet":{"text":"Shipping v0.5.0 today!","created_at":"2026-11-03","likes":88,"retweets":12,"replies":5,"author":{"name":"Alice","screen_name":"alice"},"quote":{"text":"Can we sync sessions?","author":{"name":"Bob","screen_name":"bob"}}}}`))
	})
	mux.HandleFunc("/article", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><head><title>Go 1.24 Release Notes</title><meta name="description" content="Key runtime improvements"><script>evil()</script><style>.x{color:red}</style></head><body><h1>Overview</h1><p>Swiss Tables landed in runtime.</p></body></html>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	prSummary, err := fetchLinkSummaryWithClient(context.Background(), srv.Client(), "https://github.com/acme/demo/pull/42", srv.URL, srv.URL, srv.URL)
	if err != nil || !strings.Contains(prSummary, "Fix race in worker lane") || !strings.Contains(prSummary, "internal/core/service.go") {
		t.Fatalf("unexpected PR summary (err=%v):\n%s", err, prSummary)
	}

	xSummary, err := fetchLinkSummaryWithClient(context.Background(), srv.Client(), "https://x.com/alice/status/999", srv.URL, srv.URL, srv.URL)
	if err != nil || !strings.Contains(xSummary, "Shipping v0.5.0 today!") || !strings.Contains(xSummary, "Can we sync sessions?") {
		t.Fatalf("unexpected X summary (err=%v):\n%s", err, xSummary)
	}

	htmlSummary, err := fetchLinkSummaryWithClient(context.Background(), srv.Client(), srv.URL+"/article", srv.URL, srv.URL, srv.URL)
	if err != nil || !strings.Contains(htmlSummary, "Go 1.24 Release Notes") || !strings.Contains(htmlSummary, "Swiss Tables landed in runtime.") || strings.Contains(htmlSummary, "evil()") {
		t.Fatalf("unexpected HTML summary (err=%v):\n%s", err, htmlSummary)
	}

	// 4. End-to-end prompt injection in chatTurn + DisableLinkPrefetch toggle.
	eng := &memoryTestEngine{}
	svc, _ := newMemoryService(t, eng)
	svc.LinkFetcher = func(_ context.Context, rawURL string) (string, error) {
		return fetchLinkSummaryWithClient(context.Background(), srv.Client(), rawURL, srv.URL, srv.URL, srv.URL)
	}

	_ = sendSync(t, svc, platform.Inbound{Text: "帮我 review https://github.com/acme/demo/pull/42"})
	if len(eng.calls) != 1 || !strings.Contains(eng.calls[0].Prompt, "[外链预处理摘录") || !strings.Contains(eng.calls[0].Prompt, "Fix race in worker lane") {
		t.Fatalf("expected prefetched PR summary in prompt, got:\n%s", eng.calls[0].Prompt)
	}

	svc.DisableLinkPrefetch = true
	_ = sendSync(t, svc, platform.Inbound{Text: "再看下 https://github.com/acme/demo/pull/42"})
	if len(eng.calls) != 2 || strings.Contains(eng.calls[1].Prompt, "[外链预处理摘录") {
		t.Fatalf("expected no link prefetch when disabled, got:\n%s", eng.calls[1].Prompt)
	}
}

func TestInteractiveButtonsAndDeterministicTaskSubcommands(t *testing.T) {
	eng := &fakeMCPEngine{}
	svc, _ := newMemoryService(t, eng)
	svc.Now = func() time.Time { return time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC) }
	out := &buttonCaptureOut{}

	sendCmd := func(text string) (string, [][]platform.Button) {
		t.Helper()
		svc.Handle(context.Background(), platform.Inbound{
			Platform: platform.Telegram, ChatID: "c1", UserID: "u1", Text: text, TextMessage: true,
		}, out)
		waitIdle(t, svc, platform.Telegram, "c1")
		return out.last()
	}

	// 1. /status buttons
	_, btns := sendCmd("/status")
	for _, want := range []string{"/usage", "/sessions", "/task list", "/memory", "/mcp doctor", "/model"} {
		if !hasButtonCommand(btns, want) {
			t.Fatalf("missing button %q in /status: %#v", want, btns)
		}
	}

	// 2. /model buttons
	_, btns = sendCmd("/model")
	if !hasButtonCommand(btns, "/model grok-4.7") {
		t.Fatalf("expected /model grok-4.7 button, got %#v", btns)
	}

	// 3. /shell toggle buttons
	_, btns = sendCmd("/shell")
	if !hasButtonCommand(btns, "/shell on") {
		t.Fatalf("expected /shell on button when off, got %#v", btns)
	}
	_, btns = sendCmd("/shell on")
	if !hasButtonCommand(btns, "/shell off") {
		t.Fatalf("expected /shell off button when on, got %#v", btns)
	}

	// 4. /memory buttons
	_, _ = sendCmd("/memory add 输出尽量精简")
	_, btns = sendCmd("/memory")
	if !hasButtonCommand(btns, "/memory del 1") || !hasButtonCommand(btns, "/memory clear") {
		t.Fatalf("expected memory delete/clear buttons, got %#v", btns)
	}

	// 5. /task list buttons + fast deterministic /task pause 1 & /task resume 1 (0 LLM calls!)
	_, _ = sendCmd("/task cron 0 9 * * * 每日早报")
	_, btns = sendCmd("/task list")
	for _, want := range []string{"/task run 1", "/task pause 1", "/task del 1"} {
		if !hasButtonCommand(btns, want) {
			t.Fatalf("missing button %q in /task list: %#v", want, btns)
		}
	}

	callsBefore := len(eng.calls)
	pauseReply, _ := sendCmd("/task pause 1")
	if !strings.Contains(pauseReply, "已暂停 #1") || len(eng.calls) != callsBefore {
		t.Fatalf("expected zero-LLM instant pause, got reply=%q calls=%d->%d", pauseReply, callsBefore, len(eng.calls))
	}

	_, btns = sendCmd("/task list")
	if !hasButtonCommand(btns, "/task resume 1") {
		t.Fatalf("expected /task resume 1 button while paused, got %#v", btns)
	}

	resumeReply, _ := sendCmd("/task resume 1")
	if !strings.Contains(resumeReply, "已恢复 #1") || len(eng.calls) != callsBefore {
		t.Fatalf("expected zero-LLM instant resume, got reply=%q", resumeReply)
	}
}
