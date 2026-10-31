package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/rshun/grok-bot/internal/actions"
	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/platform"
	"github.com/rshun/grok-bot/internal/schedule"
	"github.com/rshun/grok-bot/internal/store"
)

const taskUsage = `用法：
/task 每天早上九点总结科技新闻
    用一句话创建定时任务
/task 取消明天的定时任务 / 暂停定时任务 / 恢复定时任务
    用自然语言跳过下次、暂停、恢复或删除定时任务
/task list
    查看这个聊天的定时任务
/task del 编号
    删除定时任务`

const notUnderstood = "没看懂时间，请换个说法。"

var errNotTask = errors.New("not a task request")

// missedGrace separates a run the scheduler is about to pick up from one missed while the bot was down.
const missedGrace = time.Minute

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) location() *time.Location {
	if s.Location != nil {
		return s.Location
	}
	return time.Local
}

func (s *Service) taskLimit() int {
	if s.TaskLimit < 1 {
		return 20
	}
	return s.TaskLimit
}

func (s *Service) minInterval() time.Duration {
	if s.TaskMinInterval <= 0 {
		return time.Hour
	}
	return s.TaskMinInterval
}

func (s *Service) taskCommand(ctx context.Context, in platform.Inbound, out platform.Replier, arg string, onTool func(grok.ToolUse)) (string, []actions.File, error) {
	fields := strings.Fields(arg)
	switch {
	case len(fields) == 0:
		return taskUsage, nil, nil
	case len(fields) == 1 && fields[0] == "list":
		reply, err := s.taskList(ctx, in)
		return reply, nil, err
	case fields[0] == "run" && len(fields) == 2:
		id, err := strconv.ParseInt(strings.TrimPrefix(fields[1], "#"), 10, 64)
		if err != nil {
			return s.taskCreateWithProgress(ctx, in, out, arg, onTool)
		}
		existing, err := s.Store.Tasks(ctx, in.Platform, in.ChatID)
		if err != nil {
			return "", nil, err
		}
		return s.taskManage(ctx, in, out, existing, schedule.Parsed{Action: "run", TaskID: id}, s.now(), onTool)
	case fields[0] == "del" && len(fields) == 2:
		id, err := strconv.ParseInt(strings.TrimPrefix(fields[1], "#"), 10, 64)
		if err != nil {
			return s.taskCreateWithProgress(ctx, in, out, arg, onTool)
		}
		removed, err := s.Store.DeleteTask(ctx, in.Platform, in.ChatID, id)
		if err != nil {
			return "", nil, err
		}
		if !removed {
			return "没有这个任务。", nil, nil
		}
		s.syncCrontab(ctx)
		return fmt.Sprintf("已删除 #%d。", id), nil, nil
	case fields[0] == "del" && len(fields) == 1:
		return taskUsage, nil, nil
	default:
		reply, files, err := s.taskCreateWithProgress(ctx, in, out, arg, onTool)
		if errors.Is(err, errNotTask) {
			return notUnderstood, nil, nil
		}
		return reply, files, err
	}
}

func (s *Service) taskList(ctx context.Context, in platform.Inbound) (string, error) {
	tasks, err := s.Store.Tasks(ctx, in.Platform, in.ChatID)
	if err != nil {
		return "", err
	}
	if len(tasks) == 0 {
		return "这个聊天没有定时任务。", nil
	}
	var b strings.Builder
	b.WriteString("定时任务：\n")
	for _, t := range tasks {
		if t.Paused {
			fmt.Fprintf(&b, "#%d %s（已暂停）\n", t.ID, t.Summary)
			continue
		}
		fmt.Fprintf(&b, "#%d %s（下次 %s）\n", t.ID, t.Summary, t.NextRun.In(s.location()).Format("01-02 15:04"))
	}
	return strings.TrimSpace(b.String()), nil
}

