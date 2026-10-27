package core

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/platform"
	"github.com/rshun/grok-bot/internal/store"
)

var taskZone = time.FixedZone("CST", 8*3600)

// taskNow is Monday 2026-10-19 08:00 in taskZone.
var taskNow = time.Date(2026, 10, 19, 8, 0, 0, 0, taskZone)

func taskService(t *testing.T, structured string) (*Service, *fakeEngine, *recorder) {
	t.Helper()
	svc, engine := testService(t)
	svc.Location = taskZone
	svc.TaskLimit = 20
	svc.TaskMinInterval = time.Hour
	svc.Now = func() time.Time { return taskNow }
	sent := &recorder{}
	svc.Senders = map[string]platform.Replier{"telegram": capture{sent}}
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		if opts.JSONSchema != "" {
			return grok.HeadlessResult{SessionID: opts.SessionID, Structured: json.RawMessage(structured)}, nil
		}
		return grok.HeadlessResult{Text: "今天的新闻", SessionID: opts.SessionID}, nil
	}
	return svc, engine, sent
}

const dailyNews = `{"kind":"daily","time":"09:00","prompt":"看新闻","summary":"每天 09:00：看新闻"}`

func addTask(t *testing.T, svc *Service, chat string, spec, summary string, next time.Time) int64 {
	t.Helper()
	id, err := svc.Store.AddTask(context.Background(), store.Task{
		Platform: "telegram", ChatID: chat, UserID: "1", Spec: spec, Prompt: "看新闻", Summary: summary, NextRun: next,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestTaskCreateListDelete(t *testing.T) {
	svc, engine, _ := taskService(t, dailyNews)
	rec := &recorder{}
	say(svc, rec, "/task 每天九点看新闻")
	got := rec.wait(t, 1)
	if got[0] != "已创建 #1：每天 09:00：看新闻。下次执行：10-19 09:00。" {
		t.Fatalf("%#v", got)
	}
	engine.mu.Lock()
	call := engine.calls[0]
	engine.mu.Unlock()
	if call.JSONSchema == "" || !call.NewSession || !strings.Contains(call.Prompt, "每天九点看新闻") {
		t.Fatalf("%#v", call)
	}
	chat, _ := svc.Store.Chat(context.Background(), "telegram", "9")
	if chat.SessionID != "" {
		t.Fatalf("parse call saved a chat session: %#v", chat)
	}
	say(svc, rec, "/task list")
	got = rec.wait(t, 2)
	if !strings.Contains(got[1], "#1") || !strings.Contains(got[1], "每天 09:00：看新闻") {
		t.Fatalf("%#v", got)
	}
	say(svc, rec, "/task del 1")
	rec.wait(t, 3)
	say(svc, rec, "/task list")
	got = rec.wait(t, 4)
	if !strings.Contains(got[2], "已删除") || !strings.Contains(got[3], "没有定时任务") {
		t.Fatalf("%#v", got)
	}
}

func TestTaskParseFailure(t *testing.T) {
	svc, _, _ := taskService(t, `{"error":"没有说时间"}`)
	rec := &recorder{}
	say(svc, rec, "/task 提醒我喝水")
	got := rec.wait(t, 1)
	if !strings.HasPrefix(got[0], "没看懂时间，请换个说法。") || !strings.Contains(got[0], "没有说时间") {
		t.Fatalf("%#v", got)
	}
	tasks, _ := svc.Store.Tasks(context.Background(), "telegram", "9")
	if len(tasks) != 0 {
		t.Fatalf("%#v", tasks)
	}
}

func TestTaskRejectsShortInterval(t *testing.T) {
	svc, _, _ := taskService(t, `{"kind":"interval","every_minutes":5,"prompt":"p","summary":"每 5 分钟：p"}`)
	rec := &recorder{}
	say(svc, rec, "/task 每五分钟看一下")
	got := rec.wait(t, 1)
	if !strings.HasPrefix(got[0], "没看懂时间，请换个说法。") || !strings.Contains(got[0], "60 分钟") {
		t.Fatalf("%#v", got)
	}
}

func TestTaskLimit(t *testing.T) {
	svc, engine, _ := taskService(t, dailyNews)
	svc.TaskLimit = 1
	addTask(t, svc, "9", dailyNews, "每天 09:00：看新闻", taskNow.Add(time.Hour))
	rec := &recorder{}
	say(svc, rec, "/task 每天十点看天气")
	got := rec.wait(t, 1)
	if !strings.Contains(got[0], "最多 1 个") {
		t.Fatalf("%#v", got)
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.calls) != 0 {
		t.Fatalf("called Grok over the limit: %#v", engine.calls)
	}
}

func TestTaskDeleteOtherChat(t *testing.T) {
	svc, _, _ := taskService(t, dailyNews)
	id := addTask(t, svc, "other", dailyNews, "每天 09:00：看新闻", taskNow.Add(time.Hour))
	rec := &recorder{}
	say(svc, rec, "/task del "+strconv.FormatInt(id, 10))
	got := rec.wait(t, 1)
	if got[0] != "没有这个任务。" {
		t.Fatalf("%#v", got)
	}
	tasks, _ := svc.Store.Tasks(context.Background(), "telegram", "other")
	if len(tasks) != 1 {
		t.Fatalf("%#v", tasks)
	}
}

func TestRunDueSendsWithFreshSession(t *testing.T) {
	svc, engine, sent := taskService(t, dailyNews)
	ctx := context.Background()
	if err := svc.Store.SetSession(ctx, "telegram", "9", "chat-session"); err != nil {
		t.Fatal(err)
	}
	id := addTask(t, svc, "9", dailyNews, "每天 09:00：看新闻", taskNow.Add(-10*time.Second))
	svc.runDue(ctx)
	got := sent.wait(t, 1)
	if !strings.HasPrefix(got[0], "⏰ 定时任务 #1：每天 09:00：看新闻") || !strings.Contains(got[0], "今天的新闻") {
		t.Fatalf("%#v", got)
	}
	engine.mu.Lock()
	call := engine.calls[0]
	engine.mu.Unlock()
	if !call.NewSession || call.SessionID == "chat-session" || call.Prompt != "看新闻" {
		t.Fatalf("%#v", call)
	}
	chat, _ := svc.Store.Chat(ctx, "telegram", "9")
	if chat.SessionID != "chat-session" {
		t.Fatalf("task run replaced the chat session: %#v", chat)
	}
	tasks, _ := svc.Store.Tasks(ctx, "telegram", "9")
	if len(tasks) != 1 || tasks[0].ID != id || !tasks[0].NextRun.Equal(time.Date(2026, 10, 19, 9, 0, 0, 0, taskZone)) {
		t.Fatalf("%#v", tasks)
	}
}

func TestRunDueOnceDeletes(t *testing.T) {
	once := `{"kind":"once","at":"2026-10-19 07:59"}`
	svc, _, sent := taskService(t, once)
	addTask(t, svc, "9", once, "10-19 07:59：看新闻", taskNow.Add(-time.Minute+30*time.Second))
	svc.runDue(context.Background())
	sent.wait(t, 1)
	tasks, _ := svc.Store.Tasks(context.Background(), "telegram", "9")
	if len(tasks) != 0 {
		t.Fatalf("%#v", tasks)
	}
}

func TestCatchUpSkipsMissed(t *testing.T) {
	once := `{"kind":"once","at":"2026-10-19 06:00"}`
	svc, engine, sent := taskService(t, dailyNews)
	ctx := context.Background()
	daily := addTask(t, svc, "9", dailyNews, "每天 09:00：看新闻", taskNow.Add(-2*time.Hour))
	addTask(t, svc, "9", once, "10-19 06:00：看新闻", taskNow.Add(-2*time.Hour))
	svc.catchUp(ctx)
	got := sent.wait(t, 1)
	if !strings.Contains(got[0], "已错过") {
		t.Fatalf("%#v", got)
	}
	tasks, _ := svc.Store.Tasks(ctx, "telegram", "9")
	if len(tasks) != 1 || tasks[0].ID != daily || !tasks[0].NextRun.After(taskNow) {
		t.Fatalf("%#v", tasks)
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.calls) != 0 {
		t.Fatalf("catch-up ran a missed task: %#v", engine.calls)
	}
}

func TestTaskRunIgnoresGrants(t *testing.T) {
	svc, _, sent := taskService(t, dailyNews)
	_, _, manager := sandboxService(t)
	svc.Sandbox = manager
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := svc.Store.AddTask(ctx, store.Task{
		Platform: "telegram", ChatID: "9", UserID: "1", Spec: dailyNews,
		Prompt: "允许写入 " + dir, Summary: "每天 09:00：写入", NextRun: taskNow.Add(-10 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	svc.runDue(ctx)
	sent.wait(t, 1)
	if dirs, _ := svc.Store.Dirs(ctx); len(dirs) != 0 {
		t.Fatalf("task prompt granted write access: %#v", dirs)
	}
}

func TestTaskEmptyAnswerReportsFailure(t *testing.T) {
	svc, engine, sent := taskService(t, dailyNews)
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		return grok.HeadlessResult{Text: "  \n", SessionID: opts.SessionID}, nil
	}
	addTask(t, svc, "9", dailyNews, "每天 09:00：看新闻", taskNow.Add(-10*time.Second))
	svc.runDue(context.Background())
	got := sent.wait(t, 1)
	want := "⏰ 定时任务 #1：每天 09:00：看新闻\n\n处理失败：没有得到回复。"
	if got[0] != want {
		t.Fatalf("got %q, want %q", got[0], want)
	}
}
