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

type blockingEngine struct {
	started chan struct{}
}

func (e *blockingEngine) Reply(ctx context.Context, opts grok.ReplyOptions) (grok.HeadlessResult, error) {
	select {
	case e.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return grok.HeadlessResult{}, ctx.Err()
}

func (e *blockingEngine) Models(context.Context) (grok.ModelList, error) {
	return grok.ModelList{Names: []string{"grok-4.7"}}, nil
}

func (e *blockingEngine) Allowance(context.Context) (grok.Allowance, error) {
	return grok.Allowance{}, nil
}

type syncOut struct {
	mu   sync.Mutex
	msgs []string
}

func (o *syncOut) Send(_ context.Context, _, text string) error {
	o.mu.Lock()
	o.msgs = append(o.msgs, text)
	o.mu.Unlock()
	return nil
}

func (o *syncOut) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.msgs...)
}

func TestStopInterruptsRunningTurnAndClearsQueue(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data"), "grok-4.7")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	eng := &blockingEngine{started: make(chan struct{}, 2)}
	svc := &Service{
		Store:        st,
		Engine:       eng,
		AllowedUsers: map[string]map[string]struct{}{platform.Telegram: {"u1": {}}},
		QueueLimit:   4,
	}
	out := &syncOut{}

	// Start a blocking turn + 1 queued message.
	svc.Handle(context.Background(), platform.Inbound{
		Platform: platform.Telegram, ChatID: "c1", UserID: "u1", Text: "慢任务", TextMessage: true,
	}, out)
	<-eng.started

	svc.Handle(context.Background(), platform.Inbound{
		Platform: platform.Telegram, ChatID: "c1", UserID: "u1", Text: "第二条排队消息", TextMessage: true,
	}, out)

	// Send /stop: must immediately cancel the active turn and drain the queued message.
	svc.Handle(context.Background(), platform.Inbound{
		Platform: platform.Telegram, ChatID: "c1", UserID: "u1", Text: "/stop", TextMessage: true,
	}, out)

	ln := svc.lane(platform.Telegram, "c1")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !ln.busy() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ln.busy() {
		t.Fatal("lane still busy after /stop")
	}

	got := strings.Join(out.snapshot(), " | ")
	if !strings.Contains(got, "已停止当前任务，并取消了 1 条排队中的消息") {
		t.Fatalf("unexpected replies: %s", got)
	}
	if strings.Contains(got, "处理失败") {
		t.Fatalf("cancelled turn should not emit failure reply: %s", got)
	}
}

type fakeMCPEngine struct {
	memoryTestEngine
	addedArgs []string
	removed   string
	toggled   string
	enabled   bool
	restarts  int
}

func (f *fakeMCPEngine) Version(context.Context) (string, error) {
	return "grok 1.0.70", nil
}

func (f *fakeMCPEngine) MCPList(context.Context) (string, error) {
	return "  Theseus-mcp: https://internal.example.com/mcp\n  rag-mcp: https://rag.example.com/mcp (disabled)\n", nil
}

func (f *fakeMCPEngine) MCPDoctor(_ context.Context, name string) (string, error) {
	if name != "" {
		return name + ": OK", nil
	}
	return "9/9 connected", nil
}

func (f *fakeMCPEngine) MCPAdd(_ context.Context, args []string) (string, error) {
	f.addedArgs = append([]string(nil), args...)
	return "Added MCP server", nil
}

func (f *fakeMCPEngine) MCPRemove(_ context.Context, name string) (string, error) {
	f.removed = name
	return "Removed " + name, nil
}

func (f *fakeMCPEngine) MCPEnable(_ context.Context, name string, enable bool) (string, error) {
	f.toggled = name
	f.enabled = enable
	return "Updated " + name, nil
}

func (f *fakeMCPEngine) ResetLeader(context.Context) {
	f.restarts++
}

