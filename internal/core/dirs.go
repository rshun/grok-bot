package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/platform"
	"github.com/rshun/grok-bot/internal/sandbox"
)

const dirUsage = `用法：
/dir
    查看开放的目录
/dir add ro /绝对路径
    开放只读目录
/dir add rw /绝对路径
    开放可读写目录
/dir del /绝对路径
    移除目录`

func (s *Service) dirCommand(ctx context.Context, in platform.Inbound, arg string) (string, error) {
	if arg == "" || arg == "list" {
		return s.dirList(ctx, in)
	}
	fields := strings.SplitN(arg, " ", 3)
	switch {
	case fields[0] == "add" && len(fields) == 3 && (fields[1] == "ro" || fields[1] == "rw"):
		if s.Sandbox == nil {
			return "没有配置沙箱，不能开放目录。", nil
		}
		path, err := s.Sandbox.Policy.CheckDir(fields[2])
		if err != nil {
			return err.Error(), nil
		}
		if err := s.Store.PutDir(ctx, path, fields[1]); err != nil {
			return "", err
		}
		reply := fmt.Sprintf("已添加 %s（%s）。新会话立即可用；已有会话可能需要 /reset。", path, modeLabel(fields[1]))
		if ready, _, _, reason := s.fileAccess(ctx); !ready {
			reply += "\n文件功能当前不可用：" + reason
		}
		return reply, nil
	case fields[0] == "del" && len(fields) >= 2:
		path := strings.TrimSpace(strings.TrimPrefix(arg, "del"))
		removed, err := s.Store.DeleteDir(ctx, path)
		if err != nil {
			return "", err
		}
		if !removed && filepath.Clean(path) != path {
			removed, err = s.Store.DeleteDir(ctx, filepath.Clean(path))
			if err != nil {
				return "", err
			}
		}
		if !removed {
			return "名单里没有 " + path + "。", nil
		}
		// Rewrite the profile now so the removal applies to the next grok run.
		s.fileAccess(ctx)
		return "已移除 " + path + "。", nil
	default:
		return dirUsage, nil
	}
}

func (s *Service) dirList(ctx context.Context, in platform.Inbound) (string, error) {
	dirs, err := s.Store.Dirs(ctx)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if len(dirs) == 0 {
		b.WriteString("还没有开放目录。")
	} else {
		b.WriteString("开放的目录：\n")
		for _, d := range dirs {
			fmt.Fprintf(&b, "- %s（%s）\n", d.Path, modeLabel(d.Mode))
		}
	}
	ready, _, _, reason := s.fileAccess(ctx)
	if !ready {
		b.WriteString("\n文件功能当前不可用：" + reason)
		return strings.TrimSpace(b.String()), nil
	}
	chat, err := s.Store.Chat(ctx, in.Platform, in.ChatID)
	if err != nil {
		return "", err
	}
	if chat.SessionID != "" && chat.Sandbox != sandbox.ProfileName {
		b.WriteString("\n这个聊天的会话创建于开启文件功能之前，发送 /reset 后可以使用文件功能。")
	}
	return strings.TrimSpace(b.String()), nil
}

func modeLabel(mode string) string {
	if mode == "rw" {
		return "可读写"
	}
	return "只读"
}

// fileAccess syncs the sandbox profile and reports whether file tools can be offered.
// reason explains to the chat user why they cannot.
func (s *Service) fileAccess(ctx context.Context) (ready bool, ro, rw []string, reason string) {
	if s.Sandbox == nil {
		return false, nil, nil, "没有配置沙箱。"
	}
	if !s.Sandbox.Enabled {
		return false, nil, nil, "这台机器的系统不支持 grok 沙箱（只支持 Linux 和 macOS）。"
	}
	dirs, err := s.Store.Dirs(ctx)
	if err != nil {
		slog.Error("load dirs", "err", err)
		return false, nil, nil, "读取目录名单失败。"
	}
	for _, d := range dirs {
		if d.Mode == "rw" {
			rw = append(rw, d.Path)
		} else {
			ro = append(ro, d.Path)
		}
	}
	if err := s.Sandbox.Sync(ro, rw); err != nil {
		if errors.Is(err, sandbox.ErrUnmanaged) {
			slog.Error("sandbox profile is not managed by the bot", "path", s.Sandbox.Path)
			return false, nil, nil, s.Sandbox.Path + " 不是 bot 生成的，请管理员手动合并 grokbot 配置。"
		}
		slog.Error("write sandbox profile", "err", err)
		return false, nil, nil, "写入沙箱配置失败。"
	}
	return true, ro, rw, ""
}

// newSessionOptions fills the sandbox fields for a brand-new session and returns the profile to record.
func (s *Service) newSessionOptions(ctx context.Context, opts *grok.ReplyOptions) string {
	ready, ro, rw, _ := s.fileAccess(ctx)
	if !ready {
		return ""
	}
	opts.Sandbox = sandbox.ProfileName
	opts.FileTools = true
	opts.ReadDirs = ro
	opts.WriteDirs = rw
	return sandbox.ProfileName
}
