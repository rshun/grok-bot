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

// Memory is one persistent user instruction or fact scoped to a chat.
type Memory struct {
	ID      int64
	Content string
}

// SavedSession is one archived or active Grok conversation thread.
type SavedSession struct {
	ID        int64
	Platform  string
	ChatID    string
	SessionID string
	Title     string
	Model     string
	Sandbox   string
	UpdatedAt time.Time
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
		CREATE TABLE IF NOT EXISTS memories (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			platform TEXT NOT NULL,
			chat_id TEXT NOT NULL,
			content TEXT NOT NULL,
			created_at TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS memories_chat ON memories (platform, chat_id, id);
		CREATE TABLE IF NOT EXISTS sessions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			platform TEXT NOT NULL,
			chat_id TEXT NOT NULL,
			session_id TEXT NOT NULL,
			title TEXT NOT NULL,
			model TEXT NOT NULL,
			sandbox TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL,
			UNIQUE (platform, chat_id, session_id)
		);
		CREATE INDEX IF NOT EXISTS sessions_chat ON sessions (platform, chat_id, updated_at DESC);
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

// DeleteTaskByID removes a scheduled task by ID regardless of originating platform/chat.
func (s *Store) DeleteTaskByID(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, id)
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

// AddMemory stores a persistent memory for one chat if not already present, returning its ID and whether it was newly added.
func (s *Store) AddMemory(ctx context.Context, platform, chatID, content string) (int64, bool, error) {
	var existingID int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM memories WHERE platform = ? AND chat_id = ? AND content = ? ORDER BY id LIMIT 1`,
		platform, chatID, content,
	).Scan(&existingID)
	if err == nil {
		return existingID, false, nil
	}
	if err != sql.ErrNoRows {
		return 0, false, err
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO memories (platform, chat_id, content, created_at)
		VALUES (?, ?, ?, ?)
	`, platform, chatID, content, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, false, fmt.Errorf("save memory: %w", err)
	}
	id, err := res.LastInsertId()
	return id, true, err
}

// Memories returns one chat's persistent memories ordered by ID.
func (s *Store) Memories(ctx context.Context, platform, chatID string) ([]Memory, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, content FROM memories WHERE platform = ? AND chat_id = ? ORDER BY id`,
		platform, chatID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		var m Memory
		if err := rows.Scan(&m.ID, &m.Content); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteMemory deletes a single memory belonging to the given chat and returns its content if found.
func (s *Store) DeleteMemory(ctx context.Context, platform, chatID string, id int64) (string, bool, error) {
	var content string
	err := s.db.QueryRowContext(ctx,
		`SELECT content FROM memories WHERE id = ? AND platform = ? AND chat_id = ?`,
		id, platform, chatID,
	).Scan(&content)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM memories WHERE id = ? AND platform = ? AND chat_id = ?`,
		id, platform, chatID,
	)
	if err != nil {
		return "", false, err
	}
	n, err := res.RowsAffected()
	return content, n > 0, err
}

// ClearMemories deletes all persistent memories for one chat and returns the count removed.
func (s *Store) ClearMemories(ctx context.Context, platform, chatID string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM memories WHERE platform = ? AND chat_id = ?`, platform, chatID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// AllMemories returns all persistent memories across platforms ordered by ID.
func (s *Store) AllMemories(ctx context.Context) ([]Memory, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, content FROM memories ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		var m Memory
		if err := rows.Scan(&m.ID, &m.Content); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AddGlobalMemory stores a persistent memory shared across platforms if not already present.
func (s *Store) AddGlobalMemory(ctx context.Context, platform, chatID, content string) (int64, bool, error) {
	var existingID int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM memories WHERE content = ? ORDER BY id LIMIT 1`,
		content,
	).Scan(&existingID)
	if err == nil {
		return existingID, false, nil
	}
	if err != sql.ErrNoRows {
		return 0, false, err
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO memories (platform, chat_id, content, created_at)
		VALUES (?, ?, ?, ?)
	`, platform, chatID, content, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, false, fmt.Errorf("save memory: %w", err)
	}
	id, err := res.LastInsertId()
	return id, true, err
}

// DeleteMemoryByID deletes a single memory by ID across platforms and returns its content if found.
func (s *Store) DeleteMemoryByID(ctx context.Context, id int64) (string, bool, error) {
	var content string
	err := s.db.QueryRowContext(ctx, `SELECT content FROM memories WHERE id = ?`, id).Scan(&content)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM memories WHERE id = ?`, id)
	if err != nil {
		return "", false, err
	}
	n, err := res.RowsAffected()
	return content, n > 0, err
}

