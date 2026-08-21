// Package auth implements Bearer token authentication. Tokens are formatted
// as `dbi_<random>`; the server stores only the SHA-256 hash and never the
// plaintext after creation.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/daifuyang/db-isolation/internal/model"
	"github.com/daifuyang/db-isolation/internal/store"
)

// ErrUnauthorized means the request did not carry a valid bearer token.
var ErrUnauthorized = errors.New("unauthorized")

// HashToken returns the canonical SHA-256 hex digest of raw.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Verifier checks bearer tokens against the store and updates last_used_at.
type Verifier struct {
	Store *store.Store
}

// NewVerifier returns a Verifier bound to the given store.
func NewVerifier(s *store.Store) *Verifier { return &Verifier{Store: s} }

// Lookup parses Authorization and returns the matching Token, or
// ErrUnauthorized. The header MUST be `Bearer dbi_xxxx` and the prefix MUST
// be the public TokenPrefix — anything else is rejected.
func (v *Verifier) Lookup(ctx context.Context, authHeader string) (model.Token, error) {
	raw, ok := strings.CutPrefix(strings.TrimSpace(authHeader), "Bearer ")
	if !ok || raw == "" {
		return model.Token{}, ErrUnauthorized
	}
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, model.TokenPrefix) {
		return model.Token{}, ErrUnauthorized
	}
	tok, err := v.Store.GetTokenByHash(ctx, HashToken(raw))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return model.Token{}, ErrUnauthorized
		}
		return model.Token{}, err
	}
	if tok.RevokedAt != nil {
		return model.Token{}, ErrUnauthorized
	}
	// constant-time compare on the hash — store already keyed on it, but
	// keep the comparison uniform for any future refactor.
	if subtle.ConstantTimeCompare([]byte(tok.TokenHash), []byte(HashToken(raw))) != 1 {
		return model.Token{}, ErrUnauthorized
	}
	_ = v.Store.TouchToken(ctx, tok.ID)
	return tok, nil
}