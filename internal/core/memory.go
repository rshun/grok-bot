package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/platform"
	"github.com/rshun/grok-bot/internal/store"
)

const (
	maxMemoriesPerChat = 100
	maxMemoryRunes     = 1000
)

const memoryUsage = `用法：
/memory
    查看当前聊天的长期记忆（/reset 后仍保留）
/memory add 内容
    添加一条长期记忆（也可直接发「长期记忆：内容」）
/memory del 编号
    删除指定编号的长期记忆
/memory clear
    清空当前聊天的全部长期记忆`

const memorySchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["action", "content", "memory_id", "followup"],
  "properties": {
    "action": {
      "type": "string",
      "enum": ["add", "delete", "clear", "list", "none"]
    },
    "content": { "type": "string" },
    "memory_id": { "type": "integer" },
    "followup": { "type": "string" }
  }
}`

type parsedMemoryOp struct {
	Action   string `json:"action"`
	Content  string `json:"content"`
	MemoryID int64  `json:"memory_id"`
	Followup string `json:"followup"`
}

var (
	reMemoryDelByID = regexp.MustCompile(`^(?:请|帮我|麻烦)?(?:删除|删掉|移除|取消|忘掉|清除)(?:第\s*)?#?(\d+)(?:\s*条)?长期记忆[。！!？?\s]*$`)
	reMemoryDelTail = regexp.MustCompile(`^(?:请|帮我|麻烦)?(?:删除|删掉|移除|取消|忘掉|清除)长期记忆\s*[#：:\s]\s*#?(\d+)[。！!？?\s]*$`)
	reMemoryPutInto = regexp.MustCompile(`^(?:请|帮我|麻烦)?把[「『"“]?(.+?)[」』"”]?(?:记入|加入|写入|存入|保存到|放进|放到|设为)(?:长期记忆|永久记忆)(?:里|中)?[。！!\s]*$`)
)

func (s *Service) chatMemories(ctx context.Context, platformName, chatID string) []string {
	if s.Store == nil {
		return nil
	}
	list, err := s.Store.Memories(ctx, platformName, chatID)
	if err != nil {
		slog.Error("load memories", "err", err)
		return nil
	}
	if len(list) == 0 {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m.Content)
	}
	return out
}

func (s *Service) memoryCommand(ctx context.Context, in platform.Inbound, arg string) (string, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" || strings.EqualFold(arg, "list") {
		return s.memoryList(ctx, in)
	}
	if strings.EqualFold(arg, "help") {
		return memoryUsage, nil
	}
	if strings.EqualFold(arg, "clear") {
		return s.memoryClear(ctx, in)
	}
	fields := strings.Fields(arg)
	if len(fields) >= 1 && strings.EqualFold(fields[0], "del") {
		if len(fields) != 2 {
			return memoryUsage, nil
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(fields[1], "#"), 10, 64)
		if err != nil || id <= 0 {
			return memoryUsage, nil
		}
		return s.memoryDelete(ctx, in, id)
	}
	content := arg
	if len(fields) >= 2 && strings.EqualFold(fields[0], "add") {
		content = strings.TrimSpace(arg[len(fields[0]):])
	} else if len(fields) == 1 && strings.EqualFold(fields[0], "add") {
		return memoryUsage, nil
	}
	return s.memoryAdd(ctx, in, content)
}

func (s *Service) memoryList(ctx context.Context, in platform.Inbound) (string, error) {
	list, err := s.Store.Memories(ctx, in.Platform, in.ChatID)
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "当前没有保存的长期记忆。\n可直接发送「长期记忆：内容」或 `/memory add 内容` 添加（执行 /reset 后仍会保留）。", nil
	}
	var b strings.Builder
	b.WriteString("长期记忆（执行 /reset 后仍会保留）：\n")
	for _, m := range list {
		fmt.Fprintf(&b, "#%d %s\n", m.ID, m.Content)
	}
	return strings.TrimSpace(b.String()), nil
}

