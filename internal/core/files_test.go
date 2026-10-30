package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/platform"
)

// fileOut is a progress-capable replier that also reacts and sends files.
type fileOut struct {
	reactingOut
	limit int64
}

func (f fileOut) FileLimit() int64 { return f.limit }

func (f fileOut) SendFile(_ context.Context, _, replyTo string, file platform.OutFile) error {
	return recordFile(f.ev, replyTo, file)
}

// filePlain has no progress message; results go through Send.
type filePlain struct {
	reactingPlain
	limit int64
}

func (f filePlain) FileLimit() int64 { return f.limit }

func (f filePlain) SendFile(_ context.Context, _, replyTo string, file platform.OutFile) error {
	return recordFile(f.ev, replyTo, file)
}

// recordFile reads the file the way a platform would and logs what it saw.
func recordFile(ev *progressEvents, replyTo string, file platform.OutFile) error {
	r, size, err := file.Open()
	if err != nil {
		ev.add("open-failed:" + file.Name)
		return err
	}
	defer r.Close()
	data, _ := io.ReadAll(r)
	ev.add(fmt.Sprintf("file:%s|%s|%s|%s|%d", file.Name, file.Caption, replyTo, data, size))
	return nil
}

func rpc(t *testing.T, ts *httptest.Server, token, method string, params any) (text string, isError bool, raw json.RawMessage) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var reply struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		t.Fatal(err)
	}
	var call struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	_ = json.Unmarshal(reply.Result, &call)
	if len(call.Content) == 1 {
		text = call.Content[0].Text
	}
	return text, call.IsError, reply.Result
}

func callSendFile(t *testing.T, ts *httptest.Server, token, path, caption string) (string, bool) {
	t.Helper()
	text, isErr, _ := rpc(t, ts, token, "tools/call", map[string]any{
		"name": "send_file", "arguments": map[string]string{"path": path, "caption": caption},
	})
	return text, isErr
}

func toolNames(t *testing.T, ts *httptest.Server, token string) []string {
	t.Helper()
	_, _, raw := rpc(t, ts, token, "tools/list", map[string]any{})
	var list struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func hasTool(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func TestSendFileCheckedAndSentAfterReply(t *testing.T) {
	svc, engine, ts := grantService(t)
	svc.ProgressInterval = time.Nanosecond
	path := writeTemp(t, "a.txt", "hello")
	var result string
	var isErr bool
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		if !opts.SendFile || !opts.FileTools {
			t.Errorf("send_file not offered: send=%v files=%v", opts.SendFile, opts.FileTools)
		}
		result, isErr = callSendFile(t, ts, tokenFrom(t, opts.Env), path, "  看这个 \n")
		return grok.HeadlessResult{Text: "好了", SessionID: opts.SessionID}, nil
	}
	ev := &progressEvents{}
	handleMessage(svc, fileOut{reactingOut: reactingOut{fakeProgressOut: fakeProgressOut{ev: ev}}, limit: 1 << 20}, "发我 a.txt")
	ev.waitFor(t, 4)
	time.Sleep(50 * time.Millisecond)
	if isErr || result != "Queued a.txt for sending." {
		t.Fatalf("grok was told %q (error=%v)", result, isErr)
	}
	// The answer comes first; the file follows, quoting the original message.
	if got, want := joined(ev.snapshot()), "receipt|start:处理中…|finish:好了|file:a.txt|看这个|5|hello|5"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestSendFileWithoutProgressMessage(t *testing.T) {
	svc, engine, ts := grantService(t)
	path := writeTemp(t, "a.txt", "hello")
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		callSendFile(t, ts, tokenFrom(t, opts.Env), path, "")
		return grok.HeadlessResult{Text: "好了", SessionID: opts.SessionID}, nil
	}
	ev := &progressEvents{}
	handleMessage(svc, filePlain{reactingPlain: reactingPlain{ev: ev}, limit: 1 << 20}, "发我 a.txt")
	ev.waitFor(t, 3)
	time.Sleep(50 * time.Millisecond)
	if got, want := joined(ev.snapshot()), "receipt|send:好了|file:a.txt||5|hello|5"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestSendFileTruncatesCaption(t *testing.T) {
	svc, engine, ts := grantService(t)
	path := writeTemp(t, "a.txt", "x")
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		callSendFile(t, ts, tokenFrom(t, opts.Env), path, strings.Repeat("字", 1100))
		return grok.HeadlessResult{Text: "好了", SessionID: opts.SessionID}, nil
	}
	ev := &progressEvents{}
	handleMessage(svc, filePlain{reactingPlain: reactingPlain{ev: ev}, limit: 1 << 20}, "发我")
	ev.waitFor(t, 3)
	time.Sleep(50 * time.Millisecond)
	want := "file:a.txt|" + strings.Repeat("字", 1024) + "|5|x|1"
	if got := ev.snapshot(); len(got) != 3 || got[2] != want {
		t.Fatalf("got %v", got)
	}
}

func TestSendFileRefusedWithoutFileTools(t *testing.T) {
	// No sandbox profile: file tools stay off, so send_file is neither listed nor described.
	svc, engine, ts := actionsService(t)
	var names []string
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		if opts.SendFile || opts.FileTools {
			t.Errorf("send=%v files=%v", opts.SendFile, opts.FileTools)
		}
		names = toolNames(t, ts, tokenFrom(t, opts.Env))
		return grok.HeadlessResult{Text: "pong", SessionID: opts.SessionID}, nil
	}
	ev := &progressEvents{}
	handleMessage(svc, filePlain{reactingPlain: reactingPlain{ev: ev}, limit: 1 << 20}, "hi")
	ev.waitFor(t, 2)
	if hasTool(names, "send_file") || !hasTool(names, "react") {
		t.Fatalf("tools: %v", names)
	}

	// File tools on, but the replier cannot send files.
	svc, engine, ts = grantService(t)
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		if !opts.FileTools || opts.SendFile {
			t.Errorf("send=%v files=%v", opts.SendFile, opts.FileTools)
		}
		names = toolNames(t, ts, tokenFrom(t, opts.Env))
		return grok.HeadlessResult{Text: "pong", SessionID: opts.SessionID}, nil
	}
	rec := &recorder{}
	svc.Handle(context.Background(), platform.Inbound{Platform: "telegram", ChatID: "9", UserID: "1", Text: "hi", TextMessage: true}, capture{rec})
	rec.wait(t, 1)
	if hasTool(names, "send_file") {
		t.Fatalf("tools: %v", names)
	}
}

