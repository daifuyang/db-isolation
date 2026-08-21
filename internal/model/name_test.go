package model

import (
	"strings"
	"testing"
)

func TestValidateProjectName(t *testing.T) {
	cases := []struct {
		in    string
		valid bool
	}{
		{"opcos", true},
		{"iximei-kf", true},
		{"abc_def", true},
		{"with-123", true},
		{"a", true},
		{"", false},
		{"../../../etc", false},
		{"opcos;DROP DATABASE", false},
		{"opcos`x`", false},
		{"opcos user", false},
		{"opcos.root", false},
		{"OpCos", false},     // uppercase not allowed
		{"opcos!", false},    // punctuation not allowed
		{"-leading", false},  // must start with alnum
		{"_leading", false},  // must start with alnum
		{string(make([]byte, 70)), false}, // too long
	}
	for _, c := range cases {
		err := ValidateProjectName(c.in)
		if c.valid && err != nil {
			t.Errorf("ValidateProjectName(%q) = %v, want nil", c.in, err)
		}
		if !c.valid && err == nil {
			t.Errorf("ValidateProjectName(%q) = nil, want error", c.in)
		}
	}
}

func TestProjectIdentifiers(t *testing.T) {
	cases := []struct {
		in           string
		wantDB, wantU string
	}{
		{"opcos", "opcos_db", "opcos_user"},
		{"iximei-kf", "iximei_kf_db", "iximei_kf_user"},
		{"a", "a_db", "a_user"},
	}
	for _, c := range cases {
		gotDB, gotU := ProjectIdentifiers(c.in)
		if gotDB != c.wantDB || gotU != c.wantU {
			t.Errorf("ProjectIdentifiers(%q) = (%q,%q), want (%q,%q)",
				c.in, gotDB, gotU, c.wantDB, c.wantU)
		}
	}
}

func TestNormalizeProjectName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"OpCos", "opcos"},
		{"iximei kf", "iximei_kf"},
		{"  trim me  ", "trim_me"},
		{"@@weird!!", "weird"},
	}
	for _, c := range cases {
		if got := NormalizeProjectName(c.in); got != c.want {
			t.Errorf("NormalizeProjectName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRandomTokenFormat(t *testing.T) {
	for i := 0; i < 10; i++ {
		tok, err := RandomToken()
		if err != nil {
			t.Fatalf("RandomToken: %v", err)
		}
		if len(tok) < len(TokenPrefix)+16 {
			t.Errorf("token %q too short", tok)
		}
		if tok[:len(TokenPrefix)] != TokenPrefix {
			t.Errorf("token %q missing prefix", tok)
		}
	}
}

func TestRandomPasswordFormat(t *testing.T) {
	p, err := RandomPassword()
	if err != nil {
		t.Fatalf("RandomPassword: %v", err)
	}
	if len(p) < 16 {
		t.Errorf("password too short: %q", p)
	}
}

// TestRandomPasswordSatisfiesMysqlMedium asserts the generated password
// meets MySQL 8's default MEDIUM policy: length>=8 plus one upper, one
// lower, one digit, one special character.
func TestRandomPasswordSatisfiesMysqlMedium(t *testing.T) {
	for i := 0; i < 100; i++ {
		p, err := RandomPassword()
		if err != nil {
			t.Fatalf("RandomPassword: %v", err)
		}
		if len(p) < 8 {
			t.Fatalf("password too short: %q", p)
		}
		var hasUpper, hasLower, hasDigit, hasSpecial bool
		for _, r := range p {
			switch {
			case r >= 'A' && r <= 'Z':
				hasUpper = true
			case r >= 'a' && r <= 'z':
				hasLower = true
			case r >= '0' && r <= '9':
				hasDigit = true
			case strings.ContainsRune("!@#$%^&*", r):
				hasSpecial = true
			}
		}
		if !(hasUpper && hasLower && hasDigit && hasSpecial) {
			t.Errorf("password does not satisfy MEDIUM: upper=%v lower=%v digit=%v special=%v: %q",
				hasUpper, hasLower, hasDigit, hasSpecial, p)
		}
	}
}