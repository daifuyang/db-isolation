package api

import (
	"context"
	"log/slog"

	"github.com/daifuyang/db-isolation/internal/audit"
	"github.com/daifuyang/db-isolation/internal/auth"
	"github.com/daifuyang/db-isolation/internal/config"
	"github.com/daifuyang/db-isolation/internal/model"
)

// NewServer is a thin constructor exported so non-test code (in particular
// the CLI tests in cmd/dbi) can build an HTTP handler without touching
// unexported fields.
func NewServer(v *auth.Verifier, svc Service, au *audit.Logger, lg *slog.Logger, cfg config.Config) *Server {
	return &Server{Verifier: v, Service: svc, Audit: au, Logger: lg, Config: cfg}
}

// Avoid unused imports if someone later trims.
var (
	_ = context.Background
	_ = model.TokenPrefix
)