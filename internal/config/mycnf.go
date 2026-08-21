package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Mycnf represents a minimal subset of a MySQL client config file.
type Mycnf struct {
	User     string
	Password string
	Host     string
	Port     string
	Socket   string
}

// DSN renders the parsed values as a Go MySQL DSN. The DSN MUST NOT be
// printed by callers — it is a secret equivalent to the MySQL root password.
func (m Mycnf) DSN() string {
	var userInfo strings.Builder
	if m.User != "" {
		userInfo.WriteString(m.User)
		if m.Password != "" {
			userInfo.WriteByte(':')
			userInfo.WriteString(m.Password)
		}
		userInfo.WriteByte('@')
	}
	if m.Socket != "" {
		return fmt.Sprintf("%sunix(%s)/?parseTime=true&multiStatements=true",
			userInfo.String(), m.Socket)
	}
	host := m.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := m.Port
	if port == "" {
		port = "3306"
	}
	return fmt.Sprintf("%stcp(%s:%s)/?parseTime=true&multiStatements=true",
		userInfo.String(), host, port)
}

// LoadMycnf parses a server-side my.cnf file. We only honour the [client]
// section because that is the conventional location for the MySQL admin
// credentials on a single-host deployment.
func LoadMycnf(path string) (Mycnf, error) {
	var out Mycnf
	f, err := os.Open(path) // #nosec G304 — operator-controlled path
	if err != nil {
		return out, fmt.Errorf("open my.cnf: %w", err)
	}
	defer f.Close()

	inClient := false
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inClient = strings.EqualFold(line, "[client]")
			continue
		}
		if !inClient {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(strings.ToLower(key))
		val = strings.TrimSpace(val)
		switch key {
		case "user":
			out.User = val
		case "password":
			out.Password = val
		case "host":
			out.Host = val
		case "port":
			out.Port = val
		case "socket":
			out.Socket = val
		}
	}
	if err := scanner.Err(); err != nil {
		return out, fmt.Errorf("scan my.cnf: %w", err)
	}
	return out, nil
}