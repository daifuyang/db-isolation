package auth

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/daifuyang/db-isolation/internal/model"
	"github.com/daifuyang/db-isolation/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "auth.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestHashTokenIsDeterministicAndStable(t *testing.T) {
	a := HashToken("dbi_xxx")
	b := HashToken("dbi_xxx")
	if a != b {
		t.Fatalf("hash not deterministic")
	}
	if len(a) != 64 {
		t.Fatalf("hash wrong length: %d", len(a))
	}
}

func TestValidToken(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	raw, _ := model.RandomToken()
	if _, err := s.CreateToken(ctx, "admin", HashToken(raw)); err != nil {
		t.Fatalf("create: %v", err)
	}
	v := NewVerifier(s)
	tok, err := v.Lookup(ctx, "Bearer "+raw)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if tok.Name != "admin" {
		t.Fatalf("name = %s", tok.Name)
	}
}

func TestWrongToken(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	raw, _ := model.RandomToken()
	if _, err := s.CreateToken(ctx, "admin", HashToken(raw)); err != nil {
		t.Fatalf("create: %v", err)
	}
	v := NewVerifier(s)
	if _, err := v.Lookup(ctx, "Bearer dbi_wrong"); err != ErrUnauthorized {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}

func TestNoHeader(t *testing.T) {
	s := openStore(t)
	v := NewVerifier(s)
	if _, err := v.Lookup(context.Background(), ""); err != ErrUnauthorized {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}

func TestRevokedToken(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	raw, _ := model.RandomToken()
	tok, _ := s.CreateToken(ctx, "admin", HashToken(raw))
	if err := s.RevokeToken(ctx, tok.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	v := NewVerifier(s)
	if _, err := v.Lookup(ctx, "Bearer "+raw); err != ErrUnauthorized {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}

func TestWrongPrefix(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	raw, _ := model.RandomToken()
	if _, err := s.CreateToken(ctx, "admin", HashToken(raw)); err != nil {
		t.Fatalf("create: %v", err)
	}
	v := NewVerifier(s)
	if _, err := v.Lookup(ctx, "Bearer xxx_"+raw[4:]); err != ErrUnauthorized {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}