package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rshun/grok-bot/internal/config"
	"github.com/rshun/grok-bot/internal/platform"
	"github.com/rshun/grok-bot/internal/platform/discord"
	"github.com/rshun/grok-bot/internal/platform/telegram"
	"github.com/rshun/grok-bot/internal/sandbox"
	"github.com/rshun/grok-bot/internal/store"
)

type cliMode int

const (
	modeDaemon cliMode = iota
	modeVersion
	modeHelp
	modeRunTask
	modePush
)

type cliInvocation struct {
	Mode     cliMode
	TaskID   int64
	PushArgs []string
}

const cliUsage = `用法：
  grok-bot                         启动机器人常驻服务（Telegram / Discord）
  grok-bot send [选项] "消息内容"   主动推送文字或文件到私聊（支持管道输入）
  grok-bot run-task <任务编号>      单次执行指定编号的定时任务
  grok-bot --version               查看版本号
  grok-bot --help                  查看帮助说明

推送示例：
  grok-bot send "服务升级成功"                # 默认同时推送所有已配置平台（TG + Discord）
  grok-bot send -p tg "仅发 Telegram"        # 仅推 Telegram（-p 写在 send 前后均可）
  grok-bot send -p dc "仅发 Discord"         # 仅推 Discord
  grok-bot send "日报" -f /srv/report.csv    # 推送文字并附带本机文件
  df -h | grok-bot send                      # 从标准输入管道读取内容推送`

const pushUsage = `用法：
  grok-bot send [选项] "消息内容"
  echo "消息内容" | grok-bot send [选项]

选项：
  -p, --platform <telegram|discord>  指定推送平台（支持缩写 tg / dc；默认推送所有已配置平台）
  -c, --chat <CHAT_ID>               指定接收私聊 ID（默认发给最近活跃会话或首个白名单用户）
  -f, --file <路径>                  附带发送本机文件（可重复指定，自动执行安全路径校验）
  -h, --help                         查看推送帮助`

// parseCLI classifies os.Args[1:] before loading config or starting network pollers,
// preventing mistyped arguments from accidentally launching a duplicate daemon.
func parseCLI(args []string) (cliInvocation, error) {
	if len(args) == 0 {
		return cliInvocation{Mode: modeDaemon}, nil
	}
	if len(args) == 1 {
		switch args[0] {
		case "version", "-v", "--version", "-version":
			return cliInvocation{Mode: modeVersion}, nil
		case "help", "-h", "--help", "-help":
			return cliInvocation{Mode: modeHelp}, nil
		case "serve", "daemon":
			return cliInvocation{Mode: modeDaemon}, nil
		case "send", "notify":
			return cliInvocation{Mode: modePush}, nil
		case "run-task":
			return cliInvocation{}, errors.New("run-task 需要指定任务编号，例如：grok-bot run-task 1")
		}
	}
	if args[0] == "run-task" {
		if len(args) != 2 {
			return cliInvocation{}, errors.New("用法：grok-bot run-task <任务编号>")
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || id <= 0 {
			return cliInvocation{}, fmt.Errorf("无效的任务编号 %q", args[1])
		}
		return cliInvocation{Mode: modeRunTask, TaskID: id}, nil
	}
	if args[0] == "send" || args[0] == "notify" {
		return cliInvocation{Mode: modePush, PushArgs: args[1:]}, nil
	}
	// Also accept push flags before `send` / `notify` (e.g. `grok-bot -p dc send "msg"`).
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if tok == "--" {
			break
		}
		switch {
		case tok == "-p" || tok == "--platform" || tok == "-c" || tok == "--chat" || tok == "-f" || tok == "--file":
			i++
		case strings.HasPrefix(tok, "--platform=") || strings.HasPrefix(tok, "--chat=") || strings.HasPrefix(tok, "--file=") || tok == "-h" || tok == "--help":
		case tok == "send" || tok == "notify":
			rest := make([]string, 0, len(args)-1)
			rest = append(rest, args[:i]...)
			rest = append(rest, args[i+1:]...)
			return cliInvocation{Mode: modePush, PushArgs: rest}, nil
		default:
			return cliInvocation{}, fmt.Errorf("未知命令或参数：%s", strings.Join(args, " "))
		}
	}
	return cliInvocation{}, fmt.Errorf("未知命令或参数：%s", strings.Join(args, " "))
}

func isPushHelp(args []string) bool {
	for _, tok := range args {
		if tok == "--" {
			return false
		}
		if tok == "-h" || tok == "--help" {
			return true
		}
	}
	return false
}

type pushRequest struct {
	Platform string
	ChatID   string
	Files    []string
	Text     string
	Help     bool
}

