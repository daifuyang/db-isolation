package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/daifuyang/db-isolation/internal/model"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "dbi.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestProjectsRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	p := model.DatabaseProject{
		Name: "opcos", Engine: "mysql",
		DatabaseName: "opcos_db", DatabaseUser: "opcos_user",
		SecretPath: "/etc/db-isolation/apps/opcos.env",
		Status:     model.StatusReady,
	}
	if _, err := s.CreateProject(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetProject(ctx, "opcos")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.DatabaseName != p.DatabaseName || got.DatabaseUser != p.DatabaseUser {
		t.Fatalf("mismatch: %+v", got)
	}
	// Re-creating must surface a conflict.
	if _, err := s.CreateProject(ctx, p); err != ErrConflict {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
}

func TestTokens(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateToken(ctx, "local-admin", "hash-abc"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.GetTokenByHash(ctx, "hash-abc"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, err := s.CreateToken(ctx, "dup", "hash-abc"); err == nil {
		t.Fatal("expected conflict")
	}
	if err := s.RevokeToken(ctx, 1); err != nil {
		t.Fatalf("revoke: %v", err)
	}
}

func TestAudit(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.WriteAudit(ctx, model.AuditLog{Action: "test", Success: true, Message: "ok"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	logs, err := s.ListAudit(ctx, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(logs) != 1 || logs[0].Action != "test" {
		t.Fatalf("unexpected logs: %+v", logs)
	}
}

func TestConcurrentCreate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const N = 8
	type result struct {
		p   model.DatabaseProject
		err error
	}
	results := make(chan result, N)
	for i := 0; i < N; i++ {
		go func() {
			release := s.LockCreate("opcos")
			defer release()
			p, err := s.CreateProject(ctx, model.DatabaseProject{
				Name: "opcos", Engine: "mysql",
				DatabaseName: "opcos_db", DatabaseUser: "opcos_user",
				SecretPath: "/tmp/opcos.env", Status: model.StatusReady,
			})
			results <- result{p, err}
		}()
	}
	successes := 0
	conflicts := 0
	for i := 0; i < N; i++ {
		r := <-results
		if r.err == nil {
			successes++
			continue
		}
		if r.err == ErrConflict {
			conflicts++
		} else {
			t.Errorf("unexpected error: %v", r.err)
		}
	}
	if successes != 1 {
		t.Errorf("expected exactly 1 success, got %d (conflicts=%d)", successes, conflicts)
	}
}