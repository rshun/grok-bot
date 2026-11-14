//go:build !windows

package upgrade

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// checkSudoHelper verifies that helperPath exists as a root-owned regular file without
// group/world write permissions and is authorized for passwordless execution via sudo -n -l.
func checkSudoHelper(helperPath string) bool {
	helperPath = strings.TrimSpace(helperPath)
	if helperPath == "" {
		return false
	}
	info, err := os.Lstat(helperPath)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	// Reject group-writable or world-writable helper scripts.
	if info.Mode().Perm()&0o022 != 0 {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 {
		return false
	}
	if _, err := exec.LookPath("sudo"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sudo", "-n", "-l", helperPath)
	cmd.Stdin = nil
	cmd.Env = sanitizedHelperEnv()
	return cmd.Run() == nil
}

// RestartProcess replaces the current Unix process image in-place with the updated binary
// (preserving the same PID under systemd, launchd, Docker, or a terminal).
func RestartProcess(launchPath string, args []string, env []string) error {
	if len(args) == 0 {
		args = []string{launchPath}
	}
	return syscall.Exec(launchPath, args, env)
}
