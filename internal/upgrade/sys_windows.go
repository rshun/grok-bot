//go:build windows

package upgrade

import (
	"os"
	"os/exec"
)

func checkSudoHelper(string) bool {
	return false
}

// RestartProcess spawns a fresh instance of the updated Windows executable after listeners
// and SQLite handles have been closed, allowing the caller to exit cleanly.
func RestartProcess(launchPath string, args []string, env []string) error {
	var passArgs []string
	if len(args) > 1 {
		passArgs = args[1:]
	}
	cmd := exec.Command(launchPath, passArgs...)
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Start()
}