func (s *Service) taskCreateWithProgress(ctx context.Context, in platform.Inbound, out platform.Replier, text string, onTool func(grok.ToolUse)) (string, []actions.File, error) {
	existing, err := s.Store.Tasks(ctx, in.Platform, in.ChatID)
	if err != nil {
		return "", nil, err
	}
	if len(existing) >= s.taskLimit() && !looksLikeTaskManage(text) {
		return fmt.Sprintf("这个聊天最多 %d 个定时任务，先用 /task del 删掉一些。", s.taskLimit()), nil, nil
	}
	chat, err := s.Store.Chat(ctx, in.Platform, in.ChatID)
	if err != nil {
		return "", nil, err
	}
	sessionID, err := newUUID()
	if err != nil {
		return "", nil, err
	}
	now := s.now()
	existingSpecs := make([]schedule.ExistingTask, len(existing))
	for i, t := range existing {
		existingSpecs[i] = schedule.ExistingTask{
			ID:      t.ID,
			Summary: t.Summary,
			Prompt:  t.Prompt,
			NextRun: t.NextRun,
			Paused:  t.Paused,
		}
	}
	// Parsing runs in a throwaway session so it never touches the chat's context.
	result, err := s.Engine.Reply(ctx, grok.ReplyOptions{
		SessionID:  sessionID,
		NewSession: true,
		Model:      chat.Model,
		Prompt:     schedule.Prompt(text, now, s.location(), existingSpecs...),
		JSONSchema: schedule.Schema,
	})
	if err != nil {
		return "", nil, err
	}
	parsed, err := schedule.Decode(result.Structured, result.Text)
	if err != nil {
		slog.Warn("schedule decode failed", "err", err)
		return notUnderstood, nil, nil
	}
	if parsed.Action == "none" {
		return "", nil, errNotTask
	}
	if parsed.Error != "" {
		return notUnderstood + "（" + parsed.Error + "）", nil, nil
	}
	switch parsed.Action {
	case "skip", "pause", "resume", "delete", "run", "update":
		return s.taskManage(ctx, in, out, existing, parsed, now, onTool)
	}
	if len(existing) >= s.taskLimit() {
		return fmt.Sprintf("这个聊天最多 %d 个定时任务，先用 /task del 删掉一些。", s.taskLimit()), nil, nil
	}
	if strings.TrimSpace(parsed.Prompt) == "" {
		return notUnderstood + "（没有说要做什么）", nil, nil
	}
	if err := parsed.Validate(now, s.location(), s.minInterval()); err != nil {
		return notUnderstood + "（" + err.Error() + "）", nil, nil
	}
	next, ok := parsed.Next(now, s.location())
	if !ok {
		return notUnderstood + "（这个时间已经过去了）", nil, nil
	}
	spec, err := json.Marshal(parsed.Spec)
	if err != nil {
		return "", nil, err
	}
	summary := strings.TrimRight(strings.TrimSpace(parsed.Summary), "。.")
	if summary == "" {
		summary = parsed.Prompt
	}
	id, err := s.Store.AddTask(ctx, store.Task{
		Platform: in.Platform,
		ChatID:   in.ChatID,
		UserID:   in.UserID,
		Spec:     string(spec),
		Prompt:   parsed.Prompt,
		Summary:  summary,
		NextRun:  next,
	})
	if err != nil {
		return "", nil, err
	}
	s.syncCrontab(ctx)
	return fmt.Sprintf("已创建 #%d：%s。下次执行：%s。", id, summary, next.In(s.location()).Format("01-02 15:04")), nil, nil
}

