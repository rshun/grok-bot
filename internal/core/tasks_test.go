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
	"github.com/rshun/grok-bot/internal/schedule"
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

func TestTaskNaturalLanguageSkipPauseResumeDelete(t *testing.T) {
	ctx := context.Background()
	svc, engine, sent := taskService(t, dailyNews)
	id := addTask(t, svc, "9", dailyNews, "每天 09:00：看新闻", time.Date(2026, 10, 19, 9, 0, 0, 0, taskZone))

	setStructured := func(raw string) {
		engine.mu.Lock()
		defer engine.mu.Unlock()
		engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
			if opts.JSONSchema != "" {
				return grok.HeadlessResult{SessionID: opts.SessionID, Structured: json.RawMessage(raw)}, nil
			}
			return grok.HeadlessResult{Text: "今天的新闻", SessionID: opts.SessionID}, nil
		}
	}

	// 1. Skip next run ("取消明天的定时任务" without specifying task_id when only 1 task exists)
	setStructured(`{"action":"skip"}`)
	rec := &recorder{}
	say(svc, rec, "/task 取消下次的定时任务")
	got := rec.wait(t, 1)
	if !strings.Contains(got[0], "已跳过 #1") || !strings.Contains(got[0], "10-20 09:00") {
		t.Fatalf("skip got %#v", got)
	}
	tasks, _ := svc.Store.Tasks(ctx, "telegram", "9")
	if len(tasks) != 1 || !tasks[0].NextRun.Equal(time.Date(2026, 10, 20, 9, 0, 0, 0, taskZone)) {
		t.Fatalf("after skip: %#v", tasks)
	}

	// 2. Pause task in natural language
	setStructured(`{"action":"pause","task_id":` + strconv.FormatInt(id, 10) + `}`)
	say(svc, rec, "/task 暂停看新闻的定时任务")
	got = rec.wait(t, 2)
	if !strings.Contains(got[1], "已暂停 #1") {
		t.Fatalf("pause got %#v", got)
	}
	say(svc, rec, "/task list")
	got = rec.wait(t, 3)
	if !strings.Contains(got[2], "已暂停") {
		t.Fatalf("list paused got %#v", got)
	}

	// Paused task must not fire even if next_run is in the past
	_ = svc.Store.SetTaskPaused(ctx, id, true, taskNow.Add(-time.Minute))
	svc.runDue(ctx)
	sent.mu.Lock()
	fired := len(sent.msgs)
	sent.mu.Unlock()
	if fired != 0 {
		t.Fatalf("paused task fired: %d", fired)
	}

	// 3. Resume task in natural language
	setStructured(`{"action":"resume","task_id":` + strconv.FormatInt(id, 10) + `}`)
	say(svc, rec, "/task 恢复看新闻的定时任务")
	got = rec.wait(t, 4)
	if !strings.Contains(got[3], "已恢复 #1") || !strings.Contains(got[3], "10-19 09:00") {
		t.Fatalf("resume got %#v", got)
	}

	// 4. Delete task in natural language
	setStructured(`{"action":"delete","task_id":` + strconv.FormatInt(id, 10) + `}`)
	say(svc, rec, "/task 删除看新闻的定时任务")
	got = rec.wait(t, 5)
	if !strings.Contains(got[4], "已删除 #1") {
		t.Fatalf("delete got %#v", got)
	}
	tasks, _ = svc.Store.Tasks(ctx, "telegram", "9")
	if len(tasks) != 0 {
		t.Fatalf("expected 0 tasks after delete, got %#v", tasks)
	}
}

func TestNaturalChatWithoutTaskCommand(t *testing.T) {
	ctx := context.Background()
	svc, engine, _ := taskService(t, dailyNews)

	setStructured := func(raw string) {
		engine.mu.Lock()
		defer engine.mu.Unlock()
		engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
			if opts.JSONSchema != "" {
				return grok.HeadlessResult{SessionID: opts.SessionID, Structured: json.RawMessage(raw)}, nil
			}
			return grok.HeadlessResult{Text: "普通聊天回复", SessionID: opts.SessionID}, nil
		}
	}

	rec := &recorder{}
	// 1. Create task without /task
	say(svc, rec, "每天早上九点看新闻")
	got := rec.wait(t, 1)
	if !strings.Contains(got[0], "已创建 #1") {
		t.Fatalf("natural create got %#v", got)
	}

	// 2. View task list without /task
	say(svc, rec, "查看一下现在的定时任务")
	got = rec.wait(t, 2)
	if !strings.Contains(got[1], "#1 每天 09:00：看新闻") {
		t.Fatalf("natural list got %#v", got)
	}

	// 3. Skip tomorrow without /task
	setStructured(`{"action":"skip"}`)
	say(svc, rec, "取消明天的定时任务")
	got = rec.wait(t, 3)
	if !strings.Contains(got[2], "已跳过 #1") || !strings.Contains(got[2], "10-20 09:00") {
		t.Fatalf("natural skip got %#v", got)
	}

	// 4. Pause & resume without /task
	setStructured(`{"action":"pause"}`)
	say(svc, rec, "暂停定时任务")
	got = rec.wait(t, 4)
	if !strings.Contains(got[3], "已暂停 #1") {
		t.Fatalf("natural pause got %#v", got)
	}
	setStructured(`{"action":"resume"}`)
	say(svc, rec, "恢复定时任务")
	got = rec.wait(t, 5)
	if !strings.Contains(got[4], "已恢复 #1") {
		t.Fatalf("natural resume got %#v", got)
	}

	// 5. Casual sentence containing "每天" that is not a schedule falls back to normal chat
	setStructured(`{"error":"不是定时任务"}`)
	say(svc, rec, "我每天都喝咖啡")
	got = rec.wait(t, 6)
	if got[5] != "普通聊天回复" {
		t.Fatalf("fallback got %#v", got)
	}

	// 6. Delete without /task
	setStructured(`{"action":"delete"}`)
	say(svc, rec, "删除定时任务")
	got = rec.wait(t, 7)
	if !strings.Contains(got[6], "已删除 #1") {
		t.Fatalf("natural delete got %#v", got)
	}
	tasks, _ := svc.Store.Tasks(ctx, "telegram", "9")
	if len(tasks) != 0 {
		t.Fatalf("expected 0 tasks, got %#v", tasks)
	}
}