func (s *Service) memoryAdd(ctx context.Context, in platform.Inbound, content string) (string, error) {
	content = cleanMemoryContent(content)
	if content == "" {
		return memoryUsage, nil
	}
	if utf8.RuneCountInString(content) > maxMemoryRunes {
		return fmt.Sprintf("单条长期记忆最多 %d 个字符，请精简后再试。", maxMemoryRunes), nil
	}
	existing, err := s.Store.Memories(ctx, in.Platform, in.ChatID)
	if err != nil {
		return "", err
	}
	for _, m := range existing {
		if m.Content == content {
			return fmt.Sprintf("长期记忆 #%d 已存在：%s（执行 /reset 后仍会保留）。", m.ID, content), nil
		}
	}
	if len(existing) >= maxMemoriesPerChat {
		return fmt.Sprintf("每个聊天最多保存 %d 条长期记忆，请先用 `/memory del 编号` 删除一些。", maxMemoriesPerChat), nil
	}
	id, _, err := s.Store.AddMemory(ctx, in.Platform, in.ChatID, content)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("已永久保存长期记忆 #%d：%s\n（执行 /reset 清空上下文后仍会保留，可用 /memory 查看或删除。）", id, content), nil
}

func (s *Service) memoryDelete(ctx context.Context, in platform.Inbound, id int64) (string, error) {
	content, ok, err := s.Store.DeleteMemory(ctx, in.Platform, in.ChatID, id)
	if err != nil {
		return "", err
	}
	if !ok {
		return fmt.Sprintf("没有 #%d 这条长期记忆，可发 /memory 查看。", id), nil
	}
	return fmt.Sprintf("已删除长期记忆 #%d：%s。", id, content), nil
}

func (s *Service) memoryClear(ctx context.Context, in platform.Inbound) (string, error) {
	n, err := s.Store.ClearMemories(ctx, in.Platform, in.ChatID)
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "当前没有保存的长期记忆。", nil
	}
	return fmt.Sprintf("已清空全部 %d 条长期记忆。", n), nil
}

// tryNaturalMemory handles pure natural-language memory operations (add, list, delete, clear)
// directly without polluting or depending on the ephemeral Grok session. If the user's message
// combines a memory instruction with a follow-up chat question, it returns handled=false after
// leaving inline extraction or structured followup handling to run cleanly.
func (s *Service) tryNaturalMemory(ctx context.Context, in platform.Inbound) (reply string, followup string, note string, handled bool, err error) {
	text := strings.TrimSpace(in.Text)
	if text == "" || !mentionsMemoryConcept(text) {
		return "", "", "", false, nil
	}

	if looksLikeMemoryClear(text) {
		r, err := s.memoryClear(ctx, in)
		return r, "", "", true, err
	}
	if id, ok := parseMemoryDeleteID(text); ok {
		r, err := s.memoryDelete(ctx, in, id)
		return r, "", "", true, err
	}
	if looksLikeMemoryList(text) {
		r, err := s.memoryList(ctx, in)
		return r, "", "", true, err
	}
	if content, rest, ok := parseExplicitMemoryAdd(text); ok {
		r, err := s.memoryAdd(ctx, in, content)
		if err != nil {
			return "", "", "", false, err
		}
		if rest == "" {
			return r, "", "", true, nil
		}
		return "", rest, firstNoteLine(r), false, nil
	}

	// Fallback: when the user explicitly says "长期记忆" or "永久记忆" in free-form prose,
	// ask a throwaway structured-output turn to classify whether they are managing long-term memory.
	if !strings.Contains(text, "长期记忆") && !strings.Contains(text, "永久记忆") {
		return "", "", "", false, nil
	}
	op, ok, err := s.classifyMemoryWithLLM(ctx, in, text)
	if err != nil || !ok || op.Action == "" || op.Action == "none" {
		return "", "", "", false, nil
	}
	switch op.Action {
	case "list":
		r, err := s.memoryList(ctx, in)
		return r, "", "", true, err
	case "clear":
		r, err := s.memoryClear(ctx, in)
		return r, "", "", true, err
	case "delete":
		if op.MemoryID <= 0 {
			return "", "", "", false, nil
		}
		r, err := s.memoryDelete(ctx, in, op.MemoryID)
		return r, "", "", true, err
	case "add":
		content := cleanMemoryContent(op.Content)
		if content == "" {
			return "", "", "", false, nil
		}
		r, err := s.memoryAdd(ctx, in, content)
		if err != nil {
			return "", "", "", false, err
		}
		rest := strings.TrimSpace(op.Followup)
		if rest == "" {
			return r, "", "", true, nil
		}
		return "", rest, firstNoteLine(r), false, nil
	}
	return "", "", "", false, nil
}

