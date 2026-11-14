package upgrade

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Mode describes how grok-bot can upgrade its own binary on the current host.
type Mode int

const (
	// ModeUnavailable means automatic binary replacement cannot run under the current setup.
	ModeUnavailable Mode = iota
	// ModeInPlace means the service user can atomically replace TargetPath in its parent directory.
	ModeInPlace
	// ModeSudoHelper means TargetPath is root-owned read-only, and a root-owned sudo helper is authorized.
	ModeSudoHelper
)

// Preflight summarizes whether and how grok-bot can self-upgrade on this machine.
type Preflight struct {
	Mode       Mode
	LaunchPath string
	TargetPath string
	AssetName  string
	Reason     string
	Guide      string
}

// CheckPreflight inspects build version, OS/arch support, executable location, and write/helper permissions.
func (u *Upgrader) CheckPreflight() Preflight {
	p := Preflight{
		Mode:       ModeUnavailable,
		LaunchPath: strings.TrimSpace(u.Paths.LaunchPath),
		TargetPath: strings.TrimSpace(u.Paths.TargetPath),
	}
	asset, err := AssetNameFor(u.goos(), u.goarch())
	if err != nil {
		p.Reason = err.Error()
		return p
	}
	p.AssetName = asset

	if IsDevVersion(u.CurrentVersion) {
		p.Reason = "当前运行的是本地开发构建版本 (dev)，为防止覆盖本地调试程序，已禁用聊天自动升级。"
		return p
	}
	if p.TargetPath == "" {
		p.Reason = "无法定位当前运行程序的磁盘路径。"
		return p
	}
	if isEphemeralOrReadOnlyStore(p.TargetPath) {
		p.Reason = fmt.Sprintf("当前程序位于临时或只读包管理目录 (%s)，不支持原地自动升级。", filepath.Dir(p.TargetPath))
		return p
	}

	targetDir := filepath.Dir(p.TargetPath)
	if probeDirWritable(targetDir) {
		p.Mode = ModeInPlace
		return p
	}

	if checkSudoHelper(u.sudoHelper()) {
		p.Mode = ModeSudoHelper
		return p
	}

	p.Reason = fmt.Sprintf("当前运行账户对程序所在目录 (%s) 无写入权限。", targetDir)
	p.Guide = buildPermissionGuide(u.goos(), p.LaunchPath, p.TargetPath, u.DataDir)
	return p
}

func probeDirWritable(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	f, err := os.CreateTemp(dir, ".grok-bot-writecheck-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

func isEphemeralOrReadOnlyStore(targetPath string) bool {
	clean := filepath.Clean(targetPath)
	if strings.HasPrefix(clean, "/nix/store/") {
		return true
	}
	tmpDir := filepath.Clean(os.TempDir())
	if tmpDir != "" && tmpDir != string(filepath.Separator) {
		if realTmp, err := filepath.EvalSymlinks(tmpDir); err == nil && realTmp != "" {
			tmpDir = filepath.Clean(realTmp)
		}
		rel, relErr := filepath.Rel(tmpDir, clean)
		if relErr == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			// Distinguish `go run` temporary build directories from ordinary paths.
			if strings.Contains(rel, "go-build") {
				return true
			}
		}
	}
	return false
}

func buildPermissionGuide(goos, launchPath, targetPath, dataDir string) string {
	if strings.EqualFold(goos, "windows") {
		return fmt.Sprintf("请将程序移动到当前用户可写的目录（当前为 `%s`），或以管理员身份手动替换新版 `.exe`。", targetPath)
	}
	managedDir := "/var/lib/grok-tg-bot/bin"
	if strings.TrimSpace(dataDir) != "" {
		managedDir = filepath.Join(strings.TrimSpace(dataDir), "bin")
	}
	binName := filepath.Base(targetPath)
	if binName == "" || binName == "." {
		binName = "grok-tg-bot"
	}
	entryPath := launchPath
	if entryPath == "" {
		entryPath = targetPath
	}
	return fmt.Sprintf(
		"如需启用聊天一键升级，可在服务器任选一种方式一次性配置：\n"+
			"1. 软链接托管至数据目录（免 sudo 推荐）：\n"+
			"`sudo install -d -o $(whoami) -g $(id -gn) %s && sudo mv %s %s/%s && sudo ln -s %s/%s %s`\n"+
			"2. 受限零参数升级脚本：`sudo install -o root -g root -m 0755 deploy/grok-bot-upgrade /usr/local/sbin/grok-bot-upgrade` 并授权 `NOPASSWD: /usr/local/sbin/grok-bot-upgrade`\n"+
			"3. 或直接在服务器手动升级：`sudo sh deploy/upgrade.sh`",
		managedDir, targetPath, managedDir, binName, managedDir, binName, entryPath,
	)
}
