package model

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// TokenPrefix is the public prefix every API token must carry. The token
// itself is a base64url-encoded 192-bit random value.
const TokenPrefix = "dbi_"

// RandomToken produces a new bearer token of the form dbi_<random>. The
// caller is responsible for showing it to the user exactly once and storing
// only the SHA-256 hash afterwards.
func RandomToken() (raw string, err error) {
	var buf [24]byte
	if _, err = rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

// RandomPassword produces a fresh MySQL user password.
//
// The output is at least 20 characters long and always satisfies MySQL 8's
// default MEDIUM password policy:
//   - length >= 8
//   - at least 1 uppercase, 1 lowercase, 1 digit, 1 special character
//
// Approach: generate 24 random bytes encoded as base64url (32 chars drawn
// from A-Z a-z 0-9 - _). The alphabet already contains all four character
// classes in arbitrary positions. With 32 chars, the probability that any
// given class is missing is negligible — but to make the guarantee 100%
// (and to be defensive against short base64url slices that happen to lack
// one class), we REPLACE four distinct random positions with one character
// from each required class. The replacement positions are chosen to be
// distinct so we never overwrite a class-required char with another class
// char.
func RandomPassword() (string, error) {
	const (
		lowerSet = "abcdefghijklmnopqrstuvwxyz"
		upperSet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
		digitSet = "0123456789"
		specSet  = "!@#$%^&*"
	)
	var buf [24]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	pw := []byte(base64.RawURLEncoding.EncodeToString(buf[:]))
	n := len(pw)
	if n < 8 {
		return "", fmt.Errorf("internal: base64-encoded buffer too short: %d", n)
	}
	// Pick four distinct random positions in [0, n).
	var idx [4]byte
	if _, err := rand.Read(idx[:]); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	pos := []int{int(idx[0]) % n, int(idx[1]) % n, int(idx[2]) % n, int(idx[3]) % n}
	// Deduplicate. If any two collide, swap to a safe unused slot.
	seen := map[int]bool{}
	for i, p := range pos {
		if seen[p] {
			for j := 0; j < n; j++ {
				if !seen[j] {
					pos[i] = j
					seen[j] = true
					break
				}
			}
		} else {
			seen[p] = true
		}
	}
	pw[pos[0]] = lowerSet[int(idx[0])%len(lowerSet)]
	pw[pos[1]] = upperSet[int(idx[1])%len(upperSet)]
	pw[pos[2]] = digitSet[int(idx[2])%len(digitSet)]
	pw[pos[3]] = specSet[int(idx[3])%len(specSet)]
	return string(pw), nil
}