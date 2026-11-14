package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
		ok   bool
	}{
		{"v0.9.1", "v0.9.2", -1, true},
		{"0.9.2", "v0.9.2", 0, true},
		{"v0.10.0", "v0.9.9", 1, true},
		{"v1.0.0-rc1", "v1.0.0", -1, true},
		{"v1.0.0", "v1.0.0-beta1", 1, true},
		{"dev", "v0.9.2", 0, false},
		{"v0.9.1", "invalid", 0, false},
	}
	for _, tc := range cases {
		got, ok := CompareVersions(tc.a, tc.b)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("CompareVersions(%q, %q) = (%d, %v), want (%d, %v)", tc.a, tc.b, got, ok, tc.want, tc.ok)
		}
	}
}

func TestAssetNameFor(t *testing.T) {
	if got, err := AssetNameFor("linux", "amd64"); err != nil || got != "grok-bot-linux-amd64" {
		t.Fatalf("linux/amd64: %q %v", got, err)
	}
	if got, err := AssetNameFor("windows", "amd64"); err != nil || got != "grok-bot-windows-amd64.exe" {
		t.Fatalf("windows/amd64: %q %v", got, err)
	}
	if got, err := AssetNameFor("darwin", "arm64"); err != nil || got != "grok-bot-darwin-arm64" {
		t.Fatalf("darwin/arm64: %q %v", got, err)
	}
	if _, err := AssetNameFor("freebsd", "386"); err == nil {
		t.Fatal("expected error for unsupported OS/arch")
	}
}

func TestParseSHA256Sums(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	content := "# checksums\n" +
		strings.Repeat("11", 32) + "  grok-bot-darwin-arm64\n" +
		strings.ToUpper(digest) + " *grok-bot-linux-amd64\n"

	got, err := ParseSHA256Sums(content, "grok-bot-linux-amd64")
	if err != nil || got != digest {
		t.Fatalf("got %q err=%v, want %q", got, err, digest)
	}
	if _, err := ParseSHA256Sums(content, "grok-bot-windows-amd64.exe"); err == nil {
		t.Fatal("expected missing asset error")
	}
}

func TestCheckLatestAndDownloadApplyUnixAndWindows(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			assetName, err := AssetNameFor(goos, "amd64")
			if err != nil {
				t.Fatal(err)
			}
			var payload []byte
			if goos == "windows" {
				payload = []byte("MZ-fake-pe-new-version-v0.9.2")
			} else {
				payload = append([]byte{0x7f, 'E', 'L', 'F'}, []byte("-fake-elf-new-version-v0.9.2")...)
			}
			sum := sha256.Sum256(payload)
			sumHex := hex.EncodeToString(sum[:])
			sumsBody := fmt.Sprintf("%s  %s\n", sumHex, assetName)

			var srv *httptest.Server
			mux := http.NewServeMux()
			mux.HandleFunc("/repos/rshun/grok-bot/releases/latest", func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"tag_name":   "v0.9.2",
					"html_url":   srv.URL + "/releases/tag/v0.9.2",
					"draft":      false,
					"prerelease": false,
					"assets": []map[string]string{
						{"name": assetName, "browser_download_url": srv.URL + "/dl/" + assetName},
						{"name": "SHA256SUMS", "browser_download_url": srv.URL + "/dl/SHA256SUMS"},
					},
				})
			})
			mux.HandleFunc("/dl/"+assetName, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(payload)
			})
			mux.HandleFunc("/dl/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(sumsBody))
			})
			srv = httptest.NewServer(mux)
			defer srv.Close()

			dir := t.TempDir()
			binName := "grok-tg-bot"
			if goos == "windows" {
				binName = "grok-bot.exe"
			}
			targetPath := filepath.Join(dir, binName)
			oldBytes := []byte("old-binary-v0.9.1")
			if err := os.WriteFile(targetPath, oldBytes, 0o755); err != nil {
				t.Fatal(err)
			}

			// Symlink entry path pointing to targetPath (when supported on this host OS).
			launchPath := filepath.Join(t.TempDir(), "entry-"+binName)
			if err := os.Symlink(targetPath, launchPath); err != nil {
				launchPath = targetPath
			}

			upg := &Upgrader{
				CurrentVersion:    "v0.9.1",
				Paths:             ExePaths{LaunchPath: launchPath, TargetPath: targetPath},
				DataDir:           filepath.Join(t.TempDir(), "data"),
				GOOS:              goos,
				GOARCH:            "amd64",
				APIBase:           srv.URL,
				HTTP:              srv.Client(),
				AllowInsecureHTTP: true,
			}

			pf := upg.CheckPreflight()
			if pf.Mode != ModeInPlace {
				t.Fatalf("expected ModeInPlace, got %#v", pf)
			}

			rel, err := upg.CheckLatest(context.Background())
			if err != nil {
				t.Fatalf("CheckLatest: %v", err)
			}
			if rel.TagName != "v0.9.2" || rel.AssetName != assetName {
				t.Fatalf("unexpected release info: %#v", rel)
			}

			bakPath, err := upg.DownloadAndApply(context.Background(), rel)
			if err != nil {
				t.Fatalf("DownloadAndApply: %v", err)
			}
			if bakPath != targetPath+".bak" {
				t.Fatalf("bakPath = %q, want %q", bakPath, targetPath+".bak")
			}

			gotNew, err := os.ReadFile(targetPath)
			if err != nil || string(gotNew) != string(payload) {
				t.Fatalf("target binary mismatch: %q err=%v", string(gotNew), err)
			}
			gotBak, err := os.ReadFile(bakPath)
			if err != nil || string(gotBak) != string(oldBytes) {
				t.Fatalf("backup binary mismatch: %q err=%v", string(gotBak), err)
			}
			if launchPath != targetPath {
				gotViaLink, err := os.ReadFile(launchPath)
				if err != nil || string(gotViaLink) != string(payload) {
					t.Fatalf("symlink broken after atomic swap: %v", err)
				}
			}
		})
	}
}

