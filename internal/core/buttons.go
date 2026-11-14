package core

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/platform"
	"github.com/rshun/grok-bot/internal/store"
)

func shortPlatformName(plat string) string {
	switch plat {
	case platform.Telegram:
		return "TG"
	case platform.Discord:
		return "Discord"
	default:
		return plat
	}
}

func fullPlatformName(plat string) string {
	switch plat {
	case platform.Telegram:
		return "Telegram"
	case platform.Discord:
		return "Discord"
	default:
		return plat
	}
}

func shortButtonTitle(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	r := []rune(s)
	return string(r[:maxRunes]) + "…"
}

func statusButtons() [][]platform.Button {
	return [][]platform.Button{
		{
			{Label: "📊 剩余额度", Command: "/usage", Style: platform.ButtonSecondary},
			{Label: "💬 历史会话", Command: "/sessions", Style: platform.ButtonSecondary},
			{Label: "⏰ 定时任务", Command: "/task list", Style: platform.ButtonSecondary},
		},
		{
			{Label: "🧠 长期记忆", Command: "/memory", Style: platform.ButtonSecondary},
			{Label: "🔌 MCP 诊断", Command: "/mcp doctor", Style: platform.ButtonSecondary},
			{Label: "🤖 切换模型", Command: "/model", Style: platform.ButtonPrimary},
		},
		{
			{Label: "⬆️ 检查更新", Command: "/upgrade", Style: platform.ButtonSecondary},
		},
	}
}

func modelButtons(current string, list grok.ModelList) [][]platform.Button {
	if len(list.Names) == 0 {
		return nil
	}
	const perRow = 2
	const maxRows = 5
	var rows [][]platform.Button
	var curRow []platform.Button
	for _, name := range list.Names {
		if len(rows) >= maxRows {
			break
		}
		cmd := "/model " + name
		if len(cmd) > 64 {
			continue
		}
		btn := platform.Button{
			Label:   name,
			Command: cmd,
			Style:   platform.ButtonSecondary,
		}
		if name == current {
			btn.Label = "✓ " + name
			btn.Style = platform.ButtonSuccess
		}
		curRow = append(curRow, btn)
		if len(curRow) == perRow {
			rows = append(rows, curRow)
			curRow = nil
		}
	}
	if len(curRow) > 0 && len(rows) < maxRows {
		rows = append(rows, curRow)
	}
	return rows
}

func sessionsButtons(currentPlat, activeSessionID string, list []store.SavedSession) [][]platform.Button {
	if len(list) == 0 {
		return [][]platform.Button{{
			{Label: "➕ 开启新话题", Command: "/new", Style: platform.ButtonPrimary},
		}}
	}
	const perRow = 2
	const maxSessionRows = 4
	var rows [][]platform.Button
	var curRow []platform.Button
	for _, item := range list {
		if len(rows) >= maxSessionRows {
			break
		}
		title := item.Title
		if title == "" {
			title = "未命名对话"
		}
		shortTitle := shortButtonTitle(title, 10)
		btn := platform.Button{
			Label:   fmt.Sprintf("#%d %s", item.ID, shortTitle),
			Command: fmt.Sprintf("/sessions %d", item.ID),
			Style:   platform.ButtonSecondary,
		}
		switch {
		case activeSessionID != "" && item.SessionID == activeSessionID:
			btn.Label = fmt.Sprintf("✓ #%d %s", item.ID, shortTitle)
			btn.Style = platform.ButtonSuccess
		case item.Platform != "" && item.Platform != currentPlat:
			btn.Label = fmt.Sprintf("🔄 #%d [%s] %s", item.ID, shortPlatformName(item.Platform), shortTitle)
			btn.Style = platform.ButtonPrimary
		}
		curRow = append(curRow, btn)
		if len(curRow) == perRow {
			rows = append(rows, curRow)
			curRow = nil
		}
	}
	if len(curRow) > 0 && len(rows) < maxSessionRows {
		rows = append(rows, curRow)
	}
	rows = append(rows, []platform.Button{
		{Label: "➕ 开启新话题", Command: "/new", Style: platform.ButtonPrimary},
	})
	return rows
}

func newTopicButtons(archivedID int64) [][]platform.Button {
	if archivedID <= 0 {
		return nil
	}
	return [][]platform.Button{{
		{Label: fmt.Sprintf("↩️ 切回 #%d", archivedID), Command: fmt.Sprintf("/sessions %d", archivedID), Style: platform.ButtonSecondary},
		{Label: "📋 全部会话", Command: "/sessions", Style: platform.ButtonSecondary},
	}}
}

func taskListButtons(tasks []store.Task) [][]platform.Button {
	if len(tasks) == 0 {
		return nil
	}
	const maxTaskRows = 5
	var rows [][]platform.Button
	for _, t := range tasks {
		if len(rows) >= maxTaskRows {
			break
		}
		toggleBtn := platform.Button{
			Label:   fmt.Sprintf("⏸ 暂停 #%d", t.ID),
			Command: fmt.Sprintf("/task pause %d", t.ID),
			Style:   platform.ButtonSecondary,
		}
		if t.Paused {
			toggleBtn = platform.Button{
				Label:   fmt.Sprintf("🔔 恢复 #%d", t.ID),
				Command: fmt.Sprintf("/task resume %d", t.ID),
				Style:   platform.ButtonSuccess,
			}
		}
		rows = append(rows, []platform.Button{
			{Label: fmt.Sprintf("▶️ 试跑 #%d", t.ID), Command: fmt.Sprintf("/task run %d", t.ID), Style: platform.ButtonPrimary},
			toggleBtn,
			{Label: fmt.Sprintf("🗑 删除 #%d", t.ID), Command: fmt.Sprintf("/task del %d", t.ID), Style: platform.ButtonDanger},
		})
	}
	return rows
}

func shellButtons(enabled bool) [][]platform.Button {
	if enabled {
		return [][]platform.Button{{
			{Label: "🔴 关闭命令执行", Command: "/shell off", Style: platform.ButtonDanger},
		}}
	}
	return [][]platform.Button{{
		{Label: "🟢 开启命令执行", Command: "/shell on", Style: platform.ButtonSuccess},
	}}
}

func memoryListButtons(list []store.Memory) [][]platform.Button {
	if len(list) == 0 {
		return nil
	}
	const perRow = 3
	const maxDelRows = 3
	var rows [][]platform.Button
	var curRow []platform.Button
	for _, m := range list {
		if len(rows) >= maxDelRows {
			break
		}
		curRow = append(curRow, platform.Button{
			Label:   fmt.Sprintf("🗑 删 #%d", m.ID),
			Command: fmt.Sprintf("/memory del %d", m.ID),
			Style:   platform.ButtonSecondary,
		})
		if len(curRow) == perRow {
			rows = append(rows, curRow)
			curRow = nil
		}
	}
	if len(curRow) > 0 && len(rows) < maxDelRows {
		rows = append(rows, curRow)
	}
	rows = append(rows, []platform.Button{
		{Label: "🧹 清空全部记忆", Command: "/memory clear", Style: platform.ButtonDanger},
	})
	return rows
}
