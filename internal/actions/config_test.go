package actions

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteConfigCreatesAndUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".grok", "config.toml")
	if err := WriteConfig(path, 41234); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.HasPrefix(text, "# managed by grok-bot; do not edit\n") {
		t.Fatalf("missing marker: %q", text)
	}
	for _, want := range []string{"[mcp_servers.grokbot]", "127.0.0.1:41234/mcp", "${GROKBOT_TURN_TOKEN}"} {
		if !strings.Contains(text, want) {
			t.Fatalf("config lacks %q:\n%s", want, text)
		}
	}

	if err := WriteConfig(path, 50001); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if !strings.Contains(string(data), "127.0.0.1:50001/mcp") || strings.Contains(string(data), "41234") {
		t.Fatalf("port not updated:\n%s", data)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestWriteConfigRefusesUnmanaged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	const own = "[mcp_servers.mine]\nurl = \"http://example.invalid\"\n"
	if err := os.WriteFile(path, []byte(own), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteConfig(path, 41234); !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("got %v, want ErrUnmanaged", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != own {
		t.Fatalf("file changed: %q", data)
	}
}