// ClearAllMemories deletes all persistent memories across platforms and returns the count removed.
func (s *Store) ClearAllMemories(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM memories`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

const maxSavedSessionsPerChat = 20

// SaveSessionHistory records or updates a session in the chat's session history.
// When overwriteTitle is false and an entry already has a non-empty title, the existing title is kept.
func (s *Store) SaveSessionHistory(ctx context.Context, platform, chatID, sessionID, title, model, sandbox string, overwriteTitle bool) (int64, error) {
	if sessionID == "" {
		return 0, nil
	}
	if model == "" {
		model = s.defaultModel
	}
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	var id int64
	var existingTitle string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, title FROM sessions WHERE platform = ? AND chat_id = ? AND session_id = ?`,
		platform, chatID, sessionID,
	).Scan(&id, &existingTitle)
	if err == nil {
		finalTitle := existingTitle
		if overwriteTitle && title != "" {
			finalTitle = title
		} else if finalTitle == "" {
			finalTitle = title
		}
		_, err = s.db.ExecContext(ctx,
			`UPDATE sessions SET title = ?, model = ?, sandbox = ?, updated_at = ? WHERE id = ?`,
			finalTitle, model, sandbox, nowStr, id,
		)
		return id, err
	}
	if err != sql.ErrNoRows {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (platform, chat_id, session_id, title, model, sandbox, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, platform, chatID, sessionID, title, model, sandbox, nowStr)
	if err != nil {
		return 0, fmt.Errorf("save session history: %w", err)
	}
	id, err = res.LastInsertId()
	if err != nil {
		return 0, err
	}
	// Keep only the most recent maxSavedSessionsPerChat entries per chat.
	_, _ = s.db.ExecContext(ctx, `
		DELETE FROM sessions
		WHERE platform = ? AND chat_id = ? AND id NOT IN (
			SELECT id FROM sessions WHERE platform = ? AND chat_id = ?
			ORDER BY updated_at DESC, id DESC LIMIT ?
		)
	`, platform, chatID, platform, chatID, maxSavedSessionsPerChat)
	return id, nil
}

// Sessions lists saved sessions for one chat, most recently active first.
func (s *Store) Sessions(ctx context.Context, platform, chatID string) ([]SavedSession, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, platform, chat_id, session_id, title, model, sandbox, updated_at FROM sessions WHERE platform = ? AND chat_id = ? ORDER BY updated_at DESC, id DESC`,
		platform, chatID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SavedSession
	for rows.Next() {
		var item SavedSession
		var rawTime string
		if err := rows.Scan(&item.ID, &item.Platform, &item.ChatID, &item.SessionID, &item.Title, &item.Model, &item.Sandbox, &rawTime); err != nil {
			return nil, err
		}
		if t, parseErr := time.Parse(time.RFC3339Nano, rawTime); parseErr == nil {
			item.UpdatedAt = t
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// AllSessions lists saved sessions across all platforms/chats deduplicated by session_id
// (keeping the most recently active entry for each Grok session), up to maxSavedSessionsPerChat.
func (s *Store) AllSessions(ctx context.Context) ([]SavedSession, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, platform, chat_id, session_id, title, model, sandbox, updated_at FROM sessions ORDER BY updated_at DESC, id DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SavedSession
	seen := make(map[string]int) // session_id -> index in out
	for rows.Next() {
		var item SavedSession
		var rawTime string
		if err := rows.Scan(&item.ID, &item.Platform, &item.ChatID, &item.SessionID, &item.Title, &item.Model, &item.Sandbox, &rawTime); err != nil {
			return nil, err
		}
		if t, parseErr := time.Parse(time.RFC3339Nano, rawTime); parseErr == nil {
			item.UpdatedAt = t
		}
		if idx, exists := seen[item.SessionID]; exists {
			if out[idx].Title == "" && item.Title != "" {
				out[idx].Title = item.Title
			}
			continue
		}
		if len(out) >= maxSavedSessionsPerChat {
			continue
		}
		seen[item.SessionID] = len(out)
		out = append(out, item)
	}
	return out, rows.Err()
}

// SessionByID loads one saved session belonging to the given chat.
func (s *Store) SessionByID(ctx context.Context, platform, chatID string, id int64) (SavedSession, bool, error) {
	var item SavedSession
	var rawTime string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, platform, chat_id, session_id, title, model, sandbox, updated_at FROM sessions WHERE id = ? AND platform = ? AND chat_id = ?`,
		id, platform, chatID,
	).Scan(&item.ID, &item.Platform, &item.ChatID, &item.SessionID, &item.Title, &item.Model, &item.Sandbox, &rawTime)
	if err == sql.ErrNoRows {
		return SavedSession{}, false, nil
	}
	if err != nil {
		return SavedSession{}, false, err
	}
	if t, parseErr := time.Parse(time.RFC3339Nano, rawTime); parseErr == nil {
		item.UpdatedAt = t
	}
	return item, true, nil
}

