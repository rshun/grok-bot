package core

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/platform"
	"github.com/rshun/grok-bot/internal/sandbox"
	"github.com/rshun/grok-bot/internal/store"
)

// Engine is the Grok subscription surface used by the chat core.
type Engine interface {
	Reply(ctx context.Context, opts grok.ReplyOptions) (grok.HeadlessResult, error)
	Models(ctx context.Context) (grok.ModelList, error)
	Allowance(ctx context.Context) (grok.Allowance, error)
}

// Service accepts chat messages, keeps per-chat context, and answers commands.
type Service struct {
	Store  *store.Store
	Engine Engine
	// AllowedUsers maps a platform name to the user IDs that may talk to the bot there.
	AllowedUsers map[string]map[string]struct{}
	QueueLimit   int
	Version      string
	// WebSearch lets Grok search the web and open pages during chat turns.
	WebSearch bool
	// Sandbox limits file access for new sessions; nil turns file tools off.
	Sandbox *sandbox.Manager
	// Senders post scheduled task results to a chat, keyed by platform name.
	Senders         map[string]platform.Replier
	Location        *time.Location
	TaskLimit       int
	TaskMinInterval time.Duration
	// ProgressInterval is the shortest gap between edits of a progress message; 0 means 3 seconds.
	ProgressInterval time.Duration
	// Now is the clock for scheduling; nil means time.Now.
	Now func() time.Time

	mu    sync.Mutex
	lanes map[string]*lane
}

type lane struct {
	ch    chan item
	mu    sync.Mutex
	depth int
}

type item struct {
	ctx context.Context
	in  platform.Inbound
	out platform.Replier
	// task is set when the item is a scheduled run rather than a user message.
	task *store.Task
}

// Handle marks an accepted message and queues it on that chat.
func (s *Service) Handle(ctx context.Context, in platform.Inbound, out platform.Replier) {
	if in.Platform == "" {
		in.Platform = platform.Telegram
	}
	if _, ok := s.AllowedUsers[in.Platform][in.UserID]; !ok {
		s.send(ctx, out, in.ChatID, "没有权限。")
		return
	}
	if !in.TextMessage {
		s.send(ctx, out, in.ChatID, "目前只处理文字。")
		return
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return
	}
	in.Text = text
	lane := s.lane(in.Platform, in.ChatID)
	ahead, ok := lane.reserve(s.limit())
	if !ok {
		s.send(ctx, out, in.ChatID, "前面的消息还在处理，这条先放不下了。等回复后再发。")
		return
	}
	if _, _, command := parseCommand(in.Text); !command {
		if receipt, ok := out.(platform.Receipt); ok {
			if err := receipt.MarkReceived(ctx, in); err != nil {
				// A missing reaction or typing indicator must not prevent the reply.
				slog.Warn("message receipt failed", "platform", in.Platform)
			}
		}
	}
	if ahead > 0 {
		s.send(ctx, out, in.ChatID, fmt.Sprintf("已排队，前面还有 %d 条。", ahead))
	}
	lane.enqueue(item{ctx: ctx, in: in, out: out})
}

func (s *Service) limit() int {
	if s.QueueLimit < 1 {
		return 8
	}
	return s.QueueLimit
}

func (s *Service) lane(platformName, chatID string) *lane {
	key := platformName + "\x00" + chatID
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lanes == nil {
		s.lanes = map[string]*lane{}
	}
	ln, ok := s.lanes[key]
	if ok {
		return ln
	}
	ln = &lane{ch: make(chan item, s.limit())}
	s.lanes[key] = ln
	go s.work(ln)
	return ln
}

func (ln *lane) reserve(limit int) (ahead int, ok bool) {
	ln.mu.Lock()
	defer ln.mu.Unlock()
	if ln.depth >= limit {
		return 0, false
	}
	ahead = ln.depth
	ln.depth++
	return ahead, true
}

func (ln *lane) enqueue(it item) {
	ln.ch <- it
}

func (ln *lane) done() {
	ln.mu.Lock()
	ln.depth--
	ln.mu.Unlock()
}

func (s *Service) work(ln *lane) {
	for it := range ln.ch {
		s.dispatch(it)
		ln.done()
	}
}

