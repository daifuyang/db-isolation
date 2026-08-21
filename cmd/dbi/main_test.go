package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/daifuyang/db-isolation/internal/api"
	"github.com/daifuyang/db-isolation/internal/audit"
	"github.com/daifuyang/db-isolation/internal/auth"
	"github.com/daifuyang/db-isolation/internal/config"
	"github.com/daifuyang/db-isolation/internal/model"
	"github.com/daifuyang/db-isolation/internal/provision"
	"github.com/daifuyang/db-isolation/internal/secrets"
	"github.com/daifuyang/db-isolation/internal/store"
)

// e2eServer wires up the real HTTP API and a fake (no-MySQL) service so we
// can exercise the CLI binary's full request path end-to-end.
func e2eServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "dbi.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Default()
	cfg.Storage.Path = filepath.Join(dir, "dbi.db")
	cfg.Audit = config.AuditConfig{ToFile: false}
	au, err := audit.New(cfg.Audit, st)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	t.Cleanup(func() { _ = au.Close() })
	sc, err := secrets.New(filepath.Join(dir, "apps"))
	if err != nil {
		t.Fatalf("secrets: %v", err)
	}
	fs := &fakeService{store: st, secrets: sc}
	raw, _ := model.RandomToken()
	if _, err := st.CreateToken(context.Background(), "cli", auth.HashToken(raw)); err != nil {
		t.Fatalf("token: %v", err)
	}

	srv := api.NewServer(auth.NewVerifier(st), fs, au,
		slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
	hs := httptest.NewServer(srv.Routes())
	t.Cleanup(hs.Close)
	return hs, raw
}

// runCLI captures stdout/stderr and returns (out, errMessage). It exits via
// the public CLI dispatcher and returns the dispatch exit code.
func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	oldArgv := os.Args
	oldStdout := os.Stdout
	oldStderr := os.Stderr
	t.Cleanup(func() {
		os.Args = oldArgv
		os.Stdout = oldStdout
		os.Stderr = oldStderr
	})

	os.Args = append([]string{"dbi"}, args...)

	stdoutR, stdoutW, _ := os.Pipe()
	stderrR, stderrW, _ := os.Pipe()
	os.Stdout = stdoutW
	os.Stderr = stderrW

	code := dispatchMain()

	_ = stdoutW.Close()
	_ = stderrW.Close()
	var so, se strings.Builder
	io.Copy(&so, stdoutR)
	io.Copy(&se, stderrR)
	if code != 0 {
		return so.String(), &cliErr{code: code, msg: se.String()}
	}
	return so.String(), nil
}

type cliErr struct {
	code int
	msg  string
}

func (c *cliErr) Error() string { return c.msg }

func TestCLIListEmpty(t *testing.T) {
	ts, tok := e2eServer(t)
	out, err := runCLI(t, "--url", ts.URL, "--token", tok, "list")
	if err != nil {
		t.Fatalf("cli: %v", err)
	}
	if !strings.Contains(out, "(no databases)") {
		t.Fatalf("unexpected output: %s", out)
	}
}

func TestCLICreateAndList(t *testing.T) {
	ts, tok := e2eServer(t)
	out, err := runCLI(t, "--url", ts.URL, "--token", tok, "create", "opcos")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, want := range []string{"Project: opcos", "Database: opcos_db", "User: opcos_user", "Status: ready"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(strings.ToLower(out), "password") {
		t.Errorf("output leaks password: %s", out)
	}
	out, err = runCLI(t, "--url", ts.URL, "--token", tok, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "opcos") {
		t.Errorf("list missing opcos:\n%s", out)
	}
}

func TestCLIDeleteRequiresConfirm(t *testing.T) {
	ts, tok := e2eServer(t)
	if _, err := runCLI(t, "--url", ts.URL, "--token", tok, "create", "opcos"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := runCLI(t, "--url", ts.URL, "--token", tok, "delete", "opcos"); err == nil {
		t.Fatalf("expected delete without confirm to fail")
	}
	if _, err := runCLI(t, "--url", ts.URL, "--token", tok, "delete", "opcos", "--confirm", "wrong"); err == nil {
		t.Fatalf("expected delete with wrong confirm to fail")
	}
	out, err := runCLI(t, "--url", ts.URL, "--token", tok, "delete", "opcos", "--confirm", "opcos")
	if err != nil {
		t.Fatalf("delete ok: %v", err)
	}
	if !strings.Contains(out, "Deleted") {
		t.Errorf("unexpected output: %s", out)
	}
}

func TestCLIJSON(t *testing.T) {
	ts, tok := e2eServer(t)
	if _, err := runCLI(t, "--url", ts.URL, "--token", tok, "create", "opcos"); err != nil {
		t.Fatalf("create: %v", err)
	}
	out, err := runCLI(t, "--url", ts.URL, "--token", tok, "--json", "status", "opcos")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &doc); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, out)
	}
	if doc["name"] != "opcos" {
		t.Fatalf("name = %v", doc["name"])
	}
}

// fakeService mirrors the api fakeService but uses the local createResult
// alias instead of provision.CreateResult to avoid the import.
type fakeService struct {
	mu      sync.Mutex
	store   *store.Store
	secrets *secrets.Writer
}

func (f *fakeService) Create(ctx context.Context, name, _ string) (provision.CreateResult, error) {
	if err := model.ValidateProjectName(name); err != nil {
		return provision.CreateResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, err := f.store.GetProject(ctx, name); err == nil {
		return provision.CreateResult{Project: p, Idempotent: true}, nil
	}
	dbName, dbUser := model.ProjectIdentifiers(name)
	pwd, err := model.RandomPassword()
	if err != nil {
		return provision.CreateResult{}, err
	}
	path, err := f.secrets.Write(secrets.Secret{Name: name, Database: dbName, User: dbUser, Password: pwd})
	if err != nil {
		return provision.CreateResult{}, err
	}
	p := model.DatabaseProject{
		Name: name, Engine: "mysql",
		DatabaseName: dbName, DatabaseUser: dbUser,
		SecretPath: path, Status: model.StatusReady,
	}
	stored, err := f.store.CreateProject(ctx, p)
	if err != nil {
		return provision.CreateResult{}, err
	}
	return provision.CreateResult{Project: stored}, nil
}

func (f *fakeService) Get(ctx context.Context, name, _ string) (model.DatabaseProject, error) {
	return f.store.GetProject(ctx, name)
}

func (f *fakeService) List(ctx context.Context) ([]model.DatabaseProject, error) {
	return f.store.ListProjects(ctx)
}

func (f *fakeService) Rotate(ctx context.Context, name, _ string) (model.DatabaseProject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, err := f.store.GetProject(ctx, name)
	if err != nil {
		return p, err
	}
	pwd, err := model.RandomPassword()
	if err != nil {
		return p, err
	}
	if _, err := f.secrets.Write(secrets.Secret{Name: name, Database: p.DatabaseName, User: p.DatabaseUser, Password: pwd}); err != nil {
		return p, err
	}
	return p, nil
}

func (f *fakeService) Delete(ctx context.Context, name, confirm, _ string) error {
	if confirm == "" {
		return provision.ErrConfirmationNeeded
	}
	if confirm != name {
		return provision.ErrConfirmMismatch
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.secrets.Delete(name); err != nil {
		return err
	}
	return f.store.DeleteProject(ctx, name)
}

// createResult alias kept for tests that may still reference it.
type createResult = provision.CreateResult

var (
	_ = provision.ErrConfirmationNeeded
	_ = errors.New
	_ = http.MethodGet
)