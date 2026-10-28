package core

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/rshun/grok-bot/internal/actions"
	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/platform"
	"github.com/rshun/grok-bot/internal/sandbox"
)

// noReply is what Grok answers with after reacting when there is nothing to add to the reaction.
const noReply = "[[NO_REPLY]]"

// beginTurn opens the MCP channel for one grok run. It returns the turn, the
// environment entries that hand its token to grok, and a function that ends the
// turn. With no MCP server it returns a nil turn, no env and a no-op end.
// The callbacks that make tools available (react, send_file) are set on the
// turn here as they are added; a tool whose callback is nil is not offered.
func (s *Service) beginTurn(in platform.Inbound, out platform.Replier, fileTools bool) (*actions.Turn, []string, func()) {
	if s.Actions == nil {
		return nil, nil, func() {}
	}
	turn := &actions.Turn{}
	// Scheduled runs have no message to react to.
	if reactor, ok := out.(platform.Reactor); ok && in.MessageID != "" {
		turn.React = func(ctx context.Context, emoji string) error {
			err := reactor.React(ctx, in, emoji)
			var toolErr *actions.ToolError
			if err != nil && !errors.As(err, &toolErr) {
				// Error values can embed request URLs, so only the platform is logged.
				slog.Warn("reaction failed", "platform", in.Platform)
			}
			return err
		}
	}
	// Only a turn with file tools may send files, and only to the chat it came from.
	if sender, ok := out.(platform.FileSender); ok && fileTools && s.Sandbox != nil {
		turn.CheckFile = checkFile(s.Sandbox.Policy, sender.FileLimit())
	}
	env, end := s.Actions.Begin(turn)
	return turn, []string{env}, end
}

// withTurn adds the channel fields for turn to opts.
func withTurn(opts grok.ReplyOptions, turn *actions.Turn, env []string) grok.ReplyOptions {
	if turn == nil {
		return opts
	}
	opts.Actions = true
	opts.React = turn.React != nil
	opts.SendFile = turn.CheckFile != nil
	opts.Env = env
	return opts
}

// maxCaption is the longest caption Telegram accepts; Discord takes more.
const maxCaption = 1024

const errTooBig = "文件超过这个平台的大小上限。"

// checkFile returns the queue-time check for send_file. Its errors are fixed texts that are safe
// to show to Grok.
func checkFile(policy sandbox.Policy, limit int64) func(string) (actions.File, error) {
	return func(path string) (actions.File, error) {
		real, size, err := policy.CheckSendable(path)
		if err != nil {
			return actions.File{}, &actions.ToolError{Msg: err.Error()}
		}
		if size > limit {
			return actions.File{}, &actions.ToolError{Msg: errTooBig}
		}
		return actions.File{Path: real, Name: filepath.Base(real), Size: size}, nil
	}
}

// flushFiles sends the files a turn queued, in call order, each quoting replyTo (empty for none).
// It runs after the answer has been shown. A file that cannot be sent gets a short note without
// the error text, and the remaining files still go out. Nothing is sent to a replier that cannot
// send files.
func (s *Service) flushFiles(ctx context.Context, out platform.Replier, chatID, replyTo string, files []actions.File) {
	sender, ok := out.(platform.FileSender)
	if !ok || len(files) == 0 {
		return
	}
	limit := sender.FileLimit()
	for _, f := range files {
		if ctx.Err() != nil {
			return
		}
		path := f.Path
		file := platform.OutFile{
			Name:    f.Name,
			Caption: clipCaption(f.Caption),
			// Every open checks the file again on the descriptor it returns, so the bytes
			// sent are the bytes that passed the check, whatever happened to the path since.
			Open: func() (io.ReadCloser, int64, error) {
				r, size, err := s.Sandbox.Policy.OpenSendable(path)
				if err != nil {
					return nil, 0, err
				}
				if size > limit {
					r.Close()
					return nil, 0, errors.New(errTooBig)
				}
				return r, size, nil
			},
		}
		if err := sender.SendFile(ctx, chatID, replyTo, file); err != nil {
			slog.Error("send file failed", "chat", chatID, "err", safeErr(err))
			s.send(ctx, out, chatID, "文件 "+f.Name+" 发送失败。")
		}
	}
}

// clipCaption trims a caption and cuts it to maxCaption characters.
func clipCaption(caption string) string {
	caption = strings.TrimSpace(caption)
	if runes := []rune(caption); len(runes) > maxCaption {
		return strings.TrimSpace(string(runes[:maxCaption]))
	}
	return caption
}
