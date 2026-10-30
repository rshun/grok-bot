package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
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

func TestCheckSendableRefusesDataDir(t *testing.T) {
	p := testPolicy(t)
	if p.DataDir == "" {
		t.Fatal("test policy has no DataDir")
	}
	for _, rel := range []string{
		filepath.Join("inbox", "prompt-123.json"),
		filepath.Join("workspace", ".grok", "config.toml"),
		"other.txt",
	} {
		file := filepath.Join(p.DataDir, rel)
		writeFile(t, file, "x")
		if _, _, err := p.CheckSendable(file); err == nil {
			t.Fatalf("%s: sendable", rel)
		}
		if f, _, err := p.OpenSendable(file); err == nil {
			f.Close()
			t.Fatalf("%s: openable for sending", rel)
		}
	}
}

func TestCheckSendableRefusesKernelFiles(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs is Linux only")
	}
	p := testPolicy(t)
	for _, path := range []string{"/proc/self/environ", "/proc/self/cmdline", "/dev/fd/0"} {
		if real, _, err := p.CheckSendable(path); err != errSendDenied {
			t.Fatalf("%s: got %q, %v, want deny error", path, real, err)
		}
		if f, _, err := p.OpenSendable(path); err != errSendDenied {
			if f != nil {
				f.Close()
			}
			t.Fatalf("%s: OpenSendable got %v, want deny error", path, err)
		}
	}
}

// A second name for a denied file must not slip past: this exercises the
// SameFile branch on every OS that can hard link.
func TestCheckSendableRefusesHardLinkToDenied(t *testing.T) {
	p := testPolicy(t)
	writeFile(t, p.AuthPath, "PLACEHOLDER")
	for name, target := range map[string]string{"env file": p.EnvFile, "auth file": p.AuthPath} {
		t.Run(name, func(t *testing.T) {
			link := filepath.Join(t.TempDir(), "harmless.txt")
			if err := os.Link(target, link); err != nil {
				t.Skipf("hard link unavailable: %v", err)
			}
			if real, _, err := p.CheckSendable(link); err != errSendDenied {
				t.Fatalf("got %q, %v, want deny error", real, err)
			}
			if f, _, err := p.OpenSendable(link); err != errSendDenied {
				if f != nil {
					f.Close()
				}
				t.Fatalf("OpenSendable got %v, want deny error", err)
			}
		})
	}
}

func TestCheckSendableRefusesAnyHardLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("link count is not checked on Windows")
	}
	p := testPolicy(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	writeFile(t, file, "x")
	link := filepath.Join(dir, "b.txt")
	if err := os.Link(file, link); err != nil {
		t.Skipf("hard link unavailable: %v", err)
	}
	for _, path := range []string{file, link} {
		if real, _, err := p.CheckSendable(path); err != errSendDenied {
			t.Fatalf("CheckSendable %s: got %q, %v, want deny error", path, real, err)
		}
		if f, _, err := p.OpenSendable(path); err != errSendDenied {
			if f != nil {
				f.Close()
			}
			t.Fatalf("OpenSendable %s: got %v, want deny error", path, err)
		}
	}
}

func TestOpenSendableAccepts(t *testing.T) {
	p := testPolicy(t)
	file := filepath.Join(t.TempDir(), "notes.txt")
	writeFile(t, file, "hello")
	f, size, err := p.OpenSendable(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if size != 5 {
		t.Fatalf("size %d, want 5", size)
	}
	buf := make([]byte, 8)
	n, _ := f.Read(buf)
	if string(buf[:n]) != "hello" {
		t.Fatalf("read %q", buf[:n])
	}
}

func TestOpenSendableRefuses(t *testing.T) {
	p := testPolicy(t)
	dir := t.TempDir()
	dotenv := filepath.Join(dir, ".env")
	writeFile(t, dotenv, "PLACEHOLDER")
	cases := map[string]string{
		"dir":      dir,
		"denied":   dotenv,
		"env file": p.EnvFile,
		"missing":  filepath.Join(dir, "missing.txt"),
		"relative": "a.txt",
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			f, _, err := p.OpenSendable(path)
			if err == nil {
				f.Close()
				t.Fatalf("opened %q", path)
			}
			if f != nil {
				t.Fatal("file returned with error")
			}
		})
	}
}

func TestCheckSendableDenyPatternsMatchDirectoryNames(t *testing.T) {
	p := testPolicy(t)
	refused := []string{"/x/.env/secret", "/x/keys.pem/a", "/x/.ENV/a", "/x/Keys.PEM/deep/a.txt"}
	for _, path := range refused {
		path = filepath.FromSlash(path)
		if !p.denied(filepath.Clean(path)) {
			t.Errorf("%s not denied", path)
		}
	}
	allowed := []string{"/x/.envrc/a", "/x/a.pem.txt", "/x/env/a"}
	for _, path := range allowed {
		path = filepath.FromSlash(path)
		if p.denied(filepath.Clean(path)) {
			t.Errorf("%s denied", path)
		}
	}
	// End to end, through a real directory with such a name.
	dir := filepath.Join(t.TempDir(), "keys.pem")
	writeFile(t, filepath.Join(dir, "a.txt"), "x")
	if _, _, err := p.CheckSendable(filepath.Join(dir, "a.txt")); err == nil {
		t.Fatal("file inside keys.pem directory accepted")
	}
}
