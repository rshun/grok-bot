package grok

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
)

// maxStreamLine bounds one stream line; tool results can be large.
const maxStreamLine = 8 << 20

// ToolUse is one tool call the model made during a turn.
type ToolUse struct {
	Name  string
	Input json.RawMessage
}

type streamLine struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	IsError   bool            `json:"is_error"`
	Result    string          `json:"result"`
	SessionID string          `json:"session_id"`
	Errors    []string        `json:"errors"`
	Message   json.RawMessage `json:"message"`
}

type streamBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Content   json.RawMessage `json:"content"`
}

type streamState struct {
	lastText     string
	imageToolIDs map[string]bool
	genImages    []string
}

// ParseStream reads --output-format streaming-messages-json output, calling
// onTool for each tool call as it arrives, and returns the final result.
// onTool may be nil. A line longer than maxStreamLine is skipped, not fatal.
func ParseStream(r io.Reader, onTool func(ToolUse)) (HeadlessResult, error) {
	reader := bufio.NewReaderSize(r, 64<<10)
	st := &streamState{imageToolIDs: map[string]bool{}}
	for {
		raw, readErr := readStreamLine(reader)
		if result, done, err := parseStreamLine(strings.TrimSpace(string(raw)), onTool, st); done {
			return result, err
		}
		if readErr == io.EOF {
			return HeadlessResult{}, errors.New("grok stream ended without a result")
		}
		if readErr != nil {
			return HeadlessResult{}, fmt.Errorf("read grok stream: %w", readErr)
		}
	}
}

// readStreamLine returns the next line without its newline. A line longer than maxStreamLine
// is consumed in full but returned empty, so one huge line cannot end the turn. The error is
// io.EOF after the last line, which may come with data when it had no trailing newline.
func readStreamLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	tooLong := false
	for {
		chunk, err := r.ReadSlice('\n')
		if !tooLong {
			line = append(line, chunk...)
			if len(line) > maxStreamLine {
				tooLong = true
				line = nil
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if tooLong {
			line = nil
		}
		return line, err
	}
}

// parseStreamLine handles one stream line. done is true when the line ends the turn.
func parseStreamLine(line string, onTool func(ToolUse), st *streamState) (result HeadlessResult, done bool, err error) {
	if line == "" || line[0] != '{' {
		return HeadlessResult{}, false, nil
	}
	var msg streamLine
	if json.Unmarshal([]byte(line), &msg) != nil {
		return HeadlessResult{}, false, nil
	}
	switch msg.Type {
	case "assistant":
		if text := reportAssistantBlocks(msg.Message, onTool, st); text != "" && st != nil {
			st.lastText = text
		}
	case "user":
		if st != nil && len(st.imageToolIDs) > 0 {
			collectImageToolResults(msg.Message, st)
		}
	case "error":
		var detail string
		_ = json.Unmarshal(msg.Message, &detail)
		if detail == "" {
			detail = "grok reported an error"
		}
		return HeadlessResult{}, true, &RunError{Message: detail}
	case "result":
		var genImages []string
		if st != nil && len(st.genImages) > 0 {
			genImages = append([]string(nil), st.genImages...)
		}
		if msg.IsError || msg.Subtype != "success" {
			runErr := &RunError{
				Message:   resultFailure(msg),
				SessionID: msg.SessionID,
			}
			if msg.Subtype == "error_max_turns" || IsMaxTurns(runErr) {
				if trimmed := strings.TrimSpace(msg.Result); trimmed != "" {
					runErr.PartialText = trimmed
				} else if st != nil && st.lastText != "" {
					runErr.PartialText = st.lastText
				}
			}
			return HeadlessResult{SessionID: msg.SessionID, GeneratedImages: genImages}, true, runErr
		}
		return HeadlessResult{
			Text:            strings.TrimSpace(msg.Result),
			SessionID:       msg.SessionID,
			GeneratedImages: genImages,
		}, true, nil
	}
	return HeadlessResult{}, false, nil
}

func reportAssistantBlocks(raw json.RawMessage, onTool func(ToolUse), st *streamState) string {
	var message struct {
		Content []streamBlock `json:"content"`
	}
	if json.Unmarshal(raw, &message) != nil {
		return ""
	}
	var texts []string
	for _, block := range message.Content {
		if block.Type == "text" {
			if t := strings.TrimSpace(block.Text); t != "" {
				texts = append(texts, t)
			}
		}
		if block.Type == "tool_use" || block.Type == "server_tool_use" {
			if st != nil && block.ID != "" && (block.Name == "image_gen" || block.Name == "image_edit") {
				st.imageToolIDs[block.ID] = true
			}
			// Web search arrives as server_tool_use; local and MCP tools as tool_use.
			if onTool != nil {
				onTool(ToolUse{Name: block.Name, Input: block.Input})
			}
		}
	}
	return strings.Join(texts, "\n\n")
}

var reAbsImagePath = regexp.MustCompile(`(?i)((?:[A-Za-z]:[\\/]|/)[^\r\n"'<>|?*]+?\.(?:jpg|jpeg|png|webp|gif))`)

func collectImageToolResults(raw json.RawMessage, st *streamState) {
	var message struct {
		Content []streamBlock `json:"content"`
	}
	if json.Unmarshal(raw, &message) != nil {
		return
	}
	for _, block := range message.Content {
		if block.Type != "tool_result" || block.IsError || !st.imageToolIDs[block.ToolUseID] {
			continue
		}
		for _, text := range extractToolResultTexts(block.Content) {
			for _, candidate := range extractImagePaths(text) {
				if !containsString(st.genImages, candidate) {
					st.genImages = append(st.genImages, candidate)
				}
			}
		}
	}
}

func extractToolResultTexts(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var single string
	if json.Unmarshal(raw, &single) == nil && single != "" {
		return []string{single}
	}
	var items []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &items) == nil {
		var out []string
		for _, item := range items {
			if item.Text != "" {
				out = append(out, item.Text)
			}
		}
		return out
	}
	return nil
}

func extractImagePaths(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if isLikelyImagePath(trimmed) {
			out = append(out, trimmed)
			continue
		}
		for _, m := range reAbsImagePath.FindAllString(trimmed, -1) {
			if isLikelyImagePath(m) {
				out = append(out, m)
			}
		}
	}
	return out
}

func isLikelyImagePath(p string) bool {
	if p == "" {
		return false
	}
	if !filepath.IsAbs(p) && !strings.HasPrefix(p, "/") {
		return false
	}
	switch strings.ToLower(filepath.Ext(p)) {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif":
		return true
	}
	return false
}

func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func resultFailure(msg streamLine) string {
	if len(msg.Errors) > 0 && msg.Errors[0] != "" {
		return msg.Errors[0]
	}
	if msg.Subtype == "error_max_turns" {
		return "grok stopped: max turns reached"
	}
	if text := strings.TrimSpace(msg.Result); text != "" {
		return firstLine(text)
	}
	if msg.Subtype != "" {
		return "grok failed: " + msg.Subtype
	}
	return "grok failed"
}