func TestNewSessionsSwitchStatusAndMCPCommands(t *testing.T) {
	eng := &fakeMCPEngine{}
	svc, st := newMemoryService(t, eng)
	svc.Version = "v0.4.0"
	svc.Now = func() time.Time { return time.Date(2026, 11, 2, 20, 0, 0, 0, time.UTC) }

	// 1. First conversation turn creates session #1.
	_ = sendSync(t, svc, platform.Inbound{Text: "排查一下Theseus连接"})
	chat1, _ := st.Chat(context.Background(), platform.Telegram, "c1")
	if chat1.SessionID == "" {
		t.Fatal("expected active session 1")
	}

	// 2. /new archives session #1 and clears active session.
	newReply := sendSync(t, svc, platform.Inbound{Text: "/new Theseus连接问题"})
	if !strings.Contains(newReply, "已开启新话题") || !strings.Contains(newReply, "#1") {
		t.Fatalf("/new reply: %s", newReply)
	}

	// 3. Second conversation creates session #2.
	_ = sendSync(t, svc, platform.Inbound{Text: "写一段Python脚本"})
	chat2, _ := st.Chat(context.Background(), platform.Telegram, "c1")
	if chat2.SessionID == "" || chat2.SessionID == chat1.SessionID {
		t.Fatalf("expected distinct session 2, got %q vs %q", chat2.SessionID, chat1.SessionID)
	}

	// 4. /sessions lists both, and `/sessions 1` switches back to session #1!
	listReply := sendSync(t, svc, platform.Inbound{Text: "/sessions"})
	if !strings.Contains(listReply, "#1 Theseus连接问题") || !strings.Contains(listReply, "#2 [当前]") {
		t.Fatalf("/sessions list: %s", listReply)
	}
	swReply := sendSync(t, svc, platform.Inbound{Text: "/sessions 1"})
	if !strings.Contains(swReply, "已切回会话 #1：Theseus连接问题") {
		t.Fatalf("/sessions 1: %s", swReply)
	}
	switched, _ := st.Chat(context.Background(), platform.Telegram, "c1")
	if switched.SessionID != chat1.SessionID {
		t.Fatalf("expected active session to return to %q, got %q", chat1.SessionID, switched.SessionID)
	}

	// 5. /status and /mcp show MCP server names without exposing URLs.
	statusReply := sendSync(t, svc, platform.Inbound{Text: "/status"})
	for _, want := range []string{"v0.4.0", "grok 1.0.70", "Theseus-mcp", "rag-mcp (disabled)"} {
		if !strings.Contains(statusReply, want) {
			t.Fatalf("missing %q in /status:\n%s", want, statusReply)
		}
	}
	if strings.Contains(statusReply, "internal.example.com") || strings.Contains(statusReply, "rag.example.com") {
		t.Fatalf("/status leaked MCP URL:\n%s", statusReply)
	}
	mcpListReply := sendSync(t, svc, platform.Inbound{Text: "/mcp"})
	if !strings.Contains(mcpListReply, "Theseus-mcp\nrag-mcp (disabled)") || strings.Contains(mcpListReply, "https://") {
		t.Fatalf("/mcp list should show names only without URLs:\n%s", mcpListReply)
	}

	// 6. /mcp CRUD + doctor
	if got := sendSync(t, svc, platform.Inbound{Text: "/mcp doctor"}); !strings.Contains(got, "9/9 connected") {
		t.Fatalf("/mcp doctor: %s", got)
	}
	addOut := sendSync(t, svc, platform.Inbound{Text: `/mcp add api https://mcp.example.com/mcp -H "Authorization: Bearer your_token_here"`})
	if !strings.Contains(addOut, "已保存 MCP 配置") || len(eng.addedArgs) != 4 || eng.addedArgs[3] != "Authorization: Bearer your_token_here" {
		t.Fatalf("/mcp add: %s (%#v)", addOut, eng.addedArgs)
	}
	if got := sendSync(t, svc, platform.Inbound{Text: "/mcp disable api"}); !strings.Contains(got, "已停用 MCP") || eng.toggled != "api" || eng.enabled {
		t.Fatalf("/mcp disable: %s", got)
	}
	if got := sendSync(t, svc, platform.Inbound{Text: "/mcp del api"}); !strings.Contains(got, "已移除 MCP") || eng.removed != "api" {
		t.Fatalf("/mcp del: %s", got)
	}

	// 7. /task cron direct 5-field cron creation
	cronReply := sendSync(t, svc, platform.Inbound{Text: "/task cron 30 9 1 * * 汇总上月账单"})
	if !strings.Contains(cronReply, "已创建 #1：Cron (30 9 1 * *)") {
		t.Fatalf("/task cron: %s", cronReply)
	}
}
