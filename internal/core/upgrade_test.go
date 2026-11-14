package core

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/platform"
	"github.com/rshun/grok-bot/internal/store"
	"github.com/rshun/grok-bot/internal/upgrade"
)

type fakeCLIUpgraderEngine struct {
	memoryTestEngine
	cliVer      string
	nextCLIVer  string
	targetPath  string
	preflightEr error
	models      []string
	updateCalls int
}

func (f *fakeCLIUpgraderEngine) Version(context.Context) (string, error) {
	if f.cliVer == "" {
		return "grok 0.4.1", nil
	}
	return f.cliVer, nil
}

func (f *fakeCLIUpgraderEngine) CheckUpdatePreflight() (string, error) {
	if f.preflightEr != nil {
		return f.targetPath, f.preflightEr
	}
	if f.targetPath == "" {
		return "/home/grok/.local/bin/grok", nil
	}
	return f.targetPath, nil
}

func (f *fakeCLIUpgraderEngine) UpdateCLI(context.Context) (grok.CLIUpdateResult, error) {
	f.updateCalls++
	before := f.cliVer
	if before == "" {
		before = "grok 0.4.1"
	}
	after := f.nextCLIVer
	if after == "" {
		after = before
	}
	f.cliVer = after
	return grok.CLIUpdateResult{
		TargetPath:    "/home/grok/.local/bin/grok",
		BeforeVersion: before,
		AfterVersion:  after,
		Updated:       before != after,
	}, nil
}

func (f *fakeCLIUpgraderEngine) Models(context.Context) (grok.ModelList, error) {
	if len(f.models) > 0 {
		return grok.ModelList{Names: f.models}, nil
	}
	return grok.ModelList{Names: []string{"grok-4.7"}}, nil
}

func TestUpgradeAdminAuthorization(t *testing.T) {
	eng := &fakeCLIUpgraderEngine{cliVer: "grok 0.4.1"}
	svc, _ := newMemoryService(t, eng)
	svc.Version = "v0.9.1"
	binPath := filepath.Join(t.TempDir(), "grok-bot")
	_ = os.WriteFile(binPath, []byte("old"), 0o755)
	svc.Upgrader = &upgrade.Upgrader{
		CurrentVersion: "dev",
		Paths:          upgrade.ExePaths{LaunchPath: binPath, TargetPath: binPath},
		GOOS:           "linux",
		GOARCH:         "amd64",
	}

	// 1. Single-owner dual-platform (1 TG user + 1 Discord user, no ADMIN_USER_IDS): both allowed!
	svc.AllowedUsers = map[string]map[string]struct{}{
		platform.Telegram: {"tg-owner": {}},
		platform.Discord:  {"dc-owner": {}},
	}
	if got := sendSync(t, svc, platform.Inbound{Platform: platform.Telegram, ChatID: "c1", UserID: "tg-owner", Text: "/upgrade cli"}); !strings.Contains(got, "Grok CLI 引擎") {
		t.Fatalf("single-owner TG denied: %s", got)
	}
	if got := sendSync(t, svc, platform.Inbound{Platform: platform.Discord, ChatID: "dc1", UserID: "dc-owner", Text: "/upgrade cli"}); !strings.Contains(got, "Grok CLI 引擎") {
		t.Fatalf("single-owner Discord denied: %s", got)
	}

	// 2. Multi-user shared bot without ADMIN_USER_IDS: refuses until admin is configured.
	svc.AllowedUsers[platform.Telegram] = map[string]struct{}{"u1": {}, "u2": {}}
	gotShared := sendSync(t, svc, platform.Inbound{Platform: platform.Telegram, ChatID: "c1", UserID: "u1", Text: "/upgrade"})
	if !strings.Contains(gotShared, "ADMIN_USER_IDS") {
		t.Fatalf("expected ADMIN_USER_IDS prompt for multi-user setup, got: %s", gotShared)
	}

	// 3. Multi-user shared bot with explicit ADMIN_USER_IDS={"u1"}: u2 denied, u1 allowed.
	svc.AdminUsers = map[string]map[string]struct{}{
		platform.Telegram: {"u1": {}},
	}
	gotDenied := sendSync(t, svc, platform.Inbound{Platform: platform.Telegram, ChatID: "c2", UserID: "u2", Text: "/upgrade"})
	if !strings.Contains(gotDenied, "仅管理员可执行版本升级") {
		t.Fatalf("non-admin u2 was not denied: %s", gotDenied)
	}
	gotAdmin := sendSync(t, svc, platform.Inbound{Platform: platform.Telegram, ChatID: "c1", UserID: "u1", Text: "/upgrade cli"})
	if !strings.Contains(gotAdmin, "Grok CLI 引擎") {
		t.Fatalf("admin u1 failed: %s", gotAdmin)
	}
}

