package grok

import (
	"reflect"
	"strings"
	"testing"
)

func TestRedactSensitive(t *testing.T) {
	in := "Authorization: Bearer your_token_here\nAPI_KEY=sk-your_api_key_here\nRAG_TOKEN=your_rag_token_here\nurl=https://mcp.example.com"
	got := RedactSensitive(in)
	for _, leak := range []string{"your_token_here", "your_api_key_here", "your_rag_token_here"} {
		if strings.Contains(got, leak) {
			t.Fatalf("leaked %q in:\n%s", leak, got)
		}
	}
	if !strings.Contains(got, "https://mcp.example.com") {
		t.Fatalf("redacted safe URL:\n%s", got)
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
