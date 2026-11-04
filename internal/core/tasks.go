package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rshun/grok-bot/internal/actions"
	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/media"
	"github.com/rshun/grok-bot/internal/platform"
	"github.com/rshun/grok-bot/internal/schedule"
	"github.com/rshun/grok-bot/internal/store"
)

const taskUsage = `用法：
/task 每天早上九点总结科技新闻
    用一句话创建定时任务（支持每天/每周/每月/月末最后一天/工作日）
/task cron 30 9 1 * * 汇总上月账单
    直接用标准 5 段 Cron 表达式创建定时任务
/task 取消明天的定时任务 / 暂停定时任务 / 恢复定时任务
    用自然语言跳过下次、暂停、恢复或删除定时任务
/task list
    查看定时任务
/task run 编号
    立即试跑一次定时任务
/task pause 编号 / /task resume 编号 / /task skip 编号
    暂停、恢复或跳过下次定时任务
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

func (s *Service) taskCommand(ctx context.Context, in platform.Inbound, out platform.Replier, arg string, onTool func(grok.ToolUse)) (string, []actions.File, [][]platform.Button, error) {
	fields := strings.Fields(arg)
	switch {
	case len(fields) == 0:
		return taskUsage, nil, nil, nil
	case len(fields) == 1 && fields[0] == "list":
		reply, btns, err := s.taskList(ctx, in)
		return reply, nil, btns, err
	case (fields[0] == "run" || fields[0] == "pause" || fields[0] == "resume" || fields[0] == "skip") && len(fields) == 2:
		action := fields[0]
		id, err := strconv.ParseInt(strings.TrimPrefix(fields[1], "#"), 10, 64)
		if err != nil {
			reply, files, createErr := s.taskCreateWithProgress(ctx, in, out, arg, onTool)
			return reply, files, nil, createErr
		}
		existing, err := s.Store.AllTasks(ctx)
		if err != nil {
			return "", nil, nil, err
		}
		reply, files, manageErr := s.taskManage(ctx, in, out, existing, schedule.Parsed{Action: action, TaskID: id}, s.now(), onTool)
		return reply, files, nil, manageErr
	case fields[0] == "del" && len(fields) == 2:
		id, err := strconv.ParseInt(strings.TrimPrefix(fields[1], "#"), 10, 64)
		if err != nil {
			reply, files, createErr := s.taskCreateWithProgress(ctx, in, out, arg, onTool)
			return reply, files, nil, createErr
		}
		removed, err := s.Store.DeleteTaskByID(ctx, id)
		if err != nil {
			return "", nil, nil, err
		}
		if !removed {
			return "没有这个任务。", nil, nil, nil
		}
		warn := s.syncScheduler(ctx)
		return fmt.Sprintf("已删除 #%d。", id) + warn, nil, nil, nil
	case (fields[0] == "del" || fields[0] == "pause" || fields[0] == "resume" || fields[0] == "skip") && len(fields) == 1:
		return taskUsage, nil, nil, nil
	case fields[0] == "cron" && len(fields) >= 7:
		reply, err := s.taskCreateRawCron(ctx, in, fields[1:6], strings.Join(fields[6:], " "))
		return reply, nil, nil, err
	case fields[0] == "cron":
		return "用法：`/task cron <分> <时> <日> <月> <周> <指令内容>`（例如：`/task cron 30 9 1 * * 汇总上月账单`）", nil, nil, nil
	default:
		reply, files, err := s.taskCreateWithProgress(ctx, in, out, arg, onTool)
		if errors.Is(err, errNotTask) {
			return notUnderstood, nil, nil, nil
		}
		return reply, files, nil, err
	}
}

func (s *Service) taskList(ctx context.Context, _ platform.Inbound) (string, [][]platform.Button, error) {
	tasks, err := s.Store.AllTasks(ctx)
	if err != nil {
		return "", nil, err
	}
	if len(tasks) == 0 {
		return "当前没有定时任务。", nil, nil
	}
	var b strings.Builder
	b.WriteString("定时任务（TG / Discord 同步共享并同时推送）：\n")
	for _, t := range tasks {
		if t.Paused {
			fmt.Fprintf(&b, "#%d %s（已暂停）\n", t.ID, t.Summary)
			continue
		}
		fmt.Fprintf(&b, "#%d %s（下次 %s）\n", t.ID, t.Summary, t.NextRun.In(s.location()).Format("01-02 15:04"))
	}
	return strings.TrimSpace(b.String()), taskListButtons(tasks), nil
}

func (s *Service) taskCreateWithProgress(ctx context.Context, in platform.Inbound, out platform.Replier, text string, onTool func(grok.ToolUse)) (string, []actions.File, error) {
	existing, err := s.Store.AllTasks(ctx)
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
	warn := s.syncScheduler(ctx)
	return fmt.Sprintf("已创建 #%d：%s。下次执行：%s。", id, summary, next.In(s.location()).Format("01-02 15:04")) + warn, nil, nil
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
			Platform:    in.Platform,
			ChatID:      in.ChatID,
			UserID:      in.UserID,
			MessageID:   in.MessageID,
			Text:        target.Prompt,
			TextMessage: true,
		}
		reply, files := s.runTaskWithProgress(ctx, runIn, out, *target, onTool)
		s.mirrorTaskToPeers(ctx, *target, in.Platform, reply, files)
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
		warn := s.syncScheduler(ctx)
		return fmt.Sprintf("已更新 #%d：%s。下次执行：%s。", target.ID, summary, next.In(s.location()).Format("01-02 15:04")) + warn, nil, nil
	case "delete":
		if _, err := s.Store.DeleteTaskByID(ctx, target.ID); err != nil {
			return "", nil, err
		}
		warn := s.syncScheduler(ctx)
		return fmt.Sprintf("已删除 #%d：%s。", target.ID, target.Summary) + warn, nil, nil
	case "pause":
		if target.Paused {
			return fmt.Sprintf("#%d（%s）已经处于暂停状态。", target.ID, target.Summary), nil, nil
		}
		if err := s.Store.SetTaskPaused(ctx, target.ID, true, target.NextRun); err != nil {
			return "", nil, err
		}
		warn := s.syncScheduler(ctx)
		return fmt.Sprintf("已暂停 #%d：%s。", target.ID, target.Summary) + warn, nil, nil
	case "resume":
		next, ok := taskNext(*target, now, s.location())
		if !ok {
			return fmt.Sprintf("#%d（%s）没有未来的执行时间，无法恢复。", target.ID, target.Summary), nil, nil
		}
		if err := s.Store.SetTaskPaused(ctx, target.ID, false, next); err != nil {
			return "", nil, err
		}
		warn := s.syncScheduler(ctx)
		return fmt.Sprintf("已恢复 #%d：%s。下次执行：%s。", target.ID, target.Summary, next.In(s.location()).Format("01-02 15:04")) + warn, nil, nil
	case "skip":
		base := target.NextRun
		if base.Before(now) {
			base = now
		}
		oldNext := target.NextRun.In(s.location()).Format("01-02 15:04")
		next, ok := taskNext(*target, base, s.location())
		if !ok {
			if _, err := s.Store.DeleteTaskByID(ctx, target.ID); err != nil {
				return "", nil, err
			}
			warn := s.syncScheduler(ctx)
			return fmt.Sprintf("已取消 #%d（%s）原定 %s 的执行（无后续周期，任务已结束）。", target.ID, target.Summary, oldNext) + warn, nil, nil
		}
		if err := s.Store.SetNextRun(ctx, target.ID, next); err != nil {
			return "", nil, err
		}
		warn := s.syncScheduler(ctx)
		return fmt.Sprintf("已跳过 #%d（%s）原定 %s 的执行，下次执行顺延至：%s。", target.ID, target.Summary, oldNext, next.In(s.location()).Format("01-02 15:04")) + warn, nil, nil
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
func (s *Service) tryNaturalTask(ctx context.Context, in platform.Inbound, out platform.Replier, onTool func(grok.ToolUse)) (string, []actions.File, [][]platform.Button, bool, error) {
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return "", nil, nil, false, nil
	}
	existing, err := s.Store.AllTasks(ctx)
	if err != nil {
		return "", nil, nil, false, err
	}
	if looksLikeTaskList(text, len(existing) > 0) {
		reply, btns, err := s.taskList(ctx, in)
		return reply, nil, btns, true, err
	}
	if !looksLikeNaturalTask(text, existing) {
		return "", nil, nil, false, nil
	}
	reply, files, err := s.taskCreateWithProgress(ctx, in, out, text, onTool)
	if errors.Is(err, errNotTask) {
		return "", nil, nil, false, nil
	}
	if err != nil {
		return "", nil, nil, false, err
	}
	if strings.HasPrefix(reply, notUnderstood) && !mentionsTaskExplicitly(text) {
		return "", nil, nil, false, nil
	}
	return reply, files, nil, true, nil
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
	for _, kw := range []string{"每天", "每日", "每周", "每月", "每个月", "月初", "月底", "每个工作日", "工作日", "每隔", "每小时", "整点", "提醒我"} {
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

// InitScheduler performs startup catch-up of tasks missed while offline and synchronizes
// all active tasks into the host OS's native scheduler (Linux crontab, macOS launchd,
// Windows Task Scheduler). Scheduled execution is triggered exclusively by the OS
// scheduler (`grok-bot run-task <id>`), never by an in-process timer.
func (s *Service) InitScheduler(ctx context.Context) {
	s.catchUp(ctx)
	_ = s.syncScheduler(ctx)
}

// RunScheduler syncs tasks into the host OS scheduler and blocks until ctx is cancelled,
// without running any in-process polling timer.
func (s *Service) RunScheduler(ctx context.Context, _ time.Duration) {
	s.InitScheduler(ctx)
	<-ctx.Done()
}

func (s *Service) scheduler() schedule.Backend {
	if s.Scheduler != nil {
		return s.Scheduler
	}
	if s.Crontab != nil {
		return s.Crontab
	}
	return nil
}

func (s *Service) schedulerStatus() (string, error) {
	b := s.scheduler()
	s.mu.Lock()
	syncErr := s.lastSchedErr
	s.mu.Unlock()
	if b == nil {
		if s.SchedulerInitErr != nil {
			return "未就绪", s.SchedulerInitErr
		}
		return "未配置", nil
	}
	if syncErr != nil {
		return b.Name(), syncErr
	}
	return b.Name(), s.SchedulerInitErr
}

func (s *Service) syncScheduler(ctx context.Context) string {
	b := s.scheduler()
	if b == nil {
		if s.SchedulerInitErr != nil {
			return "\n⚠️ 系统调度器不可用（" + grok.RedactSensitive(s.SchedulerInitErr.Error()) + "），任务尚未注册到系统定时服务。"
		}
		return ""
	}
	tasks, err := s.Store.AllTasks(ctx)
	if err != nil {
		slog.Error("load tasks for os scheduler", "scheduler", b.Name(), "err", err)
		return ""
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
	syncErr := b.Sync(entries, s.location())
	s.mu.Lock()
	s.lastSchedErr = syncErr
	s.mu.Unlock()
	if syncErr != nil {
		slog.Error("sync os scheduler", "scheduler", b.Name(), "err", syncErr)
		return "\n⚠️ 同步到 " + b.Name() + " 失败：" + grok.RedactSensitive(syncErr.Error())
	}
	return ""
}

// RunTaskByID executes a single scheduled task triggered by the host OS scheduler (`grok-bot run-task <id>`).
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
		_, err = s.Store.DeleteTaskByID(ctx, t.ID)
	}
	if err != nil {
		return err
	}
	primary, ok := s.primaryTaskTarget(ctx, t)
	if !ok {
		_ = s.syncScheduler(ctx)
		return fmt.Errorf("no sender configured for task %d", t.ID)
	}
	in := platform.Inbound{Platform: primary.Platform, ChatID: primary.ChatID, UserID: t.UserID, Text: t.Prompt, TextMessage: true}
	text, files := s.runTask(ctx, in, primary.Sender, t)
	s.deliverTask(ctx, t, primary.Platform, primary.ChatID, primary.Sender, text, files)
	_ = s.syncScheduler(ctx)
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
		if _, err := s.Store.DeleteTaskByID(ctx, t.ID); err != nil {
			slog.Error("delete missed task", "task", t.ID, "err", err)
			continue
		}
		msg := taskHeader(t) + "\n\n已错过执行时间（当时 bot 没有运行），任务已删除。"
		for _, target := range s.taskTargets(ctx, t) {
			s.send(ctx, target.Sender, target.ChatID, msg)
		}
	}
}

// runDue moves each due task to its next run, then queues it on its primary chat lane.
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
			_, err = s.Store.DeleteTaskByID(ctx, t.ID)
		}
		if err != nil {
			slog.Error("reschedule task", "task", t.ID, "err", err)
			continue
		}
		primary, ok := s.primaryTaskTarget(ctx, t)
		if !ok {
			slog.Error("no sender for task", "task", t.ID, "platform", t.Platform)
			continue
		}
		ln := s.lane(primary.Platform, primary.ChatID)
		_, epoch, ok := ln.reserve(s.limit())
		if !ok {
			slog.Warn("chat queue full, skipped task run", "task", t.ID)
			continue
		}
		task := t
		ln.enqueue(item{
			ctx:   ctx,
			in:    platform.Inbound{Platform: primary.Platform, ChatID: primary.ChatID, UserID: t.UserID, Text: t.Prompt, TextMessage: true},
			out:   primary.Sender,
			epoch: epoch,
			task:  &task,
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
	var unsafeMedia atomic.Bool
	imageGen := s.canGenerateImages(out)
	opts := grok.ReplyOptions{
		SessionID:  sessionID,
		NewSession: true,
		Model:      chat.Model,
		Prompt:     t.Prompt,
		WebSearch:  s.WebSearch,
		ImageGen:   imageGen,
		OnTool:     wrapToolSafetyMonitor(onTool, imageGen, &unsafeMedia),
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
	text := result.Text
	if len(result.GeneratedImages) > 0 {
		text = cleanLocalImageRefs(text)
	}
	if unsafeMedia.Load() {
		return header + "\n\n" + media.ErrUnsafeMedia.Error(), nil
	}
	if len(result.GeneratedImages) > 0 {
		files = s.appendGeneratedImages(files, result.GeneratedImages, out)
	}
	if strings.TrimSpace(text) == "" {
		if len(files) > 0 {
			return header + "\n\n" + mediaDeliveredFallback, files
		}
		return header + "\n\n" + emptyReplyText, files
	}
	return header + "\n\n" + text, files
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

func (s *Service) taskCreateRawCron(ctx context.Context, in platform.Inbound, cronFields []string, prompt string) (string, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "用法：`/task cron <分> <时> <日> <月> <周> <指令内容>`", nil
	}
	existing, err := s.Store.AllTasks(ctx)
	if err != nil {
		return "", err
	}
	if len(existing) >= s.taskLimit() {
		return fmt.Sprintf("这个聊天最多 %d 个定时任务，先用 /task del 删掉一些。", s.taskLimit()), nil
	}
	expr := strings.Join(cronFields, " ")
	specObj := schedule.Spec{Kind: "cron", Cron: expr}
	now := s.now()
	if err := specObj.Validate(now, s.location(), s.minInterval()); err != nil {
		return notUnderstood + "（" + err.Error() + "）", nil
	}
	next, ok := specObj.Next(now, s.location())
	if !ok {
		return notUnderstood + "（Cron 表达式没有未来的执行时间）", nil
	}
	rawSpec, err := json.Marshal(specObj)
	if err != nil {
		return "", err
	}
	summary := fmt.Sprintf("Cron (%s)：%s", expr, summarizeSessionTitle(prompt))
	id, err := s.Store.AddTask(ctx, store.Task{
		Platform: in.Platform,
		ChatID:   in.ChatID,
		UserID:   in.UserID,
		Spec:     string(rawSpec),
		Prompt:   prompt,
		Summary:  summary,
		NextRun:  next,
	})
	if err != nil {
		return "", err
	}
	warn := s.syncScheduler(ctx)
	return fmt.Sprintf("已创建 #%d：%s。下次执行：%s。", id, summary, next.In(s.location()).Format("01-02 15:04")) + warn, nil
}

type taskTarget struct {
	Platform string
	ChatID   string
	Sender   platform.Replier
}

func (s *Service) rememberChat(plat, chatID string) {
	if plat == "" || chatID == "" {
		return
	}
	s.mu.Lock()
	if s.dmChannels == nil {
		s.dmChannels = map[string]string{}
	}
	s.dmChannels[plat] = chatID
	s.mu.Unlock()
}

func (s *Service) cachedChat(plat string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dmChannels[plat]
}

// taskTargets returns all configured platform recipients (Telegram and Discord) for a scheduled task.
func (s *Service) taskTargets(ctx context.Context, t store.Task) []taskTarget {
	if len(s.Senders) == 0 {
		return nil
	}
	order := []string{platform.Telegram, platform.Discord}
	seen := map[string]bool{platform.Telegram: true, platform.Discord: true}
	var extras []string
	for plat := range s.Senders {
		if !seen[plat] {
			extras = append(extras, plat)
		}
	}
	sort.Strings(extras)
	order = append(order, extras...)

	var targets []taskTarget
	for _, plat := range order {
		sender := s.Senders[plat]
		if sender == nil {
			continue
		}
		chatID := s.resolvePlatformChat(ctx, plat, t.Platform, t.ChatID)
		if chatID == "" {
			continue
		}
		targets = append(targets, taskTarget{Platform: plat, ChatID: chatID, Sender: sender})
	}
	return targets
}

func (s *Service) primaryTaskTarget(ctx context.Context, t store.Task) (taskTarget, bool) {
	targets := s.taskTargets(ctx, t)
	if len(targets) == 0 {
		return taskTarget{}, false
	}
	for _, target := range targets {
		if target.Platform == t.Platform {
			return target, true
		}
	}
	return targets[0], true
}

// deliverTask sends a scheduled task's output to its primary chat and simultaneously to all other configured platforms.
func (s *Service) deliverTask(ctx context.Context, t store.Task, primaryPlat, primaryChatID string, primaryOut platform.Replier, text string, files []actions.File) {
	if primaryOut != nil && primaryChatID != "" {
		s.send(ctx, primaryOut, primaryChatID, text)
		s.flushFiles(ctx, primaryOut, primaryChatID, "", files)
	}
	s.mirrorTaskToPeers(ctx, t, primaryPlat, text, files)
}

func (s *Service) mirrorTaskToPeers(ctx context.Context, t store.Task, skipPlatform, text string, files []actions.File) {
	for _, target := range s.taskTargets(ctx, t) {
		if target.Platform == skipPlatform {
			continue
		}
		s.send(ctx, target.Sender, target.ChatID, text)
		s.flushFiles(ctx, target.Sender, target.ChatID, "", files)
	}
}

func (s *Service) resolvePlatformChat(ctx context.Context, plat, originPlat, originChatID string) string {
	if plat == originPlat && originChatID != "" {
		return originChatID
	}
	if cached := s.cachedChat(plat); cached != "" {
		return cached
	}
	if s.Store != nil {
		if known, err := s.Store.KnownChats(ctx, plat); err == nil && len(known) > 0 {
			s.rememberChat(plat, known[0])
			return known[0]
		}
	}
	uid := firstSortedKey(s.AllowedUsers[plat])
	if uid == "" {
		return ""
	}
	if opener, ok := s.Senders[plat].(platform.DMOpener); ok {
		dmID, err := opener.OpenDM(ctx, uid)
		if err != nil {
			slog.Error("open dm for scheduled task", "platform", plat, "err", safeErr(err))
			return ""
		}
		s.rememberChat(plat, dmID)
		return dmID
	}
	return uid
}

func firstSortedKey(m map[string]struct{}) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys[0]
}
