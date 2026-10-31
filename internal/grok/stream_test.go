package grok

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const streamInit = `{"type":"system","subtype":"init","session_id":"sess-1","mcp_servers":[{"name":"grokbot","status":"connected"}],"tools":["web_search","use_tool"]}`

func TestParseStreamResult(t *testing.T) {
	stream := strings.Join([]string{
		streamInit,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Let me check."},{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{"query":"weather in Paris"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"web_search_tool_result","tool_use_id":"srv_1","content":[]}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"use_tool","input":{"tool_name":"grokbot__react","tool_input":{"emoji":"👍"}}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"ok"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"\n  It is sunny.\n","session_id":"sess-2"}`,
	}, "\n") + "\n"

	var got []ToolUse
	res, err := ParseStream(strings.NewReader(stream), func(tu ToolUse) { got = append(got, tu) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "It is sunny." {
		t.Fatalf("text: %q", res.Text)
	}
	if res.SessionID != "sess-2" {
		t.Fatalf("session id: %q", res.SessionID)
	}
	if len(got) != 2 || got[0].Name != "web_search" || got[1].Name != "use_tool" {
		t.Fatalf("tool uses: %+v", got)
	}
	var search struct{ Query string }
	if err := json.Unmarshal(got[0].Input, &search); err != nil || search.Query != "weather in Paris" {
		t.Fatalf("web_search input: %s (%v)", got[0].Input, err)
	}
	var call struct {
		ToolName string `json:"tool_name"`
	}
	if err := json.Unmarshal(got[1].Input, &call); err != nil || call.ToolName != "grokbot__react" {
		t.Fatalf("use_tool input: %s (%v)", got[1].Input, err)
	}
}

func TestParseStreamNilCallback(t *testing.T) {
	stream := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"grep","input":{"pattern":"x"}}]}}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s"}` + "\n"
	res, err := ParseStream(strings.NewReader(stream), nil)
	if err != nil || res.Text != "done" {
		t.Fatalf("got %+v, %v", res, err)
	}
}

func TestParseStreamError(t *testing.T) {
	tests := []struct {
		name   string
		stream string
		want   string
	}{
		{
			"execution error",
			streamInit + "\n" + `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["Couldn't set model 'x': unknown model id"],"session_id":"s"}`,
			"Couldn't set model 'x': unknown model id",
		},
		{
			"max turns",
			streamInit + "\n" + `{"type":"result","subtype":"error_max_turns","is_error":true,"result":null,"session_id":"s"}`,
			"max turns",
		},
		{
			"error line",
			`{"type":"error","message":"unknown model id"}`,
			"unknown model id",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseStream(strings.NewReader(tt.stream+"\n"), nil)
			var runErr *RunError
			if !errors.As(err, &runErr) {
				t.Fatalf("got %T %v, want *RunError", err, err)
			}
			if !strings.Contains(runErr.Message, tt.want) {
				t.Fatalf("message %q does not contain %q", runErr.Message, tt.want)
			}
		})
	}
}

func TestParseStreamErrorMessageIsFirstError(t *testing.T) {
	stream := `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["first","second"]}` + "\n"
	_, err := ParseStream(strings.NewReader(stream), nil)
	var runErr *RunError
	if !errors.As(err, &runErr) || runErr.Message != "first" {
		t.Fatalf("got %v", err)
	}
}

func TestParseStreamIgnoresUnknown(t *testing.T) {
	stream := strings.Join([]string{
		streamInit,
		`not json at all`,
		`{"type":"telemetry","payload":{"a":1}}`,
		``,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hmm"}]}}`,
		`Session sess-1 found locally (originally in /somewhere)`,
		`{"type":"result","subtype":"success","is_error":false,"result":"fine","session_id":"s"}`,
	}, "\n") + "\n"
	res, err := ParseStream(strings.NewReader(stream), nil)
	if err != nil || res.Text != "fine" {
		t.Fatalf("got %+v, %v", res, err)
	}
}

func TestParseStreamEmptyResult(t *testing.T) {
	stream := `{"type":"result","subtype":"success","is_error":false,"result":"","session_id":"s"}` + "\n"
	res, err := ParseStream(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatalf("empty result should not fail: %v", err)
	}
	if res.Text != "" {
		t.Fatalf("text: %q", res.Text)
	}
}

func TestParseStreamNoResult(t *testing.T) {
	for _, stream := range []string{"", streamInit + "\n"} {
		if _, err := ParseStream(strings.NewReader(stream), nil); err == nil {
			t.Fatalf("no error for %q", stream)
		}
	}
}

func TestParseStreamLongLine(t *testing.T) {
	// A single tool result line can be far larger than bufio's default token size.
	big := strings.Repeat("a", 2<<20)
	stream := `{"type":"user","message":{"content":[{"type":"tool_result","content":"` + big + `"}]}}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"s"}` + "\n"
	res, err := ParseStream(strings.NewReader(stream), nil)
	if err != nil || res.Text != "ok" {
		t.Fatalf("got %+v, %v", res, err)
	}
}

func TestParseStreamSkipsOverlongLine(t *testing.T) {
	junk := strings.Repeat("x", maxStreamLine+(1<<20))
	stream := `{"type":"user","message":"` + junk + `"}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"web_search","input":{}}]}}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"s"}` + "\n"
	var tools []string
	res, err := ParseStream(strings.NewReader(stream), func(tu ToolUse) { tools = append(tools, tu.Name) })
	if err != nil || res.Text != "ok" || res.SessionID != "s" {
		t.Fatalf("got %+v, %v", res, err)
	}
	if len(tools) != 1 || tools[0] != "web_search" {
		t.Fatalf("tools after the skipped line: %v", tools)
	}
}

func TestParseStreamOverlongLineWithoutNewlineAtEOF(t *testing.T) {
	stream := `{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"s"}` + "\n" +
		strings.Repeat("x", maxStreamLine+10)
	res, err := ParseStream(strings.NewReader(stream), nil)
	if err != nil || res.Text != "ok" {
		t.Fatalf("got %+v, %v", res, err)
	}
}

func TestParseStreamMaxTurnsKeepsPartialTextAndSession(t *testing.T) {
	stream := strings.Join([]string{
		streamInit,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"已完成前两步检查，发现 3 个异常日志。"},{"type":"tool_use","name":"read_file","input":{"path":"/var/log/app.log"}}]}}`,
		`{"type":"result","subtype":"error_max_turns","is_error":true,"errors":["Reached the maximum number of turns"],"session_id":"sess-42"}`,
	}, "\n") + "\n"

	res, err := ParseStream(strings.NewReader(stream), nil)
	if !IsMaxTurns(err) {
		t.Fatalf("expected IsMaxTurns=true, got %v", err)
	}
	if res.SessionID != "sess-42" {
		t.Fatalf("session id: %q", res.SessionID)
	}
	facing := UserFacing(err)
	if !strings.Contains(facing, "已完成前两步检查") || !strings.Contains(facing, "继续") {
		t.Fatalf("unexpected user facing text: %q", facing)
	}
}