func (s *Service) dispatch(it item) {
	ctx := it.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if it.task != nil {
		s.send(ctx, it.out, it.in.ChatID, s.runTask(ctx, *it.task))
		return
	}
	view := s.startProgress(ctx, it)
	reply, err := s.answer(ctx, it.in, view.onTool(ctx))
	if err != nil {
		slog.Error("chat turn failed", "chat", it.in.ChatID, "err", err)
		reply = grok.UserFacing(err)
	}
	if strings.TrimSpace(reply) == "" {
		reply = "处理失败：没有得到回复。"
	}
	if view != nil {
		// Finish falls back to new messages itself and logs its own failure; do not resend.
		_ = view.Finish(ctx, reply)
		return
	}
	s.send(ctx, it.out, it.in.ChatID, reply)
}

func (s *Service) answer(ctx context.Context, in platform.Inbound, onTool func(grok.ToolUse)) (string, error) {
	if cmd, arg, ok := parseCommand(in.Text); ok {
		return s.command(ctx, in, cmd, arg)
	}
	return s.chat(ctx, in, onTool)
}

func (s *Service) command(ctx context.Context, in platform.Inbound, cmd, arg string) (string, error) {
	switch cmd {
	case "/start", "/help":
		return helpText(s.Version), nil
	case "/model":
		return s.modelCommand(ctx, in, arg)
	case "/usage":
		allowance, err := s.Engine.Allowance(ctx)
		if err != nil {
			return "", err
		}
		text := grok.FormatAllowance(allowance)
		if strings.TrimSpace(text) == "" {
			return "暂时查不到剩余额度。", nil
		}
		return text, nil
	case "/dir":
		return s.dirCommand(ctx, in, arg)
	case "/task":
		return s.taskCommand(ctx, in, arg)
	case "/reset":
		if err := s.Store.ClearSession(ctx, in.Platform, in.ChatID); err != nil {
			return "", err
		}
		return "已清空这个聊天的上下文。", nil
	default:
		return "没有这个命令。\n\n" + helpText(s.Version), nil
	}
}

func (s *Service) modelCommand(ctx context.Context, in platform.Inbound, arg string) (string, error) {
	list, err := s.Engine.Models(ctx)
	if err != nil {
		return "", err
	}
	chat, err := s.Store.Chat(ctx, in.Platform, in.ChatID)
	if err != nil {
		return "", err
	}
	if arg == "" {
		return formatModels(chat.Model, list), nil
	}
	if !grok.ValidModelName(arg) || !containsModel(list, arg) {
		return "没有这个模型。用 /model 查看可用模型。", nil
	}
	if err := s.Store.SetModel(ctx, in.Platform, in.ChatID, arg); err != nil {
		return "", err
	}
	return "已切换到 " + arg + "。下一条消息开始使用。", nil
}

// chat applies write grants from the message, runs the turn, and adds the
// confirmations before the reply and the /reset reminder after it.
func (s *Service) chat(ctx context.Context, in platform.Inbound, onTool func(grok.ToolUse)) (string, error) {
	notes := s.applyGrants(ctx, in.Text)
	reply, err := s.chatTurn(ctx, in, onTool)
	if err != nil {
		return "", err
	}
	if len(notes) > 0 {
		reply = strings.Join(notes, "\n") + "\n\n" + reply
	}
	if mentionsPath(in.Text) && s.needsReset(ctx, in) {
		reply += "\n\n" + resetReminder
	}
	return reply, nil
}

// needsReset reports whether file tools are available but this chat's session predates them.
func (s *Service) needsReset(ctx context.Context, in platform.Inbound) bool {
	if ready, _, _ := s.fileAccess(ctx); !ready {
		return false
	}
	chat, err := s.Store.Chat(ctx, in.Platform, in.ChatID)
	return err == nil && chat.SessionID != "" && chat.Sandbox != sandbox.ProfileName
}

