// Package provision implements the create / rotate / delete / list workflow
// that ties together the metadata store, the MySQL admin connection, and
// the per-project secret writer.
package provision

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/daifuyang/db-isolation/internal/audit"
	"github.com/daifuyang/db-isolation/internal/config"
	"github.com/daifuyang/db-isolation/internal/model"
	"github.com/daifuyang/db-isolation/internal/mysqlx"
	"github.com/daifuyang/db-isolation/internal/secrets"
	"github.com/daifuyang/db-isolation/internal/store"
)

// Sentinel errors returned by the service. The HTTP layer translates these
// into stable error codes.
var (
	ErrInvalidName       = errors.New("invalid project name")
	ErrAlreadyExists     = errors.New("database project already exists")
	ErrNotFound          = errors.New("database project not found")
	ErrConfirmationNeeded = errors.New("confirmation required")
	ErrConfirmMismatch   = errors.New("confirmation does not match project name")
)

// Service coordinates database lifecycle operations.
type Service struct {
	Store     *store.Store
	MySQL     *mysqlx.Provisioner
	Secrets   *secrets.Writer
	Audit     *audit.Logger
	Logger    *slog.Logger
	Config    config.MySQLConfig

	// inflight tracks per-project locks across multiple concurrent
	// operations. Held separately from store.LockCreate so that rotate and
	// delete also benefit from serialization against concurrent creates.
	mu       sync.Mutex
	inflight map[string]*sync.Mutex
}

// New constructs a Service.
func New(st *store.Store, my *mysqlx.Provisioner, sc *secrets.Writer,
	au *audit.Logger, log *slog.Logger, cfg config.MySQLConfig) *Service {
	return &Service{
		Store: st, MySQL: my, Secrets: sc, Audit: au,
		Logger: log, Config: cfg,
		inflight: map[string]*sync.Mutex{},
	}
}

