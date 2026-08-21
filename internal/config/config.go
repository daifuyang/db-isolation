// Package config loads configuration from YAML and environment variables.
//
// The server must NEVER bake in MySQL root credentials; they are read either
// from a server-side ~/.my.cnf or from the MYSQL_ADMIN_DSN environment
// variable. This package therefore exposes only operational settings — not
// secrets.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the resolved top-level configuration. Secret values are loaded
// from the environment, never from YAML.
type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Storage   StorageConfig   `yaml:"storage"`
	MySQL     MySQLConfig     `yaml:"mysql"`
	Secrets   SecretsConfig   `yaml:"secrets"`
	Audit     AuditConfig     `yaml:"audit"`
	Logging   LoggingConfig   `yaml:"logging"`
}

// ServerConfig controls how the HTTP server binds.
type ServerConfig struct {
	Addr            string `yaml:"addr"`
	ShutdownTimeout int    `yaml:"shutdown_timeout_seconds"`
}

// StorageConfig points to the SQLite metadata + audit database file.
type StorageConfig struct {
	Path string `yaml:"path"`
}

// MySQLConfig controls how the provisioner connects to MySQL as admin.
// AdminDSN is loaded from MYSQL_ADMIN_DSN — never from YAML.
type MySQLConfig struct {
	// AdminDSN holds a Go MySQL DSN such as
	//   root:secret@tcp(127.0.0.1:3306)/?parseTime=true&multiStatements=true
	// It is the only place MySQL root credentials live in memory.
	AdminDSN string `yaml:"-"`
	// AdminConfigPath is the path to a server-side my.cnf we read at boot to
	// populate AdminDSN. Default: /root/.my.cnf
	AdminConfigPath string `yaml:"admin_config_path"`
	// AllowDrop grants the DROP privilege on project_db.* to project users.
	// Enable only if your ORM/migrations require DROP TABLE.
	AllowDrop bool `yaml:"allow_drop"`
}

// SecretsConfig controls where per-project env files are written.
type SecretsConfig struct {
	Dir string `yaml:"dir"`
}

// AuditConfig controls audit log behaviour.
type AuditConfig struct {
	// ToFile, when true, also writes audit entries as JSON-lines to File.
	ToFile bool   `yaml:"to_file"`
	File   string `yaml:"file"`
}

// LoggingConfig controls log verbosity.
type LoggingConfig struct {
	Level string `yaml:"level"`
}

// Default returns a configuration populated with sensible defaults for the
// MVP single-ECS deployment.
func Default() Config {
	return Config{
		Server: ServerConfig{
			Addr:            "127.0.0.1:8787",
			ShutdownTimeout: 10,
		},
		Storage: StorageConfig{
			Path: "/var/lib/db-isolation/db-isolation.db",
		},
		MySQL: MySQLConfig{
			AdminConfigPath: "/root/.my.cnf",
		},
		Secrets: SecretsConfig{
			Dir: "/etc/db-isolation/apps",
		},
		Audit: AuditConfig{
			ToFile: true,
			File:   "/var/log/db-isolation/audit.log",
		},
		Logging: LoggingConfig{Level: "info"},
	}
}

// Load reads YAML from path (if it exists), applies defaults, and resolves
// secrets from environment. Returns the merged configuration or an error.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return cfg, fmt.Errorf("read config %q: %w", path, err)
		}
		if err == nil {
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return cfg, fmt.Errorf("parse config %q: %w", path, err)
			}
		}
	}
	if err := cfg.applyEnv(); err != nil {
		return cfg, err
	}
	cfg.expandPaths()
	return cfg, cfg.validate()
}

func (c *Config) applyEnv() error {
	if v := os.Getenv("DB_ISOLATION_ADDR"); v != "" {
		c.Server.Addr = v
	}
	if v := os.Getenv("DB_ISOLATION_STORAGE"); v != "" {
		c.Storage.Path = v
	}
	if v := os.Getenv("DB_ISOLATION_SECRETS_DIR"); v != "" {
		c.Secrets.Dir = v
	}
	if v := os.Getenv("DB_ISOLATION_LOG_LEVEL"); v != "" {
		c.Logging.Level = v
	}
	if v := os.Getenv("DB_ISOLATION_MYSQL_CNF"); v != "" {
		c.MySQL.AdminConfigPath = v
	}
	if v := os.Getenv("DB_ISOLATION_ALLOW_DROP"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("DB_ISOLATION_ALLOW_DROP: %w", err)
		}
		c.MySQL.AllowDrop = b
	}
	// MYSQL_ADMIN_DSN is the only way to inject admin credentials outside of
	// the server-side my.cnf.
	if v := os.Getenv("MYSQL_ADMIN_DSN"); v != "" {
		c.MySQL.AdminDSN = v
	}
	return nil
}

func (c *Config) expandPaths() {
	if !filepath.IsAbs(c.Storage.Path) {
		c.Storage.Path = filepath.Join("/var/lib/db-isolation", c.Storage.Path)
	}
	if !filepath.IsAbs(c.Secrets.Dir) {
		c.Secrets.Dir = filepath.Join("/etc/db-isolation/apps", c.Secrets.Dir)
	}
	if !filepath.IsAbs(c.Audit.File) {
		c.Audit.File = filepath.Join("/var/log/db-isolation", c.Audit.File)
	}
}

func (c *Config) validate() error {
	if c.Server.Addr == "" {
		return fmt.Errorf("server.addr is required")
	}
	if c.Storage.Path == "" {
		return fmt.Errorf("storage.path is required")
	}
	if c.Secrets.Dir == "" {
		return fmt.Errorf("secrets.dir is required")
	}
	level := strings.ToLower(c.Logging.Level)
	switch level {
	case "debug", "info", "warn", "error":
		c.Logging.Level = level
	default:
		return fmt.Errorf("logging.level must be one of debug/info/warn/error")
	}
	return nil
}