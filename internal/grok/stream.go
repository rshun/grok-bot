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
// onTool may be nil. A line longer than maxStreamLine is skipped, not fatal.
func ParseStream(r io.Reader, onTool func(ToolUse)) (HeadlessResult, error) {
	reader := bufio.NewReaderSize(r, 64<<10)
	for {
		raw, readErr := readStreamLine(reader)
		if result, done, err := parseStreamLine(strings.TrimSpace(string(raw)), onTool); done {
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
func parseStreamLine(line string, onTool func(ToolUse)) (result HeadlessResult, done bool, err error) {
	if line == "" || line[0] != '{' {
		return HeadlessResult{}, false, nil
	}
	var msg streamLine
	if json.Unmarshal([]byte(line), &msg) != nil {
		return HeadlessResult{}, false, nil
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
		return HeadlessResult{}, true, &RunError{Message: detail}
	case "result":
		if msg.IsError || msg.Subtype != "success" {
			return HeadlessResult{}, true, &RunError{Message: resultFailure(msg)}
		}
		return HeadlessResult{
			Text:      strings.TrimSpace(msg.Result),
			SessionID: msg.SessionID,
		}, true, nil
	}
	return HeadlessResult{}, false, nil
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
