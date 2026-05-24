package threadstore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaFS embed.FS

type SQLiteStore struct {
	db *sql.DB
}

func OpenSQLite(path string) (*SQLiteStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("empty sqlite path")
	}
	dsn := filepath.Clean(path) + "?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	store := &SQLiteStore{db: db}
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	log.Printf("threadstore sqlite opened at %s", filepath.Clean(path))
	return store, nil
}

func (s *SQLiteStore) migrate(ctx context.Context) error {
	schema, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("read schema: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, string(schema)); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

func (s *SQLiteStore) UploadThread(ctx context.Context, thread *Thread) error {
	t, err := thread.normalized()
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO threads
			(id, v, created, updated_at, title, agent_mode, reasoning_effort,
			 creator_user_id, raw_json, message_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			v = excluded.v,
			created = excluded.created,
			updated_at = excluded.updated_at,
			title = excluded.title,
			agent_mode = excluded.agent_mode,
			reasoning_effort = excluded.reasoning_effort,
			creator_user_id = excluded.creator_user_id,
			raw_json = excluded.raw_json,
			message_count = excluded.message_count
		WHERE excluded.v > threads.v`,
		t.ID, t.V, t.Created, t.UpdatedAt, t.Title, t.AgentMode,
		t.ReasoningEffort, t.CreatorUserID, []byte(t.Raw), len(t.Messages))
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrVersionConflict
	}
	return nil
}

func (s *SQLiteStore) GetThread(ctx context.Context, id string) (*Thread, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT raw_json FROM threads WHERE id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return ParseThread(raw)
}

func (s *SQLiteStore) ListThreads(ctx context.Context, opts ListOptions) ([]*ThreadSummary, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, title, agent_mode, created, updated_at, message_count
		FROM threads
		ORDER BY updated_at DESC, id ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ThreadSummary
	for rows.Next() {
		var item ThreadSummary
		if err := rows.Scan(&item.ID, &item.Title, &item.AgentMode,
			&item.Created, &item.UpdatedAt, &item.MessageCount); err != nil {
			return nil, err
		}
		out = append(out, &item)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) DeleteThread(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM threads WHERE id = ?`, id)
	return err
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}
