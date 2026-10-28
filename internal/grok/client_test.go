package grok

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestHeadlessFailureDoesNotHideErrorBehindSessionNotice(t *testing.T) {
	const notice = "Session test-session found locally (originally in /test/old-workspace)"
	const modelError = "Error: Couldn't set model 'unsupported-model': Invalid params: unknown model id"
	tests := []struct {
		name   string
		stdout string
		stderr string
		want   string
	}{
		{"structured stdout", `{"type":"error","message":"unknown model id"}`, notice, "unknown model id"},
		{"structured stderr", "", notice + "\n" + `{"type":"error","message":"unknown model id"}`, "unknown model id"},
		{"plain stderr", "", notice + "\r\n\n" + modelError, modelError},
		{"plain stdout", modelError, notice, modelError},
		{"notice only", "", notice, "exit status 1"},
		{"ordinary failure", "", "permission denied", "permission denied"},
		{"missing session", "", "Session test-session not found", "Session test-session not found"},
		{"empty output", "", "", "exit status 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, parseErr := ParseHeadless(tt.stdout)
			err := headlessFailure(tt.stdout, tt.stderr, parseErr, errors.New("exit status 1"))
			if err == nil || err.Error() != tt.want {
				t.Fatalf("got %v, want %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "/test/old-workspace") {
				t.Fatal("session lookup notice leaked into the failure")
			}
		})
	}
}

func TestHeadlessFailurePreservesParseErrorWithoutProcessError(t *testing.T) {
	_, parseErr := ParseHeadless("")
	if got := headlessFailure("", "", parseErr, nil); got != parseErr {
		t.Fatalf("got %v, want original parse error %v", got, parseErr)
	}
}

func argValue(t *testing.T, args []string, flag string) string {
	t.Helper()
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("%s missing from %q", flag, args)
	return ""
}

func hasArg(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}

func hasTool(list, name string) bool {
	for _, item := range strings.Split(list, ",") {
		if item == name {
			return true
		}
	}
	return false
}

func TestReplyArgsWebSearch(t *testing.T) {
	base := ReplyOptions{SessionID: "sess", NewSession: true, Model: "grok-4.7", Prompt: "hi"}

	off := replyArgs("/ws", base)
	if !hasArg(off, "--disable-web-search") {
		t.Fatalf("web search not disabled: %q", off)
	}
	tools := argValue(t, off, "--disallowed-tools")
	if !hasTool(tools, "web_search") || !hasTool(tools, "web_fetch") {
		t.Fatalf("web tools allowed while off: %s", tools)
	}
	if argValue(t, off, "-s") != "sess" {
		t.Fatalf("new session flag: %q", off)
	}

	on := base
	on.WebSearch = true
	on.NewSession = false
	args := replyArgs("/ws", on)
	if hasArg(args, "--disable-web-search") {
		t.Fatalf("web search still disabled: %q", args)
	}
	tools = argValue(t, args, "--disallowed-tools")
	if hasTool(tools, "web_search") || hasTool(tools, "web_fetch") {
		t.Fatalf("web tools still disallowed: %s", tools)
	}
	if !hasTool(tools, "run_terminal_cmd") || !hasTool(tools, "read_file") {
		t.Fatalf("shell or file tools opened: %s", tools)
	}
	if !strings.Contains(argValue(t, args, "--system-prompt-override"), "search the web") {
		t.Fatal("system prompt does not mention web search")
	}
	if argValue(t, args, "-r") != "sess" {
		t.Fatalf("resume flag: %q", args)
	}
}

var wantFileTools = []string{"read_file", "list_dir", "grep", "search_replace", "write"}

func TestReplyArgsFiles(t *testing.T) {
	opts := ReplyOptions{
		SessionID: "sess", NewSession: true, Model: "grok-4.7", Prompt: "hi",
		Sandbox: "grokbot", FileTools: true, WriteDirs: []string{"/srv/out"},
	}
	args := replyArgs("/ws", opts)
	if argValue(t, args, "--sandbox") != "grokbot" {
		t.Fatalf("sandbox: %q", args)
	}
	tools := argValue(t, args, "--disallowed-tools")
	for _, name := range wantFileTools {
		if hasTool(tools, name) {
			t.Fatalf("%s still disallowed: %s", name, tools)
		}
	}
	if !hasTool(tools, "run_terminal_cmd") || !hasTool(tools, "run_terminal_command") {
		t.Fatalf("shell opened: %s", tools)
	}
	prompt := argValue(t, args, "--system-prompt-override")
	for _, want := range []string{
		"You may read any file the system lets you read.",
		"You may write only in: /srv/out.",
		"Never run files or commands.",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("missing %q in prompt: %s", want, prompt)
		}
	}
	if strings.Contains(prompt, "do not read or edit files") {
		t.Fatalf("prompt still forbids files: %s", prompt)
	}
	opts.WriteDirs = nil
	if prompt := argValue(t, replyArgs("/ws", opts), "--system-prompt-override"); !strings.Contains(prompt, "You may write only in: (none).") {
		t.Fatalf("empty write list: %s", prompt)
	}

	opts.FileTools = false
	opts.Sandbox = ""
	args = replyArgs("/ws", opts)
	if hasArg(args, "--sandbox") {
		t.Fatalf("sandbox passed: %q", args)
	}
	tools = argValue(t, args, "--disallowed-tools")
	for _, name := range wantFileTools {
		if !hasTool(tools, name) {
			t.Fatalf("%s allowed without file access: %s", name, tools)
		}
	}
}

