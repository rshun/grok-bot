package grok

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	Type  string          `json:"type"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ParseStream reads --output-format streaming-messages-json output, calling
// onTool for each tool call as it arrives, and returns the final result.
// onTool may be nil.
func ParseStream(r io.Reader, onTool func(ToolUse)) (HeadlessResult, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), maxStreamLine)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line[0] != '{' {
			continue
		}
		var msg streamLine
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "assistant":
			if onTool != nil {
				reportToolUses(msg.Message, onTool)
			}
		case "error":
			var detail string
			_ = json.Unmarshal(msg.Message, &detail)
			if detail == "" {
				detail = "grok reported an error"
			}
			return HeadlessResult{}, &RunError{Message: detail}
		case "result":
			if msg.IsError || msg.Subtype != "success" {
				return HeadlessResult{}, &RunError{Message: resultFailure(msg)}
			}
			return HeadlessResult{
				Text:      strings.TrimSpace(msg.Result),
				SessionID: msg.SessionID,
			}, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return HeadlessResult{}, fmt.Errorf("read grok stream: %w", err)
	}
	return HeadlessResult{}, errors.New("grok stream ended without a result")
}

func reportToolUses(raw json.RawMessage, onTool func(ToolUse)) {
	var message struct {
		Content []streamBlock `json:"content"`
	}
	if json.Unmarshal(raw, &message) != nil {
		return
	}
	for _, block := range message.Content {
		// Web search arrives as server_tool_use; local and MCP tools as tool_use.
		if block.Type == "tool_use" || block.Type == "server_tool_use" {
			onTool(ToolUse{Name: block.Name, Input: block.Input})
		}
	}
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