// lock returns a per-name mutex so all operations on a project are
// serialized within a single process.
func (s *Service) lock(name string) func() {
	s.mu.Lock()
	m, ok := s.inflight[name]
	if !ok {
		m = &sync.Mutex{}
		s.inflight[name] = m
	}
	s.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// CreateResult is the API-friendly view of a project after a create.
type CreateResult struct {
	Project   model.DatabaseProject
	Idempotent bool // true if returned because the project already existed
}

// Create provisions a new project database idempotently. If a project with
// the same name already exists, the existing record is returned along with
// Idempotent=true; the call never creates duplicates.
func (s *Service) Create(ctx context.Context, name string, remoteIP string) (CreateResult, error) {
	if err := model.ValidateProjectName(name); err != nil {
		s.auditFailure(ctx, model.ActionDatabaseCreate, name, remoteIP, err.Error())
		return CreateResult{}, ErrInvalidName
	}
	release := s.Store.LockCreate(name)
	defer release()
	ioLock := s.lock(name)
	defer ioLock()

	if existing, err := s.Store.GetProject(ctx, name); err == nil {
		if existing.Status == model.StatusReady {
			s.auditSuccess(ctx, model.ActionDatabaseCreate, name, remoteIP, "idempotent")
			return CreateResult{Project: existing, Idempotent: true}, nil
		}
		// If a previous attempt left the row in `error` or `pending`, we
		// refuse — operator must delete the failed record or fix the
		// underlying MySQL state. This is the conservative choice.
		return CreateResult{}, fmt.Errorf("project %q exists with status %q; delete it first", name, existing.Status)
	} else if !errors.Is(err, store.ErrNotFound) {
		return CreateResult{}, err
	}

	dbName, dbUser := model.ProjectIdentifiers(name)
	secretPath := s.Secrets.Path(name)

	p := model.DatabaseProject{
		Name:         name,
		Engine:       "mysql",
		DatabaseName: dbName,
		DatabaseUser: dbUser,
		SecretPath:   secretPath,
		Status:       model.StatusPending,
	}
	if _, err := s.Store.CreateProject(ctx, p); err != nil {
		if errors.Is(err, store.ErrConflict) {
			// Concurrent create beat us — fetch the existing row.
			if existing, gerr := s.Store.GetProject(ctx, name); gerr == nil {
				s.auditSuccess(ctx, model.ActionDatabaseCreate, name, remoteIP, "idempotent")
				return CreateResult{Project: existing, Idempotent: true}, nil
			}
			return CreateResult{}, ErrAlreadyExists
		}
		s.auditFailure(ctx, model.ActionDatabaseCreate, name, remoteIP, err.Error())
		return CreateResult{}, err
	}

	password, err := model.RandomPassword()
	if err != nil {
		s.markError(ctx, name, err)
		return CreateResult{}, err
	}

	if err := s.MySQL.EnsureDatabase(ctx, dbName); err != nil {
		s.markError(ctx, name, err)
		s.auditFailure(ctx, model.ActionDatabaseCreate, name, remoteIP, err.Error())
		return CreateResult{}, err
	}
	if err := s.MySQL.EnsureUser(ctx, dbUser, password); err != nil {
		// Best-effort rollback of the database to avoid leaving orphan
		// DBs on failure.
		_ = s.MySQL.DropDatabase(ctx, dbName)
		s.markError(ctx, name, err)
		s.auditFailure(ctx, model.ActionDatabaseCreate, name, remoteIP, err.Error())
		return CreateResult{}, err
	}
	if err := s.MySQL.GrantProjectPrivileges(ctx, dbName, dbUser, s.Config.AllowDrop); err != nil {
		_ = s.MySQL.DropUser(ctx, dbUser)
		_ = s.MySQL.DropDatabase(ctx, dbName)
		s.markError(ctx, name, err)
		s.auditFailure(ctx, model.ActionDatabaseCreate, name, remoteIP, err.Error())
		return CreateResult{}, err
	}
	if _, err := s.Secrets.Write(secrets.Secret{
		Name: name, Database: dbName, User: dbUser, Password: password,
	}); err != nil {
		// MySQL state is now consistent but the env file is missing —
		// record an error so an operator can retry the secret write.
		_ = s.Store.UpdateProjectStatus(ctx, name, model.StatusError, err.Error())
		s.auditFailure(ctx, model.ActionDatabaseCreate, name, remoteIP, err.Error())
		return CreateResult{}, err
	}
	if err := s.Store.UpdateProjectStatus(ctx, name, model.StatusReady, ""); err != nil {
		s.auditFailure(ctx, model.ActionDatabaseCreate, name, remoteIP, err.Error())
		return CreateResult{}, err
	}
	final, err := s.Store.GetProject(ctx, name)
	if err != nil {
		s.auditFailure(ctx, model.ActionDatabaseCreate, name, remoteIP, err.Error())
		return CreateResult{}, err
	}
	s.auditSuccess(ctx, model.ActionDatabaseCreate, name, remoteIP, "created")
	return CreateResult{Project: final}, nil
}

// Rotate generates a new password, updates MySQL and the secret file
// atomically. The MySQL user is granted the same privileges as before.
func (s *Service) Rotate(ctx context.Context, name, remoteIP string) (model.DatabaseProject, error) {
	if err := model.ValidateProjectName(name); err != nil {
		s.auditFailure(ctx, model.ActionDatabaseRotate, name, remoteIP, err.Error())
		return model.DatabaseProject{}, ErrInvalidName
	}
	ioLock := s.lock(name)
	defer ioLock()

	p, err := s.Store.GetProject(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.auditFailure(ctx, model.ActionDatabaseRotate, name, remoteIP, ErrNotFound.Error())
			return model.DatabaseProject{}, ErrNotFound
		}
		return model.DatabaseProject{}, err
	}
	password, err := model.RandomPassword()
	if err != nil {
		return model.DatabaseProject{}, err
	}
	// Update MySQL first. If the env file write fails afterwards, the
	// operator can re-run rotate — the secret file is the source of
	// truth for application credentials.
	if err := s.MySQL.SetUserPassword(ctx, p.DatabaseUser, password); err != nil {
		s.auditFailure(ctx, model.ActionDatabaseRotate, name, remoteIP, err.Error())
		return model.DatabaseProject{}, err
	}
	if _, err := s.Secrets.Write(secrets.Secret{
		Name: name, Database: p.DatabaseName, User: p.DatabaseUser, Password: password,
	}); err != nil {
		s.auditFailure(ctx, model.ActionDatabaseRotate, name, remoteIP, err.Error())
		return model.DatabaseProject{}, err
	}
	s.auditSuccess(ctx, model.ActionDatabaseRotate, name, remoteIP, "rotated")
	return s.Store.GetProject(ctx, name)
}

