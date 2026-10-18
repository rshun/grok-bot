package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testPolicy(t *testing.T) Policy {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	for _, dir := range []string{
		filepath.Join(home, ".grok", "sessions"),
		filepath.Join(home, ".ssh"),
		filepath.Join(root, "data"),
		filepath.Join(root, "etc"),
	} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	env := filepath.Join(root, "etc", "bot.env")
	if err := os.WriteFile(env, []byte("PLACEHOLDER=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return Policy{
		Home:     home,
		DataDir:  filepath.Join(root, "data"),
		EnvFile:  env,
		AuthPath: filepath.Join(home, ".grok", "auth.json"),
	}
}

func TestCheckDirRejects(t *testing.T) {
	p := testPolicy(t)
	file := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ok := t.TempDir()
	cases := map[string]string{
		"relative":            "relative/dir",
		"missing":             filepath.Join(ok, "missing"),
		"file":                file,
		"glob":                filepath.Join(ok, "*"),
		"leading space":       " " + ok,
		"trailing space":      ok + " ",
		"quote":               filepath.Join(ok, `a"b`),
		"control":             filepath.Join(ok, "a\tb"),
		"grok home":           filepath.Join(p.Home, ".grok"),
		"inside grok home":    filepath.Join(p.Home, ".grok", "sessions"),
		"home above grok":     p.Home,
		"data dir":            p.DataDir,
		"env file dir":        filepath.Dir(p.EnvFile),
		"dotdot to ssh":       filepath.Join(p.DataDir, "..", "home", ".ssh") + string(filepath.Separator),
		"dotdot inside clean": p.Home + string(filepath.Separator) + "x" + string(filepath.Separator) + ".." + string(filepath.Separator) + ".ssh",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if got, err := p.CheckDir(raw); err == nil {
				t.Fatalf("accepted %q as %q", raw, got)
			}
		})
	}
}

func TestCheckDirRejectsSymlinkIntoGrokHome(t *testing.T) {
	p := testPolicy(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(filepath.Join(p.Home, ".grok"), link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if got, err := p.CheckDir(link); err == nil {
		t.Fatalf("accepted symlink as %q", got)
	}
}

func TestCheckDirRejectsSystemPaths(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("system paths are Linux paths")
	}
	p := testPolicy(t)
	for _, raw := range []string{"/", "/etc", "/proc", "/etc/ssh"} {
		if got, err := p.CheckDir(raw); err == nil {
			t.Fatalf("accepted %q as %q", raw, got)
		}
	}
}

func TestCheckDirAccepts(t *testing.T) {
	p := testPolicy(t)
	dir := t.TempDir()
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.CheckDir(dir + string(filepath.Separator))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRender(t *testing.T) {
	p := Policy{Home: "/home/grok", DataDir: "/var/lib/bot", EnvFile: "/etc/bot.env", AuthPath: "/home/grok/.grok/auth.json"}
	out := Render([]string{`C:\rw`}, p)
	lines := strings.Split(out, "\n")
	if lines[0] != marker {
		t.Fatalf("first line %q", lines[0])
	}
	for _, want := range []string{
		"[profiles.grokbot]",
		`extends = "read-only"`,
		"restrict_network = false",
		`read_write = ["C:\\rw"]`,
		`"/home/grok/.grok/auth.json"`,
		tomlPath(filepath.Join("/home/grok", ".ssh")),
		`"/etc/bot.env"`,
		tomlPath(filepath.Join("/var/lib/bot", "bot.db")),
		tomlPath(filepath.Join("/var/lib/bot", "bot.db-wal")),
		tomlPath(filepath.Join("/var/lib/bot", "bot.db-shm")),
		`"**/.env"`,
		`"**/*.pem"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "read_only") || strings.Contains(out, `extends = "strict"`) {
		t.Fatalf("old strict profile fields remain:\n%s", out)
	}
	if empty := Render(nil, p); !strings.Contains(empty, "read_write = []") {
		t.Fatalf("empty list:\n%s", empty)
	}
}

// tomlPath quotes a path the way it must appear in the profile on this OS.
func tomlPath(path string) string {
	return `"` + strings.ReplaceAll(path, `\`, `\\`) + `"`
}

func TestSyncRefusesUnmanagedFile(t *testing.T) {
	p := testPolicy(t)
	path := filepath.Join(t.TempDir(), "sandbox.toml")
	m := &Manager{Path: path, Policy: p, Enabled: true}
	if err := m.Sync([]string{"/a"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), `"/a"`) {
		t.Fatalf("created %q %v", raw, err)
	}
	if err := m.Sync([]string{"/b"}); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(path)
	if !strings.Contains(string(raw), `"/b"`) || strings.Contains(string(raw), `"/a"`) {
		t.Fatalf("not replaced: %s", raw)
	}
	manual := "[profiles.mine]\nextends = \"workspace\"\n"
	if err := os.WriteFile(path, []byte(manual), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Sync([]string{"/c"}); !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("got %v, want ErrUnmanaged", err)
	}
	raw, _ = os.ReadFile(path)
	if string(raw) != manual {
		t.Fatalf("unmanaged file changed: %s", raw)
	}
}
