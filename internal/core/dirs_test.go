package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/platform"
	"github.com/rshun/grok-bot/internal/sandbox"
)

func sandboxService(t *testing.T) (*Service, *fakeEngine, *sandbox.Manager) {
	t.Helper()
	svc, engine := testService(t)
	root := t.TempDir()
	manager := &sandbox.Manager{
		Path:    filepath.Join(root, "grok-home", "sandbox.toml"),
		Enabled: true,
		Policy: sandbox.Policy{
			Home:     filepath.Join(root, "home"),
			DataDir:  filepath.Join(root, "data"),
			EnvFile:  filepath.Join(root, "etc", "bot.env"),
			AuthPath: filepath.Join(root, "home", ".grok", "auth.json"),
		},
	}
	svc.Sandbox = manager
	return svc, engine, manager
}

func say(svc *Service, rec *recorder, text string) {
	svc.Handle(context.Background(), platform.Inbound{
		Platform: "telegram", ChatID: "9", UserID: "1", Text: text, TextMessage: true,
	}, capture{rec})
}

func TestDirAddListDel(t *testing.T) {
	svc, _, _ := sandboxService(t)
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	say(svc, rec, "/dir add ro "+dir)
	got := rec.wait(t, 1)
	if !strings.Contains(got[0], "已添加") || !strings.Contains(got[0], real) {
		t.Fatalf("%#v", got)
	}
	say(svc, rec, "/dir")
	got = rec.wait(t, 2)
	if !strings.Contains(got[1], real) || !strings.Contains(got[1], "只读") {
		t.Fatalf("%#v", got)
	}
	say(svc, rec, "/dir add rw "+dir)
	rec.wait(t, 3)
	say(svc, rec, "/dir")
	got = rec.wait(t, 4)
	if !strings.Contains(got[3], "可读写") {
		t.Fatalf("%#v", got)
	}
	say(svc, rec, "/dir del "+real)
	rec.wait(t, 5)
	say(svc, rec, "/dir")
	got = rec.wait(t, 6)
	if !strings.Contains(got[4], "已移除") || strings.Contains(got[5], real) {
		t.Fatalf("%#v", got)
	}
	say(svc, rec, "/dir add ro relative")
	got = rec.wait(t, 7)
	if !strings.Contains(got[6], "绝对路径") {
		t.Fatalf("%#v", got)
	}
}

func TestNewSessionUsesSandbox(t *testing.T) {
	svc, engine, manager := sandboxService(t)
	rec := &recorder{}
	say(svc, rec, "hello")
	rec.wait(t, 1)
	engine.mu.Lock()
	call := engine.calls[0]
	engine.mu.Unlock()
	if !call.NewSession || call.Sandbox != sandbox.ProfileName || !call.FileTools {
		t.Fatalf("%#v", call)
	}
	chat, err := svc.Store.Chat(context.Background(), "telegram", "9")
	if err != nil || chat.Sandbox != sandbox.ProfileName {
		t.Fatalf("%#v %v", chat, err)
	}
	if _, err := os.Stat(manager.Path); err != nil {
		t.Fatalf("profile not written: %v", err)
	}
}

func TestOldSessionKeepsContextWithoutFiles(t *testing.T) {
	svc, engine, _ := sandboxService(t)
	ctx := context.Background()
	if err := svc.Store.SetSession(ctx, "telegram", "9", "old"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.PutDir(ctx, t.TempDir(), "ro"); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	say(svc, rec, "hello")
	rec.wait(t, 1)
	engine.mu.Lock()
	call := engine.calls[0]
	engine.mu.Unlock()
	if call.NewSession || call.SessionID != "old" || call.Sandbox != "" || call.FileTools {
		t.Fatalf("%#v", call)
	}
	say(svc, rec, "/dir")
	got := rec.wait(t, 2)
	if !strings.Contains(got[1], "/reset") {
		t.Fatalf("%#v", got)
	}
}

func TestMissingOldSessionRebuildsWithSandbox(t *testing.T) {
	svc, engine, _ := sandboxService(t)
	ctx := context.Background()
	if err := svc.Store.SetSession(ctx, "telegram", "9", "old"); err != nil {
		t.Fatal(err)
	}
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		if !opts.NewSession {
			return grok.HeadlessResult{}, errors.New("Session old not found")
		}
		return grok.HeadlessResult{Text: "pong", SessionID: opts.SessionID}, nil
	}
	rec := &recorder{}
	say(svc, rec, "hello")
	rec.wait(t, 1)
	engine.mu.Lock()
	calls := append([]grok.ReplyOptions(nil), engine.calls...)
	engine.mu.Unlock()
	if len(calls) != 2 || !calls[1].NewSession || calls[1].Sandbox != sandbox.ProfileName || !calls[1].FileTools {
		t.Fatalf("%#v", calls)
	}
	chat, err := svc.Store.Chat(ctx, "telegram", "9")
	if err != nil || chat.Sandbox != sandbox.ProfileName || chat.SessionID != calls[1].SessionID {
		t.Fatalf("%#v %v", chat, err)
	}
}

func TestUnmanagedProfileDisablesFiles(t *testing.T) {
	svc, engine, manager := sandboxService(t)
	if err := os.MkdirAll(filepath.Dir(manager.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.Path, []byte("[profiles.mine]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	say(svc, rec, "hello")
	rec.wait(t, 1)
	engine.mu.Lock()
	call := engine.calls[0]
	engine.mu.Unlock()
	if call.Sandbox != "" || call.FileTools {
		t.Fatalf("%#v", call)
	}
	chat, _ := svc.Store.Chat(context.Background(), "telegram", "9")
	if chat.Sandbox != "" {
		t.Fatalf("%#v", chat)
	}
	say(svc, rec, "/dir")
	got := rec.wait(t, 2)
	if !strings.Contains(got[1], "手动合并") {
		t.Fatalf("%#v", got)
	}
}