func TestReplyArgsStreaming(t *testing.T) {
	base := ReplyOptions{SessionID: "sess", NewSession: true, Model: "grok-4.7", Prompt: "hi"}
	if got := argValue(t, replyArgs("/ws", base), "--output-format"); got != "streaming-messages-json" {
		t.Fatalf("chat output format: %q", got)
	}
	base.JSONSchema = `{"type":"object"}`
	if got := argValue(t, replyArgs("/ws", base), "--output-format"); got != "json" {
		t.Fatalf("schema output format: %q", got)
	}
}

func TestReplyArgsJSONSchema(t *testing.T) {
	args := replyArgs("/ws", ReplyOptions{
		SessionID: "sess", NewSession: true, Model: "grok-4.7", Prompt: "hi",
		WebSearch: true, Sandbox: "grokbot", FileTools: true, JSONSchema: `{"type":"object"}`,
	})
	if argValue(t, args, "--json-schema") != `{"type":"object"}` {
		t.Fatalf("schema: %q", args)
	}
	if argValue(t, args, "--max-turns") != "2" {
		t.Fatalf("max turns: %q", args)
	}
	if hasArg(args, "--sandbox") {
		t.Fatalf("sandbox passed for schema call: %q", args)
	}
	tools := argValue(t, args, "--disallowed-tools")
	for _, name := range append([]string{"web_search", "web_fetch", "search_tool", "use_tool"}, wantFileTools...) {
		if !hasTool(tools, name) {
			t.Fatalf("%s allowed in schema call: %s", name, tools)
		}
	}
}

// TestHelperGrok is re-executed as a fake grok process by replyStream tests.
func TestHelperGrok(t *testing.T) {
	if os.Getenv("GROK_BOT_HELPER") != "1" {
		return
	}
	fmt.Fprintln(os.Stdout, `{"type":"system","subtype":"init"}`)
	fmt.Fprintln(os.Stdout, "Error: Couldn't set model 'unsupported-model': Invalid params")
	fmt.Fprintln(os.Stderr, "Session test-session found locally (originally in /test/old-workspace)")
	os.Exit(1)
}

func TestReplyStreamKeepsPlainStdoutError(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperGrok$")
	cmd.Env = append(os.Environ(), "GROK_BOT_HELPER=1")
	_, err := replyStream(context.Background(), cmd, ReplyOptions{SessionID: "s"})
	const want = "Error: Couldn't set model 'unsupported-model': Invalid params"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}

func TestPlainCaptureKeepsOnlyNonJSONLinesBounded(t *testing.T) {
	var c plainCapture
	input := `{"type":"system"}` + "\n" + "boom: first\n" + strings.Repeat("y", 10<<10) + "\n"
	// Feed in odd-sized chunks so lines straddle writes.
	for i := 0; i < len(input); i += 7 {
		end := i + 7
		if end > len(input) {
			end = len(input)
		}
		if _, err := c.Write([]byte(input[i:end])); err != nil {
			t.Fatal(err)
		}
	}
	got := c.String()
	if !strings.HasPrefix(got, "boom: first\n") || strings.Contains(got, `"type"`) {
		t.Fatalf("%q", got)
	}
	if len(got) > plainCaptureMax+plainLineMax {
		t.Fatalf("capture not bounded: %d", len(got))
	}
}

func TestReplyArgsTrust(t *testing.T) {
	base := ReplyOptions{SessionID: "sess", NewSession: true, Model: "grok-4.7", Prompt: "hi"}
	if hasArg(replyArgs("/ws", base), "--trust") {
		t.Fatal("--trust passed without a channel")
	}
	on := base
	on.Actions = true
	if !hasArg(replyArgs("/ws", on), "--trust") {
		t.Fatalf("--trust missing: %q", replyArgs("/ws", on))
	}
	on.JSONSchema = `{"type":"object"}`
	if hasArg(replyArgs("/ws", on), "--trust") {
		t.Fatal("--trust passed for a schema call")
	}
}

func TestSystemPromptActions(t *testing.T) {
	base := ReplyOptions{SessionID: "sess", Model: "grok-4.7", Prompt: "hi", Actions: true}
	off := systemPromptFor(base)
	if strings.Contains(off, "grokbot__react") || strings.Contains(off, "[[NO_REPLY]]") {
		t.Fatalf("react mentioned while off: %s", off)
	}
	for _, files := range []bool{false, true} {
		on := base
		on.React = true
		on.FileTools = files
		prompt := systemPromptFor(on)
		if !strings.Contains(prompt, "grokbot__react") || !strings.Contains(prompt, "[[NO_REPLY]]") {
			t.Fatalf("react missing (files=%v): %s", files, prompt)
		}
	}
	schema := base
	schema.React = true
	schema.JSONSchema = `{"type":"object"}`
	if strings.Contains(systemPromptFor(schema), "grokbot__react") {
		t.Fatal("react mentioned in a schema call")
	}
}

func TestSystemPromptSendFile(t *testing.T) {
	const sentence = " You can send a file to the user with the MCP tool grokbot__send_file; path must be absolute."
	base := ReplyOptions{SessionID: "sess", Model: "grok-4.7", Prompt: "hi", Actions: true, FileTools: true}
	if strings.Contains(systemPromptFor(base), "send_file") {
		t.Fatal("send_file mentioned while off")
	}
	on := base
	on.SendFile = true
	if !strings.Contains(systemPromptFor(on), sentence) {
		t.Fatalf("send_file missing: %s", systemPromptFor(on))
	}
	schema := on
	schema.JSONSchema = `{"type":"object"}`
	if strings.Contains(systemPromptFor(schema), "send_file") {
		t.Fatal("send_file mentioned in a schema call")
	}
}
