package core

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/platform"
	"github.com/rshun/grok-bot/internal/store"
)

const ackText = "收到，正在处理。"

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
}

// Handle acknowledges a message and queues it on that chat.
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
	ack := ackText
	if ahead > 0 {
		ack = fmt.Sprintf("%s前面还有 %d 条。", ackText, ahead)
	}
	// Acknowledge before the worker starts so the receipt is always first.
	s.send(ctx, out, in.ChatID, ack)
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
	reply, err := s.answer(ctx, it.in)
	if err != nil {
		slog.Error("chat turn failed", "chat", it.in.ChatID, "err", err)
		reply = grok.UserFacing(err)
	}
	if strings.TrimSpace(reply) == "" {
		reply = "处理失败：没有得到回复。"
	}
	s.send(ctx, it.out, it.in.ChatID, reply)
}

func (s *Service) answer(ctx context.Context, in platform.Inbound) (string, error) {
	if cmd, arg, ok := parseCommand(in.Text); ok {
		return s.command(ctx, in, cmd, arg)
	}
	return s.chat(ctx, in)
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

func (s *Service) chat(ctx context.Context, in platform.Inbound) (string, error) {
	chat, err := s.Store.Chat(ctx, in.Platform, in.ChatID)
	if err != nil {
		return "", err
	}
	newSession := chat.SessionID == ""
	sessionID := chat.SessionID
	if newSession {
		sessionID, err = newUUID()
		if err != nil {
			return "", err
		}
	}
	result, err := s.Engine.Reply(ctx, grok.ReplyOptions{
		SessionID:  sessionID,
		NewSession: newSession,
		Model:      chat.Model,
		Prompt:     in.Text,
	})
	if err != nil && !newSession && sessionMissing(err) {
		sessionID, idErr := newUUID()
		if idErr != nil {
			return "", err
		}
		result, err = s.Engine.Reply(ctx, grok.ReplyOptions{
			SessionID:  sessionID,
			NewSession: true,
			Model:      chat.Model,
			Prompt:     in.Text,
		})
		if err != nil {
			return "", err
		}
		if saveErr := s.Store.SetSession(ctx, in.Platform, in.ChatID, result.SessionID); saveErr != nil {
			return "", saveErr
		}
		return "之前的上下文已经失效，这是新的一段对话。\n\n" + result.Text, nil
	}
	if err != nil {
		return "", err
	}
	if err := s.Store.SetSession(ctx, in.Platform, in.ChatID, result.SessionID); err != nil {
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
