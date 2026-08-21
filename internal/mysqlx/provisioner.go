// Package mysqlx is the only place the server talks to MySQL as admin. It
// owns the privileged DSN and is the only package that issues CREATE
// DATABASE / CREATE USER / GRANT statements.
//
// Every identifier (database name, user name) that the provisioner uses is
// either derived from a validated project name via
// model.ProjectIdentifiers or comes from a sanitized literal the caller
// already vetted. We never accept caller-supplied GRANT or DDL.
package mysqlx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// Provisioner executes DDL/DCL as MySQL admin.
type Provisioner struct {
	DB        *sql.DB
	AllowDrop bool
}

// New opens an admin connection using adminDSN. The DSN is logged in a way
// that obscures the password — log it via RedactDSN if you must.
func New(adminDSN string) (*Provisioner, error) {
	if adminDSN == "" {
		return nil, errors.New("admin DSN is required")
	}
	cfg, err := mysql.ParseDSN(adminDSN)
	if err != nil {
		return nil, fmt.Errorf("parse admin DSN: %w", err)
	}
	cfg.ParseTime = true
	cfg.MultiStatements = true
	conn, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("open admin connection: %w", err)
	}
	if err := conn.PingContext(context.Background()); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ping MySQL: %w", err)
	}
	return &Provisioner{DB: conn}, nil
}

// Close releases the admin connection.
func (p *Provisioner) Close() error { return p.DB.Close() }

// EnsureDatabase creates the database if missing. Idempotent.
func (p *Provisioner) EnsureDatabase(ctx context.Context, dbName string) error {
	if err := assertIdentifier(dbName); err != nil {
		return err
	}
	q := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci", dbName)
	if _, err := p.DB.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("create database: %w", err)
	}
	return nil
}

// DropDatabase drops the database. Returns nil if it does not exist.
func (p *Provisioner) DropDatabase(ctx context.Context, dbName string) error {
	if err := assertIdentifier(dbName); err != nil {
		return err
	}
	q := fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", dbName)
	if _, err := p.DB.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("drop database: %w", err)
	}
	return nil
}

// EnsureUser creates the user on both 127.0.0.1 and localhost. The user has
// no privileges until GrantProjectPrivileges is called.
func (p *Provisioner) EnsureUser(ctx context.Context, user, password string) error {
	if err := assertIdentifier(user); err != nil {
		return err
	}
	if password == "" {
		return errors.New("password is required")
	}
	hosts := []string{"127.0.0.1", "localhost"}
	for _, host := range hosts {
		stmt := fmt.Sprintf(
			"CREATE USER IF NOT EXISTS `%s`@`%s` IDENTIFIED BY %s",
			user, host, quoteMySQLString(password),
		)
		if _, err := p.DB.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("create user@%s: %w", host, err)
		}
	}
	return nil
}

// SetUserPassword updates the password on both 127.0.0.1 and localhost.
func (p *Provisioner) SetUserPassword(ctx context.Context, user, password string) error {
	if err := assertIdentifier(user); err != nil {
		return err
	}
	if password == "" {
		return errors.New("password is required")
	}
	hosts := []string{"127.0.0.1", "localhost"}
	for _, host := range hosts {
		stmt := fmt.Sprintf("ALTER USER `%s`@`%s` IDENTIFIED BY %s",
			user, host, quoteMySQLString(password))
		if _, err := p.DB.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("alter user@%s: %w", host, err)
		}
	}
	return nil
}

// DropUser removes the user from both hosts.
func (p *Provisioner) DropUser(ctx context.Context, user string) error {
	if err := assertIdentifier(user); err != nil {
		return err
	}
	hosts := []string{"127.0.0.1", "localhost"}
	for _, host := range hosts {
		stmt := fmt.Sprintf("DROP USER IF EXISTS `%s`@`%s`", user, host)
		if _, err := p.DB.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("drop user@%s: %w", host, err)
		}
	}
	return nil
}

