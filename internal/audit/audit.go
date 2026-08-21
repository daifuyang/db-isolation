// Package audit writes structured audit log entries to both SQLite and
// (optionally) a JSON-lines file.
//
// The package NEVER includes secrets (passwords, DSNs, tokens) in messages.
// Callers are responsible for crafting messages without sensitive data.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/daifuyang/db-isolation/internal/config"
	"github.com/daifuyang/db-isolation/internal/model"
	"github.com/daifuyang/db-isolation/internal/store"
)

// Logger writes audit records. Safe for concurrent use.
type Logger struct {
	cfg     config.AuditConfig
	store   *store.Store
	mu      sync.Mutex
	out     io.WriteCloser
	closers []io.Closer
}

// New constructs a Logger. If to-file is enabled, it opens File (creating
// parent directories) for append-only writes.
func New(cfg config.AuditConfig, st *store.Store) (*Logger, error) {
	l := &Logger{cfg: cfg, store: st}
	if cfg.ToFile && cfg.File != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.File), 0o750); err != nil {
			return nil, fmt.Errorf("mkdir audit dir: %w", err)
		}
		f, err := os.OpenFile(cfg.File, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
		if err != nil {
			return nil, fmt.Errorf("open audit file: %w", err)
		}
		l.out = f
		l.closers = append(l.closers, f)
	}
	return l, nil
}

// Close releases any open file handles.
func (l *Logger) Close() error {
	var errs []error
	for _, c := range l.closers {
		if err := c.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Entry is the JSON-serialisable shape of an audit record. It mirrors the
// database row but is independent of internal types so that the on-disk
// schema can evolve without breaking the JSON contract.
type Entry struct {
	Timestamp    string `json:"timestamp"`
	Action       string `json:"action"`
	Resource     string `json:"resource"`
	ResourceName string `json:"resource_name"`
	Success      bool   `json:"success"`
	Message      string `json:"message"`
	RemoteIP     string `json:"remote_ip"`
}

// Write persists an entry. Errors are non-fatal — the caller may log them
// but should not fail the user-facing operation just because audit failed.
// We still return the error so the HTTP layer can decide.
func (l *Logger) Write(ctx context.Context, e model.AuditLog) error {
	now := time.Now().UTC()
	if e.CreatedAt.IsZero() {
		e.CreatedAt = now
	}
	if err := l.store.WriteAudit(ctx, e); err != nil {
		return fmt.Errorf("sqlite audit: %w", err)
	}
	if l.out != nil {
		entry := Entry{
			Timestamp:    e.CreatedAt.Format(time.RFC3339Nano),
			Action:       e.Action,
			Resource:     e.Resource,
			ResourceName: e.ResourceName,
			Success:      e.Success,
			Message:      e.Message,
			RemoteIP:     e.RemoteIP,
		}
		buf, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		buf = append(buf, '\n')
		l.mu.Lock()
		_, err = l.out.Write(buf)
		l.mu.Unlock()
		if err != nil {
			return fmt.Errorf("file audit: %w", err)
		}
	}
	return nil
}