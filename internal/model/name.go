package model

import (
	"fmt"
	"regexp"
	"strings"
)

// allowed project name characters.
var projectNameRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// normalizedPrefixRegex strips non-allowed characters from a candidate to
// produce a fallback identifier used for tests / suggestions.
var normalizedPrefixRegex = regexp.MustCompile(`[^a-z0-9_-]`)

// MaxProjectNameLen caps the user-supplied project name.
const MaxProjectNameLen = 63

// ValidateProjectName enforces a strict, conservative naming policy. The same
// validator is used everywhere a project name comes from the outside (API,
// CLI, MCP). Returning an error here is the first and most important line of
// defense against injection into generated SQL identifiers and file paths.
func ValidateProjectName(name string) error {
	if name == "" {
		return fmt.Errorf("project name is required")
	}
	if len(name) > MaxProjectNameLen {
		return fmt.Errorf("project name too long (max %d)", MaxProjectNameLen)
	}
	if !projectNameRegex.MatchString(name) {
		return fmt.Errorf("project name must match %s", projectNameRegex.String())
	}
	return nil
}

// NormalizeProjectName converts arbitrary input into a safe identifier while
// preserving the original name where possible. The result is lowercase and
// has all non-allowed characters collapsed to '_'. The returned value is
// NOT validated — call ValidateProjectName afterwards.
func NormalizeProjectName(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = normalizedPrefixRegex.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_-")
	if s == "" {
		return ""
	}
	if len(s) > MaxProjectNameLen {
		s = s[:MaxProjectNameLen]
	}
	return s
}

// ProjectIdentifiers computes the canonical MySQL database and user names for
// a validated project name. It does not itself call ValidateProjectName so the
// caller may choose how to handle invalid input.
func ProjectIdentifiers(name string) (dbName, userName string) {
	safe := strings.ReplaceAll(name, "-", "_")
	return safe + "_db", safe + "_user"
}