package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Chat is the Grok session and model for one platform conversation.
type Chat struct {
	SessionID string
	Model     string
}

// Store persists chat state in a local SQLite file.
type Store struct {
	db           *sql.DB
	defaultModel string
}

func Open(dataDir, defaultModel string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.Join(dataDir, "bot.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, defaultModel: defaultModel}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS chats (
			platform TEXT NOT NULL,
			chat_id TEXT NOT NULL,
			session_id TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (platform, chat_id)
		)
	`)
	return err
}

func (s *Store) Chat(ctx context.Context, platform, chatID string) (Chat, error) {
	var chat Chat
	err := s.db.QueryRowContext(ctx,
		`SELECT session_id, model FROM chats WHERE platform = ? AND chat_id = ?`,
		platform, chatID,
	).Scan(&chat.SessionID, &chat.Model)
	if err == sql.ErrNoRows {
		return Chat{Model: s.defaultModel}, nil
	}
	if err != nil {
		return Chat{}, err
	}
	if chat.Model == "" {
		chat.Model = s.defaultModel
	}
	return chat, nil
}

func (s *Store) SetModel(ctx context.Context, platform, chatID, model string) error {
	chat, err := s.Chat(ctx, platform, chatID)
	if err != nil {
		return err
	}
	return s.upsert(ctx, platform, chatID, chat.SessionID, model)
}

func (s *Store) SetSession(ctx context.Context, platform, chatID, sessionID string) error {
	chat, err := s.Chat(ctx, platform, chatID)
	if err != nil {
		return err
	}
	return s.upsert(ctx, platform, chatID, sessionID, chat.Model)
}

func (s *Store) ClearSession(ctx context.Context, platform, chatID string) error {
	chat, err := s.Chat(ctx, platform, chatID)
	if err != nil {
		return err
	}
	return s.upsert(ctx, platform, chatID, "", chat.Model)
}

func (s *Store) upsert(ctx context.Context, platform, chatID, sessionID, model string) error {
	if model == "" {
		model = s.defaultModel
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO chats (platform, chat_id, session_id, model, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (platform, chat_id) DO UPDATE SET
			session_id = excluded.session_id,
			model = excluded.model,
			updated_at = excluded.updated_at
	`, platform, chatID, sessionID, model, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("save chat: %w", err)
	}
	return nil
}