func parsePushArgs(args []string, stdin io.Reader, stdinPiped bool) (pushRequest, error) {
	var req pushRequest
	var words []string
	afterDashDash := false

	for i := 0; i < len(args); i++ {
		tok := args[i]
		if afterDashDash {
			words = append(words, tok)
			continue
		}
		if tok == "--" {
			afterDashDash = true
			continue
		}
		switch {
		case tok == "-h" || tok == "--help":
			req.Help = true
			return req, nil
		case tok == "-p" || tok == "--platform":
			if i+1 >= len(args) {
				return req, errors.New("参数 --platform 缺少值")
			}
			i++
			req.Platform = normalizePlatform(args[i])
			if req.Platform == "" {
				return req, fmt.Errorf("不支持的平台 %q（可选 telegram 或 discord）", args[i])
			}
		case strings.HasPrefix(tok, "--platform="):
			val := strings.TrimPrefix(tok, "--platform=")
			req.Platform = normalizePlatform(val)
			if req.Platform == "" {
				return req, fmt.Errorf("不支持的平台 %q（可选 telegram 或 discord）", val)
			}
		case tok == "-c" || tok == "--chat":
			if i+1 >= len(args) {
				return req, errors.New("参数 --chat 缺少值")
			}
			i++
			req.ChatID = strings.TrimSpace(args[i])
		case strings.HasPrefix(tok, "--chat="):
			req.ChatID = strings.TrimSpace(strings.TrimPrefix(tok, "--chat="))
		case tok == "-f" || tok == "--file":
			if i+1 >= len(args) {
				return req, errors.New("参数 --file 缺少文件路径")
			}
			i++
			req.Files = append(req.Files, strings.TrimSpace(args[i]))
		case strings.HasPrefix(tok, "--file="):
			req.Files = append(req.Files, strings.TrimSpace(strings.TrimPrefix(tok, "--file=")))
		case tok == "-":
			words = append(words, "-")
		case strings.HasPrefix(tok, "-"):
			return req, fmt.Errorf("未知选项 %s", tok)
		default:
			words = append(words, tok)
		}
	}

	text := strings.TrimSpace(strings.Join(words, " "))
	if text == "-" || (text == "" && stdinPiped && stdin != nil) {
		raw, err := io.ReadAll(io.LimitReader(stdin, 64<<10))
		if err != nil {
			return req, fmt.Errorf("读取标准输入失败：%w", err)
		}
		text = strings.TrimSpace(string(raw))
	}
	req.Text = text
	if req.Text == "" && len(req.Files) == 0 {
		return req, errors.New("请提供要发送的消息文字或 --file 附件")
	}
	return req, nil
}

func normalizePlatform(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "telegram", "tg":
		return platform.Telegram
	case "discord", "dc":
		return platform.Discord
	default:
		return ""
	}
}

func runPushCommand(ctx context.Context, cfg config.Config, db *store.Store, policy sandbox.Policy, args []string) error {
	piped := false
	if fi, err := os.Stdin.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) == 0 {
		piped = true
	}
	req, err := parsePushArgs(args, os.Stdin, piped)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error()+"\n\n"+pushUsage)
		return err
	}
	if req.Help {
		fmt.Println(pushUsage)
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	// Pre-validate every requested file before sending anything.
	for _, f := range req.Files {
		abs := f
		if !filepath.IsAbs(abs) {
			if resolved, absErr := filepath.Abs(abs); absErr == nil {
				abs = resolved
			}
		}
		if _, _, checkErr := policy.CheckSendable(abs); checkErr != nil {
			err := fmt.Errorf("文件 %s 校验未通过：%w", f, checkErr)
			fmt.Fprintln(os.Stderr, err)
			return err
		}
	}

	platforms, err := targetPlatforms(ctx, cfg, db, req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return err
	}

	var errs []error
	for _, plat := range platforms {
		if sendErr := sendToPlatform(ctx, plat, cfg, db, policy, req); sendErr != nil {
			wrapped := fmt.Errorf("%s 推送失败：%w", plat, sendErr)
			fmt.Fprintln(os.Stderr, wrapped)
			errs = append(errs, wrapped)
		}
	}
	return errors.Join(errs...)
}

