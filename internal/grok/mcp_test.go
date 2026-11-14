package grok

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	if stateBase := os.Getenv("GROK_FAKE_CLI_STATE"); stateBase != "" {
		runFakeGrokCLI(stateBase, os.Args[1:])
		return
	}
	os.Exit(m.Run())
}

func runFakeGrokCLI(stateBase string, args []string) {
	verFile := stateBase + ".ver"
	logFile := stateBase + ".log"
	appendLog := func(entry string) {
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.WriteString(entry + "\n")
			_ = f.Close()
		}
	}
	if len(args) == 1 && args[0] == "--version" {
		raw, err := os.ReadFile(verFile)
		if err != nil || strings.TrimSpace(string(raw)) == "" {
			fmt.Println("grok 0.4.1")
		} else {
			fmt.Println(strings.TrimSpace(string(raw)))
		}
		os.Exit(0)
	}
	if len(args) == 2 && args[0] == "leader" && args[1] == "kill" {
		appendLog("leader_kill")
		os.Exit(0)
	}
	if len(args) == 1 && args[0] == "update" {
		for _, secretVar := range []string{"TELEGRAM_BOT_TOKEN", "DISCORD_BOT_TOKEN", "ADMIN_USER_IDS", "DISCORD_ADMIN_USER_IDS", "GROK_DISABLE_AUTOUPDATER"} {
			if val := os.Getenv(secretVar); val != "" {
				fmt.Fprintf(os.Stderr, "unexpected env %s=%s\n", secretVar, val)
				os.Exit(2)
			}
		}
		if os.Getenv("CI") != "1" || os.Getenv("NONINTERACTIVE") != "1" {
			fmt.Fprintln(os.Stderr, "missing CI/NONINTERACTIVE flags")
			os.Exit(2)
		}
		appendLog("update")
		if os.Getenv("GROK_FAKE_CLI_FAIL") == "1" {
			fmt.Fprintln(os.Stderr, "update failed: token=sk-FakeSecretLeakValue123")
			os.Exit(1)
		}
		if os.Getenv("GROK_FAKE_CLI_SAME") == "1" {
			fmt.Println("Already up to date (0.4.1)")
			os.Exit(0)
		}
		_ = os.WriteFile(verFile, []byte("grok 0.4.3\n"), 0o600)
		fmt.Println("Successfully updated grok to 0.4.3")
		os.Exit(0)
	}
	os.Exit(1)
}

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

func stageFakeCLIBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	name := "grok"
	if runtime.GOOS == "windows" {
		name = "grok.exe"
	}
	dst := filepath.Join(dir, name)
	src, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return dst
}

func TestUpdateCLISuccessAndEnvIsolation(t *testing.T) {
	binPath := stageFakeCLIBinary(t)
	stateBase := filepath.Join(t.TempDir(), "cli-state")
	t.Setenv("GROK_FAKE_CLI_STATE", stateBase)
	t.Setenv("TELEGRAM_BOT_TOKEN", "123456:AAFakeTokenShouldNeverReachCLI")
	t.Setenv("ADMIN_USER_IDS", "10001")
	t.Setenv("GROK_DISABLE_AUTOUPDATER", "1")

	client := &Client{Bin: binPath}
	res, err := client.UpdateCLI(context.Background())
	if err != nil {
		t.Fatalf("UpdateCLI error: %v", err)
	}
	if !res.Updated || res.BeforeVersion != "grok 0.4.1" || res.AfterVersion != "grok 0.4.3" {
		t.Fatalf("unexpected CLIUpdateResult: %#v", res)
	}
	logRaw, err := os.ReadFile(stateBase + ".log")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(logRaw)); got != "leader_kill\nupdate\nleader_kill" {
		t.Fatalf("unexpected lifecycle order: %q", got)
	}

	// Running again when already up to date reports Updated=false.
	t.Setenv("GROK_FAKE_CLI_SAME", "1")
	res2, err := client.UpdateCLI(context.Background())
	if err != nil {
		t.Fatalf("UpdateCLI same-version error: %v", err)
	}
	if res2.Updated || res2.BeforeVersion != "grok 0.4.3" || res2.AfterVersion != "grok 0.4.3" {
		t.Fatalf("expected Updated=false, got %#v", res2)
	}

	// Failure redacts sensitive tokens from error output.
	t.Setenv("GROK_FAKE_CLI_FAIL", "1")
	_, err = client.UpdateCLI(context.Background())
	if err == nil {
		t.Fatal("expected error when CLI update fails")
	}
	if strings.Contains(err.Error(), "FakeSecretLeakValue123") {
		t.Fatalf("unredacted secret in error: %v", err)
	}
}
