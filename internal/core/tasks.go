package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/platform"
	"github.com/rshun/grok-bot/internal/schedule"
	"github.com/rshun/grok-bot/internal/store"
)

const taskUsage = `用法：
/task 每天早上九点总结科技新闻
    用一句话创建定时任务
/task list
    查看这个聊天的定时任务
/task del 编号
    删除定时任务`

const notUnderstood = "没看懂时间，请换个说法。"

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

func (s *Service) taskCommand(ctx context.Context, in platform.Inbound, arg string) (string, error) {
	fields := strings.Fields(arg)
	switch {
	case len(fields) == 0:
		return taskUsage, nil
	case len(fields) == 1 && fields[0] == "list":
		return s.taskList(ctx, in)
	case fields[0] == "del":
		if len(fields) != 2 {
			return taskUsage, nil
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(fields[1], "#"), 10, 64)
		if err != nil {
			return taskUsage, nil
		}
		removed, err := s.Store.DeleteTask(ctx, in.Platform, in.ChatID, id)
		if err != nil {
			return "", err
		}
		if !removed {
			return "没有这个任务。", nil
		}
		return fmt.Sprintf("已删除 #%d。", id), nil
	default:
		return s.taskCreate(ctx, in, arg)
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
		fmt.Fprintf(&b, "#%d %s（下次 %s）\n", t.ID, t.Summary, t.NextRun.In(s.location()).Format("01-02 15:04"))
	}
	return strings.TrimSpace(b.String()), nil
}

func (s *Service) taskCreate(ctx context.Context, in platform.Inbound, text string) (string, error) {
	existing, err := s.Store.Tasks(ctx, in.Platform, in.ChatID)
	if err != nil {
		return "", err
	}
	if len(existing) >= s.taskLimit() {
		return fmt.Sprintf("这个聊天最多 %d 个定时任务，先用 /task del 删掉一些。", s.taskLimit()), nil
	}
	chat, err := s.Store.Chat(ctx, in.Platform, in.ChatID)
	if err != nil {
		return "", err
	}
	sessionID, err := newUUID()
	if err != nil {
		return "", err
	}
	now := s.now()
	// Parsing runs in a throwaway session so it never touches the chat's context.
	result, err := s.Engine.Reply(ctx, grok.ReplyOptions{
		SessionID:  sessionID,
		NewSession: true,
		Model:      chat.Model,
		Prompt:     schedule.Prompt(text, now, s.location()),
		JSONSchema: schedule.Schema,
	})
	if err != nil {
		return "", err
	}
	parsed, err := schedule.Decode(result.Structured, result.Text)
	if err != nil {
		slog.Warn("schedule decode failed", "err", err)
		return notUnderstood, nil
	}
	if parsed.Error != "" {
		return notUnderstood + "（" + parsed.Error + "）", nil
	}
	if strings.TrimSpace(parsed.Prompt) == "" {
		return notUnderstood + "（没有说要做什么）", nil
	}
	if err := parsed.Validate(now, s.location(), s.minInterval()); err != nil {
		return notUnderstood + "（" + err.Error() + "）", nil
	}
	next, ok := parsed.Next(now, s.location())
	if !ok {
		return notUnderstood + "（这个时间已经过去了）", nil
	}
	spec, err := json.Marshal(parsed.Spec)
	if err != nil {
		return "", err
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
		return "", err
	}
	return fmt.Sprintf("已创建 #%d：%s。下次执行：%s。", id, summary, next.In(s.location()).Format("01-02 15:04")), nil
}

// RunScheduler runs due tasks every interval until ctx is cancelled.
func (s *Service) RunScheduler(ctx context.Context, every time.Duration) {
	s.catchUp(ctx)
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
func (s *Service) runTask(ctx context.Context, t store.Task) string {
	header := taskHeader(t)
	chat, err := s.Store.Chat(ctx, t.Platform, t.ChatID)
	if err != nil {
		return header + "\n\n" + grok.UserFacing(err)
	}
	sessionID, err := newUUID()
	if err != nil {
		return header + "\n\n" + grok.UserFacing(err)
	}
	opts := grok.ReplyOptions{
		SessionID:  sessionID,
		NewSession: true,
		Model:      chat.Model,
		Prompt:     t.Prompt,
		WebSearch:  s.WebSearch,
	}
	s.newSessionOptions(ctx, &opts)
	result, err := s.Engine.Reply(ctx, opts)
	if err != nil {
		slog.Error("task run failed", "task", t.ID, "err", err)
		return header + "\n\n" + grok.UserFacing(err)
	}
	if strings.TrimSpace(result.Text) == "" {
		return header + "\n\n" + emptyReplyText
	}
	return header + "\n\n" + result.Text
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
