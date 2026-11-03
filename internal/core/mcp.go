package core

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/platform"
)

// MCPEngine is an optional extension implemented by *grok.Client for status and MCP management.
type MCPEngine interface {
	Version(ctx context.Context) (string, error)
	MCPList(ctx context.Context) (string, error)
	MCPDoctor(ctx context.Context, name string) (string, error)
	MCPAdd(ctx context.Context, args []string) (string, error)
	MCPRemove(ctx context.Context, name string) (string, error)
	MCPEnable(ctx context.Context, name string, enable bool) (string, error)
	ResetLeader(ctx context.Context)
}

const mcpUsage = `用法：
/mcp 或 /mcp list
    查看已配置的 MCP 服务列表
/mcp doctor [名称]
    检查全部（或单个）MCP 服务的握手连通状态
/mcp add <名称> <HTTP_URL>
    新增或更新远程 HTTP MCP（同名自动覆盖更新）
/mcp add <名称> -- <命令> [参数...]
    新增或更新本地 stdio MCP（如 /mcp add fetch -- uvx mcp-server-fetch）
/mcp enable <名称> / /mcp disable <名称>
    启用或停用某个 MCP 服务
/mcp del <名称>
    删除某个 MCP 服务

安全提示：涉及鉴权 Token 的变量建议写入服务器 ~/.bash_profile，避免在聊天记录中明文发送密钥。`

func (s *Service) statusCommand(ctx context.Context, in platform.Inbound) (string, [][]platform.Button, error) {
	chat, err := s.Store.Chat(ctx, in.Platform, in.ChatID)
	if err != nil {
		return "", nil, err
	}
	memories, _ := s.Store.AllMemories(ctx)
	sessions, _ := s.Store.AllSessions(ctx)
	tasks, _ := s.Store.AllTasks(ctx)
	rwDirs, _ := s.writeDirs(ctx)

	activeTasks, pausedTasks := 0, 0
	for _, t := range tasks {
		if t.Paused {
			pausedTasks++
		} else {
			activeTasks++
		}
	}

	sessionDesc := "未建立（下一条消息开启新会话）"
	if chat.SessionID != "" {
		short := chat.SessionID
		if len(short) > 8 {
			short = short[:8] + "…"
		}
		sessionDesc = short
		for _, item := range sessions {
			if item.SessionID == chat.SessionID {
				sessionDesc = fmt.Sprintf("#%d %s (%s)", item.ID, item.Title, short)
				break
			}
		}
	}

	var b strings.Builder
	botVer := s.Version
	if botVer == "" {
		botVer = "dev"
	}
	fmt.Fprintf(&b, "运行状态（grok-bot %s）\n", botVer)
	if mcpEng, ok := s.Engine.(MCPEngine); ok {
		if cliVer, verErr := mcpEng.Version(ctx); verErr == nil && cliVer != "" {
			fmt.Fprintf(&b, "- Grok CLI：%s\n", cliVer)
		}
	}
	fmt.Fprintf(&b, "- 当前模型：%s\n", chat.Model)
	fmt.Fprintf(&b, "- 当前会话：%s（共保存 %d 个跨平台历史会话）\n", sessionDesc, len(sessions))
	fmt.Fprintf(&b, "- 联网搜索：%s\n", onOffText(s.WebSearch))
	fmt.Fprintf(&b, "- 外链预处理：%s\n", onOffText(!s.DisableLinkPrefetch))
	fmt.Fprintf(&b, "- 命令执行：%s\n", onOffText(s.shellEnabled(ctx)))
	fmt.Fprintf(&b, "- 可写目录：%d 个\n", len(rwDirs))
	fmt.Fprintf(&b, "- 长期记忆：%d 条（TG / Discord 同步，/reset 后保留）\n", len(memories))
	fmt.Fprintf(&b, "- 定时任务：%d 个运行中 / %d 个已暂停\n", activeTasks, pausedTasks)

	if mcpEng, ok := s.Engine.(MCPEngine); ok {
		if mcpOut, mcpErr := mcpEng.MCPList(ctx); mcpErr == nil && mcpOut != "" {
			b.WriteString("\nMCP 服务列表（可用 `/mcp doctor` 检查握手连通性）：\n```\n")
			b.WriteString(mcpOut)
			b.WriteString("\n```")
		}
	}
	return strings.TrimSpace(b.String()), statusButtons(), nil
}

func onOffText(on bool) string {
	if on {
		return "已开启"
	}
	return "已关闭"
}

