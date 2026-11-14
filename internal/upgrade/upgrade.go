// Package upgrade implements self-inspection, verified GitHub Release downloads,
// atomic binary replacement, and cross-platform process handover for grok-bot.
package upgrade

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultRepo is the official GitHub repository from which release binaries are fetched.
	DefaultRepo = "rshun/grok-bot"
	// DefaultAPIBase is the GitHub REST API base URL.
	DefaultAPIBase = "https://api.github.com"
	// DefaultSudoHelperPath is the optional root-owned helper script authorized via sudoers.
	DefaultSudoHelperPath = "/usr/local/sbin/grok-bot-upgrade"
	// ExitCodeUpgrade is returned when falling back to systemd RestartForceExitStatus=42.
	ExitCodeUpgrade = 42

	maxSumsBytes   = 64 << 10
	maxBinaryBytes = 200 << 20
)

// ExePaths holds the startup snapshot of the running grok-bot executable.
type ExePaths struct {
	// LaunchPath is the absolute path reported by os.Executable() at startup (may be a symlink).
	LaunchPath string
	// TargetPath is the symlink-resolved physical file path on disk.
	TargetPath string
}

// SnapshotExecutable captures LaunchPath and TargetPath immediately at process startup,
// avoiding late `/proc/self/exe (deleted)` resolution issues on Linux.
func SnapshotExecutable() ExePaths {
	exe, err := os.Executable()
	if err != nil || strings.TrimSpace(exe) == "" {
		return ExePaths{}
	}
	exe = strings.TrimSuffix(exe, " (deleted)")
	launch := exe
	if abs, absErr := filepath.Abs(exe); absErr == nil && abs != "" {
		launch = abs
	}
	target := launch
	if real, symErr := filepath.EvalSymlinks(launch); symErr == nil && real != "" {
		target = strings.TrimSuffix(real, " (deleted)")
	}
	return ExePaths{
		LaunchPath: filepath.Clean(launch),
		TargetPath: filepath.Clean(target),
	}
}

// ReleaseInfo describes a verified latest GitHub Release and its matching platform assets.
type ReleaseInfo struct {
	TagName   string
	HTMLURL   string
	AssetName string
	AssetURL  string
	SumsURL   string
}

// Upgrader inspects, downloads, verifies, and applies grok-bot binary updates.
type Upgrader struct {
	CurrentVersion string
	Paths          ExePaths
	DataDir        string
	GOOS           string
	GOARCH         string
	Repo           string
	APIBase        string
	HTTP           *http.Client
	SudoHelperPath string
	// AllowInsecureHTTP permits http:// localhost URLs in offline unit tests only.
	AllowInsecureHTTP bool
}

func (u *Upgrader) goos() string {
	if u != nil && strings.TrimSpace(u.GOOS) != "" {
		return strings.ToLower(strings.TrimSpace(u.GOOS))
	}
	return runtime.GOOS
}

func (u *Upgrader) goarch() string {
	if u != nil && strings.TrimSpace(u.GOARCH) != "" {
		return strings.ToLower(strings.TrimSpace(u.GOARCH))
	}
	return runtime.GOARCH
}

func (u *Upgrader) repo() string {
	if u != nil && strings.TrimSpace(u.Repo) != "" {
		return strings.TrimSpace(u.Repo)
	}
	return DefaultRepo
}

func (u *Upgrader) apiBase() string {
	if u != nil && strings.TrimSpace(u.APIBase) != "" {
		return strings.TrimRight(strings.TrimSpace(u.APIBase), "/")
	}
	return DefaultAPIBase
}

func (u *Upgrader) sudoHelper() string {
	if u != nil && strings.TrimSpace(u.SudoHelperPath) != "" {
		return strings.TrimSpace(u.SudoHelperPath)
	}
	return DefaultSudoHelperPath
}

func (u *Upgrader) httpClient(timeout time.Duration) *http.Client {
	if u != nil && u.HTTP != nil {
		return u.HTTP
	}
	return &http.Client{Timeout: timeout}
}

