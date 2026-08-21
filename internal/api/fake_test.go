package api

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/daifuyang/db-isolation/internal/model"
	"github.com/daifuyang/db-isolation/internal/provision"
	"github.com/daifuyang/db-isolation/internal/secrets"
	"github.com/daifuyang/db-isolation/internal/store"
)

// fakeService implements the api.Service contract without involving MySQL.
// It is enough for HTTP-layer tests.
type fakeService struct {
	mu      sync.Mutex
	store   *store.Store
	secrets *secrets.Writer
}

// buildService assembles a fakeService for tests.
func buildService(st *store.Store, sc *secrets.Writer) Service {
	return &fakeService{store: st, secrets: sc}
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
	secretPath, err := f.secrets.Write(secrets.Secret{
		Name: name, Database: dbName, User: dbUser, Password: pwd,
	})
	if err != nil {
		return provision.CreateResult{}, fmt.Errorf("secret: %w", err)
	}
	p := model.DatabaseProject{
		Name: name, Engine: "mysql",
		DatabaseName: dbName, DatabaseUser: dbUser,
		SecretPath: secretPath, Status: model.StatusReady,
	}
	stored, err := f.store.CreateProject(ctx, p)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			if g, gerr := f.store.GetProject(ctx, name); gerr == nil {
				return provision.CreateResult{Project: g, Idempotent: true}, nil
			}
		}
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
	if err := model.ValidateProjectName(name); err != nil {
		return model.DatabaseProject{}, err
	}
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
	if _, err := f.secrets.Write(secrets.Secret{
		Name: name, Database: p.DatabaseName, User: p.DatabaseUser, Password: pwd,
	}); err != nil {
		return p, err
	}
	return p, nil
}

func (f *fakeService) Delete(ctx context.Context, name, confirm, _ string) error {
	if err := model.ValidateProjectName(name); err != nil {
		return err
	}
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