func (s *Service) chatTurn(ctx context.Context, in platform.Inbound, onTool func(grok.ToolUse)) (string, error) {
	chat, err := s.Store.Chat(ctx, in.Platform, in.ChatID)
	if err != nil {
		return "", err
	}
	base := grok.ReplyOptions{Model: chat.Model, Prompt: in.Text, WebSearch: s.WebSearch, OnTool: onTool}
	if chat.SessionID == "" {
		return s.startSession(ctx, in, base)
	}
	opts := base
	opts.SessionID = chat.SessionID
	// A resumed session keeps the sandbox it was created with; grok refuses a different one.
	opts.Sandbox = chat.Sandbox
	if chat.Sandbox == sandbox.ProfileName {
		if ready, rw, _ := s.fileAccess(ctx); ready {
			opts.FileTools = true
			opts.WriteDirs = rw
		}
	}
	result, err := s.Engine.Reply(ctx, opts)
	if err != nil && sessionMissing(err) {
		text, err := s.startSession(ctx, in, base)
		if err != nil {
			return "", err
		}
		return "之前的上下文已经失效，这是新的一段对话。\n\n" + text, nil
	}
	if err != nil {
		return "", err
	}
	if err := s.Store.SetSession(ctx, in.Platform, in.ChatID, result.SessionID); err != nil {
		return "", err
	}
	return result.Text, nil
}

// startSession runs the turn in a new Grok session and records the sandbox it was created with.
func (s *Service) startSession(ctx context.Context, in platform.Inbound, opts grok.ReplyOptions) (string, error) {
	sessionID, err := newUUID()
	if err != nil {
		return "", err
	}
	opts.SessionID = sessionID
	opts.NewSession = true
	profile := s.newSessionOptions(ctx, &opts)
	result, err := s.Engine.Reply(ctx, opts)
	if err != nil {
		return "", err
	}
	if err := s.Store.StartSession(ctx, in.Platform, in.ChatID, result.SessionID, profile); err != nil {
		return "", err
	}
	return result.Text, nil
}

func (s *Service) send(ctx context.Context, out platform.Replier, chatID, text string) {
	if err := out.Send(ctx, chatID, text); err != nil {
		slog.Error("send failed", "chat", chatID, "err", err)
	}
}

func parseCommand(text string) (cmd, arg string, ok bool) {
	if !strings.HasPrefix(text, "/") {
		return "", "", false
	}
	fields := strings.SplitN(text, " ", 2)
	name := fields[0]
	if i := strings.IndexByte(name, '@'); i >= 0 {
		name = name[:i]
	}
	name = strings.ToLower(name)
	if name == "/" {
		return "", "", false
	}
	if len(fields) == 2 {
		arg = strings.TrimSpace(fields[1])
	}
	return name, arg, true
}

func helpText(version string) string {
	text := `直接发文字，我会接着这个聊天的上下文回复。

/model
    查看可用模型和当前模型
/model 模型名
    切换这个聊天之后使用的模型
/usage
    查看订阅剩余额度
/dir
    查看或管理 Grok 可以读写的目录
/task 一句话
    创建定时任务，例如 /task 每天九点总结科技新闻
/task list
    查看定时任务
/task del 编号
    删除定时任务
/reset
    清空这个聊天的上下文
/help
    显示这条说明`
	if version != "" && version != "dev" {
		text += "\n\n版本：" + version
	}
	return text
}

func formatModels(current string, list grok.ModelList) string {
	var b strings.Builder
	fmt.Fprintf(&b, "当前模型：%s\n\n可用模型：\n", current)
	shown := map[string]struct{}{}
	for _, name := range list.Names {
		shown[name] = struct{}{}
		if name == current {
			fmt.Fprintf(&b, "- %s（当前）\n", name)
			continue
		}
		fmt.Fprintf(&b, "- %s\n", name)
	}
	if _, ok := shown[current]; !ok && current != "" {
		fmt.Fprintf(&b, "- %s（当前，不在本次列表里）\n", current)
	}
	return strings.TrimSpace(b.String())
}

func containsModel(list grok.ModelList, name string) bool {
	for _, item := range list.Names {
		if item == name {
			return true
		}
	}
	return false
}

func sessionMissing(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "session") && (strings.Contains(msg, "not found") ||
		strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "no such") ||
		strings.Contains(msg, "couldn't start") ||
		strings.Contains(msg, "could not"))
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
