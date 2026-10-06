package store

import (
	"context"
	"path/filepath"
	"testing"
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