// GrantProjectPrivileges grants the minimum set of privileges needed to run
// an application against its own database. No GRANT OPTION, no global
// privileges, no DROP DATABASE / CREATE USER.
//
// If allowDrop is set (MySQLConfig.AllowDrop), DROP is also granted so
// that ORM migrations (e.g. drop-and-recreate tables) can succeed.
func (p *Provisioner) GrantProjectPrivileges(ctx context.Context, dbName, user string, allowDrop bool) error {
	if err := assertIdentifier(dbName); err != nil {
		return err
	}
	if err := assertIdentifier(user); err != nil {
		return err
	}
	privs := []string{"SELECT", "INSERT", "UPDATE", "DELETE", "CREATE", "ALTER", "INDEX", "REFERENCES"}
	if allowDrop {
		privs = append(privs, "DROP")
	}
	privList := strings.Join(privs, ", ")
	hosts := []string{"127.0.0.1", "localhost"}
	for _, host := range hosts {
		stmt := fmt.Sprintf("GRANT %s ON `%s`.* TO `%s`@`%s`",
			privList, dbName, user, host)
		if _, err := p.DB.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("grant@%s: %w", host, err)
		}
	}
	if _, err := p.DB.ExecContext(ctx, "FLUSH PRIVILEGES"); err != nil {
		return fmt.Errorf("flush privileges: %w", err)
	}
	return nil
}

// VerifyProjectIsolation logs in as the project user and confirms it can
// USE the project database and that it cannot reach a sibling database or
// execute CREATE USER. Used by the integration test suite.
func (p *Provisioner) VerifyProjectIsolation(ctx context.Context, dbName, user, password string) error {
	dsn := fmt.Sprintf("%s:%s@tcp(127.0.0.1:3306)/?parseTime=true",
		user, password)
	if err := assertIdentifier(user); err != nil {
		return err
	}
	if err := assertIdentifier(dbName); err != nil {
		return err
	}
	conn, err := sql.Open("mysql", dsn)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `"+dbName+"`"); err != nil {
		return fmt.Errorf("use db: %w", err)
	}
	// Should succeed: SELECT on a non-existent table is allowed; CREATE
	// TABLE is allowed.
	if _, err := conn.ExecContext(ctx,
		"CREATE TABLE IF NOT EXISTS _isolation_probe (id INT)"); err != nil {
		return fmt.Errorf("create probe: %w", err)
	}
	// Should fail: CREATE USER.
	if _, err := conn.ExecContext(ctx, "CREATE USER `nope`@`localhost`"); err == nil {
		return errors.New("project user was able to CREATE USER (privilege too broad)")
	}
	// Should fail: access to another database.
	if _, err := conn.ExecContext(ctx, "USE mysql"); err == nil {
		return errors.New("project user was able to USE mysql database")
	}
	return nil
}

// Helpers ----------------------------------------------------------------

// assertIdentifier permits the same character class as project names —
// letters, digits, and underscore. ProjectIdentifiers already converts
// hyphens to underscores, so a validated project name always produces a
// safe identifier here.
func assertIdentifier(s string) error {
	if s == "" {
		return errors.New("identifier is required")
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_':
		default:
			return fmt.Errorf("invalid identifier %q", s)
		}
	}
	return nil
}

// quoteMySQLString renders a literal that is safe inside a single-quoted
// SQL string. We do not allow arbitrary bytes — only printable ASCII
// without backslash, single quote, double quote, NUL, or newline. We
// also bound length.
func quoteMySQLString(s string) string {
	if !isSafePasswordLiteral(s) {
		// Defensive: passwords are generated server-side by
		// model.RandomPassword which produces only base64url +
		// limited special characters. We refuse rather than escape,
		// because escaping bugs here would be a privilege-escalation
		// vector.
		panic("refusing to embed unsafe literal in SQL")
	}
	return "'" + s + "'"
}

// isSafePasswordLiteral accepts:
//   - ASCII letters (A-Z, a-z)
//   - digits (0-9)
//   - the special characters "!@#$%^&*" used to satisfy MySQL's MEDIUM
//     password policy
//   - hyphen and underscore for compatibility with the older
//     base64url-only password format
//
// It REJECTS:
//   - single quote, double quote, backslash, NUL, newline (SQL injection
//     vectors)
//   - any byte > 0x7E (multi-byte UTF-8 could break the MySQL parser on
//     some server builds)
func isSafePasswordLiteral(s string) bool {
	if s == "" {
		return false
	}
	if len(s) > 128 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		case r == '!' || r == '@' || r == '#' || r == '$' || r == '%' || r == '^' || r == '&' || r == '*':
		default:
			return false
		}
	}
	return true
}

// Ping performs a connectivity check.
func (p *Provisioner) Ping(ctx context.Context) error { return p.DB.PingContext(ctx) }

// SetAllowDrop toggles DROP privilege at runtime. Tests use this.
func (p *Provisioner) SetAllowDrop(b bool) { p.AllowDrop = b }