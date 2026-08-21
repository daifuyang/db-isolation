// Package api wires the HTTP layer. Endpoints are intentionally thin: they
// parse, dispatch to the provision service, and translate sentinel errors
// into stable JSON error envelopes.
package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/daifuyang/db-isolation/internal/provision"
)

// ErrorEnvelope is the wire shape of every non-2xx response.
type ErrorEnvelope struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody carries a stable code and a human-readable message.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Stable error codes returned to clients.
const (
	CodeUnauthorized         = "UNAUTHORIZED"
	CodeInvalidProjectName   = "INVALID_PROJECT_NAME"
	CodeDatabaseNotFound     = "DATABASE_NOT_FOUND"
	CodeDatabaseAlreadyExists = "DATABASE_ALREADY_EXISTS"
	CodeMySQLError           = "MYSQL_ERROR"
	CodeSecretWriteError     = "SECRET_WRITE_ERROR"
	CodeConfirmationRequired = "CONFIRMATION_REQUIRED"
	CodeInternalError        = "INTERNAL_ERROR"
	CodeBadRequest           = "BAD_REQUEST"
)

// writeError emits a JSON error envelope. It deliberately does NOT include
// the underlying error text when it might contain SQL fragments or paths.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorEnvelope{Error: ErrorBody{Code: code, Message: msg}})
}

// writeInternal logs the underlying cause server-side and returns a
// sanitized envelope to the client. The internal cause is never leaked.
func writeInternal(w http.ResponseWriter, log interface{ Error(string, ...any) }, err error, code string) {
	if log != nil {
		log.Error("api", "code", code, "error", err.Error())
	}
	writeError(w, http.StatusInternalServerError, CodeInternalError, "internal error")
}

// mapProvisionError translates provision sentinel errors into HTTP status +
// envelope codes.
func mapProvisionError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, provision.ErrInvalidName):
		writeError(w, http.StatusBadRequest, CodeInvalidProjectName, err.Error())
	case errors.Is(err, provision.ErrNotFound):
		writeError(w, http.StatusNotFound, CodeDatabaseNotFound, "database project not found")
	case errors.Is(err, provision.ErrAlreadyExists):
		writeError(w, http.StatusConflict, CodeDatabaseAlreadyExists, "database project already exists")
	case errors.Is(err, provision.ErrConfirmationNeeded):
		writeError(w, http.StatusBadRequest, CodeConfirmationRequired, "confirmation required")
	case errors.Is(err, provision.ErrConfirmMismatch):
		writeError(w, http.StatusBadRequest, CodeConfirmationRequired, "confirmation does not match project name")
	default:
		// Unknown error — surface as internal so we never leak SQL or DSN.
		writeError(w, http.StatusInternalServerError, CodeInternalError, "internal error")
	}
	return true
}