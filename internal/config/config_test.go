package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Server.Addr != "127.0.0.1:8787" {
		t.Errorf("addr = %q", c.Server.Addr)
	}
	if c.MySQL.AdminConfigPath == "" {
		t.Errorf("missing default my.cnf path")
	}
	if c.Logging.Level != "info" {
		t.Errorf("level = %q", c.Logging.Level)
	}
}

func TestLoadFromYAML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := writeFile(p, `
server:
  addr: 127.0.0.1:9000
  shutdown_timeout_seconds: 5
storage:
  path: `+dir+`/dbi.db
secrets:
  dir: `+dir+`/apps
logging:
  level: debug
mysql:
  admin_config_path: /tmp/my.cnf
  allow_drop: true
`); err != nil {
		t.Fatalf("write: %v", err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Server.Addr != "127.0.0.1:9000" {
		t.Errorf("addr = %q", c.Server.Addr)
	}
	if c.Logging.Level != "debug" {
		t.Errorf("level = %q", c.Logging.Level)
	}
	if !c.MySQL.AllowDrop {
		t.Errorf("allow_drop = false")
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	t.Setenv("DB_ISOLATION_ADDR", "127.0.0.1:9999")
	t.Setenv("DB_ISOLATION_LOG_LEVEL", "warn")
	t.Setenv("MYSQL_ADMIN_DSN", "root:s3cret@tcp(127.0.0.1:3306)/")
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Server.Addr != "127.0.0.1:9999" {
		t.Errorf("addr = %q", c.Server.Addr)
	}
	if c.Logging.Level != "warn" {
		t.Errorf("level = %q", c.Logging.Level)
	}
	if c.MySQL.AdminDSN == "" {
		t.Errorf("admin DSN empty")
	}
}

func TestLoadMycnf(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "my.cnf")
	if err := writeFile(p, "[client]\nuser=root\npassword=s3cret\nhost=127.0.0.1\nport=3306\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	mc, err := LoadMycnf(p)
	if err != nil {
		t.Fatalf("LoadMycnf: %v", err)
	}
	if mc.User != "root" || mc.Password != "s3cret" || mc.Host != "127.0.0.1" || mc.Port != "3306" {
		t.Errorf("parsed = %+v", mc)
	}
	if !strings.HasPrefix(mc.DSN(), "root:") {
		t.Errorf("DSN missing user prefix: %q", redactSecret(mc.DSN()))
	}
}

func TestLoadInvalidLogLevel(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	if err := writeFile(p, "logging:\n  level: silly\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected validation error")
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

func redactSecret(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			for j := i + 1; j < len(s); j++ {
				if s[j] == '@' {
					return s[:i+1] + "***" + s[j:]
				}
			}
		}
	}
	return s
}