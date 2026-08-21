package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAndReadBack(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	path, err := w.Write(Secret{Name: "opcos", Database: "opcos_db", User: "opcos_user", Password: "p4ss"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if path != filepath.Join(dir, "opcos.env") {
		t.Fatalf("unexpected path %q", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %o, want 600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "DATABASE_URL=mysql://opcos_user:p4ss@127.0.0.1:3306/opcos_db") {
		t.Fatalf("body missing DATABASE_URL: %q", body)
	}
}

func TestAtomicWriteLeavesNoTmpOnSuccess(t *testing.T) {
	dir := t.TempDir()
	w, _ := New(dir)
	if _, err := w.Write(Secret{Name: "opcos", Database: "opcos_db", User: "opcos_user", Password: "p"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("tmp file left behind: %s", e.Name())
		}
	}
}

func TestWriteRejectsInvalidIdentifier(t *testing.T) {
	dir := t.TempDir()
	w, _ := New(dir)
	if _, err := w.Write(Secret{Name: "bad name", Database: "x", User: "x", Password: "p"}); err == nil {
		t.Fatal("expected error for bad identifier")
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	w, _ := New(dir)
	_, _ = w.Write(Secret{Name: "opcos", Database: "opcos_db", User: "opcos_user", Password: "p"})
	if err := w.Delete("opcos"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := w.Delete("opcos"); err != nil {
		t.Fatalf("delete idempotent: %v", err)
	}
}

// TestWriteAndDeleteAcceptHyphen reproduces the regression where
// `iximei-kf` (a name containing '-') was rejected by validateIdentifier.
// Hyphen is legal in on-disk paths and project names.
func TestWriteAndDeleteAcceptHyphen(t *testing.T) {
	dir := t.TempDir()
	w, _ := New(dir)
	if _, err := w.Write(Secret{
		Name: "iximei-kf", Database: "iximei_kf_db",
		User: "iximei_kf_user", Password: "p",
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "iximei-kf.env")); err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err := w.Delete("iximei-kf"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}