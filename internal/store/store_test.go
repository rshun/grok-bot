package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestChatRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "data"), "grok-4.7")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	chat, err := s.Chat(ctx, "telegram", "1")
	if err != nil {
		t.Fatal(err)
	}
	if chat.Model != "grok-4.7" || chat.SessionID != "" {
		t.Fatalf("%#v", chat)
	}
	if err := s.SetSession(ctx, "telegram", "1", "sess"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetModel(ctx, "telegram", "1", "grok-4.6"); err != nil {
		t.Fatal(err)
	}
	chat, err = s.Chat(ctx, "telegram", "1")
	if err != nil {
		t.Fatal(err)
	}
	if chat.SessionID != "sess" || chat.Model != "grok-4.6" {
		t.Fatalf("%#v", chat)
	}
	if err := s.ClearSession(ctx, "telegram", "1"); err != nil {
		t.Fatal(err)
	}
	chat, err = s.Chat(ctx, "telegram", "1")
	if err != nil {
		t.Fatal(err)
	}
	if chat.SessionID != "" || chat.Model != "grok-4.6" {
		t.Fatalf("%#v", chat)
	}
}

func TestMigrateAddsSandboxToExistingChats(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	old, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(`
		CREATE TABLE chats (
			platform TEXT NOT NULL,
			chat_id TEXT NOT NULL,
			session_id TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (platform, chat_id)
		);
		INSERT INTO chats VALUES ('telegram', '1', 'old-session', 'grok-4.6', '2026-01-01T00:00:00Z');
	`)
	old.Close()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		s, err := Open(dir, "grok-4.7")
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		chat, err := s.Chat(context.Background(), "telegram", "1")
		s.Close()
		if err != nil {
			t.Fatal(err)
		}
		if chat.SessionID != "old-session" || chat.Model != "grok-4.6" || chat.Sandbox != "" {
			t.Fatalf("open %d: %#v", i, chat)
		}
	}
}