func (s *Service) taskManage(ctx context.Context, in platform.Inbound, out platform.Replier, existing []store.Task, parsed schedule.Parsed, now time.Time, onTool func(grok.ToolUse)) (string, []actions.File, error) {
	if len(existing) == 0 {
		return "这个聊天没有定时任务。", nil, nil
	}
	targetID := parsed.TaskID
	if targetID == 0 && len(existing) == 1 {
		targetID = existing[0].ID
	}
	var target *store.Task
	for i := range existing {
		if existing[i].ID == targetID {
			target = &existing[i]
			break
		}
	}
	if target == nil {
		if targetID > 0 {
			return fmt.Sprintf("没有 #%d 这个定时任务，可发 /task list 查看。", targetID), nil, nil
		}
		return "请说明要操作哪个定时任务，或先发 /task list 查看。", nil, nil
	}
	switch parsed.Action {
	case "run":
		runIn := platform.Inbound{
			Platform:    target.Platform,
			ChatID:      target.ChatID,
			UserID:      target.UserID,
			MessageID:   in.MessageID,
			Text:        target.Prompt,
			TextMessage: true,
		}
		reply, files := s.runTaskWithProgress(ctx, runIn, out, *target, onTool)
		return reply, files, nil
	case "update":
		if err := parsed.Validate(now, s.location(), s.minInterval()); err != nil {
			return notUnderstood + "（" + err.Error() + "）", nil, nil
		}
		next, ok := parsed.Next(now, s.location())
		if !ok {
			return notUnderstood + "（这个时间已经过去了）", nil, nil
		}
		spec, err := json.Marshal(parsed.Spec)
		if err != nil {
			return "", nil, err
		}
		prompt := strings.TrimSpace(parsed.Prompt)
		if prompt == "" {
			prompt = target.Prompt
		}
		summary := strings.TrimRight(strings.TrimSpace(parsed.Summary), "。.")
		if summary == "" {
			summary = target.Summary
		}
		if err := s.Store.UpdateTask(ctx, target.ID, string(spec), prompt, summary, next); err != nil {
			return "", nil, err
		}
		s.syncCrontab(ctx)
		return fmt.Sprintf("已更新 #%d：%s。下次执行：%s。", target.ID, summary, next.In(s.location()).Format("01-02 15:04")), nil, nil
	case "delete":
		if _, err := s.Store.DeleteTask(ctx, in.Platform, in.ChatID, target.ID); err != nil {
			return "", nil, err
		}
		s.syncCrontab(ctx)
		return fmt.Sprintf("已删除 #%d：%s。", target.ID, target.Summary), nil, nil
	case "pause":
		if target.Paused {
			return fmt.Sprintf("#%d（%s）已经处于暂停状态。", target.ID, target.Summary), nil, nil
		}
		if err := s.Store.SetTaskPaused(ctx, target.ID, true, target.NextRun); err != nil {
			return "", nil, err
		}
		s.syncCrontab(ctx)
		return fmt.Sprintf("已暂停 #%d：%s。", target.ID, target.Summary), nil, nil
	case "resume":
		next, ok := taskNext(*target, now, s.location())
		if !ok {
			return fmt.Sprintf("#%d（%s）没有未来的执行时间，无法恢复。", target.ID, target.Summary), nil, nil
		}
		if err := s.Store.SetTaskPaused(ctx, target.ID, false, next); err != nil {
			return "", nil, err
		}
		s.syncCrontab(ctx)
		return fmt.Sprintf("已恢复 #%d：%s。下次执行：%s。", target.ID, target.Summary, next.In(s.location()).Format("01-02 15:04")), nil, nil
	case "skip":
		base := target.NextRun
		if base.Before(now) {
			base = now
		}
		oldNext := target.NextRun.In(s.location()).Format("01-02 15:04")
		next, ok := taskNext(*target, base, s.location())
		if !ok {
			if _, err := s.Store.DeleteTask(ctx, in.Platform, in.ChatID, target.ID); err != nil {
				return "", nil, err
			}
			s.syncCrontab(ctx)
			return fmt.Sprintf("已取消 #%d（%s）原定 %s 的执行（无后续周期，任务已结束）。", target.ID, target.Summary, oldNext), nil, nil
		}
		if err := s.Store.SetNextRun(ctx, target.ID, next); err != nil {
			return "", nil, err
		}
		s.syncCrontab(ctx)
		return fmt.Sprintf("已跳过 #%d（%s）原定 %s 的执行，下次执行顺延至：%s。", target.ID, target.Summary, oldNext, next.In(s.location()).Format("01-02 15:04")), nil, nil
	}
	return notUnderstood, nil, nil
}