// AssetNameFor maps GOOS and GOARCH to the release artifact built by .github/workflows/release.yml.
func AssetNameFor(goos, goarch string) (string, error) {
	goos = strings.ToLower(strings.TrimSpace(goos))
	goarch = strings.ToLower(strings.TrimSpace(goarch))
	switch goos + "/" + goarch {
	case "linux/amd64":
		return "grok-bot-linux-amd64", nil
	case "linux/arm64":
		return "grok-bot-linux-arm64", nil
	case "darwin/arm64":
		return "grok-bot-darwin-arm64", nil
	case "darwin/amd64":
		return "grok-bot-darwin-amd64", nil
	case "windows/amd64":
		return "grok-bot-windows-amd64.exe", nil
	default:
		return "", fmt.Errorf("当前系统架构 (%s/%s) 暂无官方预编译发布包，请从源码编译升级", goos, goarch)
	}
}

// IsDevVersion reports whether v is an unversioned local development build.
func IsDevVersion(v string) bool {
	v = strings.TrimSpace(v)
	return v == "" || strings.EqualFold(v, "dev")
}

type semver struct {
	major int
	minor int
	patch int
	pre   string
}

func parseSemver(raw string) (semver, bool) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	if s == "" {
		return semver{}, false
	}
	if idx := strings.IndexByte(s, '+'); idx >= 0 {
		s = s[:idx]
	}
	var pre string
	if idx := strings.IndexByte(s, '-'); idx >= 0 {
		pre = s[idx+1:]
		s = s[:idx]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	maj, err1 := strconv.Atoi(parts[0])
	min, err2 := strconv.Atoi(parts[1])
	pat, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil || maj < 0 || min < 0 || pat < 0 {
		return semver{}, false
	}
	return semver{major: maj, minor: min, patch: pat, pre: pre}, true
}

// CompareVersions compares two semantic version tags (e.g. "v0.9.1" and "v0.9.2").
// It returns (-1, true) if a < b, (0, true) if a == b, (1, true) if a > b, or (0, false) if either is not a valid semver.
func CompareVersions(a, b string) (int, bool) {
	va, oka := parseSemver(a)
	vb, okb := parseSemver(b)
	if !oka || !okb {
		return 0, false
	}
	switch {
	case va.major != vb.major:
		if va.major < vb.major {
			return -1, true
		}
		return 1, true
	case va.minor != vb.minor:
		if va.minor < vb.minor {
			return -1, true
		}
		return 1, true
	case va.patch != vb.patch:
		if va.patch < vb.patch {
			return -1, true
		}
		return 1, true
	}
	switch {
	case va.pre == vb.pre:
		return 0, true
	case va.pre == "" && vb.pre != "":
		return 1, true
	case va.pre != "" && vb.pre == "":
		return -1, true
	case va.pre < vb.pre:
		return -1, true
	default:
		return 1, true
	}
}

