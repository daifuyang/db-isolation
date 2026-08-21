package audit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daifuyang/db-isolation/internal/config"
	"github.com/daifuyang/db-isolation/internal/model"
	"github.com/daifuyang/db-isolation/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "audit.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestAuditWritesToSQLite(t *testing.T) {
	s := openStore(t)
	l, err := New(config.AuditConfig{ToFile: false}, s)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := l.Write(context.Background(), model.AuditLog{Action: "test", Success: true}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	logs, _ := s.ListAudit(context.Background(), 10)
	if len(logs) != 1 {
		t.Fatalf("expected 1, got %d", len(logs))
	}
}

func TestAuditWritesToFile(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t)
	auditPath := filepath.Join(dir, "audit.log")
	l, err := New(config.AuditConfig{ToFile: true, File: auditPath}, s)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := l.Write(context.Background(), model.AuditLog{Action: "test", Success: true, Message: "hi"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), `"action":"test"`) {
		t.Fatalf("audit log not written: %q", string(data))
	}
}