func looksLikeTaskManage(text string) bool {
	lower := strings.ToLower(text)
	for _, kw := range []string{
		"暂停", "恢复", "继续", "跳过", "取消", "删", "停", "不执行", "不用",
		"试跑", "试运行", "测试运行",
		"马上执行", "立即执行", "立刻执行", "现在执行",
		"马上运行", "立即运行", "立刻运行", "现在运行",
		"执行", "运行", "跑一下", "跑一次", "测试",
		"改到", "改成", "改为", "修改", "调整", "推迟", "提前",
		"pause", "resume", "skip", "cancel", "del", "run",
	} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// tryNaturalTask handles natural-language task creation, listing, running, updating,
// skipping, pausing, resuming and deletion in ordinary chat without requiring /task.
func (s *Service) tryNaturalTask(ctx context.Context, in platform.Inbound, out platform.Replier, onTool func(grok.ToolUse)) (string, []actions.File, bool, error) {
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return "", nil, false, nil
	}
	existing, err := s.Store.Tasks(ctx, in.Platform, in.ChatID)
	if err != nil {
		return "", nil, false, err
	}
	if looksLikeTaskList(text, len(existing) > 0) {
		reply, err := s.taskList(ctx, in)
		return reply, nil, true, err
	}
	if !looksLikeNaturalTask(text, existing) {
		return "", nil, false, nil
	}
	reply, files, err := s.taskCreateWithProgress(ctx, in, out, text, onTool)
	if errors.Is(err, errNotTask) {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, err
	}
	if strings.HasPrefix(reply, notUnderstood) && !mentionsTaskExplicitly(text) {
		return "", nil, false, nil
	}
	return reply, files, true, nil
}

func mentionsTaskExplicitly(text string) bool {
	return strings.Contains(text, "定时任务") || strings.Contains(text, "周期任务")
}

func mentionsTaskRef(text string, existing []store.Task) bool {
	if strings.Contains(text, "任务") {
		return true
	}
	for _, t := range existing {
		if strings.Contains(text, fmt.Sprintf("#%d", t.ID)) {
			return true
		}
	}
	return false
}

func looksLikeTaskList(text string, hasExisting bool) bool {
	if looksLikeTaskManage(text) {
		return false
	}
	if mentionsTaskExplicitly(text) {
		for _, kw := range []string{"查看", "看看", "看下", "看一下", "有哪些", "有什么", "列表", "list"} {
			if strings.Contains(strings.ToLower(text), kw) {
				return true
			}
		}
	}
	if hasExisting {
		for _, kw := range []string{"任务列表", "查看任务", "看看任务", "看下任务", "看一下任务", "有哪些任务", "有什么任务"} {
			if strings.Contains(text, kw) {
				return true
			}
		}
	}
	return false
}

func looksLikeNaturalTask(text string, existing []store.Task) bool {
	if mentionsTaskExplicitly(text) {
		return true
	}
	for _, kw := range []string{"每天", "每日", "每周", "每个工作日", "每隔", "每小时", "提醒我"} {
		if strings.Contains(text, kw) {
			return true
		}
	}
	if (strings.Contains(text, "按照") || strings.Contains(text, "根据")) && strings.Contains(text, "日期") {
		return true
	}
	if len(existing) > 0 {
		for _, kw := range []string{
			"取消明天", "明天不", "跳过下次", "跳过明天", "暂停", "恢复", "删除任务", "取消任务",
			"试跑", "试运行", "测试运行",
			"马上执行", "立即执行", "立刻执行", "现在执行",
			"马上运行", "立即运行", "立刻运行", "现在运行",
			"改到", "改成", "改为",
		} {
			if strings.Contains(text, kw) {
				return true
			}
		}
		if mentionsTaskRef(text, existing) && looksLikeTaskManage(text) {
			return true
		}
	}
	return false
}

