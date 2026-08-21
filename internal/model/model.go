// Package model defines core domain types shared across packages.
package model

import "time"

// DatabaseProject represents a per-project isolated database and user.
type DatabaseProject struct {
	ID           int64
	Name         string
	Engine       string
	DatabaseName string
	DatabaseUser string
	SecretPath   string
	Status       string // pending | ready | error | deleted
	LastError    string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Token represents an API bearer token (only hash is stored).
type Token struct {
	ID         int64
	Name       string
	TokenHash  string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// AuditLog records a security-relevant operation.
type AuditLog struct {
	ID           int64
	Action       string // e.g. database.create
	Resource     string // e.g. database
	ResourceName string // e.g. opcos
	Success      bool
	Message      string
	RemoteIP     string
	CreatedAt    time.Time
}

// Status constants for DatabaseProject.
const (
	StatusPending = "pending"
	StatusReady   = "ready"
	StatusError   = "error"
	StatusDeleted = "deleted"
)

// Audit actions.
const (
	ActionDatabaseCreate = "database.create"
	ActionDatabaseList   = "database.list"
	ActionDatabaseGet    = "database.get"
	ActionDatabaseRotate = "database.rotate"
	ActionDatabaseDelete = "database.delete"
	ActionAuthSuccess    = "auth.success"
	ActionAuthFailure    = "auth.failure"
)