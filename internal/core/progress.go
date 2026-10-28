package core

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/platform"
)

const (
	progressStartText = "处理中…"
	// progressDetailMax bounds the tool detail shown in the progress message.
	progressDetailMax = 80
	// defaultProgressInterval keeps edits well under the platforms' rate limits.
	defaultProgressInterval = 3 * time.Second
	// progressCloseTimeout bounds the final edit or delete, which must outlive a cancelled turn.
	progressCloseTimeout = 10 * time.Second
)

// progressKeys are the tool input fields that carry something worth showing, in priority order.
var progressKeys = []string{"query", "url", "target_file", "path", "target_directory", "file_path", "tool_name"}

// progressText turns a tool call into one short status line.
func progressText(t grok.ToolUse) string {
	var label string
	switch t.Name {
	case "web_search":
		label = "正在搜索"
	case "web_fetch":
		label = "正在打开网页"
	case "read_file", "list_dir", "grep":
		label = "正在读取"
	case "write", "search_replace":
		label = "正在写入"
	case "use_tool":
		label = "正在调用工具"
	case "search_tool":
		return "正在查找工具"
	default:
		return withDetail("正在处理", oneLine(t.Name))
	}
	return withDetail(label, toolDetail(t.Input))
}

func withDetail(label, detail string) string {
	if detail == "" {
		return label
	}
	return label + "：" + detail
}

// toolDetail returns the first usable string field of a tool input, on one line and cut short.
func toolDetail(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(input, &fields); err != nil {
		return ""
	}
	for _, key := range progressKeys {
		raw, ok := fields[key]
		if !ok {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			continue
		}
		if value = oneLine(value); value != "" {
			return value
		}
	}
	return ""
}

// oneLine collapses all whitespace runs to single spaces and cuts the result to progressDetailMax characters.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) > progressDetailMax {
		return string(runes[:progressDetailMax])
	}
	return s
}

// progressView drives one editable progress message and rate-limits its edits.
type progressView struct {
	handle   platform.ProgressHandle
	platform string
	interval time.Duration

	mu       sync.Mutex
	lastText string
	lastEdit time.Time
}

// startProgress opens a progress message for a normal chat message. It returns nil when the
// item is a command or task, the platform cannot edit messages, or the message cannot be sent;
// callers then answer through the plain send path.
func (s *Service) startProgress(ctx context.Context, it item) *progressView {
	if it.task != nil {
		return nil
	}
	if _, _, command := parseInboundCommand(it.in); command {
		return nil
	}
	progress, ok := it.out.(platform.Progress)
	if !ok {
		return nil
	}
	// Discord's typing indicator lasts only a few seconds, so refresh it when the turn starts.
	if it.in.Platform == platform.Discord {
		if receipt, ok := it.out.(platform.Receipt); ok {
			if err := receipt.MarkReceived(ctx, it.in); err != nil {
				slog.Warn("message receipt failed", "platform", it.in.Platform)
			}
		}
	}
	handle, err := progress.StartProgress(ctx, it.in, progressStartText)
	if err != nil {
		// Error values can embed request URLs, so only the platform is logged.
		slog.Warn("progress start failed", "platform", it.in.Platform)
		return nil
	}
	interval := s.ProgressInterval
	if interval <= 0 {
		interval = defaultProgressInterval
	}
	return &progressView{handle: handle, platform: it.in.Platform, interval: interval, lastText: progressStartText}
}

// Show edits the progress message unless the text is unchanged or the last edit was too recent.
func (v *progressView) Show(ctx context.Context, text string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if text == v.lastText {
		return
	}
	if !v.lastEdit.IsZero() && time.Since(v.lastEdit) < v.interval {
		return
	}
	v.lastEdit = time.Now()
	if err := v.handle.Update(ctx, text); err != nil {
		slog.Warn("progress update failed", "platform", v.platform)
		return
	}
	v.lastText = text
}

// Finish replaces the progress message with the final text. The handle already falls back to
// new messages when the edit fails, so a returned error is only logged by the caller.
func (v *progressView) Finish(ctx context.Context, text string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	ctx, cancel := detached(ctx)
	defer cancel()
	err := v.handle.Finish(ctx, text)
	if err != nil {
		slog.Warn("progress finish failed", "platform", v.platform)
	}
	return err
}

// detached returns a short-lived context that is not cancelled with the turn, so the progress
// message is still closed when the bot shuts down mid-turn.
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), progressCloseTimeout)
}

// Delete removes the progress message.
func (v *progressView) Delete(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	ctx, cancel := detached(ctx)
	defer cancel()
	err := v.handle.Delete(ctx)
	if err != nil {
		slog.Warn("progress delete failed", "platform", v.platform)
	}
	return err
}

// onTool returns the OnTool callback for a turn, or nil when there is no progress message.
func (v *progressView) onTool(ctx context.Context) func(grok.ToolUse) {
	if v == nil {
		return nil
	}
	return func(t grok.ToolUse) { v.Show(ctx, progressText(t)) }
}