func TestParseNaturalUpgradeIntentAndSafety(t *testing.T) {
	eng := &fakeCLIUpgraderEngine{cliVer: "grok 0.4.1", nextCLIVer: "grok 0.4.3"}
	svc, _ := newMemoryService(t, eng)
	svc.Version = "dev"

	// Natural language triggers ONLY open the check panel with buttons and NEVER execute UpdateCLI.
	var out memCapture
	text, _, _, btns, err := svc.answerWithButtons(context.Background(), platform.Inbound{
		Platform: platform.Telegram, ChatID: "c1", UserID: "u1", Text: "升级一下", TextMessage: true,
	}, &out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "版本与升级检查") || len(btns) == 0 || eng.updateCalls != 0 {
		t.Fatalf("expected check panel without auto-execution, text=%q btns=%d calls=%d", text, len(btns), eng.updateCalls)
	}

	if focus, ok := parseNaturalUpgradeIntent("升级机器人"); !ok || focus != "bot" {
		t.Fatalf("升级机器人 => (%q, %v)", focus, ok)
	}
	if focus, ok := parseNaturalUpgradeIntent("更新 Grok CLI！"); !ok || focus != "cli" {
		t.Fatalf("更新 Grok CLI => (%q, %v)", focus, ok)
	}
	if focus, ok := parseNaturalUpgradeIntent("check for updates"); !ok || focus != "" {
		t.Fatalf("check for updates => (%q, %v)", focus, ok)
	}
	if focus, ok := parseNaturalUpgradeIntent("upgrade bot"); !ok || focus != "bot" {
		t.Fatalf("upgrade bot => (%q, %v)", focus, ok)
	}
	if focus, ok := parseNaturalUpgradeIntent("update grok cli"); !ok || focus != "cli" {
		t.Fatalf("update grok cli => (%q, %v)", focus, ok)
	}

	// Technical questions must NOT trigger self-upgrade panel.
	for _, question := range []string{
		"Ubuntu 怎么升级内核？",
		"帮我写一个自动升级脚本",
		"Python pip 怎么更新？",
		"how to update ubuntu",
		"https://example.com 升级一下",
	} {
		if _, ok := parseNaturalUpgradeIntent(question); ok {
			t.Fatalf("technical question %q falsely matched natural upgrade intent", question)
		}
	}
}

func TestUpgradeCLICommandAndDeprecatedModelAlert(t *testing.T) {
	eng := &fakeCLIUpgraderEngine{
		cliVer:     "grok 0.4.1",
		nextCLIVer: "grok 0.4.5",
		models:     []string{"grok-4.8", "grok-4.8-mini"},
	}
	svc, _ := newMemoryService(t, eng)

	var out memCapture
	reply, _, _, btns, err := svc.answerWithButtons(context.Background(), platform.Inbound{
		Platform: platform.Telegram, ChatID: "c1", UserID: "u1", Text: "/upgrade confirm cli", TextMessage: true,
	}, &out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "0.4.1") || !strings.Contains(reply, "0.4.5") {
		t.Fatalf("missing version diff in reply: %s", reply)
	}
	if !strings.Contains(reply, "grok-4.7") || !strings.Contains(reply, "下线或更名") || len(btns) == 0 {
		t.Fatalf("expected deprecated model alert & buttons, reply=%s btns=%d", reply, len(btns))
	}
}