func targetPlatforms(ctx context.Context, cfg config.Config, db *store.Store, req pushRequest) ([]string, error) {
	if req.Platform != "" {
		switch req.Platform {
		case platform.Telegram:
			if cfg.TelegramToken == "" {
				return nil, errors.New("未配置 TELEGRAM_BOT_TOKEN")
			}
			return []string{platform.Telegram}, nil
		case platform.Discord:
			if cfg.DiscordToken == "" {
				return nil, errors.New("未配置 DISCORD_BOT_TOKEN")
			}
			return []string{platform.Discord}, nil
		default:
			return nil, fmt.Errorf("不支持的平台 %q", req.Platform)
		}
	}

	var configured []string
	if cfg.TelegramToken != "" {
		configured = append(configured, platform.Telegram)
	}
	if cfg.DiscordToken != "" {
		configured = append(configured, platform.Discord)
	}
	if len(configured) == 0 {
		return nil, errors.New("未配置 TELEGRAM_BOT_TOKEN 或 DISCORD_BOT_TOKEN")
	}
	if len(configured) == 1 || req.ChatID == "" {
		return configured, nil
	}

	// Both platforms are configured and --chat was specified without -p: infer if unambiguous.
	tgMatch := hasID(cfg.TelegramUsers, req.ChatID) || hasKnownChat(ctx, db, platform.Telegram, req.ChatID)
	dcMatch := hasID(cfg.DiscordUsers, req.ChatID) || hasKnownChat(ctx, db, platform.Discord, req.ChatID)
	switch {
	case tgMatch && !dcMatch:
		return []string{platform.Telegram}, nil
	case dcMatch && !tgMatch:
		return []string{platform.Discord}, nil
	default:
		return nil, errors.New("同时配置了 Telegram 和 Discord，使用 --chat 时请配合 -p tg 或 -p dc 指定目标平台")
	}
}

func sendToPlatform(ctx context.Context, plat string, cfg config.Config, db *store.Store, policy sandbox.Policy, req pushRequest) error {
	var sender interface {
		platform.Replier
		platform.FileSender
	}
	chatID := req.ChatID

	switch plat {
	case platform.Telegram:
		bot, err := telegram.New(cfg.TelegramToken)
		if err != nil {
			return err
		}
		sender = bot
		if chatID == "" {
			chatID = pickRecipientChat(ctx, db, platform.Telegram, cfg.TelegramUsers)
		}
		if chatID == "" {
			return errors.New("无法确定 Telegram 接收人，请用 --chat 指定 chat_id")
		}
	case platform.Discord:
		bot, err := discord.New(cfg.DiscordToken)
		if err != nil {
			return err
		}
		sender = bot
		if chatID == "" {
			if db != nil {
				if known, err := db.KnownChats(ctx, platform.Discord); err == nil && len(known) > 0 {
					chatID = known[0]
				}
			}
			if chatID == "" {
				if uid := firstAllowedUser(cfg.DiscordUsers); uid != "" {
					dmID, dmErr := bot.OpenDM(ctx, uid)
					if dmErr != nil {
						return fmt.Errorf("创建 Discord 私聊通道失败：%w", dmErr)
					}
					chatID = dmID
				}
			}
		} else if _, isUser := cfg.DiscordUsers[chatID]; isUser {
			if dmID, dmErr := bot.OpenDM(ctx, chatID); dmErr == nil && dmID != "" {
				chatID = dmID
			}
		}
		if chatID == "" {
			return errors.New("无法确定 Discord 接收频道，请用 --chat 指定")
		}
	}

	if req.Text != "" {
		if err := sender.Send(ctx, chatID, req.Text); err != nil {
			return err
		}
	}
	limit := sender.FileLimit()
	for _, rawPath := range req.Files {
		absPath := rawPath
		if !filepath.IsAbs(absPath) {
			if resolved, absErr := filepath.Abs(absPath); absErr == nil {
				absPath = resolved
			}
		}
		realPath, size, err := policy.CheckSendable(absPath)
		if err != nil {
			return fmt.Errorf("文件 %s：%w", rawPath, err)
		}
		if size > limit {
			return fmt.Errorf("文件 %s 超过平台大小上限", filepath.Base(realPath))
		}
		outFile := platform.OutFile{
			Name: filepath.Base(realPath),
			Open: func() (io.ReadCloser, int64, error) {
				f, sz, openErr := policy.OpenSendable(realPath)
				if openErr != nil {
					return nil, 0, openErr
				}
				return f, sz, nil
			},
		}
		if err := sender.SendFile(ctx, chatID, "", outFile); err != nil {
			return fmt.Errorf("发送文件 %s 失败：%w", filepath.Base(realPath), err)
		}
	}
	return nil
}

func hasID(m map[string]struct{}, id string) bool {
	if len(m) == 0 {
		return false
	}
	_, ok := m[id]
	return ok
}

func hasKnownChat(ctx context.Context, db *store.Store, plat, id string) bool {
	if db == nil {
		return false
	}
	known, err := db.KnownChats(ctx, plat)
	if err != nil {
		return false
	}
	for _, k := range known {
		if k == id {
			return true
		}
	}
	return false
}

func pickRecipientChat(ctx context.Context, db *store.Store, plat string, allowed map[string]struct{}) string {
	if db != nil {
		if known, err := db.KnownChats(ctx, plat); err == nil && len(known) > 0 {
			return known[0]
		}
	}
	return firstAllowedUser(allowed)
}

func firstAllowedUser(allowed map[string]struct{}) string {
	if len(allowed) == 0 {
		return ""
	}
	ids := make([]string, 0, len(allowed))
	for id := range allowed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids[0]
}
