package grok

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

	withShell := base
	withShell.ShellTools = true
	shellArgs := replyArgs("/ws", withShell)
	tools = argValue(t, shellArgs, "--disallowed-tools")
	if hasTool(tools, "run_terminal_cmd") || hasTool(tools, "run_terminal_command") {
		t.Fatalf("shell tools still disallowed when ShellTools=true: %s", tools)
	}
	if !strings.Contains(argValue(t, shellArgs, "--system-prompt-override"), "run_terminal_cmd") {
		t.Fatal("system prompt does not mention run_terminal_cmd")
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
		"You may list any directory and read any file the system lets you read (using list_dir, read_file, and grep).",
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
	const sentence = " You can send a file to the user with the MCP tool grokbot__send_file; path must be absolute. When you send a file, also reply with one short line of text."
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

func TestPromptBlocks(t *testing.T) {
	body, err := promptBlocks("look", []Image{
		{MIME: "image/png", Data: []byte("one")},
		{MIME: "image/jpeg", Data: []byte("two")},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"type":"text","text":"look"},` +
		`{"type":"image","mimeType":"image/png","data":"` + base64.StdEncoding.EncodeToString([]byte("one")) + `"},` +
		`{"type":"image","mimeType":"image/jpeg","data":"` + base64.StdEncoding.EncodeToString([]byte("two")) + `"}]`
	if string(body) != want {
		t.Fatalf("got %s, want %s", body, want)
	}
	if strings.Contains(string(body), `"source"`) {
		t.Fatal("image block must use ACP mimeType/data, not an Anthropic source")
	}
}

func TestReplyArgsImages(t *testing.T) {
	inbox := t.TempDir()
	c := &Client{Workspace: "/ws", InboxDir: inbox}
	opts := ReplyOptions{SessionID: "s", Model: "grok-4.7", Prompt: "hi", Images: []Image{{MIME: "image/png", Data: []byte("png")}}}
	args, cleanup, err := c.buildArgs(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	file := argValue(t, args, "--prompt-file")
	if filepath.Dir(file) != inbox {
		t.Fatalf("prompt file %q is not in the inbox %q", file, inbox)
	}
	for _, arg := range args {
		if arg == "-p" {
			t.Fatalf("-p must not be used with images: %q", args)
		}
	}
	if argValue(t, args, "--cwd") != "/ws" {
		t.Fatalf("lost other args: %q", args)
	}
	hasVerbatim := false
	for _, arg := range args {
		hasVerbatim = hasVerbatim || arg == "--verbatim"
	}
	if !hasVerbatim {
		t.Fatalf("--verbatim missing: %q", args)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var blocks []map[string]string
	if err := json.Unmarshal(data, &blocks); err != nil || len(blocks) != 2 || blocks[0]["text"] != "hi" || blocks[1]["mimeType"] != "image/png" {
		t.Fatalf("prompt file: %s (%v)", data, err)
	}
	if info, err := os.Stat(file); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("prompt file mode: %v %v", info, err)
	}
	cleanup()
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("prompt file still there after cleanup: %v", err)
	}
	cleanup() // a second call is harmless

	// Without pictures the prompt stays on the command line and nothing is written.
	opts.Images = nil
	args, cleanup, err = c.buildArgs(opts)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if argValue(t, args, "-p") != "hi" {
		t.Fatalf("got %q", args)
	}
	for _, arg := range args {
		if arg == "--prompt-file" {
			t.Fatalf("--prompt-file without images: %q", args)
		}
	}
	if left, _ := os.ReadDir(inbox); len(left) != 0 {
		t.Fatalf("inbox not empty: %v", left)
	}

	opts.Images = []Image{{MIME: "image/png", Data: []byte("png")}}
	if _, _, err := (&Client{Workspace: "/ws"}).buildArgs(opts); err == nil {
		t.Fatal("images without an inbox dir were accepted")
	}
}

func TestPromptFileRemoved(t *testing.T) {
	opts := ReplyOptions{SessionID: "s", Model: "grok-4.7", Prompt: "hi", Images: []Image{{MIME: "image/png", Data: []byte("png")}}}
	for name, bin := range map[string]string{
		"start fails": filepath.Join(t.TempDir(), "no-such-grok"),
		"exits badly": os.Args[0], // the test binary rejects grok's flags and exits non-zero
	} {
		t.Run(name, func(t *testing.T) {
			inbox := t.TempDir()
			c := &Client{Bin: bin, Workspace: t.TempDir(), InboxDir: inbox}
			if _, err := c.Reply(context.Background(), opts); err == nil {
				t.Fatal("expected an error")
			}
			if left, _ := os.ReadDir(inbox); len(left) != 0 {
				t.Fatalf("prompt file left behind: %v", left)
			}
		})
	}
}

func TestEnvWithoutUpdateDropsBotConfig(t *testing.T) {
	botKeys := []string{
		"TELEGRAM_BOT_TOKEN", "DISCORD_BOT_TOKEN", "ALLOWED_USER_IDS", "DISCORD_ALLOWED_USER_IDS",
		"DATA_DIR", "DEFAULT_MODEL", "WEB_SEARCH", "BOT_ENV_FILE", "TIMEZONE",
		"QUEUE_LIMIT", "TASK_LIMIT", "TASK_MIN_INTERVAL",
	}
	for _, key := range botKeys {
		t.Setenv(key, "your_token_here")
	}
	t.Setenv("GROK_DISABLE_AUTOUPDATER", "0")
	t.Setenv("GROK_BOT_TEST_PLAIN", "kept")

	env := envWithoutUpdate()
	for _, key := range botKeys {
		for _, item := range env {
			if strings.HasPrefix(item, key+"=") {
				t.Errorf("env still has %s", key)
			}
		}
	}
	count := map[string]int{}
	for _, item := range env {
		count[item]++
	}
	if count["GROK_DISABLE_AUTOUPDATER=1"] != 1 || count["GROK_DISABLE_AUTOUPDATER=0"] != 0 {
		t.Errorf("env should set GROK_DISABLE_AUTOUPDATER=1 once")
	}
	if count["GROK_BOT_TEST_PLAIN=kept"] != 1 {
		t.Errorf("env dropped an unrelated variable")
	}
}