// Delete drops the database, user, and secret file. Requires confirmation
// string equal to name (case-sensitive).
func (s *Service) Delete(ctx context.Context, name, confirm, remoteIP string) error {
	if err := model.ValidateProjectName(name); err != nil {
		s.auditFailure(ctx, model.ActionDatabaseDelete, name, remoteIP, err.Error())
		return ErrInvalidName
	}
	if confirm == "" {
		s.auditFailure(ctx, model.ActionDatabaseDelete, name, remoteIP, ErrConfirmationNeeded.Error())
		return ErrConfirmationNeeded
	}
	if confirm != name {
		s.auditFailure(ctx, model.ActionDatabaseDelete, name, remoteIP, ErrConfirmMismatch.Error())
		return ErrConfirmMismatch
	}
	ioLock := s.lock(name)
	defer ioLock()

	p, err := s.Store.GetProject(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.auditFailure(ctx, model.ActionDatabaseDelete, name, remoteIP, ErrNotFound.Error())
			return ErrNotFound
		}
		return err
	}

	// Step 1: revoke privileges to disconnect any active sessions and
	// prevent further writes. We then drop database and user.
	var partial []string
	if err := s.MySQL.DropDatabase(ctx, p.DatabaseName); err != nil {
		partial = append(partial, "drop_database: "+err.Error())
	}
	if err := s.MySQL.DropUser(ctx, p.DatabaseUser); err != nil {
		partial = append(partial, "drop_user: "+err.Error())
	}
	if err := s.Secrets.Delete(name); err != nil {
		partial = append(partial, "delete_secret: "+err.Error())
	}
	if err := s.Store.DeleteProject(ctx, name); err != nil {
		partial = append(partial, "delete_metadata: "+err.Error())
	}
	if len(partial) > 0 {
		msg := "partial failure: " + joinComma(partial)
		s.auditFailure(ctx, model.ActionDatabaseDelete, name, remoteIP, msg)
		return errors.New(msg)
	}
	s.auditSuccess(ctx, model.ActionDatabaseDelete, name, remoteIP, "deleted")
	return nil
}

// Get returns the project record by name.
func (s *Service) Get(ctx context.Context, name, remoteIP string) (model.DatabaseProject, error) {
	p, err := s.Store.GetProject(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.auditFailure(ctx, model.ActionDatabaseGet, name, remoteIP, ErrNotFound.Error())
			return p, ErrNotFound
		}
		return p, err
	}
	return p, nil
}

// List returns all projects.
func (s *Service) List(ctx context.Context) ([]model.DatabaseProject, error) {
	return s.Store.ListProjects(ctx)
}

// auditSuccess records a successful operation in the audit log.
func (s *Service) auditSuccess(ctx context.Context, action, name, ip, msg string) {
	_ = s.Audit.Write(ctx, model.AuditLog{
		Action:       action,
		Resource:     "database",
		ResourceName: name,
		Success:      true,
		Message:      msg,
		RemoteIP:     ip,
	})
}

func (s *Service) auditFailure(ctx context.Context, action, name, ip, msg string) {
	_ = s.Audit.Write(ctx, model.AuditLog{
		Action:       action,
		Resource:     "database",
		ResourceName: name,
		Success:      false,
		Message:      msg,
		RemoteIP:     ip,
	})
	if s.Logger != nil {
		s.Logger.Warn("audit",
			"action", action, "resource_name", name,
			"remote_ip", ip, "message", msg)
	}
}

// markError persists an error state on the project row.
func (s *Service) markError(ctx context.Context, name string, err error) {
	_ = s.Store.UpdateProjectStatus(ctx, name, model.StatusError, err.Error())
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}