func TestUpgradeBotEndToEndAndPostRestartNotice(t *testing.T) {
	payload := append([]byte{0x7f, 'E', 'L', 'F'}, []byte("-grok-bot-v0.9.2-binary")...)
	sum := sha256.Sum256(payload)
	sumHex := hex.EncodeToString(sum[:])

	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/rshun/grok-bot/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name":   "v0.9.2",
			"html_url":   srv.URL + "/releases/tag/v0.9.2",
			"draft":      false,
			"prerelease": false,
			"assets": []map[string]string{
				{"name": "grok-bot-linux-amd64", "browser_download_url": srv.URL + "/dl/grok-bot-linux-amd64"},
				{"name": "SHA256SUMS", "browser_download_url": srv.URL + "/dl/SHA256SUMS"},
			},
		})
	})
	mux.HandleFunc("/dl/grok-bot-linux-amd64", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	})
	mux.HandleFunc("/dl/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "%s  grok-bot-linux-amd64\n", sumHex)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	dataDir := filepath.Join(t.TempDir(), "data")
	st, err := store.Open(dataDir, "grok-4.7")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "grok-tg-bot")
	if err := os.WriteFile(binPath, []byte("old-v0.9.1"), 0o755); err != nil {
		t.Fatal(err)
	}

	eng := &fakeCLIUpgraderEngine{cliVer: "grok 0.4.1", nextCLIVer: "grok 0.4.2"}
	out := &syncOut{}
	var restartCalledAfterSend atomic.Bool
	restartCh := make(chan struct{}, 1)

	svc := &Service{
		Store:        st,
		Engine:       eng,
		AllowedUsers: map[string]map[string]struct{}{platform.Telegram: {"u1": {}}},
		Senders:      map[string]platform.Replier{platform.Telegram: out},
		Version:      "v0.9.1",
		BackupDir:    filepath.Join(dataDir, "backups"),
		Upgrader: &upgrade.Upgrader{
			CurrentVersion:    "v0.9.1",
			Paths:             upgrade.ExePaths{LaunchPath: binPath, TargetPath: binPath},
			DataDir:           dataDir,
			GOOS:              "linux",
			GOARCH:            "amd64",
			APIBase:           srv.URL,
			HTTP:              srv.Client(),
			AllowInsecureHTTP: true,
		},
		OnUpgradeRestart: func() {
			if len(out.snapshot()) > 0 {
				restartCalledAfterSend.Store(true)
			}
			select {
			case restartCh <- struct{}{}:
			default:
			}
		},
	}

	// 1. Check `/upgrade` panel offers both individual and combined buttons.
	panelText, _, _, panelBtns, err := svc.answerWithButtons(context.Background(), platform.Inbound{
		Platform: platform.Telegram, ChatID: "c1", UserID: "u1", Text: "/upgrade", TextMessage: true,
	}, out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(panelText, "v0.9.1") || !strings.Contains(panelText, "v0.9.2") || len(panelBtns) < 2 {
		t.Fatalf("unexpected panel: %s (btns=%d)", panelText, len(panelBtns))
	}

	// 2. Execute combined upgrade through full Handle -> dispatch pipeline.
	svc.Handle(context.Background(), platform.Inbound{
		Platform: platform.Telegram, ChatID: "c1", UserID: "u1", Text: "/upgrade confirm all v0.9.2", TextMessage: true,
	}, out)

	select {
	case <-restartCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for OnUpgradeRestart")
	}
	if !restartCalledAfterSend.Load() {
		t.Fatal("OnUpgradeRestart fired BEFORE upgrade confirmation reply was sent")
	}

	gotBinary, _ := os.ReadFile(binPath)
	if string(gotBinary) != string(payload) {
		t.Fatalf("binary was not replaced: %q", string(gotBinary))
	}
	backups, _ := os.ReadDir(filepath.Join(dataDir, "backups"))
	if len(backups) == 0 {
		t.Fatal("expected SQLite backup in BackupDir before upgrade")
	}

	// While upgrading=true, concurrent incoming messages receive maintenance notice.
	svc.Handle(context.Background(), platform.Inbound{
		Platform: platform.Telegram, ChatID: "c1", UserID: "u1", Text: "还在吗", TextMessage: true,
	}, out)
	msgs := out.snapshot()
	if !strings.Contains(msgs[len(msgs)-1], "升级与重启") {
		t.Fatalf("expected maintenance message while upgrading, got: %v", msgs)
	}

	// 3. Simulate newly started v0.9.2 process delivering the pending upgrade receipt.
	restartedOut := &syncOut{}
	svcNew := &Service{
		Store:   st,
		Senders: map[string]platform.Replier{platform.Telegram: restartedOut},
		Version: "v0.9.2",
	}
	svcNew.DeliverPendingUpgradeNotice(context.Background())

	receipts := restartedOut.snapshot()
	if len(receipts) != 1 || !strings.Contains(receipts[0], "✅ grok-bot 升级完成") || !strings.Contains(receipts[0], "v0.9.2") || !strings.Contains(receipts[0], "0.4.2") {
		t.Fatalf("unexpected post-restart receipt: %v", receipts)
	}
	// Second call is a no-op because the notice setting was cleared.
	svcNew.DeliverPendingUpgradeNotice(context.Background())
	if len(restartedOut.snapshot()) != 1 {
		t.Fatal("duplicate post-restart receipt sent")
	}
}

