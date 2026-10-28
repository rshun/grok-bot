package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// anywhere marks a Deny entry that matches by file name in every directory.
const anywhere = "**/"

// CheckSendable decides whether the bot may read a file and send it to the
// chat. It refuses whatever the sandbox denies Grok, however the path is
// spelled, and returns the real path and size. Error messages are shown to
// Grok, so they never carry file contents or more than the caller already has.
func (p Policy) CheckSendable(path string) (real string, size int64, err error) {
	if !filepath.IsAbs(path) {
		return "", 0, errors.New("请给出绝对路径。")
	}
	clean := filepath.Clean(path)
	// Check the spelling first so a denied name never reveals whether it exists.
	if p.denied(clean) {
		return "", 0, errSendDenied
	}
	real, err = filepath.EvalSymlinks(clean)
	if err != nil {
		return "", 0, sendReadError(err)
	}
	if p.denied(real) {
		return "", 0, errSendDenied
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", 0, sendReadError(err)
	}
	if !info.Mode().IsRegular() {
		return "", 0, errors.New("这不是普通文件。")
	}
	if linkCount(info) > 1 {
		return "", 0, errSendDenied
	}
	f, err := openNoBlock(real)
	if err != nil {
		return "", 0, sendReadError(err)
	}
	f.Close()
	return real, info.Size(), nil
}

// OpenSendable runs CheckSendable and opens the file, so the caller reads from
// the descriptor it was checked on instead of a path that may change after the
// check. The descriptor must be the file that was checked, and still a plain
// file with no other names. The caller closes it. Size comes from the open file.
func (p Policy) OpenSendable(path string) (*os.File, int64, error) {
	real, _, err := p.CheckSendable(path)
	if err != nil {
		return nil, 0, err
	}
	f, err := openNoBlock(real)
	if err != nil {
		return nil, 0, sendReadError(err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, sendReadError(err)
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, 0, errors.New("这不是普通文件。")
	}
	if linkCount(info) > 1 {
		f.Close()
		return nil, 0, errSendDenied
	}
	if checked, err := os.Stat(real); err != nil || !os.SameFile(info, checked) {
		f.Close()
		return nil, 0, errSendDenied
	}
	if target, ok, err := openTarget(f); err != nil || (ok && p.denied(target)) {
		f.Close()
		return nil, 0, errSendDenied
	}
	return f, info.Size(), nil
}

var errSendDenied = errors.New("这个文件不允许发送。")

// kernelDirs hold files that look regular but expose the bot's own memory,
// environment and descriptors; they are never sent, whatever the deny list says.
var kernelDirs = []string{"/proc", "/sys", "/dev"}

func sendReadError(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("找不到这个文件。")
	}
	if errors.Is(err, os.ErrPermission) {
		return errors.New("没有权限读取这个文件。")
	}
	return errors.New("读取这个文件失败。")
}

// denied reports whether path, or any directory above it, is on the deny list,
// or lies in a kernel directory (see kernelDirs).
// A "**/" pattern matches the file name and every directory name above it, so
// files inside a directory called ".env" or "keys.pem" are refused too.
// Names are compared ignoring case, because macOS and Windows treat ".ENV" and
// ".env" as one file; being stricter than the sandbox costs nothing here.
func (p Policy) denied(path string) bool {
	for _, dir := range kernelDirs {
		if within(path, dir) {
			return true
		}
	}
	var targetInfos []os.FileInfo
	for _, entry := range p.Deny() {
		if pattern, ok := strings.CutPrefix(entry, anywhere); ok {
			if nameMatches(strings.ToLower(pattern), path) {
				return true
			}
			continue
		}
		item := resolve(entry)
		if within(path, item) {
			return true
		}
		if info, err := os.Stat(item); err == nil {
			targetInfos = append(targetInfos, info)
		}
	}
	// Same file under another spelling: different case, or a hard link.
	for cur := path; ; {
		if info, err := os.Stat(cur); err == nil {
			for _, target := range targetInfos {
				if os.SameFile(info, target) {
					return true
				}
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return false
}

// nameMatches reports whether pattern (lower case) matches the last element of
// path or the name of any directory above it, ignoring case.
func nameMatches(pattern, path string) bool {
	for cur := path; ; {
		parent := filepath.Dir(cur)
		if match, err := filepath.Match(pattern, strings.ToLower(filepath.Base(cur))); err != nil || match {
			return true
		}
		if parent == cur {
			return false
		}
		cur = parent
	}
}
