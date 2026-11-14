package core

import (
	"context"
	"encoding/json"
	"errors"
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

type fakeDiscordSender struct {
	rec      *recorder
	dmOpened string
}

func (f *fakeDiscordSender) Send(_ context.Context, chatID, text string) error {
	f.rec.mu.Lock()
	f.rec.msgs = append(f.rec.msgs, chatID+":"+text)
	f.rec.mu.Unlock()
	return nil
}

func (f *fakeDiscordSender) OpenDM(_ context.Context, userID string) (string, error) {
	f.dmOpened = "dm-" + userID
	return f.dmOpened, nil
}

func TestCrossPlatformTaskSyncAndDualDelivery(t *testing.T) {
	svc, engine, tgSent := taskService(t, dailyNews)
	dcRec := &recorder{}
	dcSender := &fakeDiscordSender{rec: dcRec}
	svc.AllowedUsers[platform.Discord] = map[string]struct{}{"dc_u1": {}}
	svc.Senders[platform.Discord] = dcSender

	// 1. Create a task on Telegram.
	tgReply := &recorder{}
	say(svc, tgReply, "/task 每天九点看新闻")
	tgReply.wait(t, 1)

	// 2. Discord sees the exact same task via `/task list`.
	dcCmdReply := &recorder{}
	svc.Handle(context.Background(), platform.Inbound{
		Platform: platform.Discord, ChatID: "dc_chan_1", UserID: "dc_u1", Text: "/task list", TextMessage: true,
	}, capture{dcCmdReply})
	gotDCList := dcCmdReply.wait(t, 1)
	if !strings.Contains(gotDCList[0], "#1 每天 09:00：看新闻") {
		t.Fatalf("discord /task list missing TG task: %#v", gotDCList)
	}

	// Clear cached dc_chan_1 so we also verify cold-start OpenDM fallback when crontab runs in a fresh process.
	svc.mu.Lock()
	svc.dmChannels = nil
	svc.mu.Unlock()

	// 3. RunTaskByID (crontab mode) executes Grok ONCE and pushes simultaneously to Telegram AND Discord!
	svc.Now = func() time.Time { return time.Date(2026, 10, 19, 9, 0, 0, 0, taskZone) }
	if err := svc.RunTaskByID(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	gotTG := tgSent.wait(t, 1)
	gotDC := dcRec.wait(t, 1)
	if !strings.Contains(gotTG[0], "⏰ 定时任务 #1") || !strings.Contains(gotTG[0], "今天的新闻") {
		t.Fatalf("TG did not receive scheduled task: %#v", gotTG)
	}
	if !strings.HasPrefix(gotDC[0], "dm-dc_u1:⏰ 定时任务 #1") || !strings.Contains(gotDC[0], "今天的新闻") {
		t.Fatalf("Discord did not receive scheduled task via OpenDM: %#v", gotDC)
	}
	engine.mu.Lock()
	// 1 structured parse call + 1 task execution call = 2 total engine calls (not 3!).
	callCount := len(engine.calls)
	engine.mu.Unlock()
	if callCount != 2 {
		t.Fatalf("expected task to execute Grok once across both platforms (2 total calls), got %d", callCount)
	}

	// 4. Delete the task from Discord -> removed globally on both platforms.
	svc.Handle(context.Background(), platform.Inbound{
		Platform: platform.Discord, ChatID: "dc_chan_1", UserID: "dc_u1", Text: "/task del 1", TextMessage: true,
	}, capture{dcCmdReply})
	gotDCDel := dcCmdReply.wait(t, 2)
	if !strings.Contains(gotDCDel[1], "已删除 #1") {
		t.Fatalf("discord /task del failed: %#v", gotDCDel)
	}
	if all, _ := svc.Store.AllTasks(context.Background()); len(all) != 0 {
		t.Fatalf("expected 0 tasks after cross-platform delete, got %#v", all)
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

	// 3. Natural phrasing "请试运行一下任务2" (without "定时") triggers task #2 directly
	id2 := addTask(t, svc, "9", dailyNews, "每周一至周五 07:30：随机抽取10个单词", origNext)
	setStructured(`{"action":"run","task_id":` + strconv.FormatInt(id2, 10) + `}`)
	say(svc, rec, "请试运行一下任务2")
	got = rec.wait(t, 3)
	if !strings.Contains(got[2], "⏰ 定时任务 #2") || !strings.Contains(got[2], "随机抽取10个单词") {
		t.Fatalf("natural run task2 got %#v", got)
	}

	// 4. Conversational question about tasks falls back via action="none" and injects task list context into chat
	var chatPrompt string
	engine.mu.Lock()
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		if opts.JSONSchema != "" {
			return grok.HeadlessResult{SessionID: opts.SessionID, Structured: json.RawMessage(`{"action":"none"}`)}, nil
		}
		chatPrompt = opts.Prompt
		return grok.HeadlessResult{Text: "普通对话回复", SessionID: opts.SessionID}, nil
	}
	engine.mu.Unlock()
	say(svc, rec, "帮我看看任务2每次具体执行什么提示词？")
	got = rec.wait(t, 4)
	if got[3] != "普通对话回复" || !strings.Contains(chatPrompt, "[当前聊天的定时任务列表]") || !strings.Contains(chatPrompt, "#2 每周一至周五 07:30：随机抽取10个单词") {
		t.Fatalf("chat fallback got %q, prompt=%q", got[3], chatPrompt)
	}
}

type fakeSchedulerBackend struct {
	name    string
	syncErr error
	synced  [][]schedule.CronEntry
}

func (f *fakeSchedulerBackend) Name() string { return f.name }
func (f *fakeSchedulerBackend) Sync(entries []schedule.CronEntry, _ *time.Location) error {
	cp := append([]schedule.CronEntry(nil), entries...)
	f.synced = append(f.synced, cp)
	return f.syncErr
}

func TestInitSchedulerAndNativeSyncStatus(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := taskService(t, dailyNews)
	fb := &fakeSchedulerBackend{name: "macOS launchd (LaunchAgents)"}
	svc.Scheduler = fb

	// Pre-populate one active task and verify InitScheduler syncs it immediately without polling.
	_ = addTask(t, svc, "9", dailyNews, "每天 09:00：看新闻", time.Date(2026, 10, 20, 9, 0, 0, 0, taskZone))
	svc.InitScheduler(ctx)
	if len(fb.synced) != 1 || len(fb.synced[0]) != 1 || fb.synced[0][0].ID != 1 {
		t.Fatalf("InitScheduler did not sync task into backend: %#v", fb.synced)
	}

	// /status displays the OS native scheduler name.
	rec := &recorder{}
	say(svc, rec, "/status")
	statusMsg := rec.wait(t, 1)[0]
	if !strings.Contains(statusMsg, "macOS launchd (LaunchAgents)（系统原生触发）") {
		t.Fatalf("/status missing native scheduler line:\n%s", statusMsg)
	}

	// When OS scheduler sync fails, /task and /status surface the warning instead of hiding it.
	fb.syncErr = errors.New("permission denied")
	say(svc, rec, "/task pause 1")
	pauseReply := rec.wait(t, 2)[1]
	if !strings.Contains(pauseReply, "已暂停 #1") || !strings.Contains(pauseReply, "⚠️ 同步到 macOS launchd (LaunchAgents) 失败") {
		t.Fatalf("expected sync failure warning on /task pause, got:\n%s", pauseReply)
	}
	say(svc, rec, "/status")
	statusWarn := rec.wait(t, 3)[2]
	if !strings.Contains(statusWarn, "⚠️ permission denied") {
		t.Fatalf("expected sync failure in /status, got:\n%s", statusWarn)
	}
}

func TestTaskTargetPlatformSingleVsAll(t *testing.T) {
	ctx := context.Background()
	onlyTG := `{"action":"create","kind":"daily","time":"09:00","prompt":"看新闻","summary":"每天 09:00：看新闻","target_platform":"telegram"}`
	svc, _, tgSent := taskService(t, onlyTG)
	dcRec := &recorder{}
	svc.AllowedUsers[platform.Discord] = map[string]struct{}{"dc_u1": {}}
	svc.Senders[platform.Discord] = &fakeDiscordSender{rec: dcRec}

	// 1. Create a Telegram-only task.
	cmdRec := &recorder{}
	say(svc, cmdRec, "/task 每天九点看新闻 只推TG")
	gotCmd := cmdRec.wait(t, 1)
	if !strings.Contains(gotCmd[0], "仅推送 Telegram") {
		t.Fatalf("create reply missing platform note: %#v", gotCmd)
	}
	say(svc, cmdRec, "/task list")
	gotCmd = cmdRec.wait(t, 2)
	if !strings.Contains(gotCmd[1], "#1 [仅 TG] 每天 09:00：看新闻") {
		t.Fatalf("list missing [仅 TG]: %#v", gotCmd)
	}

	// Scheduled run delivers ONLY to Telegram.
	svc.Now = func() time.Time { return time.Date(2026, 10, 19, 9, 0, 0, 0, taskZone) }
	if err := svc.RunTaskByID(ctx, 1); err != nil {
		t.Fatal(err)
	}
	tgMsgs := tgSent.wait(t, 1)
	if !strings.Contains(tgMsgs[0], "今天的新闻") {
		t.Fatalf("TG did not receive task: %#v", tgMsgs)
	}
	dcRec.mu.Lock()
	dcCount := len(dcRec.msgs)
	dcRec.mu.Unlock()
	if dcCount != 0 {
		t.Fatalf("Discord should not receive TG-only task, got %#v", dcRec.msgs)
	}

	// 2. Switch task #1 to Discord-only via `/task platform 1 dc`.
	say(svc, cmdRec, "/task platform 1 dc")
	gotCmd = cmdRec.wait(t, 3)
	if !strings.Contains(gotCmd[2], "仅推送 Discord") {
		t.Fatalf("platform dc reply: %#v", gotCmd)
	}
	svc.Now = func() time.Time { return time.Date(2026, 10, 20, 9, 0, 0, 0, taskZone) }
	if err := svc.RunTaskByID(ctx, 1); err != nil {
		t.Fatal(err)
	}
	dcMsgs := dcRec.wait(t, 1)
	if !strings.Contains(dcMsgs[0], "今天的新闻") {
		t.Fatalf("Discord did not receive DC-only task: %#v", dcMsgs)
	}
	tgSent.mu.Lock()
	tgCount := len(tgSent.msgs)
	tgSent.mu.Unlock()
	if tgCount != 1 {
		t.Fatalf("TG should not receive DC-only task, got %d messages", tgCount)
	}

	// Manual `/task run 1` from Telegram delivers to Discord and confirms in Telegram.
	say(svc, cmdRec, "/task run 1")
	gotCmd = cmdRec.wait(t, 4)
	if !strings.Contains(gotCmd[3], "仅推送至 Discord") {
		t.Fatalf("cross-platform trial reply: %#v", gotCmd)
	}
	dcRec.wait(t, 2)

	// 3. Reset to all platforms via `/task platform 1 all`.
	say(svc, cmdRec, "/task platform 1 all")
	gotCmd = cmdRec.wait(t, 5)
	if !strings.Contains(gotCmd[4], "全平台推送") {
		t.Fatalf("platform all reply: %#v", gotCmd)
	}
	svc.Now = func() time.Time { return time.Date(2026, 10, 21, 9, 0, 0, 0, taskZone) }
	if err := svc.RunTaskByID(ctx, 1); err != nil {
		t.Fatal(err)
	}
	tgSent.wait(t, 2)
	dcRec.wait(t, 3)

	// 4. `/task cron -p tg` creates a Telegram-only cron task.
	say(svc, cmdRec, "/task cron -p tg 30 9 1 * * 汇总上月账单")
	gotCmd = cmdRec.wait(t, 6)
	if !strings.Contains(gotCmd[5], "已创建 #2") || !strings.Contains(gotCmd[5], "仅推送 Telegram") {
		t.Fatalf("cron -p tg reply: %#v", gotCmd)
	}
	t2, ok, err := svc.Store.TaskByID(ctx, 2)
	if err != nil || !ok || t2.TargetPlatform != platform.Telegram {
		t.Fatalf("task #2 target_platform: %#v ok=%v err=%v", t2, ok, err)
	}
}

func TestTaskNoReplySuppressesScheduledPush(t *testing.T) {
	ctx := context.Background()
	svc, engine, tgSent := taskService(t, dailyNews)
	dcRec := &recorder{}
	svc.AllowedUsers[platform.Discord] = map[string]struct{}{"dc_u1": {}}
	svc.Senders[platform.Discord] = &fakeDiscordSender{rec: dcRec}

	var sawScheduledFlag bool
	replyText := "  [[NO_REPLY]]\n"
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		if opts.JSONSchema != "" {
			return grok.HeadlessResult{
				SessionID:  opts.SessionID,
				Structured: json.RawMessage(`{"action":"run","task_id":1}`),
			}, nil
		}
		sawScheduledFlag = opts.ScheduledTask
		return grok.HeadlessResult{Text: replyText, SessionID: opts.SessionID}, nil
	}

	addTask(t, svc, "9", dailyNews, "每天 03:00：交易日检查并显示T-1的fills文件", time.Date(2026, 10, 19, 9, 0, 0, 0, taskZone))

	// 1. Scheduled RunTaskByID with [[NO_REPLY]] advances NextRun and suppresses push on ALL platforms.
	svc.Now = func() time.Time { return time.Date(2026, 10, 19, 9, 0, 0, 0, taskZone) }
	if err := svc.RunTaskByID(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if !sawScheduledFlag {
		t.Fatal("expected ScheduledTask=true on Grok ReplyOptions")
	}
	tgSent.mu.Lock()
	tgCount := len(tgSent.msgs)
	tgSent.mu.Unlock()
	dcRec.mu.Lock()
	dcCount := len(dcRec.msgs)
	dcRec.mu.Unlock()
	if tgCount != 0 || dcCount != 0 {
		t.Fatalf("expected 0 pushes on [[NO_REPLY]], got tg=%d dc=%d", tgCount, dcCount)
	}
	t1, _, _ := svc.Store.TaskByID(ctx, 1)
	if !t1.NextRun.Equal(time.Date(2026, 10, 20, 9, 0, 0, 0, taskZone)) {
		t.Fatalf("NextRun did not advance: %v", t1.NextRun)
	}

	// 2. Manual `/task run 1` confirms no-output locally without mirroring or showing raw [[NO_REPLY]].
	cmdRec := &recorder{}
	say(svc, cmdRec, "/task run 1")
	gotCmd := cmdRec.wait(t, 1)
	if strings.Contains(gotCmd[0], noReply) || !strings.Contains(gotCmd[0], "无结果产出") {
		t.Fatalf("unexpected manual trial reply: %q", gotCmd[0])
	}
	dcRec.mu.Lock()
	dcAfterTrial := len(dcRec.msgs)
	dcRec.mu.Unlock()
	if dcAfterTrial != 0 {
		t.Fatalf("manual trial with [[NO_REPLY]] leaked to Discord: %#v", dcRec.msgs)
	}

	// 3. Next day when real output is produced, both platforms receive the push.
	replyText = "T-1 共 12 笔成交"
	svc.Now = func() time.Time { return time.Date(2026, 10, 20, 9, 0, 0, 0, taskZone) }
	if err := svc.RunTaskByID(ctx, 1); err != nil {
		t.Fatal(err)
	}
	gotTG := tgSent.wait(t, 1)
	gotDC := dcRec.wait(t, 1)
	if !strings.Contains(gotTG[0], "T-1 共 12 笔成交") || !strings.Contains(gotDC[0], "T-1 共 12 笔成交") {
		t.Fatalf("real output not delivered: tg=%#v dc=%#v", gotTG, gotDC)
	}
}