func TestSendFileRespectsPlatformLimit(t *testing.T) {
	svc, engine, ts := grantService(t)
	path := writeTemp(t, "big.bin", "12345678901") // 11 bytes
	var result string
	var isErr bool
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		result, isErr = callSendFile(t, ts, tokenFrom(t, opts.Env), path, "")
		return grok.HeadlessResult{Text: "好了", SessionID: opts.SessionID}, nil
	}
	ev := &progressEvents{}
	handleMessage(svc, filePlain{reactingPlain: reactingPlain{ev: ev}, limit: 10}, "发我")
	ev.waitFor(t, 2)
	time.Sleep(50 * time.Millisecond)
	if !isErr || result != "文件超过这个平台的大小上限。" {
		t.Fatalf("grok was told %q (error=%v)", result, isErr)
	}
	if got, want := joined(ev.snapshot()), "receipt|send:好了"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestSendFileRefusesDeniedFile(t *testing.T) {
	svc, engine, ts := grantService(t)
	// The bot's own env file is on the sandbox deny list, so it can not leave through the chat.
	deny := svc.Sandbox.Policy.EnvFile
	if err := os.MkdirAll(filepath.Dir(deny), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deny, []byte("TELEGRAM_TOKEN=your_token_here"), 0o600); err != nil {
		t.Fatal(err)
	}
	var result string
	var isErr bool
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		result, isErr = callSendFile(t, ts, tokenFrom(t, opts.Env), deny, "")
		return grok.HeadlessResult{Text: "好了", SessionID: opts.SessionID}, nil
	}
	ev := &progressEvents{}
	handleMessage(svc, filePlain{reactingPlain: reactingPlain{ev: ev}, limit: 1 << 20}, "发我")
	ev.waitFor(t, 2)
	time.Sleep(50 * time.Millisecond)
	if !isErr || result != "这个文件不允许发送。" {
		t.Fatalf("grok was told %q (error=%v)", result, isErr)
	}
	if got, want := joined(ev.snapshot()), "receipt|send:好了"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestFlushFilesReportsFailureAndContinues(t *testing.T) {
	svc, engine, ts := grantService(t)
	svc.ProgressInterval = time.Nanosecond
	first := writeTemp(t, "a.txt", "one")
	second := writeTemp(t, "b.txt", "two")
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		token := tokenFrom(t, opts.Env)
		callSendFile(t, ts, token, first, "")
		callSendFile(t, ts, token, second, "")
		// The first file disappears between queueing and sending.
		if err := os.Remove(first); err != nil {
			t.Fatal(err)
		}
		return grok.HeadlessResult{Text: "好了", SessionID: opts.SessionID}, nil
	}
	ev := &progressEvents{}
	handleMessage(svc, fileOut{reactingOut: reactingOut{fakeProgressOut: fakeProgressOut{ev: ev}}, limit: 1 << 20}, "发我")
	ev.waitFor(t, 6)
	time.Sleep(50 * time.Millisecond)
	want := "receipt|start:处理中…|finish:好了|open-failed:a.txt|send:文件 a.txt 发送失败。|file:b.txt||5|two|3"
	if got := joined(ev.snapshot()); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestSendFileMaxPerReply(t *testing.T) {
	svc, engine, ts := grantService(t)
	path := writeTemp(t, "a.txt", "x")
	var last string
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		for i := 0; i < 11; i++ {
			last, _ = callSendFile(t, ts, tokenFrom(t, opts.Env), path, "")
		}
		return grok.HeadlessResult{Text: "好了", SessionID: opts.SessionID}, nil
	}
	ev := &progressEvents{}
	handleMessage(svc, filePlain{reactingPlain: reactingPlain{ev: ev}, limit: 1 << 20}, "发我")
	ev.waitFor(t, 12)
	time.Sleep(50 * time.Millisecond)
	if last != "too many files in this reply" || len(ev.snapshot()) != 12 {
		t.Fatalf("last=%q events=%d", last, len(ev.snapshot()))
	}
}

