package core

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rshun/grok-bot/internal/actions"
	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/platform"
	"github.com/rshun/grok-bot/internal/sandbox"
	"github.com/rshun/grok-bot/internal/schedule"
	"github.com/rshun/grok-bot/internal/store"
)

// emptyReplyText is shown when a turn finishes without any answer text.
const emptyReplyText = "处理失败：没有得到回复。"

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
	// AllowShell is the default shell execution setting when not overridden via /shell.
	AllowShell bool
	// UploadDir is where user-uploaded document attachments are saved for Grok to inspect.
	UploadDir string
	// Sandbox limits file access for new sessions; nil turns file tools off.
	Sandbox *sandbox.Manager
	// Actions is the local MCP server for per-turn chat actions; nil means no MCP channel.
	Actions *actions.Server
	// Senders post scheduled task results to a chat, keyed by platform name.
	Senders         map[string]platform.Replier
	Location        *time.Location
	TaskLimit       int
	TaskMinInterval time.Duration
	// Crontab syncs scheduled tasks into the OS user crontab when non-nil.
	Crontab *schedule.Crontab
	// ShowProgress controls whether an editable "处理中…" status message is sent before the answer.
	ShowProgress bool
	// ProgressInterval is the shortest gap between edits of a progress message; 0 means 3 seconds.
	ProgressInterval time.Duration
	// ImageLoadTimeout bounds downloading the pictures of one message; 0 means 3 minutes.
	ImageLoadTimeout time.Duration
	// Now is the clock for scheduling; nil means time.Now.
	Now func() time.Time

	mu    sync.Mutex
	lanes map[string]*lane
}

type lane struct {
	ch        chan item
	mu        sync.Mutex
	depth     int
	active    context.CancelFunc
	runSeq    uint64
	stopEpoch uint64
}