type blockingCLIUpgraderEngine struct {
	blockingEngine
	updateCalls int
}

func (b *blockingCLIUpgraderEngine) Version(context.Context) (string, error) {
	return "grok 0.4.1", nil
}

func (b *blockingCLIUpgraderEngine) CheckUpdatePreflight() (string, error) {
	return "/home/grok/.local/bin/grok", nil
}

func (b *blockingCLIUpgraderEngine) UpdateCLI(context.Context) (grok.CLIUpdateResult, error) {
	b.updateCalls++
	return grok.CLIUpdateResult{Updated: true}, nil
}

func TestUpgradeDefersWhenAnotherUserTurnActive(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data"), "grok-4.7")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	blockEng := &blockingCLIUpgraderEngine{
		blockingEngine: blockingEngine{started: make(chan struct{}, 1)},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc := &Service{
		Store:        st,
		Engine:       blockEng,
		AllowedUsers: map[string]map[string]struct{}{platform.Telegram: {"admin": {}, "member": {}}},
		AdminUsers:   map[string]map[string]struct{}{platform.Telegram: {"admin": {}}},
		DrainTimeout: 120 * time.Millisecond,
	}
	memberOut := &syncOut{}
	svc.Handle(ctx, platform.Inbound{
		Platform: platform.Telegram, ChatID: "chat-member", UserID: "member", Text: "正在长对话", TextMessage: true,
	}, memberOut)
	<-blockEng.started

	var adminOut memCapture
	reply, _, _, _, err := svc.answerWithButtons(context.Background(), platform.Inbound{
		Platform: platform.Telegram, ChatID: "chat-admin", UserID: "admin", Text: "/upgrade confirm cli", TextMessage: true,
	}, &adminOut, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "暂缓升级") || blockEng.updateCalls != 0 {
		t.Fatalf("expected safe deferral while member chat is active, got reply=%q calls=%d", reply, blockEng.updateCalls)
	}
	if svc.upgrading.Load() {
		t.Fatal("upgrading flag remained stuck after deferral")
	}
}