// RunScheduler syncs tasks into system crontab when available; otherwise it runs
// due tasks every interval until ctx is cancelled.
func (s *Service) RunScheduler(ctx context.Context, every time.Duration) {
	s.catchUp(ctx)
	s.syncCrontab(ctx)
	if s.Crontab != nil {
		<-ctx.Done()
		return
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runDue(ctx)
		}
	}
}

func (s *Service) syncCrontab(ctx context.Context) {
	if s.Crontab == nil {
		return
	}
	tasks, err := s.Store.AllTasks(ctx)
	if err != nil {
		slog.Error("load tasks for crontab", "err", err)
		return
	}
	entries := make([]schedule.CronEntry, len(tasks))
	for i, t := range tasks {
		entries[i] = schedule.CronEntry{
			ID:      t.ID,
			Spec:    t.Spec,
			Summary: t.Summary,
			NextRun: t.NextRun,
			Paused:  t.Paused,
		}
	}
	if err := s.Crontab.Sync(entries, s.location()); err != nil {
		slog.Error("sync crontab", "err", err)
	}
}

// RunTaskByID executes a single scheduled task triggered by system crontab (`grok-bot run-task <id>`).
func (s *Service) RunTaskByID(ctx context.Context, id int64) error {
	t, ok, err := s.Store.TaskByID(ctx, id)
	if err != nil {
		return err
	}
	if !ok || t.Paused {
		return nil
	}
	var spec schedule.Spec
	if err := json.Unmarshal([]byte(t.Spec), &spec); err != nil {
		return fmt.Errorf("bad task spec %d: %w", t.ID, err)
	}
	now := s.now()
	if !spec.MatchesRun(now, t.NextRun, s.location()) {
		return nil
	}
	if next, hasNext := spec.Next(now, s.location()); hasNext {
		err = s.Store.SetNextRun(ctx, t.ID, next)
	} else {
		_, err = s.Store.DeleteTask(ctx, t.Platform, t.ChatID, t.ID)
	}
	if err != nil {
		return err
	}
	s.syncCrontab(ctx)
	out := s.Senders[t.Platform]
	if out == nil {
		return fmt.Errorf("no sender for platform %q", t.Platform)
	}
	in := platform.Inbound{Platform: t.Platform, ChatID: t.ChatID, UserID: t.UserID, Text: t.Prompt, TextMessage: true}
	text, files := s.runTask(ctx, in, out, t)
	s.send(ctx, out, t.ChatID, text)
	s.flushFiles(ctx, out, t.ChatID, "", files)
	return nil
}

// catchUp skips runs missed while the bot was down instead of running them late.
func (s *Service) catchUp(ctx context.Context) {
	now := s.now()
	tasks, err := s.Store.DueTasks(ctx, now.Add(-missedGrace))
	if err != nil {
		slog.Error("load missed tasks", "err", err)
		return
	}
	for _, t := range tasks {
		next, ok := taskNext(t, now, s.location())
		if ok {
			if err := s.Store.SetNextRun(ctx, t.ID, next); err != nil {
				slog.Error("reschedule missed task", "task", t.ID, "err", err)
			}
			continue
		}
		if _, err := s.Store.DeleteTask(ctx, t.Platform, t.ChatID, t.ID); err != nil {
			slog.Error("delete missed task", "task", t.ID, "err", err)
			continue
		}
		if out := s.Senders[t.Platform]; out != nil {
			s.send(ctx, out, t.ChatID, taskHeader(t)+"\n\n已错过执行时间（当时 bot 没有运行），任务已删除。")
		}
	}
}