func (s *Service) classifyMemoryWithLLM(ctx context.Context, in platform.Inbound, text string) (parsedMemoryOp, bool, error) {
	chat, err := s.Store.Chat(ctx, in.Platform, in.ChatID)
	if err != nil {
		return parsedMemoryOp{}, false, err
	}
	existing, err := s.Store.Memories(ctx, in.Platform, in.ChatID)
	if err != nil {
		return parsedMemoryOp{}, false, err
	}
	sessionID, err := newUUID()
	if err != nil {
		return parsedMemoryOp{}, false, err
	}
	prompt := buildMemoryClassifyPrompt(text, existing)
	res, err := s.Engine.Reply(ctx, grok.ReplyOptions{
		SessionID:  sessionID,
		NewSession: true,
		Model:      chat.Model,
		Prompt:     prompt,
		JSONSchema: memorySchema,
	})
	if err != nil {
		return parsedMemoryOp{}, false, err
	}
	var raw []byte
	if len(res.Structured) > 0 {
		raw = res.Structured
	} else if strings.TrimSpace(res.Text) != "" {
		raw = []byte(strings.TrimSpace(res.Text))
	}
	if len(raw) == 0 {
		return parsedMemoryOp{}, false, nil
	}
	var op parsedMemoryOp
	if err := json.Unmarshal(raw, &op); err != nil {
		return parsedMemoryOp{}, false, nil
	}
	return op, true, nil
}

func buildMemoryClassifyPrompt(text string, existing []store.Memory) string {
	var b strings.Builder
	b.WriteString("Analyze whether the user is asking the bot to save, list, delete, or clear a persistent bot long-term memory (长期记忆 / 永久记忆).\n")
	b.WriteString("- Use action=\"add\" when the user wants the bot to permanently remember an instruction, preference, persona, or fact across sessions; put the standalone instruction/fact in \"content\" (without leading meta phrases like '长期记忆：') and any unrelated extra question in \"followup\" (empty string if none).\n")
	b.WriteString("- Use action=\"list\" when asking to view saved long-term memories.\n")
	b.WriteString("- Use action=\"delete\" with \"memory_id\" when asking to delete a specific saved memory from the existing list.\n")
	b.WriteString("- Use action=\"clear\" when asking to wipe all saved long-term memories.\n")
	b.WriteString("- Use action=\"none\" when the user is only discussing the general concept of memory (e.g. human neuroscience) rather than managing bot memory.\n\n")
	if len(existing) > 0 {
		b.WriteString("Existing bot memories:\n")
		for _, m := range existing {
			fmt.Fprintf(&b, "- #%d: %s\n", m.ID, m.Content)
		}
		b.WriteByte('\n')
	}
	b.WriteString("User message:\n")
	b.WriteString(text)
	return b.String()
}

