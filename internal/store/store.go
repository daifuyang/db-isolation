// Package store persists DatabaseProjects, Tokens, and AuditLog entries to
// a single SQLite file. We use SQLite for both metadata and audit so the
// service has no other database dependency and trivially fits the MVP
// single-ECS deployment.
//
// Concurrency: SQLite serializes writes under WAL. We still guard
// project-name uniqueness with a UNIQUE constraint and use a per-project
// application lock for the create path so two simultaneous requests can
// never produce two databases or two users.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/daifuyang/db-isolation/internal/model"
)

// ErrNotFound is returned when a record does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned on unique-constraint violations.
var ErrConflict = errors.New("conflict")

// Store wraps the SQLite database and exposes typed query helpers.
type Store struct {
	db *sql.DB

	// createLocks serializes per-name create() calls. Single-process
	// deployments do not need a real distributed lock; the SQLite unique
	// constraint + this mutex is enough to keep state consistent.
	createMu    sync.Mutex
	creatingMu  map[string]*sync.Mutex
	creatingSeq uint64
}

// Open opens (or creates) the SQLite database at path and applies the schema.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %q: %w", filepath.Dir(path), err)
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite is single-writer; keep it simple.
	if err := db.PingContext(context.Background()); err != nil {
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	s := &Store{db: db, creatingMu: map[string]*sync.Mutex{}}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// DB returns the underlying *sql.DB. Reserved for tests.
func (s *Store) DB() *sql.DB { return s.db }

// Close releases the database connection.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS projects (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			name          TEXT NOT NULL UNIQUE,
			engine        TEXT NOT NULL DEFAULT 'mysql',
			database_name TEXT NOT NULL,
			database_user TEXT NOT NULL,
			secret_path   TEXT NOT NULL,
			status        TEXT NOT NULL,
			last_error    TEXT NOT NULL DEFAULT '',
			created_at    TEXT NOT NULL,
			updated_at    TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS tokens (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			name        TEXT NOT NULL,
			token_hash  TEXT NOT NULL UNIQUE,
			created_at  TEXT NOT NULL,
			last_used_at TEXT,
			revoked_at  TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS audit_logs (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			action        TEXT NOT NULL,
			resource      TEXT NOT NULL,
			resource_name TEXT NOT NULL DEFAULT '',
			success       INTEGER NOT NULL,
			message       TEXT NOT NULL DEFAULT '',
			remote_ip     TEXT NOT NULL DEFAULT '',
			created_at    TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_action ON audit_logs(action)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_created_at ON audit_logs(created_at)`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(context.Background(), q); err != nil {
			return fmt.Errorf("exec %q: %w", q, err)
		}
	}
	return nil
}

// LockCreate acquires a per-name mutex so concurrent callers creating the
// same project serialize. The returned function releases the lock.
func (s *Store) LockCreate(name string) func() {
	s.createMu.Lock()
	mu, ok := s.creatingMu[name]
	if !ok {
		mu = &sync.Mutex{}
		s.creatingMu[name] = mu
	}
	s.createMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// now returns a consistent RFC3339Nano timestamp.
func now() time.Time { return time.Now().UTC() }

func toTime(v any) (time.Time, error) {
	if v == nil {
		return time.Time{}, nil
	}
	switch x := v.(type) {
	case string:
		if x == "" {
			return time.Time{}, nil
		}
		return time.Parse(time.RFC3339Nano, x)
	case time.Time:
		return x, nil
	}
	return time.Time{}, fmt.Errorf("unexpected time column: %T", v)
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// CreateProject inserts a new DatabaseProject. Returns ErrConflict on a
// unique-constraint violation so the caller can decide between idempotent
// return and HTTP 409.
func (s *Store) CreateProject(ctx context.Context, p model.DatabaseProject) (model.DatabaseProject, error) {
	t := now()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = t
	}
	p.UpdatedAt = t
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO projects(name, engine, database_name, database_user, secret_path, status, last_error, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		p.Name, p.Engine, p.DatabaseName, p.DatabaseUser, p.SecretPath,
		p.Status, p.LastError,
		p.CreatedAt.Format(time.RFC3339Nano), p.UpdatedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return model.DatabaseProject{}, ErrConflict
		}
		return model.DatabaseProject{}, fmt.Errorf("insert project: %w", err)
	}
	return p, nil
}

// GetProject fetches a project by name.
func (s *Store) GetProject(ctx context.Context, name string) (model.DatabaseProject, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, engine, database_name, database_user, secret_path, status, last_error, created_at, updated_at
		   FROM projects WHERE name = ?`, name)
	return scanProject(row)
}

// ListProjects returns all projects ordered by creation time.
func (s *Store) ListProjects(ctx context.Context) ([]model.DatabaseProject, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, engine, database_name, database_user, secret_path, status, last_error, created_at, updated_at
		   FROM projects ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.DatabaseProject
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdateProjectStatus mutates status and last_error atomically.
func (s *Store) UpdateProjectStatus(ctx context.Context, name, status, lastErr string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE projects SET status = ?, last_error = ?, updated_at = ? WHERE name = ?`,
		status, lastErr, now().Format(time.RFC3339Nano), name,
	)
	return err
}

// DeleteProject removes a project row. Idempotent.
func (s *Store) DeleteProject(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE name = ?`, name)
	return err
}

// CreateToken inserts a new token row. Caller supplies the hash.
func (s *Store) CreateToken(ctx context.Context, name, tokenHash string) (model.Token, error) {
	t := now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens(name, token_hash, created_at) VALUES(?,?,?)`,
		name, tokenHash, t.Format(time.RFC3339Nano),
	)
	if err != nil {
		return model.Token{}, fmt.Errorf("insert token: %w", err)
	}
	return s.GetTokenByHash(ctx, tokenHash)
}

// GetTokenByHash returns the token row matching hash, or ErrNotFound.
func (s *Store) GetTokenByHash(ctx context.Context, hash string) (model.Token, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, token_hash, created_at, last_used_at, revoked_at FROM tokens WHERE token_hash = ?`, hash)
	var t model.Token
	var createdAt string
	var lastUsed, revoked sql.NullString
	if err := row.Scan(&t.ID, &t.Name, &t.TokenHash, &createdAt, &lastUsed, &revoked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Token{}, ErrNotFound
		}
		return t, err
	}
	t.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	if lastUsed.Valid {
		v, _ := time.Parse(time.RFC3339Nano, lastUsed.String)
		t.LastUsedAt = &v
	}
	if revoked.Valid {
		v, _ := time.Parse(time.RFC3339Nano, revoked.String)
		t.RevokedAt = &v
	}
	return t, nil
}

