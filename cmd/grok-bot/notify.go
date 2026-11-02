package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rshun/grok-bot/internal/config"
	"github.com/rshun/grok-bot/internal/platform"
	"github.com/rshun/grok-bot/internal/platform/discord"
	"github.com/rshun/grok-bot/internal/platform/telegram"
	"github.com/rshun/grok-bot/internal/sandbox"
	"github.com/rshun/grok-bot/internal/store"
)

const pushUsage = `用法：
  grok-bot send [选项] "消息内容"
  echo "消息内容" | grok-bot send [选项]

选项：
  -p, --platform <telegram|discord>  指定推送平台（默认优先 Telegram，其次 Discord）
  -c, --chat <CHAT_ID>               指定接收私聊 ID（默认发给最近活跃会话或首个白名单用户）
  -f, --file <绝对路径>              附带发送本机文件（可重复指定，自动执行安全路径校验）`

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

	plat := req.Platform
	if plat == "" {
		if cfg.TelegramToken != "" {
			plat = platform.Telegram
		} else {
			plat = platform.Discord
		}
	}

	// Pre-validate every requested file before sending anything.
	for _, f := range req.Files {
		abs := f
		if !filepath.IsAbs(abs) {
			if resolved, absErr := filepath.Abs(abs); absErr == nil {
				abs = resolved
			}
		}
		if _, _, checkErr := policy.CheckSendable(abs); checkErr != nil {
			return fmt.Errorf("文件 %s 校验未通过：%w", f, checkErr)
		}
	}

	var sender interface {
		platform.Replier
		platform.FileSender
	}
	chatID := req.ChatID

	switch plat {
	case platform.Telegram:
		if cfg.TelegramToken == "" {
			return errors.New("未配置 TELEGRAM_BOT_TOKEN")
		}
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
		if cfg.DiscordToken == "" {
			return errors.New("未配置 DISCORD_BOT_TOKEN")
		}
		bot, err := discord.New(cfg.DiscordToken)
		if err != nil {
			return err
		}
		sender = bot
		if chatID == "" {
			if known, _ := db.KnownChats(ctx, platform.Discord); len(known) > 0 {
				chatID = known[0]
			} else if uid := firstAllowedUser(cfg.DiscordUsers); uid != "" {
				dmID, dmErr := bot.OpenDM(ctx, uid)
				if dmErr != nil {
					return fmt.Errorf("创建 Discord 私聊通道失败：%w", dmErr)
				}
				chatID = dmID
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