type item struct {
	ctx     context.Context
	baseCtx context.Context
	in      platform.Inbound
	out     platform.Replier
	epoch   uint64
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
		s.send(ctx, out, in.ChatID, unsupportedText)
		return
	}
	text := strings.TrimSpace(in.Text)
	if text == "" && len(in.Images) == 0 && len(in.Files) == 0 {
		return
	}
	in.Text = text
	limitImages(&in)
	limitFiles(&in)
	// Nothing is downloaded yet: the declared size is enough to refuse an oversize attachment.
	if oversize(in.Images) {
		s.send(ctx, out, in.ChatID, imageTooLargeText)
		return
	}
	if oversizeFiles(in.Files) {
		s.send(ctx, out, in.ChatID, fileTooLargeText)
		return
	}
	lane := s.lane(in.Platform, in.ChatID)
	if isStopInbound(in, lane.busy()) {
		running, queued := lane.stop()
		s.send(ctx, out, in.ChatID, stopReplyText(running, queued))
		return
	}
	ahead, epoch, ok := lane.reserve(s.limit())
	if !ok {
		s.send(ctx, out, in.ChatID, "前面的消息还在处理，这条先放不下了。等回复后再发。")
		return
	}
	if _, _, command := parseInboundCommand(in); !command {
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
	lane.enqueue(item{ctx: ctx, in: in, out: out, epoch: epoch})
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

func (ln *lane) reserve(limit int) (ahead int, epoch uint64, ok bool) {
	ln.mu.Lock()
	defer ln.mu.Unlock()
	if ln.depth >= limit {
		return 0, 0, false
	}
	ahead = ln.depth
	ln.depth++
	return ahead, ln.stopEpoch, true
}

func (ln *lane) busy() bool {
	ln.mu.Lock()
	defer ln.mu.Unlock()
	return ln.depth > 0
}

func (ln *lane) stop() (stoppedRunning bool, droppedQueued int) {
	ln.mu.Lock()
	defer ln.mu.Unlock()
	hadWork := ln.depth > 0
	ln.stopEpoch++
	for {
		select {
		case <-ln.ch:
			ln.depth--
			droppedQueued++
		default:
			goto drained
		}
	}
drained:
	if ln.active != nil {
		ln.runSeq++
		ln.active()
		ln.active = nil
		stoppedRunning = true
	} else if hadWork && ln.depth > 0 {
		// An item was popped from ln.ch and will be dropped immediately via stopEpoch.
		stoppedRunning = true
	}
	return stoppedRunning, droppedQueued
}

func (ln *lane) enqueue(it item) {
	ln.ch <- it
}

func (s *Service) work(ln *lane) {
	for it := range ln.ch {
		baseCtx := it.ctx
		if baseCtx == nil {
			baseCtx = context.Background()
		}
		runCtx, cancel := context.WithCancel(baseCtx)
		ln.mu.Lock()
		if it.epoch < ln.stopEpoch {
			ln.depth--
			ln.mu.Unlock()
			cancel()
			continue
		}
		ln.runSeq++
		seq := ln.runSeq
		ln.active = cancel
		ln.mu.Unlock()

		it.baseCtx = baseCtx
		it.ctx = runCtx
		s.dispatch(it)

		ln.mu.Lock()
		if ln.runSeq == seq {
			ln.active = nil
		}
		ln.depth--
		ln.mu.Unlock()
		cancel()
	}
}

func (s *Service) dispatch(it item) {
	ctx := it.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	baseCtx := it.baseCtx
	stoppedByUser := func() bool {
		return it.baseCtx != nil && ctx.Err() != nil && baseCtx.Err() == nil
	}
	if baseCtx == nil {
		baseCtx = ctx
	}
	if it.task != nil {
		text, files := s.runTask(ctx, it.in, it.out, *it.task)
		if stoppedByUser() {
			return
		}
		s.send(baseCtx, it.out, it.in.ChatID, text)
		// A scheduled run has no message to quote.
		s.flushFiles(baseCtx, it.out, it.in.ChatID, "", files)
		return
	}
	view := s.startProgress(ctx, it)
	reply, quiet, files, err := s.answer(ctx, it.in, it.out, view.onTool(ctx))
	if stoppedByUser() {
		if view != nil {
			_ = view.Delete(baseCtx)
		}
		return
	}
	if err != nil {
		slog.Error("chat turn failed", "chat", it.in.ChatID, "err", err)
		reply, quiet, files = grok.UserFacing(err), false, nil
	}
	// The answer is shown first; the files follow it, each quoting the user's message.
	defer func() { s.flushFiles(baseCtx, it.out, it.in.ChatID, it.in.MessageID, files) }()
	if strings.TrimSpace(reply) == "" {
		if quiet {
			// The turn only reacted: the reaction is the answer, so the progress message goes away.
			if view != nil {
				_ = view.Delete(baseCtx)
			}
			return
		}
		reply = emptyReplyText
	}
	if view != nil {
		// Finish falls back to new messages itself and logs its own failure; do not resend.
		_ = view.Finish(baseCtx, reply)
		return
	}
	s.send(baseCtx, it.out, it.in.ChatID, reply)
}

// answer returns the text to show and the files to send after it. quiet means the turn was
// answered by a reaction alone, so an empty text is not a failure.
func (s *Service) answer(ctx context.Context, in platform.Inbound, out platform.Replier, onTool func(grok.ToolUse)) (text string, quiet bool, files []actions.File, err error) {
	if cmd, arg, ok := parseInboundCommand(in); ok {
		text, files, err = s.command(ctx, in, out, cmd, arg, onTool)
		return text, false, files, err
	}
	uploaded, fileFailure := s.saveUploads(ctx, in)
	if fileFailure != "" {
		return fileFailure, false, nil, nil
	}
	var memNote string
	if len(in.Images) == 0 && len(uploaded) == 0 {
		reply, followup, note, handled, err := s.tryNaturalMemory(ctx, in)
		if handled || err != nil {
			return reply, false, nil, err
		}
		if followup != "" {
			in.Text = followup
			memNote = note
		}
	} else if content, rest, ok := parseExplicitMemoryAdd(in.Text); ok {
		r, err := s.memoryAdd(ctx, in, content)
		if err != nil {
			return "", false, nil, err
		}
		memNote = firstNoteLine(r)
		in.Text = rest
	}
	if len(in.Images) == 0 {
		taskIn := in
		if len(uploaded) > 0 {
			taskIn.Text = formatUploadedFilesPrompt(in.Text, uploaded, false)
		}
		if reply, taskFiles, handled, err := s.tryNaturalTask(ctx, taskIn, out, onTool); handled || err != nil {
			if memNote != "" && reply != "" {
				reply = memNote + "\n\n" + reply
			}
			return reply, false, taskFiles, err
		}
	}
	images, failure := s.loadImages(ctx, in)
	if failure != "" {
		return failure, false, nil, nil
	}
	return s.chat(ctx, in, out, images, uploaded, memNote, onTool)
}

func (s *Service) command(ctx context.Context, in platform.Inbound, out platform.Replier, cmd, arg string, onTool func(grok.ToolUse)) (string, []actions.File, error) {
	switch cmd {
	case "/start", "/help":
		return helpText(s.Version), nil, nil
	case "/status":
		text, err := s.statusCommand(ctx, in)
		return text, nil, err
	case "/stop", "/cancel":
		return "当前没有正在执行的任务。", nil, nil
	case "/new":
		text, err := s.newTopicCommand(ctx, in, arg)
		return text, nil, err
	case "/sessions", "/session":
		text, err := s.sessionsCommand(ctx, in, arg)
		return text, nil, err
	case "/mcp":
		text, err := s.mcpCommand(ctx, arg)
		return text, nil, err
	case "/model":
		text, err := s.modelCommand(ctx, in, arg)
		return text, nil, err
	case "/usage":
		allowance, err := s.Engine.Allowance(ctx)
		if err != nil {
			return "", nil, err
		}
		text := grok.FormatAllowance(allowance)
		if strings.TrimSpace(text) == "" {
			return "暂时查不到剩余额度。", nil, nil
		}
		return text, nil, nil
	case "/dir":
		text, err := s.dirCommand(ctx, in, arg)
		return text, nil, err
	case "/shell":
		text, err := s.shellCommand(ctx, arg)
		return text, nil, err
	case "/memory":
		text, err := s.memoryCommand(ctx, in, arg)
		return text, nil, err
	case "/task":
		return s.taskCommand(ctx, in, out, arg, onTool)
	case "/reset":
		if err := s.Store.ClearSession(ctx, in.Platform, in.ChatID); err != nil {
			return "", nil, err
		}
		if memories := s.chatMemories(ctx, in.Platform, in.ChatID); len(memories) > 0 {
			return fmt.Sprintf("已清空这个聊天的上下文（%d 条长期记忆继续保留）。", len(memories)), nil, nil
		}
		return "已清空这个聊天的上下文。", nil, nil
	default:
		return "没有这个命令。\n\n" + helpText(s.Version), nil, nil
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
// quiet is true when Grok reacted and had nothing to add: reply then holds only the
// confirmations (a grant must always be confirmed), which may be empty.
func (s *Service) chat(ctx context.Context, in platform.Inbound, out platform.Replier, images []grok.Image, uploaded []uploadedFile, memNote string, onTool func(grok.ToolUse)) (reply string, quiet bool, files []actions.File, err error) {
	notes := s.applyGrants(ctx, in.Text)
	if memNote != "" {
		notes = append([]string{memNote}, notes...)
	}
	if in.Skipped {
		notes = append([]string{skippedNote}, notes...)
	}
	res, err := s.chatTurn(ctx, in, out, images, uploaded, onTool)
	if err != nil {
		return "", false, nil, err
	}
	// The single place that decides whether the turn produced anything to show. It runs
	// before notes and the reminder are added around the reply, so they cannot hide an empty answer.
	text := strings.TrimSpace(res.text)
	empty := text == "" || text == noReply
	if empty && res.reacted {
		if res.rebuilt {
			notes = append(notes, rebuiltNotice)
		}
		if mentionsPath(in.Text) && s.needsReset(ctx, in) {
			notes = append(notes, resetReminder)
		}
		return strings.Join(notes, "\n"), true, res.files, nil
	}
	reply = res.text
	if empty {
		reply = emptyReplyText
	}
	if res.rebuilt {
		reply = rebuiltNotice + "\n\n" + reply
	}
	if len(notes) > 0 {
		reply = strings.Join(notes, "\n") + "\n\n" + reply
	}
	if mentionsPath(in.Text) && s.needsReset(ctx, in) {
		reply += "\n\n" + resetReminder
	}
	return reply, false, res.files, nil
}

// needsReset is kept as false now that existing sessions also receive file tools directly.
func (s *Service) needsReset(ctx context.Context, in platform.Inbound) bool {
	return false
}

// rebuiltNotice opens a reply that was answered in a new session because the old one was gone.
const rebuiltNotice = "之前的上下文已经失效，这是新的一段对话。"

// chatResult is the outcome of one chat turn.
type chatResult struct {
	text    string
	reacted bool           // Grok put a reaction on the user's message
	files   []actions.File // files Grok asked to send, in call order
	rebuilt bool           // the answer came from a new session after the old one went missing
}

func (s *Service) chatTurn(ctx context.Context, in platform.Inbound, out platform.Replier, images []grok.Image, uploaded []uploadedFile, onTool func(grok.ToolUse)) (chatResult, error) {
	chat, err := s.Store.Chat(ctx, in.Platform, in.ChatID)
	if err != nil {
		return chatResult{}, err
	}
	prompt := in.Text
	if len(images) > 0 {
		prompt = imagePrompt(prompt, len(images))
	}
	if len(uploaded) > 0 {
		prompt = formatUploadedFilesPrompt(prompt, uploaded, true)
	}
	prompt = s.withChatTasksContext(ctx, in, prompt)
	prompt = withReplyQuote(prompt, in.ReplyToText)

	// The images and long-term memories stay in base so new, resumed, and rebuilt sessions all receive them.
	base := grok.ReplyOptions{
		Model:     chat.Model,
		Prompt:    prompt,
		WebSearch: s.WebSearch,
		OnTool:    onTool,
		Images:    images,
		Memories:  s.chatMemories(ctx, in.Platform, in.ChatID),
	}
	if chat.SessionID == "" {
		return s.startSession(ctx, in, out, base)
	}
	opts := base
	opts.SessionID = chat.SessionID
	opts.ShellTools = s.shellEnabled(ctx)
	// A resumed session keeps the sandbox it was created with; when bubblewrap is missing on Linux
	// grok refuses any custom sandbox profile, so run unsandboxed instead.
	opts.Sandbox = chat.Sandbox
	if ready, profile, rw, _ := s.fileAccess(ctx); ready {
		if profile == "" {
			opts.Sandbox = ""
		}
		opts.FileTools = true
		opts.WriteDirs = rw
	}
	res, err := s.reply(ctx, in, out, opts)
	if err != nil && opts.Sandbox != "" && sandboxUnavailable(err) {
		opts.Sandbox = ""
		res, err = s.reply(ctx, in, out, opts)
	}
	if err != nil && sessionMissing(err) {
		res, err := s.startSession(ctx, in, out, base)
		if err != nil {
			return chatResult{}, err
		}
		res.rebuilt = true
		return res, nil
	}
	if err != nil {
		if grok.IsMaxTurns(err) {
			_ = s.Store.SetSession(ctx, in.Platform, in.ChatID, res.sessionID)
			s.recordSessionTurn(ctx, in, res.sessionID, chat.Model, opts.Sandbox)
			res.text = grok.UserFacing(err)
			return res.chatResult, nil
		}
		return chatResult{}, err
	}
	if err := s.Store.SetSession(ctx, in.Platform, in.ChatID, res.sessionID); err != nil {
		return chatResult{}, err
	}
	s.recordSessionTurn(ctx, in, res.sessionID, chat.Model, opts.Sandbox)
	return res.chatResult, nil
}

// startSession runs the turn in a new Grok session and records the sandbox it was created with.
func (s *Service) startSession(ctx context.Context, in platform.Inbound, out platform.Replier, opts grok.ReplyOptions) (chatResult, error) {
	sessionID, err := newUUID()
	if err != nil {
		return chatResult{}, err
	}
	opts.SessionID = sessionID
	opts.NewSession = true
	profile := s.newSessionOptions(ctx, &opts)
	res, err := s.reply(ctx, in, out, opts)
	if err != nil && opts.Sandbox != "" && sandboxUnavailable(err) {
		opts.Sandbox = ""
		profile = ""
		res, err = s.reply(ctx, in, out, opts)
	}
	if err != nil {
		if grok.IsMaxTurns(err) {
			_ = s.Store.StartSession(ctx, in.Platform, in.ChatID, res.sessionID, profile)
			s.recordSessionTurn(ctx, in, res.sessionID, opts.Model, profile)
			res.text = grok.UserFacing(err)
			return res.chatResult, nil
		}
		return chatResult{}, err
	}
	if err := s.Store.StartSession(ctx, in.Platform, in.ChatID, res.sessionID, profile); err != nil {
		return chatResult{}, err
	}
	s.recordSessionTurn(ctx, in, res.sessionID, opts.Model, profile)
	return res.chatResult, nil
}

// replyResult is a chat result together with the session grok answered in.
type replyResult struct {
	chatResult
	sessionID string
}

// reply runs one grok call inside its own turn. The turn opens just before the call, once opts
// says which tools this session gets, and its token ends when the call returns. The reaction
// and queued files are those of this call alone.
func (s *Service) reply(ctx context.Context, in platform.Inbound, out platform.Replier, opts grok.ReplyOptions) (replyResult, error) {
	turn, env, end := s.beginTurn(in, out, opts.FileTools || opts.ShellTools)
	defer end()
	result, err := s.Engine.Reply(ctx, withTurn(opts, turn, env))
	sessionID := result.SessionID
	if sessionID == "" {
		sessionID = opts.SessionID
	}
	res := replyResult{chatResult: chatResult{text: result.Text}, sessionID: sessionID}
	if turn != nil {
		res.reacted = turn.Reacted()
		res.files = turn.Files()
	}
	if err != nil {
		return res, err
	}
	return res, nil
}

func (s *Service) send(ctx context.Context, out platform.Replier, chatID, text string) {
	if err := out.Send(ctx, chatID, text); err != nil {
		slog.Error("send failed", "chat", chatID, "err", safeErr(err))
	}
}

// safeErr renders an error for logs without any request URL. Platform HTTP clients return
// *url.Error whose text embeds the request URL, and a Telegram URL contains the bot token.
func safeErr(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Op + ": " + urlErr.Err.Error()
	}
	return err.Error()
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

func isStopInbound(in platform.Inbound, busy bool) bool {
	if len(in.Images) > 0 || len(in.Files) > 0 {
		return false
	}
	if cmd, arg, ok := parseCommand(in.Text); ok && arg == "" && (cmd == "/stop" || cmd == "/cancel") {
		return true
	}
	if !busy {
		return false
	}
	trimmed := strings.TrimRight(strings.TrimSpace(in.Text), "。！!？?")
	switch trimmed {
	case "停止", "停一下", "停止当前任务", "取消当前任务", "停止执行", "终止任务":
		return true
	}
	return false
}

func stopReplyText(running bool, queued int) string {
	switch {
	case running && queued > 0:
		return fmt.Sprintf("已停止当前任务，并取消了 %d 条排队中的消息。", queued)
	case running:
		return "已停止当前任务。"
	case queued > 0:
		return fmt.Sprintf("已取消 %d 条排队中的消息。", queued)
	default:
		return "当前没有正在执行的任务。"
	}
}

func helpText(version string) string {
	text := `直接发文字、图片或文件，我会接着这个聊天的上下文回复；引用任意消息回复时会自动带上被引用内容。

/status
    查看运行状态、权限开关与 MCP 列表概览
/stop
    立即中断当前正在执行的对话或任务
/new [备注]
    归档当前会话并开启新话题（长期记忆继续保留）
/sessions
    查看最近保存的会话列表（/sessions 编号 切回旧话题）
/mcp
    查看、诊断或增删改 MCP 服务（/mcp doctor、/mcp add、/mcp del）
/model [模型名]
    查看或切换这个聊天使用的模型
/usage
    查看订阅剩余额度
/dir
    查看或管理 Grok 可以读写的目录
/shell
    查看或开关服务器命令执行权限（/shell on、/shell off）
/memory
    查看或管理长期记忆（也可直接说「长期记忆：内容」，执行 /reset 后仍会保留）
/task 一句话
    创建或管理定时任务（支持每天/每周/每月/最后一天/工作日/5段Cron表达式）
/task list
    查看定时任务
/task run 编号
    立即试跑一次定时任务（不影响正常排期）
/task del 编号
    删除定时任务
/reset
    清空这个聊天的上下文（长期记忆会保留）
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

func sandboxUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "could not enforce its deny list") || strings.Contains(msg, "bwrap exec failed")
}

func (s *Service) withChatTasksContext(ctx context.Context, in platform.Inbound, prompt string) string {
	if !strings.Contains(in.Text, "任务") {
		return prompt
	}
	tasks, err := s.Store.Tasks(ctx, in.Platform, in.ChatID)
	if err != nil || len(tasks) == 0 {
		return prompt
	}
	var b strings.Builder
	b.WriteString("[当前聊天的定时任务列表]\n")
	for _, t := range tasks {
		status := fmt.Sprintf("下次 %s", t.NextRun.In(s.location()).Format("01-02 15:04"))
		if t.Paused {
			status = "已暂停"
		}
		fmt.Fprintf(&b, "#%d %s（指令：%s；状态：%s）\n", t.ID, t.Summary, t.Prompt, status)
	}
	b.WriteString("[列表结束]\n\n")
	b.WriteString(prompt)
	return b.String()
}

func withReplyQuote(prompt, quote string) string {
	quote = strings.TrimSpace(quote)
	if quote == "" {
		return prompt
	}
	return fmt.Sprintf("[用户正在引用以下消息进行回复]\n%s\n[引用结束]\n\n%s", quote, prompt)
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
