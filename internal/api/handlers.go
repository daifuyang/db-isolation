package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/daifuyang/db-isolation/internal/audit"
	"github.com/daifuyang/db-isolation/internal/auth"
	"github.com/daifuyang/db-isolation/internal/config"
	"github.com/daifuyang/db-isolation/internal/model"
	"github.com/daifuyang/db-isolation/internal/provision"
	"github.com/daifuyang/db-isolation/internal/store"
)

// Service is the HTTP-layer dependency on the provision workflow. Defined
// here (consumer-side) so tests can supply a fake without spinning up
// MySQL.
type Service interface {
	Create(ctx context.Context, name, remoteIP string) (provision.CreateResult, error)
	Get(ctx context.Context, name, remoteIP string) (model.DatabaseProject, error)
	List(ctx context.Context) ([]model.DatabaseProject, error)
	Rotate(ctx context.Context, name, remoteIP string) (model.DatabaseProject, error)
	Delete(ctx context.Context, name, confirm, remoteIP string) error
}

// Server bundles the HTTP handlers.
type Server struct {
	Verifier *auth.Verifier
	Service  Service
	Audit    *audit.Logger
	Logger   *slog.Logger
	Config   config.Config
}

// Routes builds an http.ServeMux with all routes registered.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", s.health)

	// Authenticated v1 routes
	mux.Handle("GET /v1/databases", s.requireAuth(http.HandlerFunc(s.listDatabases)))
	mux.Handle("GET /v1/databases/{name}", s.requireAuth(http.HandlerFunc(s.getDatabase)))
	mux.Handle("POST /v1/databases", s.requireAuth(http.HandlerFunc(s.createDatabase)))
	mux.Handle("POST /v1/databases/{name}/rotate", s.requireAuth(http.HandlerFunc(s.rotateDatabase)))
	mux.Handle("DELETE /v1/databases/{name}", s.requireAuth(http.HandlerFunc(s.deleteDatabase)))

	return logRequest(s.Logger, mux)
}

// requireAuth is middleware that extracts and validates a Bearer token.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		tok, err := s.Verifier.Lookup(r.Context(), r.Header.Get("Authorization"))
		if err != nil {
			_ = s.Audit.Write(r.Context(), model.AuditLog{
				Action: model.ActionAuthFailure, Resource: "token",
				Success: false, Message: "invalid token", RemoteIP: ip,
			})
			writeError(w, http.StatusUnauthorized, CodeUnauthorized, "invalid or missing token")
			return
		}
		_ = s.Audit.Write(r.Context(), model.AuditLog{
			Action: model.ActionAuthSuccess, Resource: "token",
			ResourceName: tok.Name, Success: true, RemoteIP: ip,
		})
		next.ServeHTTP(w, r)
	})
}

// clientIP extracts a best-effort client IP, preferring RemoteAddr.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ProjectView is the API projection of a DatabaseProject. It deliberately
// omits every credential — secret files live on the server only.
type ProjectView struct {
	Name       string `json:"name"`
	Database   string `json:"database"`
	User       string `json:"user"`
	SecretPath string `json:"secret_path"`
	Status     string `json:"status"`
	LastError  string `json:"last_error,omitempty"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

func toView(p model.DatabaseProject) ProjectView {
	return ProjectView{
		Name: p.Name, Database: p.DatabaseName, User: p.DatabaseUser,
		SecretPath: p.SecretPath, Status: p.Status, LastError: p.LastError,
		CreatedAt: p.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt: p.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}

func (s *Server) listDatabases(w http.ResponseWriter, r *http.Request) {
	items, err := s.Service.List(r.Context())
	if err != nil {
		writeInternal(w, s.Logger, err, CodeInternalError)
		return
	}
	views := make([]ProjectView, 0, len(items))
	for _, p := range items {
		views = append(views, toView(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": views})
}

func (s *Server) getDatabase(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if err := model.ValidateProjectName(name); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidProjectName, err.Error())
		return
	}
	p, err := s.Service.Get(r.Context(), name, clientIP(r))
	if err != nil {
		if mapProvisionError(w, err) {
			return
		}
		writeInternal(w, s.Logger, err, CodeInternalError)
		return
	}
	writeJSON(w, http.StatusOK, toView(p))
}

// CreateRequest is the body of POST /v1/databases.
type CreateRequest struct {
	Name string `json:"name"`
}

func (s *Server) createDatabase(w http.ResponseWriter, r *http.Request) {
	var req CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid JSON body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if err := model.ValidateProjectName(req.Name); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidProjectName, err.Error())
		return
	}
	res, err := s.Service.Create(r.Context(), req.Name, clientIP(r))
	if err != nil {
		// Surface MySQL/secret errors with their own envelope codes so
		// operators can distinguish them.
		if strings.Contains(err.Error(), "secret") {
			writeError(w, http.StatusInternalServerError, CodeSecretWriteError, "secret file write failed")
			return
		}
		if strings.Contains(err.Error(), "create database") ||
			strings.Contains(err.Error(), "create user") ||
			strings.Contains(err.Error(), "grant") ||
			strings.Contains(err.Error(), "alter user") {
			writeError(w, http.StatusBadGateway, CodeMySQLError, "MySQL provisioning failed")
			return
		}
		if mapProvisionError(w, err) {
			return
		}
		writeInternal(w, s.Logger, err, CodeInternalError)
		return
	}
	status := http.StatusCreated
	if res.Idempotent {
		status = http.StatusOK
	}
	writeJSON(w, status, toView(res.Project))
}

// DeleteRequest is the body of DELETE /v1/databases/{name}.
type DeleteRequest struct {
	Confirm string `json:"confirm"`
}

func (s *Server) rotateDatabase(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if err := model.ValidateProjectName(name); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidProjectName, err.Error())
		return
	}
	p, err := s.Service.Rotate(r.Context(), name, clientIP(r))
	if err != nil {
		if strings.Contains(err.Error(), "secret") {
			writeError(w, http.StatusInternalServerError, CodeSecretWriteError, "secret file write failed")
			return
		}
		if mapProvisionError(w, err) {
			return
		}
		writeInternal(w, s.Logger, err, CodeInternalError)
		return
	}
	writeJSON(w, http.StatusOK, toView(p))
}

func (s *Server) deleteDatabase(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if err := model.ValidateProjectName(name); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidProjectName, err.Error())
		return
	}
	var req DeleteRequest
	// DELETE may carry no body; both empty and missing body are fine.
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid JSON body")
			return
		}
	}
	if err := s.Service.Delete(r.Context(), name, req.Confirm, clientIP(r)); err != nil {
		if mapProvisionError(w, err) {
			return
		}
		writeInternal(w, s.Logger, err, CodeInternalError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// logRequest wraps a mux with a single access-log line per request. The
// line never includes the Authorization header.
func logRequest(log *slog.Logger, h http.Handler) http.Handler {
	if log == nil {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := &statusRecorder{ResponseWriter: w, status: 200}
		h.ServeHTTP(ww, r)
		log.Info("http",
			"method", r.Method, "path", r.URL.Path,
			"status", ww.status, "remote_ip", clientIP(r))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// avoid "imported and not used" if store-only deps disappear.
var _ = store.ErrNotFound