func mentionsMemoryConcept(text string) bool {
	for _, kw := range []string{"长期记忆", "永久记忆", "永久记住", "长期记住"} {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

func looksLikeMemoryClear(text string) bool {
	trimmed := strings.TrimRight(strings.TrimSpace(text), "。！!？?")
	for _, exact := range []string{
		"清空长期记忆", "清除长期记忆", "清空所有长期记忆", "清除所有长期记忆",
		"删除所有长期记忆", "删掉所有长期记忆", "删除全部长期记忆", "删掉全部长期记忆",
		"清空全部长期记忆", "清除全部长期记忆", "清空永久记忆", "清除永久记忆",
	} {
		if trimmed == exact || trimmed == "请"+exact || trimmed == "帮我"+exact {
			return true
		}
	}
	return false
}

func parseMemoryDeleteID(text string) (int64, bool) {
	trimmed := strings.TrimSpace(text)
	for _, re := range []*regexp.Regexp{reMemoryDelByID, reMemoryDelTail} {
		if m := re.FindStringSubmatch(trimmed); len(m) == 2 {
			if id, err := strconv.ParseInt(m[1], 10, 64); err == nil && id > 0 {
				return id, true
			}
		}
	}
	return 0, false
}

func looksLikeMemoryList(text string) bool {
	trimmed := strings.TrimRight(strings.TrimSpace(text), "。！!？?")
	for _, exact := range []string{
		"查看长期记忆", "看看长期记忆", "看下长期记忆", "看一下长期记忆",
		"显示长期记忆", "列出长期记忆", "有哪些长期记忆", "有什么长期记忆",
		"我的长期记忆", "长期记忆列表", "长期记忆有哪些", "长期记忆有什么",
		"长期记忆", "查看永久记忆",
	} {
		if trimmed == exact || trimmed == "请"+exact || trimmed == "帮我"+exact {
			return true
		}
	}
	return false
}

// parseExplicitMemoryAdd extracts a direct memory instruction from text. If the message has
// multiple lines where line 1 is "长期记忆：xxx" and subsequent lines are a chat question,
// rest carries the remaining lines.
func parseExplicitMemoryAdd(text string) (content string, rest string, ok bool) {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	first := strings.TrimSpace(lines[0])
	tail := ""
	if len(lines) > 1 {
		tail = strings.TrimSpace(strings.Join(lines[1:], "\n"))
	}

	if c, matched := matchMemoryPrefix(first); matched {
		if c == "" && tail != "" {
			// User wrote:
			// 长期记忆：
			// 每次应答前面都要加上xxx
			return cleanMemoryContent(tail), "", true
		}
		if c != "" {
			return c, tail, true
		}
	}

	if tail == "" {
		if m := reMemoryPutInto.FindStringSubmatch(first); len(m) == 2 {
			if c := cleanMemoryContent(m[1]); c != "" {
				return c, "", true
			}
		}
		for _, suffix := range []string{
			"，作为长期记忆", ",作为长期记忆", "，记入长期记忆", "，加入长期记忆",
			"，写入长期记忆", "，存入长期记忆", "，设为长期记忆",
		} {
			trimmed := strings.TrimRight(first, "。！!")
			if strings.HasSuffix(trimmed, suffix) {
				if c := cleanMemoryContent(strings.TrimSuffix(trimmed, suffix)); c != "" {
					return c, "", true
				}
			}
		}
	}
	return "", "", false
}

func matchMemoryPrefix(line string) (string, bool) {
	s := strings.TrimSpace(line)
	for _, lead := range []string{"请帮我", "帮我", "麻烦", "请"} {
		if strings.HasPrefix(s, lead) {
			s = strings.TrimSpace(strings.TrimPrefix(s, lead))
			break
		}
	}
	prefixes := []string{
		"添加长期记忆", "新增长期记忆", "写入长期记忆", "保存长期记忆",
		"记入长期记忆", "加入长期记忆", "记住长期记忆", "存入长期记忆",
		"设置长期记忆", "设为长期记忆", "这是长期记忆", "作为长期记忆",
		"长期记忆", "永久记忆", "永久记住", "长期记住",
	}
	for _, p := range prefixes {
		if !strings.HasPrefix(s, p) {
			continue
		}
		rem := strings.TrimPrefix(s, p)
		if rem == "" {
			return "", false
		}
		r, size := utf8.DecodeRuneInString(rem)
		switch r {
		case '：', ':', '，', ',', '-', '—', ' ', '\t':
			return cleanMemoryContent(rem[size:]), true
		}
	}
	return "", false
}

func cleanMemoryContent(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "「」『』")
	return strings.TrimSpace(s)
}

func firstNoteLine(reply string) string {
	if i := strings.IndexByte(reply, '\n'); i >= 0 {
		return strings.TrimSpace(reply[:i])
	}
	return strings.TrimSpace(reply)
}
