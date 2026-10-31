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
	say(svc, rec, "/dir add rw "+dir)
	got := rec.wait(t, 1)
	if !strings.Contains(got[0], "已添加") || !strings.Contains(got[0], real) {
		t.Fatalf("%#v", got)
	}
	say(svc, rec, "/dir")
	got = rec.wait(t, 2)
	if !strings.Contains(got[1], real) || !strings.Contains(got[1], "读取由系统权限控制") {
		t.Fatalf("%#v", got)
	}
	say(svc, rec, "/dir add rw "+dir)
	rec.wait(t, 3)
	say(svc, rec, "/dir")
	got = rec.wait(t, 4)
	if !strings.Contains(got[3], real) {
		t.Fatalf("%#v", got)
	}
	say(svc, rec, "/dir del "+real)
	rec.wait(t, 5)
	say(svc, rec, "/dir")
	got = rec.wait(t, 6)
	if !strings.Contains(got[4], "已移除") || strings.Contains(got[5], real) {
		t.Fatalf("%#v", got)
	}
	say(svc, rec, "/dir add rw relative")
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

func TestOldSessionGetsFileToolsWithoutReset(t *testing.T) {
	svc, engine, _ := sandboxService(t)
	ctx := context.Background()
	if err := svc.Store.SetSession(ctx, "telegram", "9", "old"); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	say(svc, rec, "hello")
	rec.wait(t, 1)
	engine.mu.Lock()
	call := engine.calls[0]
	engine.mu.Unlock()
	if call.NewSession || call.SessionID != "old" || call.Sandbox != "" || !call.FileTools {
		t.Fatalf("%#v", call)
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

const wantReminder = "提示：这个聊天的会话创建于开启文件功能之前，发送 /reset 后才能读写本机文件（会清空这个聊天的上下文）。"

func realDir(t *testing.T) (dir, real string) {
	t.Helper()
	dir = t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, real
}

func TestNaturalGrantWritesBeforeTurn(t *testing.T) {
	svc, engine, _ := sandboxService(t)
	dir, real := realDir(t)
	rec := &recorder{}
	say(svc, rec, "允许写入 "+dir+"，写个文件")
	got := rec.wait(t, 1)
	if got[0] != "已允许写入 "+real+"。\n\npong" {
		t.Fatalf("%#v", got)
	}
	engine.mu.Lock()
	call := engine.calls[0]
	engine.mu.Unlock()
	if len(call.WriteDirs) != 1 || call.WriteDirs[0] != real {
		t.Fatalf("grant not applied before the turn: %#v", call)
	}
	dirs, _ := svc.Store.Dirs(context.Background())
	if len(dirs) != 1 || dirs[0].Path != real || dirs[0].Mode != "rw" {
		t.Fatalf("%#v", dirs)
	}
}

func TestNaturalGrantRejectsGuarded(t *testing.T) {
	svc, _, manager := sandboxService(t)
	if err := os.MkdirAll(manager.Policy.DataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	say(svc, rec, "允许写入 "+manager.Policy.DataDir)
	got := rec.wait(t, 1)
	if !strings.HasPrefix(got[0], "没有允许写入 "+manager.Policy.DataDir+"：") || !strings.HasSuffix(got[0], "pong") {
		t.Fatalf("%#v", got)
	}
	if dirs, _ := svc.Store.Dirs(context.Background()); len(dirs) != 0 {
		t.Fatalf("%#v", dirs)
	}
}

func TestNaturalRevoke(t *testing.T) {
	svc, _, _ := sandboxService(t)
	dir, real := realDir(t)
	if err := svc.Store.PutDir(context.Background(), real, "rw"); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	say(svc, rec, "取消 "+dir+string(filepath.Separator)+" 的写入权限")
	got := rec.wait(t, 1)
	if !strings.HasPrefix(got[0], "已取消 ") || !strings.Contains(got[0], "的写入权限。") {
		t.Fatalf("%#v", got)
	}
	if dirs, _ := svc.Store.Dirs(context.Background()); len(dirs) != 0 {
		t.Fatalf("%#v", dirs)
	}
}

func TestOldSessionGrantWorksImmediately(t *testing.T) {
	svc, engine, _ := sandboxService(t)
	ctx := context.Background()
	if err := svc.Store.SetSession(ctx, "telegram", "9", "old"); err != nil {
		t.Fatal(err)
	}
	dir, real := realDir(t)
	rec := &recorder{}
	say(svc, rec, "允许写入 "+dir)
	got := rec.wait(t, 1)
	if got[0] != "已允许写入 "+real+"。\n\npong" {
		t.Fatalf("%#v", got)
	}
	engine.mu.Lock()
	call := engine.calls[0]
	engine.mu.Unlock()
	if !call.FileTools || len(call.WriteDirs) != 1 || call.WriteDirs[0] != real {
		t.Fatalf("%#v", call)
	}
	if chat, _ := svc.Store.Chat(ctx, "telegram", "9"); chat.SessionID != "old" {
		t.Fatalf("%#v", chat)
	}
}

func TestOldSessionPlainChatNoReminder(t *testing.T) {
	svc, _, _ := sandboxService(t)
	if err := svc.Store.SetSession(context.Background(), "telegram", "9", "old"); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	say(svc, rec, "你好")
	got := rec.wait(t, 1)
	if got[0] != "pong" {
		t.Fatalf("%#v", got)
	}
}

func TestDirAddReadOnlyNotNeeded(t *testing.T) {
	svc, _, _ := sandboxService(t)
	rec := &recorder{}
	say(svc, rec, "/dir add ro "+t.TempDir())
	got := rec.wait(t, 1)
	if got[0] != "读取由系统权限控制，不需要开放。" {
		t.Fatalf("%#v", got)
	}
	if dirs, _ := svc.Store.Dirs(context.Background()); len(dirs) != 0 {
		t.Fatalf("%#v", dirs)
	}
}

func noBwrapService(t *testing.T) (*Service, *fakeEngine) {
	t.Helper()
	svc, engine, manager := sandboxService(t)
	manager.DenyAvailable = func() bool { return false }
	return svc, engine
}

func TestNoBwrapStillEnablesFileTools(t *testing.T) {
	svc, engine := noBwrapService(t)
	rec := &recorder{}
	say(svc, rec, "看看 /srv")
	got := rec.wait(t, 1)
	if got[0] != "pong" {
		t.Fatalf("%#v", got)
	}
	engine.mu.Lock()
	call := engine.calls[0]
	engine.mu.Unlock()
	if call.Sandbox != "" || !call.FileTools {
		t.Fatalf("%#v", call)
	}
	if chat, _ := svc.Store.Chat(context.Background(), "telegram", "9"); chat.Sandbox != "" {
		t.Fatalf("%#v", chat)
	}
}

func TestNoBwrapResumedSandboxSessionKeepsFileTools(t *testing.T) {
	svc, engine := noBwrapService(t)
	ctx := context.Background()
	if err := svc.Store.StartSession(ctx, "telegram", "9", "s1", sandbox.ProfileName); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	say(svc, rec, "继续")
	rec.wait(t, 1)
	engine.mu.Lock()
	call := engine.calls[0]
	engine.mu.Unlock()
	if call.NewSession || call.SessionID != "s1" || call.Sandbox != "" || !call.FileTools {
		t.Fatalf("%#v", call)
	}
}

func TestNoBwrapGrantSucceeds(t *testing.T) {
	svc, _ := noBwrapService(t)
	dir, real := realDir(t)
	rec := &recorder{}
	say(svc, rec, "允许写入 "+dir)
	got := rec.wait(t, 1)
	if !strings.Contains(got[0], "已允许写入 "+real+"。") || strings.Contains(got[0], "不可用") {
		t.Fatalf("%#v", got)
	}
	if dirs, _ := svc.Store.Dirs(context.Background()); len(dirs) != 1 {
		t.Fatalf("%#v", dirs)
	}
}

func TestEmptyReplyWithGrantStillReportsFailure(t *testing.T) {
	svc, engine, _ := sandboxService(t)
	engine.replyFor = func(opts grok.ReplyOptions) (grok.HeadlessResult, error) {
		return grok.HeadlessResult{SessionID: opts.SessionID}, nil
	}
	dir, real := realDir(t)
	rec := &recorder{}
	say(svc, rec, "允许写入 "+dir+"，写个文件")
	got := rec.wait(t, 1)
	want := "已允许写入 " + real + "。\n\n处理失败：没有得到回复。"
	if got[0] != want {
		t.Fatalf("got %q, want %q", got[0], want)
	}
}