// runDue moves each due task to its next run, then queues it on its chat.
func (s *Service) runDue(ctx context.Context) {
	now := s.now()
	tasks, err := s.Store.DueTasks(ctx, now)
	if err != nil {
		slog.Error("load due tasks", "err", err)
		return
	}
	for _, t := range tasks {
		// Reschedule first so the next tick does not pick the task up again.
		if next, ok := taskNext(t, now, s.location()); ok {
			err = s.Store.SetNextRun(ctx, t.ID, next)
		} else {
			_, err = s.Store.DeleteTask(ctx, t.Platform, t.ChatID, t.ID)
		}
		if err != nil {
			slog.Error("reschedule task", "task", t.ID, "err", err)
			continue
		}
		out := s.Senders[t.Platform]
		if out == nil {
			slog.Error("no sender for task", "task", t.ID, "platform", t.Platform)
			continue
		}
		ln := s.lane(t.Platform, t.ChatID)
		if _, ok := ln.reserve(s.limit()); !ok {
			slog.Warn("chat queue full, skipped task run", "task", t.ID)
			continue
		}
		task := t
		ln.enqueue(item{
			ctx:  ctx,
			in:   platform.Inbound{Platform: t.Platform, ChatID: t.ChatID, UserID: t.UserID, Text: t.Prompt, TextMessage: true},
			out:  out,
			task: &task,
		})
	}
}

// runTask answers a task in a fresh session that is never saved as the chat's session.
// It returns the message to send and the files the run queued.
func (s *Service) runTask(ctx context.Context, in platform.Inbound, out platform.Replier, t store.Task) (string, []actions.File) {
	return s.runTaskWithProgress(ctx, in, out, t, nil)
}

func (s *Service) runTaskWithProgress(ctx context.Context, in platform.Inbound, out platform.Replier, t store.Task, onTool func(grok.ToolUse)) (string, []actions.File) {
	header := taskHeader(t)
	chat, err := s.Store.Chat(ctx, t.Platform, t.ChatID)
	if err != nil {
		return header + "\n\n" + grok.UserFacing(err), nil
	}
	sessionID, err := newUUID()
	if err != nil {
		return header + "\n\n" + grok.UserFacing(err), nil
	}
	opts := grok.ReplyOptions{
		SessionID:  sessionID,
		NewSession: true,
		Model:      chat.Model,
		Prompt:     t.Prompt,
		WebSearch:  s.WebSearch,
		OnTool:     onTool,
		Memories:   s.chatMemories(ctx, t.Platform, t.ChatID),
	}
	_ = s.newSessionOptions(ctx, &opts)
	turn, env, end := s.beginTurn(in, out, opts.FileTools || opts.ShellTools)
	defer end()
	opts = withTurn(opts, turn, env)
	result, err := s.Engine.Reply(ctx, opts)
	if sandboxUnavailable(err) && opts.Sandbox != "" {
		opts.Sandbox = ""
		result, err = s.Engine.Reply(ctx, opts)
	}
	if err != nil {
		slog.Error("task run failed", "task", t.ID, "err", err)
		return header + "\n\n" + grok.UserFacing(err), nil
	}
	var files []actions.File
	if turn != nil {
		files = turn.Files()
	}
	if strings.TrimSpace(result.Text) == "" {
		return header + "\n\n" + emptyReplyText, files
	}
	return header + "\n\n" + result.Text, files
}

func taskHeader(t store.Task) string {
	return fmt.Sprintf("⏰ 定时任务 #%d：%s", t.ID, t.Summary)
}

func taskNext(t store.Task, now time.Time, loc *time.Location) (time.Time, bool) {
	var spec schedule.Spec
	if err := json.Unmarshal([]byte(t.Spec), &spec); err != nil {
		slog.Error("bad task spec", "task", t.ID, "err", err)
		return time.Time{}, false
	}
	return spec.Next(now, loc)
}
