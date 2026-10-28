package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCheckSendableEnforcesEveryDenyEntry keeps the sandbox profile and the
// send check on one list: every entry of Deny must block a matching file.
func TestCheckSendableEnforcesEveryDenyEntry(t *testing.T) {
	p := testPolicy(t)
	deny := p.Deny()
	if len(deny) == 0 {
		t.Fatal("empty deny list")
	}
	for _, entry := range deny {
		t.Run(entry, func(t *testing.T) {
			var target string
			switch {
			case entry == "**/.env":
				target = filepath.Join(t.TempDir(), "x", ".env")
			case entry == "**/*.pem":
				target = filepath.Join(t.TempDir(), "x", "a.pem")
			case strings.HasPrefix(entry, "**/"):
				t.Fatalf("deny glob %q has no test case", entry)
			default:
				if info, err := os.Stat(entry); err == nil && info.IsDir() {
					target = filepath.Join(entry, "id")
				} else {
					target = entry
				}
			}
			writeFile(t, target, "PLACEHOLDER")
			if real, size, err := p.CheckSendable(target); err == nil {
				t.Fatalf("accepted %q as %q (%d bytes)", target, real, size)
			}
		})
	}
}

func TestCheckSendableSymlinkIntoDenied(t *testing.T) {
	p := testPolicy(t)
	key := filepath.Join(p.Home, ".ssh", "id")
	writeFile(t, key, "PLACEHOLDER")
	dotenv := filepath.Join(t.TempDir(), "x", ".env")
	writeFile(t, dotenv, "PLACEHOLDER")
	links := t.TempDir()

	link := filepath.Join(links, "link")
	if err := os.Symlink(key, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if real, _, err := p.CheckSendable(link); err == nil {
		t.Fatalf("accepted symlink to ssh dir as %q", real)
	}
	linkTxt := filepath.Join(links, "link.txt")
	if err := os.Symlink(dotenv, linkTxt); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if real, _, err := p.CheckSendable(linkTxt); err == nil {
		t.Fatalf("accepted symlink to .env as %q", real)
	}
	// A symlinked directory in the middle of the path must not hide the target.
	dirLink := filepath.Join(links, "dir")
	if err := os.Symlink(filepath.Join(p.Home, ".ssh"), dirLink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if real, _, err := p.CheckSendable(filepath.Join(dirLink, "id")); err == nil {
		t.Fatalf("accepted file below symlinked ssh dir as %q", real)
	}
}

func TestCheckSendableRejectsDirAndMissing(t *testing.T) {
	p := testPolicy(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "x")
	cases := map[string]string{
		"dir":      dir,
		"missing":  filepath.Join(dir, "missing.txt"),
		"relative": filepath.Join("relative", "a.txt"),
		"empty":    "",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			real, _, err := p.CheckSendable(raw)
			if err == nil {
				t.Fatalf("accepted %q as %q", raw, real)
			}
			if strings.Contains(err.Error(), dir) {
				t.Fatalf("error echoes the path: %v", err)
			}
		})
	}
}

func TestCheckSendableAccepts(t *testing.T) {
	p := testPolicy(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "notes.txt")
	writeFile(t, file, "hello")
	want, err := filepath.EvalSymlinks(file)
	if err != nil {
		t.Fatal(err)
	}
	real, size, err := p.CheckSendable(file)
	if err != nil {
		t.Fatal(err)
	}
	if real != want || size != 5 {
		t.Fatalf("got %q %d, want %q 5", real, size, want)
	}
	// Names that merely resemble denied ones stay sendable.
	for _, name := range []string{"env", "x.env", "a.pem.txt", ".envrc"} {
		other := filepath.Join(dir, name)
		writeFile(t, other, "ok")
		if _, _, err := p.CheckSendable(other); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
