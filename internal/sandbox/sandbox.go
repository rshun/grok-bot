// Package sandbox writes the grok sandbox profile that limits which directories Grok can touch.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// ProfileName is the custom profile passed to grok with --sandbox.
const ProfileName = "grokbot"

// marker is the first line of a profile file the bot owns and may rewrite.
const marker = "# managed by grok-bot; do not edit"

// ErrUnmanaged means the profile file exists but was not written by the bot.
var ErrUnmanaged = errors.New("sandbox.toml 不是 bot 生成的")

// systemPaths may never be granted, nor any directory above or below them.
var systemPaths = []string{"/etc", "/proc", "/sys", "/dev", "/boot", "/root"}

// Policy holds the paths that must stay out of Grok's reach.
type Policy struct {
	Home     string
	DataDir  string
	EnvFile  string
	AuthPath string
}

// Supported reports whether grok can enforce a sandbox on this OS.
func Supported() bool {
	return runtime.GOOS == "linux" || runtime.GOOS == "darwin"
}

// CheckDir validates a directory before it is granted and returns its real path.
// Error messages are meant for the chat user.
func (p Policy) CheckDir(raw string) (string, error) {
	if raw != strings.TrimSpace(raw) {
		return "", fmt.Errorf("路径首尾不能有空格。")
	}
	if !utf8.ValidString(raw) || strings.ContainsAny(raw, `"*?[`) || strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("路径里不能有引号、通配符或控制字符。")
	}
	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("请写绝对路径，例如 /srv/notes。")
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(raw))
	if err != nil {
		return "", fmt.Errorf("找不到这个目录。")
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("找不到这个目录。")
	}
	if !info.IsDir() {
		return "", fmt.Errorf("这不是目录。")
	}
	if real == string(filepath.Separator) || real == filepath.VolumeName(real)+string(filepath.Separator) {
		return "", fmt.Errorf("不能开放根目录。")
	}
	guarded := append([]string(nil), systemPaths...)
	guarded = append(guarded, filepath.Join(p.Home, ".grok"), filepath.Join(p.Home, ".ssh"), p.DataDir)
	for _, item := range guarded {
		if item == "" {
			continue
		}
		item = resolve(item)
		if within(real, item) || within(item, real) {
			return "", fmt.Errorf("不能开放 %s 及其上下级目录。", item)
		}
	}
	// The env file only blocks the directories that contain it.
	if p.EnvFile != "" && within(resolve(p.EnvFile), real) {
		return "", fmt.Errorf("这个目录包含 bot 的环境文件，不能开放。")
	}
	return real, nil
}

// resolve follows symlinks when the path exists so comparisons use real paths.
func resolve(path string) string {
	path = filepath.Clean(path)
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return path
}

// within reports whether path is dir or below it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// Render returns the full sandbox.toml content for the granted directories.
func Render(readOnly, readWrite []string, p Policy) string {
	deny := []string{
		p.AuthPath,
		filepath.Join(p.Home, ".ssh"),
		p.EnvFile,
		filepath.Join(p.DataDir, "bot.db"),
		filepath.Join(p.DataDir, "bot.db-wal"),
		filepath.Join(p.DataDir, "bot.db-shm"),
		"**/.env",
		"**/*.pem",
	}
	var b strings.Builder
	b.WriteString(marker + "\n")
	b.WriteString("[profiles." + ProfileName + "]\n")
	b.WriteString("extends = \"strict\"\n")
	// MCP servers run as child processes and still need the network.
	b.WriteString("restrict_network = false\n")
	b.WriteString("read_only = " + tomlList(readOnly) + "\n")
	b.WriteString("read_write = " + tomlList(readWrite) + "\n")
	b.WriteString("deny = " + tomlList(deny) + "\n")
	return b.String()
}

func tomlList(items []string) string {
	quoted := make([]string, 0, len(items))
	for _, item := range items {
		if item == "" {
			continue
		}
		item = strings.ReplaceAll(item, `\`, `\\`)
		item = strings.ReplaceAll(item, `"`, `\"`)
		quoted = append(quoted, `"`+item+`"`)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// Manager keeps the profile file in step with the granted directories.
type Manager struct {
	Path    string
	Policy  Policy
	Enabled bool

	mu sync.Mutex
}

// Sync rewrites the profile file. It refuses to replace a file the bot did not write.
func (m *Manager) Sync(readOnly, readWrite []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, err := os.ReadFile(m.Path)
	switch {
	case err == nil:
		if !strings.HasPrefix(string(current), marker+"\n") {
			return ErrUnmanaged
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	content := Render(readOnly, readWrite, m.Policy)
	if string(current) == content {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.Path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(m.Path), ".sandbox-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), m.Path)
}
