package grok

import (
	"errors"
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
