package core

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/rshun/grok-bot/internal/platform"
)

const sessionsUsage = `用法：
/new [备注标题]
    归档当前会话并开启新话题（长期记忆继续保留）
/sessions
    查看最近保存的会话列表
/sessions 编号
    切换回指定编号的历史会话继续聊
/sessions del 编号
    删除指定编号的归档会话`

func (s *Service) recordSessionTurn(ctx context.Context, in platform.Inbound, sessionID, model, sandboxProfile string) {
	if s.Store == nil || sessionID == "" {
		return
	}
	title := summarizeSessionTitle(in.Text)
	_, _ = s.Store.SaveSessionHistory(ctx, in.Platform, in.ChatID, sessionID, title, model, sandboxProfile, false)
}

func summarizeSessionTitle(text string) string {
	text = strings.TrimSpace(text)
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = strings.TrimSpace(text[:i])
	}
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return "图片/附件对话"
	}
	runes := []rune(text)
	if len(runes) > 32 {
		return string(runes[:32]) + "…"
	}
	return text
}

func (s *Service) newTopicCommand(ctx context.Context, in platform.Inbound, arg string) (string, error) {
	chat, err := s.Store.Chat(ctx, in.Platform, in.ChatID)
	if err != nil {
		return "", err
	}
	note := strings.TrimSpace(arg)
	var archivedID int64
	if chat.SessionID != "" {
		overwrite := note != ""
		title := note
		if title == "" {
			title = "未命名对话"
		}
		archivedID, err = s.Store.SaveSessionHistory(ctx, in.Platform, in.ChatID, chat.SessionID, title, chat.Model, chat.Sandbox, overwrite)
		if err != nil {
			return "", err
		}
		if err := s.Store.ClearSession(ctx, in.Platform, in.ChatID); err != nil {
			return "", err
		}
	}
	memCount := len(s.chatMemories(ctx, in.Platform, in.ChatID))
	if archivedID > 0 {
		if memCount > 0 {
			return fmt.Sprintf("已开启新话题（上一段会话已归档为 #%d，可发 `/sessions %d` 切回；%d 条长期记忆继续保留）。", archivedID, archivedID, memCount), nil
		}
		return fmt.Sprintf("已开启新话题（上一段会话已归档为 #%d，可发 `/sessions %d` 随时切回）。", archivedID, archivedID), nil
	}
	if memCount > 0 {
		return fmt.Sprintf("当前已是新话题（%d 条长期记忆继续保留）。直接发消息即可开始。", memCount), nil
	}
	return "当前已是新话题，直接发消息即可开始。", nil
}

func (s *Service) sessionsCommand(ctx context.Context, in platform.Inbound, arg string) (string, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" || strings.EqualFold(arg, "list") {
		return s.sessionsList(ctx, in)
	}
	if strings.EqualFold(arg, "help") {
		return sessionsUsage, nil
	}
	fields := strings.Fields(arg)
	if len(fields) == 2 && strings.EqualFold(fields[0], "del") {
		id, err := strconv.ParseInt(strings.TrimPrefix(fields[1], "#"), 10, 64)
		if err != nil || id <= 0 {
			return sessionsUsage, nil
		}
		chat, err := s.Store.Chat(ctx, in.Platform, in.ChatID)
		if err != nil {
			return "", err
		}
		delSessionID, ok, err := s.Store.DeleteSavedSession(ctx, in.Platform, in.ChatID, id)
		if err != nil {
			return "", err
		}
		if !ok {
			return fmt.Sprintf("没有 #%d 这个历史会话，可发 /sessions 查看。", id), nil
		}
		if chat.SessionID != "" && chat.SessionID == delSessionID {
			_ = s.Store.ClearSession(ctx, in.Platform, in.ChatID)
			return fmt.Sprintf("已删除会话 #%d（同时已清空当前关联，下一条消息将开启新会话）。", id), nil
		}
		return fmt.Sprintf("已删除归档会话 #%d。", id), nil
	}

	targetToken := fields[0]
	if len(fields) == 2 && (strings.EqualFold(fields[0], "switch") || strings.EqualFold(fields[0], "use")) {
		targetToken = fields[1]
	} else if len(fields) > 1 {
		return sessionsUsage, nil
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(targetToken, "#"), 10, 64)
	if err != nil || id <= 0 {
		return sessionsUsage, nil
	}
	saved, ok, err := s.Store.SessionByID(ctx, in.Platform, in.ChatID, id)
	if err != nil {
		return "", err
	}
	if !ok {
		return fmt.Sprintf("没有 #%d 这个历史会话，可发 /sessions 查看。", id), nil
	}
	// Archive the outgoing session first so switching never loses it.
	if cur, chatErr := s.Store.Chat(ctx, in.Platform, in.ChatID); chatErr == nil && cur.SessionID != "" && cur.SessionID != saved.SessionID {
		_, _ = s.Store.SaveSessionHistory(ctx, in.Platform, in.ChatID, cur.SessionID, "未命名对话", cur.Model, cur.Sandbox, false)
	}
	if err := s.Store.ActivateSession(ctx, in.Platform, in.ChatID, saved); err != nil {
		return "", err
	}
	title := saved.Title
	if title == "" {
		title = "未命名对话"
	}
	return fmt.Sprintf("已切回会话 #%d：%s（模型：%s）。接下来发消息会继续这段上下文。", saved.ID, title, saved.Model), nil
}

func (s *Service) sessionsList(ctx context.Context, in platform.Inbound) (string, error) {
	chat, err := s.Store.Chat(ctx, in.Platform, in.ChatID)
	if err != nil {
		return "", err
	}
	list, err := s.Store.Sessions(ctx, in.Platform, in.ChatID)
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "还没有归档的历史会话。\n正常对话会自动记录，可用 `/new` 开启新话题并用 `/sessions 编号` 随时切回。", nil
	}
	var b strings.Builder
	b.WriteString("最近会话（发 `/sessions 编号` 切换，`/new` 开启新话题）：\n")
	for _, item := range list {
		title := item.Title
		if title == "" {
			title = "未命名对话"
		}
		tag := ""
		if chat.SessionID != "" && item.SessionID == chat.SessionID {
			tag = " [当前]"
		}
		ts := ""
		if !item.UpdatedAt.IsZero() {
			ts = " · " + item.UpdatedAt.In(s.location()).Format("01-02 15:04")
		}
		fmt.Fprintf(&b, "#%d%s %s（%s%s）\n", item.ID, tag, title, item.Model, ts)
	}
	return strings.TrimSpace(b.String()), nil
}
