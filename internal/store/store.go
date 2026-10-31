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
	// Sandbox is the grok sandbox profile the session was created with; empty for unsandboxed sessions.
	Sandbox string
}

// Dir is a directory Grok may read ("ro") or read and write ("rw").
type Dir struct {
	Path string
	Mode string
}

// Task is a scheduled prompt for one chat.
type Task struct {
	ID       int64
	Platform string
	ChatID   string
	UserID   string
	Spec     string
	Prompt   string
	Summary  string
	NextRun  time.Time
	Paused   bool
}

// runLayout is fixed-width UTC so next_run compares correctly as text.
const runLayout = "2006-01-02T15:04:05Z"

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
	if err != nil {
		return err
	}
	has, err := s.hasColumn("chats", "sandbox")
	if err != nil {
		return err
	}
	if !has {
		// Existing sessions were created without a sandbox and keep that.
		if _, err := s.db.Exec(`ALTER TABLE chats ADD COLUMN sandbox TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	_, err = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS dirs (
			path TEXT PRIMARY KEY,
			mode TEXT NOT NULL CHECK (mode IN ('ro', 'rw')),
			created_at TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			platform TEXT NOT NULL,
			chat_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			spec TEXT NOT NULL,
			prompt TEXT NOT NULL,
			summary TEXT NOT NULL,
			next_run TEXT NOT NULL,
			created_at TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS tasks_next_run ON tasks (next_run);
	`)
	if err != nil {
		return err
	}
	hasTaskPaused, err := s.hasColumn("tasks", "paused")
	if err != nil {
		return err
	}
	if !hasTaskPaused {
		if _, err := s.db.Exec(`ALTER TABLE tasks ADD COLUMN paused INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) hasColumn(table, column string) (bool, error) {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) Chat(ctx context.Context, platform, chatID string) (Chat, error) {
	var chat Chat
	err := s.db.QueryRowContext(ctx,
		`SELECT session_id, model, sandbox FROM chats WHERE platform = ? AND chat_id = ?`,
		platform, chatID,
	).Scan(&chat.SessionID, &chat.Model, &chat.Sandbox)
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
	return s.upsert(ctx, platform, chatID, chat.SessionID, model, chat.Sandbox)
}

// SetSession updates the session ID and keeps the sandbox profile it was created with.
func (s *Store) SetSession(ctx context.Context, platform, chatID, sessionID string) error {
	chat, err := s.Chat(ctx, platform, chatID)
	if err != nil {
		return err
	}
	return s.upsert(ctx, platform, chatID, sessionID, chat.Model, chat.Sandbox)
}

// StartSession records a new Grok session and the sandbox profile it was created with.
func (s *Store) StartSession(ctx context.Context, platform, chatID, sessionID, sandbox string) error {
	chat, err := s.Chat(ctx, platform, chatID)
	if err != nil {
		return err
	}
	return s.upsert(ctx, platform, chatID, sessionID, chat.Model, sandbox)
}

func (s *Store) ClearSession(ctx context.Context, platform, chatID string) error {
	chat, err := s.Chat(ctx, platform, chatID)
	if err != nil {
		return err
	}
	return s.upsert(ctx, platform, chatID, "", chat.Model, "")
}

func (s *Store) upsert(ctx context.Context, platform, chatID, sessionID, model, sandbox string) error {
	if model == "" {
		model = s.defaultModel
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO chats (platform, chat_id, session_id, model, sandbox, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (platform, chat_id) DO UPDATE SET
			session_id = excluded.session_id,
			model = excluded.model,
			sandbox = excluded.sandbox,
			updated_at = excluded.updated_at
	`, platform, chatID, sessionID, model, sandbox, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("save chat: %w", err)
	}
	return nil
}

// Dirs lists the directory grants ordered by path.
func (s *Store) Dirs(ctx context.Context) ([]Dir, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, mode FROM dirs ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Dir
	for rows.Next() {
		var d Dir
		if err := rows.Scan(&d.Path, &d.Mode); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// PutDir adds a directory grant or changes its mode.
func (s *Store) PutDir(ctx context.Context, path, mode string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO dirs (path, mode, created_at) VALUES (?, ?, ?)
		ON CONFLICT (path) DO UPDATE SET mode = excluded.mode
	`, path, mode, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("save dir: %w", err)
	}
	return nil
}

// DeleteDir removes a directory grant and reports whether it existed.
func (s *Store) DeleteDir(ctx context.Context, path string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM dirs WHERE path = ?`, path)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// AddTask stores a scheduled task and returns its id.
func (s *Store) AddTask(ctx context.Context, t Task) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO tasks (platform, chat_id, user_id, spec, prompt, summary, next_run, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, t.Platform, t.ChatID, t.UserID, t.Spec, t.Prompt, t.Summary,
		t.NextRun.UTC().Format(runLayout), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("save task: %w", err)
	}
	return res.LastInsertId()
}

// Tasks lists one chat's tasks ordered by id.
func (s *Store) Tasks(ctx context.Context, platform, chatID string) ([]Task, error) {
	return s.queryTasks(ctx, `WHERE platform = ? AND chat_id = ? ORDER BY id`, platform, chatID)
}

// AllTasks lists every scheduled task across all chats ordered by id.
func (s *Store) AllTasks(ctx context.Context) ([]Task, error) {
	return s.queryTasks(ctx, `ORDER BY id`)
}

// TaskByID loads a single task by id.
func (s *Store) TaskByID(ctx context.Context, id int64) (Task, bool, error) {
	tasks, err := s.queryTasks(ctx, `WHERE id = ?`, id)
	if err != nil || len(tasks) == 0 {
		return Task{}, false, err
	}
	return tasks[0], true, nil
}

// DeleteTask removes a task only if it belongs to the given chat.
func (s *Store) DeleteTask(ctx context.Context, platform, chatID string, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tasks WHERE id = ? AND platform = ? AND chat_id = ?`, id, platform, chatID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DueTasks returns unpaused tasks whose next run is at or before now.
func (s *Store) DueTasks(ctx context.Context, now time.Time) ([]Task, error) {
	return s.queryTasks(ctx, `WHERE paused = 0 AND next_run <= ? ORDER BY next_run, id`, now.UTC().Format(runLayout))
}

// SetNextRun moves a task to its next run time.
func (s *Store) SetNextRun(ctx context.Context, id int64, next time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET next_run = ? WHERE id = ?`, next.UTC().Format(runLayout), id)
	return err
}

// SetTaskPaused pauses or resumes a task and updates its next run time.
func (s *Store) SetTaskPaused(ctx context.Context, id int64, paused bool, next time.Time) error {
	flag := 0
	if paused {
		flag = 1
	}
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET paused = ?, next_run = ? WHERE id = ?`, flag, next.UTC().Format(runLayout), id)
	return err
}

// UpdateTask updates the schedule, prompt, summary and next run time of an existing task.
func (s *Store) UpdateTask(ctx context.Context, id int64, spec, prompt, summary string, next time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET spec = ?, prompt = ?, summary = ?, next_run = ?, paused = 0 WHERE id = ?`,
		spec, prompt, summary, next.UTC().Format(runLayout), id)
	return err
}

// Setting reads a persisted key-value setting.
func (s *Store) Setting(ctx context.Context, key string) (string, bool, error) {
	var val string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&val)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return val, true, nil
}

// PutSetting writes a persisted key-value setting.
func (s *Store) PutSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value)
	return err
}

func (s *Store) queryTasks(ctx context.Context, where string, args ...any) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, platform, chat_id, user_id, spec, prompt, summary, next_run, paused FROM tasks `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		var next string
		var paused int
		if err := rows.Scan(&t.ID, &t.Platform, &t.ChatID, &t.UserID, &t.Spec, &t.Prompt, &t.Summary, &next, &paused); err != nil {
			return nil, err
		}
		t.NextRun, err = time.Parse(runLayout, next)
		if err != nil {
			return nil, fmt.Errorf("task %d next_run: %w", t.ID, err)
		}
		t.Paused = paused != 0
		out = append(out, t)
	}
	return out, rows.Err()
}