func TestDownloadRejectsTamperedHashAndUntrustedHost(t *testing.T) {
	payload := append([]byte{0x7f, 'E', 'L', 'F'}, []byte("-tampered-binary")...)
	wrongHex := strings.Repeat("00", 32)

	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/dl/grok-bot-linux-amd64", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	})
	mux.HandleFunc("/dl/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "%s  grok-bot-linux-amd64\n", wrongHex)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	targetPath := filepath.Join(dir, "grok-bot")
	if err := os.WriteFile(targetPath, []byte("original-safe-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	upg := &Upgrader{
		CurrentVersion:    "v0.9.1",
		Paths:             ExePaths{LaunchPath: targetPath, TargetPath: targetPath},
		GOOS:              "linux",
		GOARCH:            "amd64",
		HTTP:              srv.Client(),
		AllowInsecureHTTP: true,
	}
	rel := ReleaseInfo{
		TagName:   "v0.9.2",
		AssetName: "grok-bot-linux-amd64",
		AssetURL:  srv.URL + "/dl/grok-bot-linux-amd64",
		SumsURL:   srv.URL + "/dl/SHA256SUMS",
	}
	if _, err := upg.DownloadAndApply(context.Background(), rel); err == nil || !strings.Contains(err.Error(), "SHA256") {
		t.Fatalf("expected SHA256 mismatch error, got %v", err)
	}
	stillOrig, _ := os.ReadFile(targetPath)
	if string(stillOrig) != "original-safe-binary" {
		t.Fatalf("original binary was modified despite hash mismatch: %q", string(stillOrig))
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("temp file leaked after failed upgrade: %s", e.Name())
		}
	}

	// Production mode (AllowInsecureHTTP=false) rejects plain HTTP and untrusted domains.
	prodUpg := &Upgrader{
		CurrentVersion: "v0.9.1",
		Paths:          ExePaths{LaunchPath: targetPath, TargetPath: targetPath},
		GOOS:           "linux",
		GOARCH:         "amd64",
	}
	if err := prodUpg.validateDownloadURL("http://github.com/rshun/grok-bot/releases/download/v0.9.2/grok-bot"); err == nil {
		t.Fatal("accepted non-HTTPS URL in production mode")
	}
	if err := prodUpg.validateDownloadURL("https://evil.example.com/grok-bot"); err == nil {
		t.Fatal("accepted untrusted domain in production mode")
	}
}

func TestPreflightDevBuildRejected(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "grok-bot")
	_ = os.WriteFile(target, []byte("x"), 0o755)

	upg := &Upgrader{
		CurrentVersion: "dev",
		Paths:          ExePaths{LaunchPath: target, TargetPath: target},
		GOOS:           "linux",
		GOARCH:         "amd64",
	}
	pf := upg.CheckPreflight()
	if pf.Mode != ModeUnavailable || !strings.Contains(pf.Reason, "dev") {
		t.Fatalf("expected dev build rejection, got %#v", pf)
	}
}