func TestCrontabSyncAndRunTaskByID(t *testing.T) {
	ctx := context.Background()
	svc, _, sent := taskService(t, dailyNews)
	var cronContent string
	svc.Crontab = &schedule.Crontab{
		CommandPrefix: "'/usr/local/bin/grok-bot'",
		Read:          func() (string, error) { return cronContent, nil },
		Write:         func(s string) error { cronContent = s; return nil },
	}

	rec := &recorder{}
	say(svc, rec, "每天早上九点看新闻")
	rec.wait(t, 1)
	if !strings.Contains(cronContent, "0 9 * * * '/usr/local/bin/grok-bot' run-task 1") {
		t.Fatalf("crontab not synced on create:\n%s", cronContent)
	}

	// Advance clock to 09:00 and run via RunTaskByID (simulating crond invocation)
	svc.Now = func() time.Time { return time.Date(2026, 10, 19, 9, 0, 0, 0, taskZone) }
	if err := svc.RunTaskByID(ctx, 1); err != nil {
		t.Fatal(err)
	}
	got := sent.wait(t, 1)
	if !strings.Contains(got[0], "今天的新闻") {
		t.Fatalf("RunTaskByID output: %#v", got)
	}

	// NextRun is now 2026-10-20 09:00; a duplicate RunTaskByID on 10-19 must be a no-op
	if err := svc.RunTaskByID(ctx, 1); err != nil {
		t.Fatal(err)
	}
	sent.mu.Lock()
	count := len(sent.msgs)
	sent.mu.Unlock()
	if count != 1 {
		t.Fatalf("expected duplicate cron fire to be skipped, got %d messages", count)
	}
}

func TestTaskRunNowAndUpdateSchedule(t *testing.T) {
	ctx := context.Background()
	svc, engine, _ := taskService(t, dailyNews)
	origNext := time.Date(2026, 10, 19, 9, 0, 0, 0, taskZone)
	id := addTask(t, svc, "9", dailyNews, "每天 09:00：看新闻", origNext)

	setStructured := func(raw string) {
		engine.mu.Lock()
		defer engine.mu.Unlock()
		engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
			if opts.JSONSchema != "" {
				return grok.HeadlessResult{SessionID: opts.SessionID, Structured: json.RawMessage(raw)}, nil
			}
			return grok.HeadlessResult{Text: "立即试跑结果", SessionID: opts.SessionID}, nil
		}
	}

	// 1. Test-run immediately in natural language without changing NextRun
	setStructured(`{"action":"run","task_id":` + strconv.FormatInt(id, 10) + `}`)
	rec := &recorder{}
	say(svc, rec, "现在试跑一下定时任务")
	got := rec.wait(t, 1)
	if !strings.Contains(got[0], "⏰ 定时任务 #1") || !strings.Contains(got[0], "立即试跑结果") {
		t.Fatalf("run got %#v", got)
	}
	tasks, _ := svc.Store.Tasks(ctx, "telegram", "9")
	if len(tasks) != 1 || !tasks[0].NextRun.Equal(origNext) {
		t.Fatalf("test-run altered NextRun: %#v", tasks)
	}

	// 2. Update schedule time in natural language
	setStructured(`{"action":"update","task_id":` + strconv.FormatInt(id, 10) + `,"kind":"daily","time":"10:30","summary":"每天 10:30：看新闻"}`)
	say(svc, rec, "把定时任务改到每天 10:30")
	got = rec.wait(t, 2)
	if !strings.Contains(got[1], "已更新 #1：每天 10:30：看新闻") || !strings.Contains(got[1], "10-19 10:30") {
		t.Fatalf("update got %#v", got)
	}
	tasks, _ = svc.Store.Tasks(ctx, "telegram", "9")
	if !tasks[0].NextRun.Equal(time.Date(2026, 10, 19, 10, 30, 0, 0, taskZone)) || tasks[0].Prompt != "看新闻" {
		t.Fatalf("updated task: %#v", tasks[0])
	}
}