type githubReleasePayload struct {
	TagName    string `json:"tag_name"`
	HTMLURL    string `json:"html_url"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

// CheckLatest queries GitHub Releases for the latest stable release and locates the platform binary and SHA256SUMS.
func (u *Upgrader) CheckLatest(ctx context.Context) (ReleaseInfo, error) {
	assetName, err := AssetNameFor(u.goos(), u.goarch())
	if err != nil {
		return ReleaseInfo{}, err
	}
	endpoint := fmt.Sprintf("%s/repos/%s/releases/latest", u.apiBase(), u.repo())
	if err := u.validateDownloadURL(endpoint); err != nil {
		return ReleaseInfo{}, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ReleaseInfo{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "grok-bot-upgrader")

	resp, err := u.httpClient(20 * time.Second).Do(req)
	if err != nil {
		return ReleaseInfo{}, fmt.Errorf("查询 GitHub 最新版本失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ReleaseInfo{}, fmt.Errorf("查询 GitHub 最新版本失败（HTTP %d）", resp.StatusCode)
	}

	var payload githubReleasePayload
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return ReleaseInfo{}, fmt.Errorf("解析版本信息失败：%w", err)
	}
	if payload.Draft || payload.Prerelease {
		return ReleaseInfo{}, errors.New("最新构建为预发布或草稿版本，跳过自动升级")
	}
	tag := strings.TrimSpace(payload.TagName)
	if _, ok := parseSemver(tag); !ok {
		return ReleaseInfo{}, fmt.Errorf("远端版本号格式无效：%q", tag)
	}

	info := ReleaseInfo{
		TagName:   tag,
		HTMLURL:   strings.TrimSpace(payload.HTMLURL),
		AssetName: assetName,
	}
	for _, a := range payload.Assets {
		switch strings.TrimSpace(a.Name) {
		case assetName:
			info.AssetURL = strings.TrimSpace(a.BrowserDownloadURL)
		case "SHA256SUMS":
			info.SumsURL = strings.TrimSpace(a.BrowserDownloadURL)
		}
	}
	if info.AssetURL == "" {
		return ReleaseInfo{}, fmt.Errorf("Release %s 中未找到当前平台二进制 (%s)", tag, assetName)
	}
	if info.SumsURL == "" {
		return ReleaseInfo{}, fmt.Errorf("Release %s 缺少 SHA256SUMS 校验文件，拒绝不安全升级", tag)
	}
	if err := u.validateDownloadURL(info.AssetURL); err != nil {
		return ReleaseInfo{}, err
	}
	if err := u.validateDownloadURL(info.SumsURL); err != nil {
		return ReleaseInfo{}, err
	}
	return info, nil
}

var trustedGitHubHosts = map[string]bool{
	"api.github.com":                       true,
	"github.com":                           true,
	"objects.githubusercontent.com":        true,
	"release-assets.githubusercontent.com": true,
	"codeload.github.com":                  true,
}

func (u *Upgrader) validateDownloadURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return errors.New("升级下载地址无效")
	}
	if u != nil && u.AllowInsecureHTTP {
		if parsed.Scheme == "http" || parsed.Scheme == "https" {
			return nil
		}
	}
	if parsed.Scheme != "https" {
		return errors.New("拒绝非 HTTPS 升级地址")
	}
	host := strings.ToLower(parsed.Hostname())
	if !trustedGitHubHosts[host] {
		return fmt.Errorf("拒绝非官方受信任下载域名：%s", host)
	}
	return nil
}

// ParseSHA256Sums extracts the expected lowercase 64-character hex SHA-256 digest for assetName.
func ParseSHA256Sums(sumsContent, assetName string) (string, error) {
	assetName = strings.TrimSpace(assetName)
	if assetName == "" {
		return "", errors.New("empty asset name")
	}
	for _, rawLine := range strings.Split(sumsContent, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		hashHex := strings.ToLower(strings.TrimSpace(fields[0]))
		name := strings.TrimSpace(fields[1])
		name = strings.TrimPrefix(name, "*")
		name = strings.TrimPrefix(name, "./")
		if name != assetName {
			continue
		}
		if len(hashHex) != 64 {
			return "", fmt.Errorf("SHA256SUMS 中 %s 的哈希长度无效", assetName)
		}
		if _, err := hex.DecodeString(hashHex); err != nil {
			return "", fmt.Errorf("SHA256SUMS 中 %s 的哈希格式无效", assetName)
		}
		return hashHex, nil
	}
	return "", fmt.Errorf("SHA256SUMS 中未找到 %s 的校验记录", assetName)
}

// DownloadAndApply downloads SHA256SUMS and the matching binary into TargetPath's directory,
// verifies both SHA-256 and executable magic bytes, backs up the old binary to <TargetPath>.bak,
// and atomically swaps the new binary into place.
func (u *Upgrader) DownloadAndApply(ctx context.Context, rel ReleaseInfo) (string, error) {
	targetPath := strings.TrimSpace(u.Paths.TargetPath)
	if targetPath == "" {
		return "", errors.New("无法定位当前可执行文件物理路径")
	}
	if err := u.validateDownloadURL(rel.SumsURL); err != nil {
		return "", err
	}
	if err := u.validateDownloadURL(rel.AssetURL); err != nil {
		return "", err
	}

	sumsBytes, err := u.fetchSmallBytes(ctx, rel.SumsURL, maxSumsBytes, 30*time.Second)
	if err != nil {
		return "", fmt.Errorf("下载 SHA256SUMS 失败：%w", err)
	}
	expectedHex, err := ParseSHA256Sums(string(sumsBytes), rel.AssetName)
	if err != nil {
		return "", err
	}

	targetDir := filepath.Dir(targetPath)
	tmpFile, err := os.CreateTemp(targetDir, ".grok-bot.upgrade-*.tmp")
	if err != nil {
		return "", fmt.Errorf("无法在程序目录创建临时文件 (%s)：%w", targetDir, err)
	}
	tmpPath := tmpFile.Name()
	keepTmp := false
	defer func() {
		if !keepTmp {
			_ = tmpFile.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	dlCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, rel.AssetURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "grok-bot-upgrader")
	resp, err := u.httpClient(5 * time.Minute).Do(req)
	if err != nil {
		return "", fmt.Errorf("下载新版本二进制失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载新版本二进制失败（HTTP %d）", resp.StatusCode)
	}

	hasher := sha256.New()
	var headerBuf bytes.Buffer
	headerCap := 8
	tee := io.LimitReader(resp.Body, maxBinaryBytes+1)
	written, err := copyWithHeader(io.MultiWriter(tmpFile, hasher), tee, &headerBuf, headerCap)
	if err != nil {
		return "", fmt.Errorf("写入新版本临时文件失败：%w", err)
	}
	if written > maxBinaryBytes {
		return "", errors.New("下载文件体积超过安全上限，已中止")
	}
	if !validExecutableHeader(u.goos(), headerBuf.Bytes()) {
		return "", fmt.Errorf("下载产物不是合法的 %s 可执行文件格式，已中止", u.goos())
	}

	gotHex := strings.ToLower(hex.EncodeToString(hasher.Sum(nil)))
	if subtle.ConstantTimeCompare([]byte(gotHex), []byte(expectedHex)) != 1 {
		return "", fmt.Errorf("SHA256 校验不匹配（期望 %s…，实际 %s…），已拒绝替换", expectedHex[:12], gotHex[:12])
	}

	perm := os.FileMode(0o755)
	if st, statErr := os.Stat(targetPath); statErr == nil {
		perm = st.Mode().Perm() | 0o700
		if perm&0o111 == 0 {
			perm |= 0o755
		}
	}
	if err := tmpFile.Sync(); err != nil {
		return "", fmt.Errorf("刷盘临时二进制失败：%w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", fmt.Errorf("关闭临时二进制失败：%w", err)
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		return "", fmt.Errorf("设置可执行权限失败：%w", err)
	}

	bakPath, err := swapBinary(u.goos(), tmpPath, targetPath, perm)
	if err != nil {
		return "", err
	}
	keepTmp = true
	return bakPath, nil
}

func (u *Upgrader) fetchSmallBytes(ctx context.Context, rawURL string, limit int64, timeout time.Duration) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "grok-bot-upgrader")
	resp, err := u.httpClient(timeout).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("响应内容超出安全大小上限")
	}
	return data, nil
}

func copyWithHeader(dst io.Writer, src io.Reader, hdr *bytes.Buffer, hdrLimit int) (int64, error) {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		nr, rErr := src.Read(buf)
		if nr > 0 {
			if hdr.Len() < hdrLimit {
				rem := hdrLimit - hdr.Len()
				if nr < rem {
					rem = nr
				}
				_, _ = hdr.Write(buf[:rem])
			}
			nw, wErr := dst.Write(buf[:nr])
			total += int64(nw)
			if wErr != nil {
				return total, wErr
			}
			if nw != nr {
				return total, io.ErrShortWrite
			}
		}
		if rErr != nil {
			if errors.Is(rErr, io.EOF) {
				return total, nil
			}
			return total, rErr
		}
	}
}

func validExecutableHeader(goos string, head []byte) bool {
	switch strings.ToLower(goos) {
	case "linux":
		return len(head) >= 4 && head[0] == 0x7f && head[1] == 'E' && head[2] == 'L' && head[3] == 'F'
	case "windows":
		return len(head) >= 2 && head[0] == 'M' && head[1] == 'Z'
	case "darwin":
		if len(head) < 4 {
			return false
		}
		magic := binary.BigEndian.Uint32(head[:4])
		switch magic {
		case 0xfeedface, 0xcefaedfe, 0xfeedfacf, 0xcffaedfe, 0xcafebabe:
			return true
		default:
			return false
		}
	default:
		return len(head) >= 4
	}
}

func swapBinary(goos, tmpPath, targetPath string, perm os.FileMode) (string, error) {
	bakPath := targetPath + ".bak"
	if strings.EqualFold(goos, "windows") {
		// Windows forbids overwriting a running .exe in place, but permits renaming it.
		if _, err := os.Stat(targetPath); err == nil {
			_ = os.Remove(bakPath)
			if _, stillErr := os.Stat(bakPath); stillErr == nil {
				_ = os.Rename(bakPath, bakPath+".old")
			}
			if err := os.Rename(targetPath, bakPath); err != nil {
				return "", fmt.Errorf("重命名当前运行程序失败：%w", err)
			}
			if err := os.Rename(tmpPath, targetPath); err != nil {
				_ = os.Rename(bakPath, targetPath)
				return "", fmt.Errorf("替换新版程序失败（已自动回滚原文件）：%w", err)
			}
			return bakPath, nil
		}
		if err := os.Rename(tmpPath, targetPath); err != nil {
			return "", fmt.Errorf("写入新版程序失败：%w", err)
		}
		return "", nil
	}

	// Unix: write backup via atomic rename first (avoids ETXTBSY if .bak was ever executed),
	// then atomically rename tmpPath onto targetPath in the same directory.
	var savedBak string
	if src, err := os.Open(targetPath); err == nil {
		if bakTmp, tmpErr := os.CreateTemp(filepath.Dir(targetPath), ".grok-bot.bak-*.tmp"); tmpErr == nil {
			bakTmpName := bakTmp.Name()
			_, copyErr := io.Copy(bakTmp, src)
			_ = bakTmp.Chmod(perm)
			closeErr := bakTmp.Close()
			if copyErr == nil && closeErr == nil {
				if renErr := os.Rename(bakTmpName, bakPath); renErr == nil {
					savedBak = bakPath
				} else {
					_ = os.Remove(bakTmpName)
				}
			} else {
				_ = os.Remove(bakTmpName)
			}
		}
		_ = src.Close()
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		return "", fmt.Errorf("原子替换新版二进制失败：%w", err)
	}
	return savedBak, nil
}

// RunSudoHelper invokes the root-owned zero-argument upgrade helper script after re-verifying ownership.
func (u *Upgrader) RunSudoHelper(ctx context.Context) error {
	helper := u.sudoHelper()
	if !checkSudoHelper(helper) {
		return fmt.Errorf("受控升级脚本 (%s) 未就绪或未配置免密 sudo 权限", helper)
	}
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "sudo", "-n", helper)
	cmd.Stdin = nil
	cmd.Env = sanitizedHelperEnv()
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(out.String())
		if msg != "" {
			if idx := strings.IndexByte(msg, '\n'); idx >= 0 {
				msg = msg[:idx]
			}
			return fmt.Errorf("受控升级脚本执行失败：%s", msg)
		}
		return fmt.Errorf("受控升级脚本执行失败：%w", err)
	}
	return nil
}

func sanitizedHelperEnv() []string {
	var clean []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy":
			clean = append(clean, kv)
		}
	}
	return clean
}
