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
    查看可写目录
/dir add rw /绝对路径
    允许写入目录
/dir del /绝对路径
    取消写入权限
也可以直接说"允许写入 /srv/out"或"取消 /srv/out 的写入权限"。`

const readNotNeeded = "读取由系统权限控制，不需要开放。"

const resetReminder = "提示：这个聊天的会话创建于开启文件功能之前，发送 /reset 后才能读写本机文件（会清空这个聊天的上下文）。"

func (s *Service) dirCommand(ctx context.Context, in platform.Inbound, arg string) (string, error) {
	if arg == "" || arg == "list" {
		return s.dirList(ctx, in)
	}
	fields := strings.SplitN(arg, " ", 3)
	switch {
	case fields[0] == "add" && len(fields) == 3 && fields[1] == "ro":
		return readNotNeeded, nil
	case fields[0] == "add" && len(fields) == 3 && fields[1] == "rw":
		path, err := s.grantDir(ctx, fields[2])
		if err != nil {
			return err.Error(), nil
		}
		reply := fmt.Sprintf("已添加 %s（可读写）。", path)
		if ready, _, _, reason := s.fileAccess(ctx); !ready {
			reply += "\n文件功能当前不可用：" + reason
		}
		return reply, nil
	case fields[0] == "del" && len(fields) >= 2:
		path := strings.TrimSpace(strings.TrimPrefix(arg, "del"))
		removed, err := s.revokeDir(ctx, path)
		if err != nil {
			return "", err
		}
		if !removed {
			return "名单里没有 " + path + "。", nil
		}
		return "已移除 " + path + "。", nil
	default:
		return dirUsage, nil
	}
}

func (s *Service) dirList(ctx context.Context, in platform.Inbound) (string, error) {
	rw, err := s.writeDirs(ctx)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("读取由系统权限控制。\n")
	if len(rw) == 0 {
		b.WriteString("还没有可写目录。")
	} else {
		b.WriteString("可写目录：\n")
		for _, path := range rw {
			fmt.Fprintf(&b, "- %s\n", path)
		}
	}
	ready, _, _, reason := s.fileAccess(ctx)
	if !ready {
		b.WriteString("\n文件功能当前不可用：" + reason)
	}
	return strings.TrimSpace(b.String()), nil
}

// applyGrants grants and revokes write access named in the user's own message
// and returns the confirmation lines for the reply.
func (s *Service) applyGrants(ctx context.Context, text string) []string {
	grant, revoke := parseGrants(text)
	var notes []string
	for _, raw := range revoke {
		removed, err := s.revokeDir(ctx, raw)
		switch {
		case err != nil:
			slog.Error("revoke dir", "err", err)
			notes = append(notes, "取消 "+raw+" 的写入权限失败。")
		case removed:
			notes = append(notes, "已取消 "+raw+" 的写入权限。")
		default:
			notes = append(notes, raw+" 本来就没有写入权限。")
		}
	}
	for _, raw := range grant {
		path, err := s.grantDir(ctx, raw)
		if err != nil {
			notes = append(notes, "没有允许写入 "+raw+"："+err.Error())
			continue
		}
		notes = append(notes, "已允许写入 "+path+"。")
	}
	if len(grant) > 0 {
		if ready, _, _, reason := s.fileAccess(ctx); !ready {
			notes = append(notes, "文件功能当前不可用："+reason)
		}
	}
	if mode, ok := parseShellToggle(text); ok {
		if err := s.Store.PutSetting(ctx, "allow_shell", mode); err != nil {
			slog.Error("save allow_shell", "err", err)
			notes = append(notes, "保存命令执行开关失败。")
		} else if mode == "true" {
			notes = append(notes, "已开启服务器命令执行权限。")
		} else {
			notes = append(notes, "已关闭服务器命令执行权限。")
		}
	}
	return notes
}

func parseShellToggle(text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	for _, on := range []string{"开启命令执行", "允许执行命令", "打开命令执行"} {
		if strings.Contains(trimmed, on) {
			return "true", true
		}
	}
	for _, off := range []string{"关闭命令执行", "禁止执行命令", "取消命令执行"} {
		if strings.Contains(trimmed, off) {
			return "false", true
		}
	}
	return "", false
}

func (s *Service) shellEnabled(ctx context.Context) bool {
	if s.Store != nil {
		if val, ok, err := s.Store.Setting(ctx, "allow_shell"); err == nil && ok {
			return val == "true"
		}
	}
	return s.AllowShell
}

func (s *Service) shellCommand(ctx context.Context, arg string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "":
		state := "已关闭"
		if s.shellEnabled(ctx) {
			state = "已开启"
		}
		return "当前服务器命令执行：" + state + "。\n用法：/shell on 开启，/shell off 关闭（或直接说「开启命令执行」/「关闭命令执行」）。", nil
	case "on", "true", "1", "enable":
		if err := s.Store.PutSetting(ctx, "allow_shell", "true"); err != nil {
			return "", err
		}
		return "已开启服务器命令执行权限。", nil
	case "off", "false", "0", "disable":
		if err := s.Store.PutSetting(ctx, "allow_shell", "false"); err != nil {
			return "", err
		}
		return "已关闭服务器命令执行权限。", nil
	default:
		return "用法：/shell on 开启，/shell off 关闭。", nil
	}
}

// grantDir checks a path and records it as writable. Errors are for the chat user.
func (s *Service) grantDir(ctx context.Context, raw string) (string, error) {
	if s.Sandbox == nil {
		return "", errors.New("没有配置沙箱，不能允许写入。")
	}
	path, err := s.Sandbox.Policy.CheckDir(raw)
	if err != nil {
		return "", err
	}
	if err := s.Store.PutDir(ctx, path, "rw"); err != nil {
		slog.Error("save dir", "err", err)
		return "", errors.New("保存失败。")
	}
	return path, nil
}

// revokeDir removes a writable directory, matching the stored real path when needed,
// and rewrites the profile so the change reaches the next grok run.
func (s *Service) revokeDir(ctx context.Context, raw string) (bool, error) {
	candidates := []string{raw, filepath.Clean(raw)}
	if real, err := filepath.EvalSymlinks(filepath.Clean(raw)); err == nil {
		candidates = append(candidates, real)
	}
	for _, path := range candidates {
		removed, err := s.Store.DeleteDir(ctx, path)
		if err != nil {
			return false, err
		}
		if removed {
			s.fileAccess(ctx)
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) writeDirs(ctx context.Context) ([]string, error) {
	dirs, err := s.Store.Dirs(ctx)
	if err != nil {
		return nil, err
	}
	var rw []string
	for _, d := range dirs {
		// Read grants from v0.2.0 stay in the table but no longer mean anything.
		if d.Mode == "rw" {
			rw = append(rw, d.Path)
		}
	}
	return rw, nil
}

// fileAccess syncs the sandbox profile and reports whether file tools can be offered,
// the sandbox profile to pass to grok (empty when bubblewrap is missing), and the writable dirs.
// reason explains to the chat user why file tools cannot be offered.
func (s *Service) fileAccess(ctx context.Context) (ready bool, profile string, rw []string, reason string) {
	if s.Sandbox == nil {
		return false, "", nil, "没有配置沙箱。"
	}
	if !s.Sandbox.Enabled {
		return false, "", nil, "这台机器的系统不支持 grok 沙箱（只支持 Linux 和 macOS）。"
	}
	rw, err := s.writeDirs(ctx)
	if err != nil {
		slog.Error("load dirs", "err", err)
		return false, "", nil, "读取目录名单失败。"
	}
	profile = sandbox.ProfileName
	if err := s.Sandbox.Sync(rw); err != nil {
		if errors.Is(err, sandbox.ErrUnmanaged) {
			slog.Error("sandbox profile is not managed by the bot", "path", s.Sandbox.Path)
			return false, "", nil, s.Sandbox.Path + " 不是 bot 生成的，请管理员手动合并 grokbot 配置。"
		}
		if !errors.Is(err, sandbox.ErrDenyUnavailable) {
			slog.Error("write sandbox profile", "err", err)
			return false, "", nil, "写入沙箱配置失败。"
		}
		// On Linux, grok adds runtime sockets (/run/systemd/private, etc.) to every custom
		// profile's deny list and refuses to start without bwrap. Run unsandboxed while
		// keeping file tools and send_file's own deny checks enabled.
		profile = ""
	}
	return true, profile, rw, ""
}

// newSessionOptions fills the sandbox and shell fields for a brand-new session and returns the profile to record.
func (s *Service) newSessionOptions(ctx context.Context, opts *grok.ReplyOptions) string {
	opts.ShellTools = s.shellEnabled(ctx)
	ready, profile, rw, _ := s.fileAccess(ctx)
	if !ready {
		return ""
	}
	opts.Sandbox = profile
	opts.FileTools = true
	opts.WriteDirs = rw
	return profile
}