// SessionByIDAny loads one saved session by ID regardless of originating platform/chat,
// enabling cross-platform session handoff between Telegram and Discord.
func (s *Store) SessionByIDAny(ctx context.Context, id int64) (SavedSession, bool, error) {
	var item SavedSession
	var rawTime string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, platform, chat_id, session_id, title, model, sandbox, updated_at FROM sessions WHERE id = ?`,
		id,
	).Scan(&item.ID, &item.Platform, &item.ChatID, &item.SessionID, &item.Title, &item.Model, &item.Sandbox, &rawTime)
	if err == sql.ErrNoRows {
		return SavedSession{}, false, nil
	}
	if err != nil {
		return SavedSession{}, false, err
	}
	if t, parseErr := time.Parse(time.RFC3339Nano, rawTime); parseErr == nil {
		item.UpdatedAt = t
	}
	return item, true, nil
}

// ActivateSession switches a chat's active session, model, and sandbox to a saved session.
func (s *Store) ActivateSession(ctx context.Context, platform, chatID string, saved SavedSession) error {
	if err := s.upsert(ctx, platform, chatID, saved.SessionID, saved.Model, saved.Sandbox); err != nil {
		return err
	}
	_, _ = s.SaveSessionHistory(ctx, platform, chatID, saved.SessionID, saved.Title, saved.Model, saved.Sandbox, false)
	return nil
}

// DeleteSavedSession deletes one saved session for a chat and returns its session_id if deleted.
func (s *Store) DeleteSavedSession(ctx context.Context, platform, chatID string, id int64) (string, bool, error) {
	var sessionID string
	err := s.db.QueryRowContext(ctx,
		`SELECT session_id FROM sessions WHERE id = ? AND platform = ? AND chat_id = ?`,
		id, platform, chatID,
	).Scan(&sessionID)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE id = ? AND platform = ? AND chat_id = ?`,
		id, platform, chatID,
	)
	if err != nil {
		return "", false, err
	}
	n, err := res.RowsAffected()
	return sessionID, n > 0, err
}

// DeleteSavedSessionByID deletes a saved session by ID across platforms (removing all history
// entries sharing that session_id and detaching any other chat pointing to it).
func (s *Store) DeleteSavedSessionByID(ctx context.Context, id int64) (string, bool, error) {
	var sessionID string
	err := s.db.QueryRowContext(ctx, `SELECT session_id FROM sessions WHERE id = ?`, id).Scan(&sessionID)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE session_id = ?`, sessionID)
	if err != nil {
		return "", false, err
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE chats SET session_id = '', sandbox = '' WHERE session_id = ?`, sessionID)
	n, err := res.RowsAffected()
	return sessionID, n > 0, err
}

// KnownChats returns previously active chat IDs on a platform ordered by most recent update.
func (s *Store) KnownChats(ctx context.Context, platform string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT chat_id FROM chats WHERE platform = ? ORDER BY updated_at DESC`,
		platform,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
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