func TestStartSessionKeepsSandboxUntilReset(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.StartSession(ctx, "telegram", "1", "s1", "grokbot"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSession(ctx, "telegram", "1", "s2"); err != nil {
		t.Fatal(err)
	}
	chat, err := s.Chat(ctx, "telegram", "1")
	if err != nil {
		t.Fatal(err)
	}
	if chat.SessionID != "s2" || chat.Sandbox != "grokbot" {
		t.Fatalf("%#v", chat)
	}
	if err := s.ClearSession(ctx, "telegram", "1"); err != nil {
		t.Fatal(err)
	}
	if chat, _ = s.Chat(ctx, "telegram", "1"); chat.Sandbox != "" || chat.SessionID != "" {
		t.Fatalf("%#v", chat)
	}
}

func TestDirsRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.PutDir(ctx, "/b", "ro"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutDir(ctx, "/a", "ro"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutDir(ctx, "/a", "rw"); err != nil {
		t.Fatal(err)
	}
	dirs, err := s.Dirs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []Dir{{Path: "/a", Mode: "rw"}, {Path: "/b", Mode: "ro"}}
	if !reflect.DeepEqual(dirs, want) {
		t.Fatalf("%#v", dirs)
	}
	if ok, err := s.DeleteDir(ctx, "/missing"); err != nil || ok {
		t.Fatalf("delete missing: %v %v", ok, err)
	}
	if ok, err := s.DeleteDir(ctx, "/a"); err != nil || !ok {
		t.Fatalf("delete /a: %v %v", ok, err)
	}
}

func TestTasksScopedAndDue(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	add := func(chat string, next time.Time) int64 {
		t.Helper()
		id, err := s.AddTask(ctx, Task{Platform: "telegram", ChatID: chat, UserID: "1", Spec: `{"kind":"daily"}`, Prompt: "p", Summary: "s", NextRun: next})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	a1 := add("A", now.Add(-time.Minute))
	add("A", now.Add(time.Hour))
	b1 := add("B", now)
	tasks, err := s.Tasks(ctx, "telegram", "A")
	if err != nil || len(tasks) != 2 || tasks[0].ID != a1 || !tasks[0].NextRun.Equal(now.Add(-time.Minute)) {
		t.Fatalf("%#v %v", tasks, err)
	}
	if ok, err := s.DeleteTask(ctx, "telegram", "A", b1); err != nil || ok {
		t.Fatalf("deleted another chat's task: %v %v", ok, err)
	}
	due, err := s.DueTasks(ctx, now)
	if err != nil || len(due) != 2 || due[0].ID != a1 || due[1].ID != b1 {
		t.Fatalf("%#v %v", due, err)
	}
	if err := s.SetNextRun(ctx, a1, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	due, _ = s.DueTasks(ctx, now)
	if len(due) != 1 || due[0].ID != b1 {
		t.Fatalf("%#v", due)
	}
}

func TestMemoriesSurviveClearSessionAndStayScoped(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.SetSession(ctx, "telegram", "1", "sess-1"); err != nil {
		t.Fatal(err)
	}
	id1, added, err := s.AddMemory(ctx, "telegram", "1", "每次应答前面都要加上xxx")
	if err != nil || !added || id1 != 1 {
		t.Fatalf("add 1: id=%d added=%v err=%v", id1, added, err)
	}
	// Duplicate content in same chat returns existing ID without duplicating.
	dupID, dupAdded, err := s.AddMemory(ctx, "telegram", "1", "每次应答前面都要加上xxx")
	if err != nil || dupAdded || dupID != id1 {
		t.Fatalf("dup: id=%d added=%v err=%v", dupID, dupAdded, err)
	}
	// Different chat is isolated.
	if _, _, err := s.AddMemory(ctx, "discord", "1", "discord专属"); err != nil {
		t.Fatal(err)
	}
	// /reset (ClearSession) must NOT clear memories.
	if err := s.ClearSession(ctx, "telegram", "1"); err != nil {
		t.Fatal(err)
	}
	list, err := s.Memories(ctx, "telegram", "1")
	if err != nil || len(list) != 1 || list[0].Content != "每次应答前面都要加上xxx" {
		t.Fatalf("after ClearSession: %#v %v", list, err)
	}
	// Cannot delete another chat's memory.
	if _, ok, err := s.DeleteMemory(ctx, "discord", "1", id1); err != nil || ok {
		t.Fatalf("cross-chat delete: %v %v", ok, err)
	}
	content, ok, err := s.DeleteMemory(ctx, "telegram", "1", id1)
	if err != nil || !ok || content != "每次应答前面都要加上xxx" {
		t.Fatalf("delete: %q %v %v", content, ok, err)
	}
	if _, _, err := s.AddMemory(ctx, "telegram", "1", "a"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddMemory(ctx, "telegram", "1", "b"); err != nil {
		t.Fatal(err)
	}
	n, err := s.ClearMemories(ctx, "telegram", "1")
	if err != nil || n != 2 {
		t.Fatalf("clear: %d %v", n, err)
	}
	if rem, _ := s.Memories(ctx, "discord", "1"); len(rem) != 1 {
		t.Fatalf("discord memory touched: %#v", rem)
	}
}

func TestSavedSessionsRoundTripAndSwitch(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	id1, err := s.SaveSessionHistory(ctx, "telegram", "1", "sess-a", "排查MCP问题", "grok-4.7", "grokbot", false)
	if err != nil || id1 == 0 {
		t.Fatalf("save 1: %d %v", id1, err)
	}
	// Updating without overwriteTitle preserves original title.
	id1Again, err := s.SaveSessionHistory(ctx, "telegram", "1", "sess-a", "后续提问", "grok-4.7", "grokbot", false)
	if err != nil || id1Again != id1 {
		t.Fatalf("save 1 again: %d %v", id1Again, err)
	}
	id2, err := s.SaveSessionHistory(ctx, "telegram", "1", "sess-b", "量化报表检查", "grok-4.6", "", false)
	if err != nil || id2 == 0 {
		t.Fatalf("save 2: %d %v", id2, err)
	}
	list, err := s.Sessions(ctx, "telegram", "1")
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %#v %v", list, err)
	}
	got1, ok, err := s.SessionByID(ctx, "telegram", "1", id1)
	if err != nil || !ok || got1.Title != "排查MCP问题" {
		t.Fatalf("by id: %#v %v %v", got1, ok, err)
	}
	if err := s.ActivateSession(ctx, "telegram", "1", got1); err != nil {
		t.Fatal(err)
	}
	chat, err := s.Chat(ctx, "telegram", "1")
	if err != nil || chat.SessionID != "sess-a" || chat.Sandbox != "grokbot" {
		t.Fatalf("activated chat: %#v %v", chat, err)
	}
	known, err := s.KnownChats(ctx, "telegram")
	if err != nil || len(known) != 1 || known[0] != "1" {
		t.Fatalf("known chats: %#v %v", known, err)
	}
	delSess, ok, err := s.DeleteSavedSession(ctx, "telegram", "1", id2)
	if err != nil || !ok || delSess != "sess-b" {
		t.Fatalf("delete saved: %q %v %v", delSess, ok, err)
	}
}

func TestGlobalMemoriesAndDeleteTaskByID(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	id1, added, err := s.AddGlobalMemory(ctx, "telegram", "1", "跨平台共享记忆")
	if err != nil || !added || id1 != 1 {
		t.Fatalf("AddGlobalMemory: id=%d added=%v err=%v", id1, added, err)
	}
	// Adding identical content from Discord deduplicates globally.
	dupID, dupAdded, err := s.AddGlobalMemory(ctx, "discord", "99", "跨平台共享记忆")
	if err != nil || dupAdded || dupID != id1 {
		t.Fatalf("dup global memory: id=%d added=%v err=%v", dupID, dupAdded, err)
	}
	all, err := s.AllMemories(ctx)
	if err != nil || len(all) != 1 || all[0].Content != "跨平台共享记忆" {
		t.Fatalf("AllMemories: %#v %v", all, err)
	}
	if content, ok, err := s.DeleteMemoryByID(ctx, id1); err != nil || !ok || content != "跨平台共享记忆" {
		t.Fatalf("DeleteMemoryByID: %q %v %v", content, ok, err)
	}

	tid, err := s.AddTask(ctx, Task{
		Platform: "telegram", ChatID: "1", UserID: "1", Spec: `{"kind":"daily"}`, Prompt: "p", Summary: "s", NextRun: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.DeleteTaskByID(ctx, tid); err != nil || !ok {
		t.Fatalf("DeleteTaskByID: %v %v", ok, err)
	}
}

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "data"), "grok-4.7")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