// ListTokens returns all tokens (never exposing hash material beyond what is
// already stored).
func (s *Store) ListTokens(ctx context.Context) ([]model.Token, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, token_hash, created_at, last_used_at, revoked_at FROM tokens ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Token
	for rows.Next() {
		var t model.Token
		var createdAt string
		var lastUsed, revoked sql.NullString
		if err := rows.Scan(&t.ID, &t.Name, &t.TokenHash, &createdAt, &lastUsed, &revoked); err != nil {
			return nil, err
		}
		t.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		if lastUsed.Valid {
			v, _ := time.Parse(time.RFC3339Nano, lastUsed.String)
			t.LastUsedAt = &v
		}
		if revoked.Valid {
			v, _ := time.Parse(time.RFC3339Nano, revoked.String)
			t.RevokedAt = &v
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TouchToken updates last_used_at.
func (s *Store) TouchToken(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tokens SET last_used_at = ? WHERE id = ?`,
		now().Format(time.RFC3339Nano), id,
	)
	return err
}

// RevokeToken marks a token as revoked. Idempotent.
func (s *Store) RevokeToken(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`,
		now().Format(time.RFC3339Nano), id,
	)
	return err
}

// WriteAudit appends an audit log entry.
func (s *Store) WriteAudit(ctx context.Context, e model.AuditLog) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_logs(action, resource, resource_name, success, message, remote_ip, created_at)
		 VALUES(?,?,?,?,?,?,?)`,
		e.Action, e.Resource, e.ResourceName, boolToInt(e.Success), e.Message, e.RemoteIP,
		e.CreatedAt.Format(time.RFC3339Nano),
	)
	return err
}

// ListAudit returns the most recent N audit entries.
func (s *Store) ListAudit(ctx context.Context, limit int) ([]model.AuditLog, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, action, resource, resource_name, success, message, remote_ip, created_at
		   FROM audit_logs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.AuditLog
	for rows.Next() {
		var e model.AuditLog
		var success int
		var createdAt string
		if err := rows.Scan(&e.ID, &e.Action, &e.Resource, &e.ResourceName,
			&success, &e.Message, &e.RemoteIP, &createdAt); err != nil {
			return nil, err
		}
		e.Success = success != 0
		e.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		out = append(out, e)
	}
	return out, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanProject(row rowScanner) (model.DatabaseProject, error) {
	var p model.DatabaseProject
	var createdAt, updatedAt string
	err := row.Scan(&p.ID, &p.Name, &p.Engine, &p.DatabaseName, &p.DatabaseUser,
		&p.SecretPath, &p.Status, &p.LastError, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return p, ErrNotFound
		}
		return p, err
	}
	p.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	p.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
	return p, nil
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return contains(msg, "UNIQUE constraint failed") || contains(msg, "constraint failed: UNIQUE")
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || indexOf(s, substr) >= 0)
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}