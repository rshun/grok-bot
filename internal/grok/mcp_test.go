package grok

import (
	"reflect"
	"strings"
	"testing"
)

func TestRedactSensitive(t *testing.T) {
	in := "Authorization: Bearer your_token_here\nAPI_KEY=sk-your_api_key_here\nRAG_TOKEN=your_rag_token_here\ntg=https://api.telegram.org/bot123456789:AAFakeTelegramBotTokenValue/getUpdates\nurl=https://mcp.example.com"
	got := RedactSensitive(in)
	for _, leak := range []string{"your_token_here", "your_api_key_here", "your_rag_token_here", "AAFakeTelegramBotTokenValue"} {
		if strings.Contains(got, leak) {
			t.Fatalf("leaked %q in:\n%s", leak, got)
		}
	}
	if !strings.Contains(got, "https://mcp.example.com") {
		t.Fatalf("redacted safe URL:\n%s", got)
	}
}

func TestFormatMCPList(t *testing.T) {
	jsonInput := `[
		{"name": "Theseus-mcp", "url": "https://internal.example.com/mcp", "enabled": true, "scope": "user"},
		{"name": "rag-mcp", "url": "http://10.0.0.8:8080/mcp", "enabled": false, "scope": "user"},
		{"name": "fetch", "command": "uvx", "args": ["mcp-server-fetch"], "enabled": true, "scope": "user"},
		{"name": "grokbot", "url": "http://127.0.0.1:41234/mcp", "enabled": true, "scope": "project"}
	]`
	gotJSON := FormatMCPList(jsonInput)
	wantJSON := "Theseus-mcp\nrag-mcp (disabled)\nfetch"
	if gotJSON != wantJSON {
		t.Fatalf("FormatMCPList(json) = %q, want %q", gotJSON, wantJSON)
	}

	textInput := "  Theseus-mcp: https://internal.example.com/mcp\n  rag-mcp: http://10.0.0.8:8080/mcp (disabled)\n  fetch: uvx mcp-server-fetch\n"
	gotText := FormatMCPList(textInput)
	if gotText != wantJSON {
		t.Fatalf("FormatMCPList(text) = %q, want %q", gotText, wantJSON)
	}

	if empty := FormatMCPList("[]"); empty != "" {
		t.Fatalf("expected empty for [], got %q", empty)
	}
	if empty := FormatMCPList("No MCP servers configured. Run `grok mcp add --help` to get started.\n"); empty != "" {
		t.Fatalf("expected empty for no-servers message, got %q", empty)
	}
}

func TestBuildMCPAddArgs(t *testing.T) {
	// Remote HTTP server
	got, err := buildMCPAddArgs([]string{"sentry", "https://mcp.sentry.dev/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"mcp", "add", "--scope", "user", "sentry", "https://mcp.sentry.dev/mcp"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("http got %q, want %q", got, want)
	}

	// Stdio server without `--` auto-inserts `--`
	got, err = buildMCPAddArgs([]string{"-e", "FOO=bar", "fetch", "uvx", "mcp-server-fetch"})
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"mcp", "add", "--scope", "user", "-e", "FOO=bar", "fetch", "--", "uvx", "mcp-server-fetch"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stdio got %q, want %q", got, want)
	}

	// Stdio server with explicit `--` preserving server `-y` flag
	got, err = buildMCPAddArgs([]string{"pg", "--", "npx", "-y", "@modelcontextprotocol/server-postgres"})
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"mcp", "add", "--scope", "user", "pg", "--", "npx", "-y", "@modelcontextprotocol/server-postgres"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("explicit -- got %q, want %q", got, want)
	}

	// Reject reserved `grokbot` and dangerous flags
	for _, bad := range [][]string{
		{"grokbot", "https://example.com"},
		{"--scope", "project", "foo", "https://example.com"},
		{"--leader-socket", "/tmp/x", "foo", "https://example.com"},
	} {
		if _, err := buildMCPAddArgs(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}