func TestReactOnlyStillSendsFiles(t *testing.T) {
	svc, engine, ts := grantService(t)
	svc.ProgressInterval = time.Nanosecond
	path := writeTemp(t, "a.txt", "hello")
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		token := tokenFrom(t, opts.Env)
		callReact(t, ts, token, "👍")
		callSendFile(t, ts, token, path, "")
		return grok.HeadlessResult{Text: noReply, SessionID: opts.SessionID}, nil
	}
	ev := &progressEvents{}
	handleMessage(svc, fileOut{reactingOut: reactingOut{fakeProgressOut: fakeProgressOut{ev: ev}}, limit: 1 << 20}, "发我")
	ev.waitFor(t, 5)
	time.Sleep(50 * time.Millisecond)
	if got, want := joined(ev.snapshot()), "receipt|start:处理中…|react:👍@5|delete|file:a.txt||5|hello|5"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestRebuiltSessionOffersFilesOfNewSandbox(t *testing.T) {
	svc, engine, ts := grantService(t)
	path := writeTemp(t, "a.txt", "hello")
	// An old session from before the sandbox existed: it cannot use file tools.
	if err := svc.Store.StartSession(context.Background(), "telegram", "9", "old", ""); err != nil {
		t.Fatal(err)
	}
	var resumeTools, rebuildTools []string
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		token := tokenFrom(t, opts.Env)
		if !opts.NewSession {
			resumeTools = toolNames(t, ts, token)
			return grok.HeadlessResult{}, &grok.RunError{Message: "session not found"}
		}
		rebuildTools = toolNames(t, ts, token)
		callSendFile(t, ts, token, path, "")
		return grok.HeadlessResult{Text: "好了", SessionID: opts.SessionID}, nil
	}
	ev := &progressEvents{}
	handleMessage(svc, filePlain{reactingPlain: reactingPlain{ev: ev}, limit: 1 << 20}, "发我")
	ev.waitFor(t, 3)
	time.Sleep(50 * time.Millisecond)
	if hasTool(resumeTools, "send_file") || !hasTool(rebuildTools, "send_file") {
		t.Fatalf("resume tools %v, rebuild tools %v", resumeTools, rebuildTools)
	}
	if got, want := joined(ev.snapshot()), "receipt|send:"+rebuiltNotice+"\n\n好了|file:a.txt||5|hello|5"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestTaskSendsFilesAfterResult(t *testing.T) {
	svc, engine, ts := grantService(t)
	path := writeTemp(t, "report.txt", "data")
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		if !opts.SendFile {
			t.Error("task run did not offer send_file")
		}
		callSendFile(t, ts, tokenFrom(t, opts.Env), path, "日报")
		return grok.HeadlessResult{Text: "今天的新闻", SessionID: opts.SessionID}, nil
	}
	ev := &progressEvents{}
	svc.Senders = map[string]platform.Replier{"telegram": filePlain{reactingPlain: reactingPlain{ev: ev}, limit: 1 << 20}}
	svc.Location = taskZone
	svc.Now = func() time.Time { return taskNow }
	addTask(t, svc, "9", dailyNews, "每天 09:00：看新闻", taskNow.Add(-10*time.Second))
	svc.runDue(context.Background())
	ev.waitFor(t, 2)
	time.Sleep(50 * time.Millisecond)
	// A task has no message to quote.
	want := "send:⏰ 定时任务 #1：每天 09:00：看新闻\n\n今天的新闻|file:report.txt|日报||data|4"
	if got := joined(ev.snapshot()); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

// growOut appends to the file after the platform has opened it, the way a
// process that keeps writing would.
type growOut struct {
	filePlain
	path string
}

func (g growOut) SendFile(_ context.Context, _, replyTo string, file platform.OutFile) error {
	r, size, err := file.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	f, err := os.OpenFile(g.path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString("-more-bytes")
	f.Close()
	if err != nil {
		return err
	}
	data, _ := io.ReadAll(r)
	g.ev.add(fmt.Sprintf("file:%s|%s|%d", data, replyTo, size))
	return nil
}

func TestSendFileReadsOnlyTheCheckedSize(t *testing.T) {
	svc, engine, ts := grantService(t)
	path := writeTemp(t, "grow.txt", "one")
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		callSendFile(t, ts, tokenFrom(t, opts.Env), path, "")
		return grok.HeadlessResult{Text: "好了", SessionID: opts.SessionID}, nil
	}
	ev := &progressEvents{}
	out := growOut{filePlain: filePlain{reactingPlain: reactingPlain{ev: ev}, limit: 1 << 20}, path: path}
	handleMessage(svc, out, "发我")
	ev.waitFor(t, 3)
	time.Sleep(50 * time.Millisecond)
	want := "receipt|send:好了|file:one|5|3"
	if got := joined(ev.snapshot()); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
