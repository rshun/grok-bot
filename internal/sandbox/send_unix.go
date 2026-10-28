//go:build !windows

package sandbox

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"syscall"
)

// openNoBlock opens a file for reading without waiting on a FIFO or device;
// regular files ignore the flag. It never follows a symlink in the last component:
// callers pass a path from EvalSymlinks, so a link there means a swap.
func openNoBlock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}

// linkCount returns how many names the file has; more than one means a hard
// link may hide a denied file under a harmless name.
func linkCount(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink)
	}
	return 1
}

// openTarget returns where an open file really lives, so a swap between the
// check and the open is caught. ok is false where the kernel cannot say.
func openTarget(f *os.File) (target string, ok bool, err error) {
	if runtime.GOOS != "linux" {
		return "", false, nil
	}
	target, err = os.Readlink(fmt.Sprintf("/proc/self/fd/%d", f.Fd()))
	if err != nil {
		return "", true, err
	}
	// The kernel appends this to a file that was unlinked after opening.
	return strings.TrimSuffix(target, " (deleted)"), true, nil
}