func (s *Service) mcpCommand(ctx context.Context, arg string) (string, error) {
	mcpEng, ok := s.Engine.(MCPEngine)
	if !ok {
		return "当前运行引擎不支持 MCP 管理命令。", nil
	}
	tokens, err := splitCommandArgs(strings.TrimSpace(arg))
	if err != nil {
		return "参数引号未闭合：" + err.Error(), nil
	}
	if len(tokens) == 0 || strings.EqualFold(tokens[0], "list") || strings.EqualFold(tokens[0], "ls") {
		out, err := mcpEng.MCPList(ctx)
		if err != nil {
			return "", err
		}
		if out == "" {
			return "当前没有配置任何 MCP 服务。\n\n" + mcpUsage, nil
		}
		return "MCP 服务列表：\n```\n" + out + "\n```", nil
	}

	sub := strings.ToLower(tokens[0])
	rest := tokens[1:]
	switch sub {
	case "help":
		return mcpUsage, nil
	case "doctor", "check", "ping":
		if len(rest) > 1 {
			return "用法：`/mcp doctor` 或 `/mcp doctor <名称>`", nil
		}
		name := ""
		if len(rest) == 1 {
			name = rest[0]
		}
		out, err := mcpEng.MCPDoctor(ctx, name)
		if err != nil {
			return "MCP 诊断失败：" + grok.RedactSensitive(err.Error()), nil
		}
		if out == "" {
			return "MCP 诊断完成（无输出）。", nil
		}
		return "MCP 连通性诊断：\n```\n" + out + "\n```", nil
	case "add", "set", "update":
		if len(rest) < 2 {
			return mcpUsage, nil
		}
		out, err := mcpEng.MCPAdd(ctx, rest)
		if err != nil {
			return "配置 MCP 失败：" + grok.RedactSensitive(err.Error()), nil
		}
		if out != "" {
			return "已保存 MCP 配置并刷新守护进程：\n```\n" + out + "\n```", nil
		}
		return "已保存 MCP 配置并刷新守护进程（可用 `/mcp doctor` 验证握手）。", nil
	case "del", "delete", "remove", "rm":
		if len(rest) != 1 {
			return "用法：`/mcp del <名称>`", nil
		}
		out, err := mcpEng.MCPRemove(ctx, rest[0])
		if err != nil {
			return "删除 MCP 失败：" + grok.RedactSensitive(err.Error()), nil
		}
		if out != "" {
			return fmt.Sprintf("已移除 MCP `%s` 并刷新守护进程：\n```\n%s\n```", rest[0], out), nil
		}
		return fmt.Sprintf("已移除 MCP `%s` 并刷新守护进程。", rest[0]), nil
	case "enable", "on":
		if len(rest) != 1 {
			return "用法：`/mcp enable <名称>`", nil
		}
		if _, err := mcpEng.MCPEnable(ctx, rest[0], true); err != nil {
			return "启用 MCP 失败：" + grok.RedactSensitive(err.Error()), nil
		}
		return fmt.Sprintf("已启用 MCP `%s` 并刷新守护进程。", rest[0]), nil
	case "disable", "off":
		if len(rest) != 1 {
			return "用法：`/mcp disable <名称>`", nil
		}
		if _, err := mcpEng.MCPEnable(ctx, rest[0], false); err != nil {
			return "停用 MCP 失败：" + grok.RedactSensitive(err.Error()), nil
		}
		return fmt.Sprintf("已停用 MCP `%s` 并刷新守护进程。", rest[0]), nil
	case "reload", "restart":
		mcpEng.ResetLeader(ctx)
		return "已重启 Grok 后台守护进程，下一轮对话将重新握手全部 MCP 服务。", nil
	default:
		return mcpUsage, nil
	}
}

// splitCommandArgs splits a shell-like argument line respecting single and double quotes.
func splitCommandArgs(s string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inQuote := rune(0)
	escaped := false
	tokenStarted := false

	for _, r := range s {
		if escaped {
			cur.WriteRune(r)
			escaped = false
			tokenStarted = true
			continue
		}
		if r == '\\' && inQuote != '\'' {
			escaped = true
			tokenStarted = true
			continue
		}
		if inQuote != 0 {
			if r == inQuote {
				inQuote = 0
			} else {
				cur.WriteRune(r)
			}
			tokenStarted = true
			continue
		}
		if r == '"' || r == '\'' {
			inQuote = r
			tokenStarted = true
			continue
		}
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if tokenStarted {
				args = append(args, cur.String())
				cur.Reset()
				tokenStarted = false
			}
			continue
		}
		cur.WriteRune(r)
		tokenStarted = true
	}
	if inQuote != 0 {
		return nil, errors.New("缺少匹配的引号")
	}
	if escaped {
		cur.WriteRune('\\')
	}
	if tokenStarted {
		args = append(args, cur.String())
	}
	return args, nil
}
